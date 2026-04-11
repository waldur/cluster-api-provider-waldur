/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkg/errors"
	infrastructurev1beta1 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1beta1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	waldurclient "github.com/waldur/go-client"

	util "sigs.k8s.io/cluster-api/util"

	uuid "github.com/google/uuid"
	openapitypes "github.com/oapi-codegen/runtime/types"
)

// WaldurClusterReconciler reconciles a WaldurCluster object
type WaldurClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Waldur waldurclient.ClientWithResponses
}

func (r *WaldurClusterReconciler) getOrCreateProject(ctx context.Context, org *waldurclient.Customer, projectSlug string) (*waldurclient.Project, error) {
	projectName := fmt.Sprintf("%s_%s", *org.Slug, projectSlug)
	customerUuids := []openapitypes.UUID{*org.Uuid}
	projectResponse, err := r.Waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{
		NameExact: &projectName,
		Customer:  &customerUuids,
	})

	if err != nil {
		return nil, err
	}

	projects := *projectResponse.JSON200

	if len(projects) < 1 {
		// create a project
		projectData := waldurclient.ProjectsCreateJSONRequestBody{
			Name:     projectName,
			Customer: *org.Url,
		}
		projectCreateResponse, err := r.Waldur.ProjectsCreateWithResponse(ctx, projectData)

		if err != nil {
			return nil, err
		}

		return projectCreateResponse.JSON201, nil
	} else {
		return &projects[0], nil
	}

}

func (r *WaldurClusterReconciler) submitTenantCreationOrder(ctx context.Context, offering *waldurclient.PublicOfferingDetails, project *waldurclient.Project) (*waldurclient.OrderDetails, error) {
	orderType := waldurclient.Create
	
	subnetCidr := "192.168.42.0/24" // TODO: make configurable
	rawAttrs := waldurclient.OpenStackTenantCreateOrderAttributes{
		Name: "test-00",
		SubnetCidr: &subnetCidr,
	}

	attrs := waldurclient.OrderCreateRequest_Attributes{}
	err := attrs.FromOpenStackTenantCreateOrderAttributes(rawAttrs)
	if err != nil {
		return nil, err
	}

	limits := map[string]int{ // TODO: calculate based on node configs
		"core": 8,
		"ram": 16 * 1024,
		"storage": 50 * 1024,
	}

	plans := *offering.Plans
	plan := plans[0] // TODO: select a plan based on user input
	planUrl := plan.Url
	acceptingTermsOfService := true

	orderPayload := waldurclient.MarketplaceOrdersCreateJSONRequestBody{
		Type:       &orderType,
		Offering:   *offering.Url,
		Project:    *project.Url,
		Plan:       planUrl,
		Attributes: &attrs,
		Limits:     &limits,
		AcceptingTermsOfService: &acceptingTermsOfService,
	}

	orderResponse, err := r.Waldur.MarketplaceOrdersCreateWithResponse(ctx, orderPayload)
	if err != nil {
		return nil, err
	}

	if orderResponse.StatusCode() != 201 {
		body := string(orderResponse.Body[:])
		return nil, errors.New(fmt.Sprintf("Unable to submit an order, details: %s", body))
	}

	return orderResponse.JSON201, nil
}

func (r *WaldurClusterReconciler) refreshTenant(ctx context.Context, existing infrastructurev1beta1.OpenStackTenant) infrastructurev1beta1.OpenStackTenant {
	tenantUuid, err := uuid.Parse(existing.Uuid)
	if err != nil {
		return existing
	}
	refreshed, err := r.getOpenStackTenant(ctx, &tenantUuid)
	if err != nil {
		return existing
	}
	return infrastructurev1beta1.OpenStackTenant{
		Uuid:  refreshed.Uuid.String(),
		State: *refreshed.State,
	}
}

func (r *WaldurClusterReconciler) createTenant(ctx context.Context, offeringSlug string, project *waldurclient.Project) (*infrastructurev1beta1.WaldurOrder, *infrastructurev1beta1.OpenStackTenant, error) {
	offering, err := r.getOffering(ctx, offeringSlug)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to get offering details")
	}

	order, err := r.submitTenantCreationOrder(ctx, offering, project)
	if err != nil || order == nil {
		return nil, nil, errors.Wrap(err, "unable to submit order")
	}
	waldurOrder := &infrastructurev1beta1.WaldurOrder{
		State:        *order.State,
		ResourceUuid: order.MarketplaceResourceUuid.String(),
	}

	tenant, err := r.getOpenStackTenant(ctx, order.ResourceUuid)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to get tenant")
	}
	openStackTenant := &infrastructurev1beta1.OpenStackTenant{
		Uuid:  tenant.Uuid.String(),
		State: *tenant.State,
	}

	return waldurOrder, openStackTenant, nil
}

func (r *WaldurClusterReconciler) getOffering(ctx context.Context, offeringSlug string) (*waldurclient.PublicOfferingDetails, error) {
	offeringParams := &waldurclient.MarketplacePublicOfferingsListParams{
		Slug: &offeringSlug,
	}
	offeringResponse, err := r.Waldur.MarketplacePublicOfferingsListWithResponse(ctx, offeringParams)
	if err != nil {
		return nil, err
	}

	offerings := *offeringResponse.JSON200
	if len(offerings) == 0 {
		msg := fmt.Sprintf("Unable to find an offering with slug %s", offeringSlug)
		return nil, errors.New(msg)
	}

	offering := offerings[0]

	return &offering, nil
}

func (r *WaldurClusterReconciler) getOpenStackTenant(ctx context.Context, tenantUuid *openapitypes.UUID) (*waldurclient.OpenStackTenant, error) {
	tenantResponse, err := r.Waldur.OpenstackTenantsRetrieveWithResponse(ctx, *tenantUuid, &waldurclient.OpenstackTenantsRetrieveParams{})
	if err != nil {
		return nil, err
	}

	tenant := tenantResponse.JSON200

	return tenant, nil
}

func (r *WaldurClusterReconciler) getCustomer(ctx context.Context, orgSlug string) (*waldurclient.Customer, error) {
	fieldFiter := []waldurclient.CustomerFieldEnum{
		waldurclient.CustomerFieldEnumUrl,
		waldurclient.CustomerFieldEnumSlug,
		waldurclient.CustomerFieldEnumUuid,
	}
	params := waldurclient.CustomersListParams{
		Slug:  &orgSlug,
		Field: &fieldFiter,
	}
	orgResponse, err := r.Waldur.CustomersListWithResponse(ctx, &params)

	if err != nil {
		return nil, err
	}

	if orgResponse.StatusCode() != 200 {
		body := string(orgResponse.Body[:])
		return nil, errors.New(fmt.Sprintf("Unable to get %s org, reason: %s", orgSlug, body))
	}

	orgs := *orgResponse.JSON200

	if len(orgs) == 0 {
		return nil, errors.New(fmt.Sprintf("Unable to fing an org with %s slug", orgSlug))
	} else {
		return &orgs[0], nil
	}
}

// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=machines;machines/status,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the WaldurCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *WaldurClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var waldurCluster infrastructurev1beta1.WaldurCluster
	if err := r.Get(ctx, req.NamespacedName, &waldurCluster); err != nil {
		if apierrors.IsNotFound(err) { // WaldurCluster isn't found
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cluster, err := util.GetOwnerCluster(ctx, r.Client, waldurCluster.ObjectMeta)

	if err != nil {
		return ctrl.Result{}, err
	}

	// cluster is nil when the CAPI core Cluster controller hasn't set the ownerReference yet.
	// This is a normal race at creation time — we return without requeuing because the
	// ownerReference being set will trigger a new watch event and re-enqueue this reconciler.
	if cluster == nil {
		log.Info("Waiting for Cluster Controller to set OwnerRef on WaldurCluster")
		return ctrl.Result{}, nil
	}

	// Check if all tenants are in OK state
	if waldurCluster.Status.Tenants != nil {
		// Check tenant statuses
		tenantsOk := 0
		for _, tenant := range waldurCluster.Status.Tenants {
			if tenant.State == waldurclient.CoreStatesOK {
				tenantsOk++
			}
		}
		if tenantsOk == len(waldurCluster.Spec.Offerings) {
			return ctrl.Result{}, nil
		}
	}

	if len(waldurCluster.Status.Conditions) == 0 {
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionUnknown,
			Reason:  "Reconciling",
			Message: "Started reconciliation",
		})
		if err := r.Status().Update(ctx, &waldurCluster); err != nil {
			log.Error(err, "Failed to update cluster status")
			return ctrl.Result{}, err
		}
	}

	orgSlug := waldurCluster.Spec.Organization
	org, err := r.getCustomer(ctx, *orgSlug)

	if err != nil {
		return ctrl.Result{}, err
	}

	projectSlug := waldurCluster.Spec.Project
	project, err := r.getOrCreateProject(ctx, org, *projectSlug)

	if err != nil {
		return ctrl.Result{}, err
	}

	tenantOfferings := waldurCluster.Spec.Offerings

	ordersNew := maps.Clone(waldurCluster.Status.Orders)
	tenantsNew := maps.Clone(waldurCluster.Status.Tenants)

	for _, offeringSlug := range tenantOfferings {
		if existing, ok := waldurCluster.Status.Tenants[offeringSlug]; ok {
			tenantsNew[offeringSlug] = r.refreshTenant(ctx, existing)
			continue
		}

		order, tenant, err := r.createTenant(ctx, offeringSlug, project)
		if err != nil {
			log.Error(err, "Unable to create tenant", "offering", offeringSlug)
			continue
		}
		ordersNew[offeringSlug] = *order
		tenantsNew[offeringSlug] = *tenant
	}

	base := waldurCluster.DeepCopy()
	waldurCluster.Status.Orders = ordersNew
	waldurCluster.Status.Tenants = tenantsNew
	if err := r.Status().Patch(ctx, &waldurCluster, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "couldn't patch status for cluster %q", waldurCluster.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WaldurClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.WaldurCluster{}).
		Named("waldurcluster").
		Complete(r)
}
