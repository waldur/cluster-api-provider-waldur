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
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkg/errors"
	infrastructurev1beta2 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1beta2"
	"k8s.io/utils/ptr"

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
	tenantName := fmt.Sprintf("%s-%s", *offering.Slug, *project.Name)
	rawAttrs := waldurclient.OpenStackTenantCreateOrderAttributes{
		Name:       tenantName,
		SubnetCidr: &subnetCidr,
	}

	attrs := waldurclient.OrderCreateRequest_Attributes{}
	err := attrs.FromOpenStackTenantCreateOrderAttributes(rawAttrs)
	if err != nil {
		return nil, err
	}

	limits := map[string]int{ // TODO: calculate based on node configs
		"cores":   8,
		"ram":     16 * 1024,
		"storage": 50 * 1024,
	}

	plans := *offering.Plans
	plan := plans[0] // TODO: select a plan based on user input
	planUrl := plan.Url
	acceptingTermsOfService := true

	orderPayload := waldurclient.MarketplaceOrdersCreateJSONRequestBody{
		Type:                    &orderType,
		Offering:                *offering.Url,
		Project:                 *project.Url,
		Plan:                    planUrl,
		Attributes:              &attrs,
		Limits:                  &limits,
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

func (r *WaldurClusterReconciler) refreshTenant(ctx context.Context, existing *infrastructurev1beta2.OpenStackTenant) error {
	if existing.Order != nil && !isOrderTerminal(existing.Order.State) {
		if err := r.refreshOrder(ctx, existing.Order); err != nil {
			return errors.Wrap(err, "unable to refresh order")
		}
		// If the order just got a tenant UUID, populate it on the tenant
		if existing.Uuid == nil && existing.Order.ResourceUuid != nil {
			existing.Uuid = existing.Order.ResourceUuid
		}
	}

	if existing.Uuid == nil {
		if existing.Order != nil && existing.Order.ResourceUuid == nil {
			return nil
		} else {
			existing.Uuid = existing.Order.ResourceUuid
		}
	}

	tenantUuid, err := uuid.Parse(*existing.Uuid)
	if err != nil {
		return err
	}
	refreshed, err := r.getOpenStackTenant(ctx, &tenantUuid)
	if err != nil {
		return err
	}

	existing.State = *refreshed.State
	existing.Name = *refreshed.Name
	return nil
}

func isOrderTerminal(state waldurclient.OrderState) bool {
	return state == waldurclient.OrderStateDone ||
		state == waldurclient.OrderStateErred ||
		state == waldurclient.OrderStateCanceled
}

func (r *WaldurClusterReconciler) refreshOrder(ctx context.Context, existingOrder *infrastructurev1beta2.WaldurOrder) error {
	orderUuid, err := uuid.Parse(existingOrder.Uuid)
	if err != nil {
		return err
	}

	orderResponse, err := r.Waldur.MarketplaceOrdersRetrieveWithResponse(ctx, orderUuid, &waldurclient.MarketplaceOrdersRetrieveParams{})
	if err != nil {
		return err
	}

	order := orderResponse.JSON200

	existingOrder.State = *order.State

	if order.ResourceUuid != nil {
		tenantUuid := order.ResourceUuid.String()
		existingOrder.ResourceUuid = &tenantUuid
	}

	return nil
}

func (r *WaldurClusterReconciler) createTenant(ctx context.Context, offeringSlug string, project *waldurclient.Project) (*infrastructurev1beta2.OpenStackTenant, error) {
	offering, err := r.getOffering(ctx, offeringSlug)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get offering details")
	}

	order, err := r.submitTenantCreationOrder(ctx, offering, project)
	if err != nil || order == nil {
		return nil, errors.Wrap(err, "unable to submit order")
	}

	waldurOrder := &infrastructurev1beta2.WaldurOrder{
		Uuid:                    order.Uuid.String(),
		Type:                    *order.Type,
		State:                   *order.State,
		MarketplaceResourceUuid: order.MarketplaceResourceUuid.String(),
	}

	tenantUuid := order.ResourceUuid
	openStackTenant := &infrastructurev1beta2.OpenStackTenant{
		Order: waldurOrder,
		Name:  *order.ResourceName,
	}

	if tenantUuid == nil {
		openStackTenant.Uuid = nil
		openStackTenant.State = waldurclient.CoreStatesCREATING
	} else {
		tenantUuidStr := tenantUuid.String()
		waldurOrder.ResourceUuid = &tenantUuidStr

		tenant, err := r.getOpenStackTenant(ctx, tenantUuid)
		if err != nil {
			return nil, errors.Wrap(err, "unable to get tenant")
		}
		openStackTenant.Uuid = &tenantUuidStr
		openStackTenant.State = *tenant.State
	}

	return openStackTenant, nil
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

	var waldurCluster infrastructurev1beta2.WaldurCluster
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

	base := waldurCluster.DeepCopy()

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

	if waldurCluster.Status.Tenants == nil {
		waldurCluster.Status.Tenants = make(map[string]infrastructurev1beta2.OpenStackTenant, len(waldurCluster.Spec.Datacenters))
	}

	for _, dc := range waldurCluster.Spec.Datacenters {
		offeringSlug := dc.OfferingSlug
		if existing, ok := waldurCluster.Status.Tenants[offeringSlug]; ok {
			err = r.refreshTenant(ctx, &existing)
			if err != nil {
				log.Error(err, "Unable to refresh tenant", "uuid", existing.Uuid)
			}
			waldurCluster.Status.Tenants[offeringSlug] = existing
			continue
		}

		tenant, err := r.createTenant(ctx, offeringSlug, project)
		if err != nil {
			log.Error(err, "Unable to create tenant", "offering", offeringSlug)
			continue
		}
		waldurCluster.Status.Tenants[offeringSlug] = *tenant
	}

	if anyTenantPending(waldurCluster.Status.Tenants) {
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "Provisioning",
			Message: "Waiting for tenants to be provisioned",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(false)}
	} else if anyTenantErred(waldurCluster.Status.Tenants) {
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "ProvisioningFailed",
			Message: "One or more tenants failed to provision",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(false)}
	} else {
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "Provisioned",
			Message: "All tenants are ready",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(true)}
	}

	if err := r.Status().Patch(ctx, &waldurCluster, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "couldn't patch status for cluster %q", waldurCluster.Name)
	}

	if anyTenantPending(waldurCluster.Status.Tenants) {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func anyTenantErred(tenants map[string]infrastructurev1beta2.OpenStackTenant) bool {
	for _, t := range tenants {
		if t.State == waldurclient.CoreStatesERRED {
			return true
		}
	}
	return false
}

func anyTenantPending(tenants map[string]infrastructurev1beta2.OpenStackTenant) bool {
	for _, t := range tenants {
		if t.State != waldurclient.CoreStatesOK && t.State != waldurclient.CoreStatesERRED {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *WaldurClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta2.WaldurCluster{}).
		Named("waldurcluster").
		Complete(r)
}
