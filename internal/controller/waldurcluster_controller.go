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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkg/errors"
	infrastructurev1beta2 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1beta2"
	"k8s.io/utils/ptr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	waldurclient "github.com/waldur/go-client"

	util "sigs.k8s.io/cluster-api/util"

	openapitypes "github.com/oapi-codegen/runtime/types"
)

const finalizer = "waldurcluster.infrastructure.cluster.waldur.com/finalizer"

// WaldurClusterReconciler reconciles a WaldurCluster object
type WaldurClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Waldur waldurclient.ClientWithResponses
}

func (r *WaldurClusterReconciler) getOrCreateProject(ctx context.Context, org *waldurclient.Customer, dcName string) (*waldurclient.Project, error) {
	log := logf.FromContext(ctx)
	projectName := fmt.Sprintf("%s_%s", *org.Slug, dcName)
	customerUuids := []openapitypes.UUID{*org.Uuid}
	projectResponse, err := r.Waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{
		NameExact: &projectName,
		Customer:  &customerUuids,
	})

	if err != nil {
		return nil, err
	}

	projects := *projectResponse.JSON200

	if len(projects) == 0 {
		log.Info("Creating Waldur project", "project", projectName)
		projectData := waldurclient.ProjectsCreateJSONRequestBody{
			Name:     projectName,
			Customer: *org.Url,
		}
		projectCreateResponse, err := r.Waldur.ProjectsCreateWithResponse(ctx, projectData)

		if err != nil {
			return nil, err
		}

		log.Info("Waldur project created", "project", projectName, "slug", ptr.Deref(projectCreateResponse.JSON201.Slug, ""))
		return projectCreateResponse.JSON201, nil
	}

	return &projects[0], nil
}

// controllerNodeCores and controllerNodeRam are the per-node resource defaults for
// the 3 controller nodes auto-provisioned by the platform (not user-configurable).
const (
	controllerNodeCores = 4
	controllerNodeRamMB = 8 * 1024
	lbNodeCores         = 2
	lbNodeRamMB         = 4 * 1024
	systemDiskMB        = 20 * 1024
)

func (r *WaldurClusterReconciler) calculateLimits(ctx context.Context, offering *waldurclient.PublicOfferingDetails, dc infrastructurev1beta2.DatacenterSpec) (map[string]int, error) {
	cores := 3*controllerNodeCores + lbNodeCores
	ram := 3*controllerNodeRamMB + lbNodeRamMB
	storage := (3 + 1) * systemDiskMB // controller + lb system disks

	for _, ng := range dc.NodeGroups {
		flavor, err := getFlavor(ctx, r.Waldur, offering, ng.Flavor)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to get flavor %q", ng.Flavor)
		}
		cores += ng.Count * *flavor.Cores
		ram += ng.Count * *flavor.Ram
		storage += ng.Count * systemDiskMB
		if ng.DataDiskSize != nil {
			storage += ng.Count * *ng.DataDiskSize * 1024
		}
		if ng.VsanDiskSize != nil {
			storage += ng.Count * *ng.VsanDiskSize * 1024
		}
	}

	return map[string]int{
		"cores":   cores,
		"ram":     ram,
		"storage": storage,
	}, nil
}

func (r *WaldurClusterReconciler) submitTenantCreationOrder(ctx context.Context, offering *waldurclient.PublicOfferingDetails, project *waldurclient.Project, dc infrastructurev1beta2.DatacenterSpec) (*waldurclient.OrderDetails, error) {
	orderType := waldurclient.Create

	subnetCidr := "192.168.42.0/24" // TODO: make configurable for multiple tenants
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

	limits, err := r.calculateLimits(ctx, offering, dc)
	if err != nil {
		return nil, errors.Wrap(err, "unable to calculate tenant resource limits")
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
		return nil, errors.Errorf("unable to submit an order, details: %s", string(orderResponse.Body))
	}

	return orderResponse.JSON201, nil
}

func (r *WaldurClusterReconciler) refreshTenant(ctx context.Context, existing *infrastructurev1beta2.OpenStackTenant) error {
	log := logf.FromContext(ctx)

	if existing.Order != nil && !isOrderTerminal(existing.Order.State) {
		prevOrderState := existing.Order.State
		if err := refreshOrder(ctx, r.Waldur, existing.Order); err != nil {
			return errors.Wrap(err, "unable to refresh order")
		}
		if existing.Order.State != prevOrderState {
			log.Info("Tenant order state changed", "tenant", existing.Name, "order", existing.Order.Uuid, "state", existing.Order.State)
		}
	}

	// Populate marketplace resource UUID from the order if not already set
	if existing.MarketplaceResourceUuid == "" && existing.Order != nil {
		existing.MarketplaceResourceUuid = existing.Order.MarketplaceResourceUuid
	}

	// No marketplace resource UUID yet — wait for next reconcile
	if existing.MarketplaceResourceUuid == "" {
		return nil
	}

	resource, err := getMarketplaceResource(ctx, r.Waldur, existing.MarketplaceResourceUuid)
	if err != nil {
		return err
	}
	if resource.State != nil {
		existing.MarketplaceResourceState = *resource.State
	}

	// scope is the URL of the backend OpenStack tenant.
	// nil means either not yet created or already terminated — either way there's no tenant to fetch.
	if resource.Scope == nil {
		return nil
	}

	if resource.ResourceUuid == nil {
		return nil
	}

	tenantUuid := resource.ResourceUuid
	refreshed, err := r.getOpenStackTenant(ctx, tenantUuid)
	if err != nil {
		return err
	}

	prevState := existing.State
	if refreshed.State != nil {
		existing.State = *refreshed.State
	}
	if refreshed.Name != nil {
		existing.Name = *refreshed.Name
	}
	existing.Uuid = ptr.To(tenantUuid.String())

	if existing.State != prevState {
		log.Info("Tenant state changed", "tenant", existing.Name, "state", existing.State, "marketplaceResourceState", existing.MarketplaceResourceState)
	}

	return nil
}

func (r *WaldurClusterReconciler) createTenant(ctx context.Context, dc infrastructurev1beta2.DatacenterSpec) (*infrastructurev1beta2.OpenStackTenant, error) {
	org, err := r.getCustomer(ctx, dc.OpenstackInfrastructure.CustomerName)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get customer")
	}

	project, err := r.getOrCreateProject(ctx, org, dc.Name)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get or create project")
	}

	offering, err := getOffering(ctx, r.Waldur, dc.OfferingSlug)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get offering details")
	}

	log := logf.FromContext(ctx)
	log.Info("Submitting tenant creation order", "offering", dc.OfferingSlug, "datacenter", dc.Name)

	order, err := r.submitTenantCreationOrder(ctx, offering, project, dc)
	if err != nil || order == nil {
		return nil, errors.Wrap(err, "unable to submit order")
	}

	log.Info("Tenant creation order submitted", "offering", dc.OfferingSlug, "order", order.Uuid.String(), "state", order.State)

	waldurOrder := &infrastructurev1beta2.WaldurOrder{
		Uuid:                    order.Uuid.String(),
		Type:                    *order.Type,
		State:                   *order.State,
		MarketplaceResourceUuid: order.MarketplaceResourceUuid.String(),
	}

	tenantUuid := order.ResourceUuid
	openStackTenant := &infrastructurev1beta2.OpenStackTenant{
		Order:                   waldurOrder,
		Name:                    *order.ResourceName,
		ProjectSlug:             project.Slug,
		MarketplaceResourceUuid: order.MarketplaceResourceUuid.String(),
	}

	if tenantUuid == nil {
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

func (r *WaldurClusterReconciler) getOpenStackTenant(ctx context.Context, tenantUuid *openapitypes.UUID) (*waldurclient.OpenStackTenant, error) {
	tenantResponse, err := r.Waldur.OpenstackTenantsRetrieveWithResponse(ctx, *tenantUuid, &waldurclient.OpenstackTenantsRetrieveParams{})
	if err != nil {
		return nil, err
	}

	tenant := tenantResponse.JSON200

	return tenant, nil
}

func (r *WaldurClusterReconciler) getCustomer(ctx context.Context, orgName string) (*waldurclient.Customer, error) {
	fieldFilter := []waldurclient.CustomerFieldEnum{
		waldurclient.CustomerFieldEnumUrl,
		waldurclient.CustomerFieldEnumSlug,
		waldurclient.CustomerFieldEnumUuid,
	}
	params := waldurclient.CustomersListParams{
		NameExact: &orgName,
		Field:     &fieldFilter,
	}
	orgResponse, err := r.Waldur.CustomersListWithResponse(ctx, &params)

	if err != nil {
		return nil, err
	}

	if orgResponse.StatusCode() != 200 {
		return nil, errors.Errorf("unable to get customer %q, reason: %s", orgName, string(orgResponse.Body))
	}

	orgs := *orgResponse.JSON200

	if len(orgs) == 0 {
		return nil, errors.Errorf("unable to find a customer with name %q", orgName)
	}

	return &orgs[0], nil
}

func (r *WaldurClusterReconciler) deleteProject(ctx context.Context, projectSlug string) error {
	projectResponse, err := r.Waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{
		Slug: &projectSlug,
	})
	if err != nil {
		return err
	}

	projects := *projectResponse.JSON200
	if len(projects) == 0 {
		return nil // already gone
	}

	projectUuid := projects[0].Uuid
	if projectUuid == nil {
		return errors.Errorf("project %q has no UUID", projectSlug)
	}

	resp, err := r.Waldur.ProjectsDestroyWithResponse(ctx, *projectUuid)
	if err != nil {
		return err
	}

	if resp.StatusCode() != 200 && resp.StatusCode() != 204 {
		return errors.Errorf("unable to delete project %q, status %d: %s", projectSlug, resp.StatusCode(), string(resp.Body))
	}

	return nil
}

func (r *WaldurClusterReconciler) reconcileDelete(ctx context.Context, waldurCluster *infrastructurev1beta2.WaldurCluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	base := waldurCluster.DeepCopy()

	tenants := make(map[string]infrastructurev1beta2.OpenStackTenant, len(waldurCluster.Status.Tenants))
	for k, v := range waldurCluster.Status.Tenants {
		tenants[k] = *v.DeepCopy()
	}

	allDone := true
	for offeringSlug, tenant := range tenants {
		// Nothing to clean up if the tenant was never actually provisioned
		if tenant.MarketplaceResourceUuid == "" {
			continue
		}

		// If there's an active termination order, refresh order + tenant state and wait
		if tenant.Order != nil && tenant.Order.Type == waldurclient.Terminate && !isOrderTerminal(tenant.Order.State) {
			if err := r.refreshTenant(ctx, &tenant); err != nil {
				log.Error(err, "Unable to refresh tenant during termination", "offering", offeringSlug)
			}
			tenants[offeringSlug] = tenant
			allDone = false
			continue
		}

		// Tenant is fully terminated once its order is done
		if tenant.Order != nil && tenant.Order.Type == waldurclient.Terminate && tenant.Order.State == waldurclient.OrderStateDone {
			continue
		}

		// Submit the termination order
		log.Info("Submitting tenant termination order", "offering", offeringSlug, "tenant", tenant.Name)
		order, err := submitTerminationOrder(ctx, r.Waldur, tenant.MarketplaceResourceUuid)
		if err != nil {
			log.Error(err, "Unable to submit termination order", "offering", offeringSlug)
			tenants[offeringSlug] = tenant
			allDone = false
			continue
		}
		log.Info("Tenant termination order submitted", "offering", offeringSlug, "tenant", tenant.Name, "order", order.Uuid)

		tenant.Order = order
		tenants[offeringSlug] = tenant
		allDone = false
	}

	waldurCluster.Status.Tenants = tenants
	if err := r.Status().Patch(ctx, waldurCluster, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "couldn't patch status for cluster %q", waldurCluster.Name)
	}

	if !allDone {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// All tenants terminated — delete the projects
	for _, tenant := range tenants {
		if tenant.ProjectSlug == nil {
			continue
		}
		log.Info("Deleting Waldur project", "project", *tenant.ProjectSlug)
		if err := r.deleteProject(ctx, *tenant.ProjectSlug); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "unable to delete project %q", *tenant.ProjectSlug)
		}
		log.Info("Waldur project deleted", "project", *tenant.ProjectSlug)
	}

	// Remove the finalizer so the API server can delete the object
	log.Info("All tenants terminated, removing finalizer")
	controllerutil.RemoveFinalizer(waldurCluster, finalizer)
	if err := r.Update(ctx, waldurCluster); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to remove finalizer")
	}

	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

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

	log.Info("Reconciling WaldurCluster", "cluster", waldurCluster.Name, "deleting", !waldurCluster.DeletionTimestamp.IsZero())

	// Handle deletion
	if !waldurCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &waldurCluster)
	}

	// Ensure our finalizer is present before doing any work
	if !controllerutil.ContainsFinalizer(&waldurCluster, finalizer) {
		log.Info("Adding finalizer", "cluster", waldurCluster.Name)
		controllerutil.AddFinalizer(&waldurCluster, finalizer)
		if err := r.Update(ctx, &waldurCluster); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to add finalizer")
		}
		return ctrl.Result{}, nil
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

	tenants := make(map[string]infrastructurev1beta2.OpenStackTenant, len(waldurCluster.Status.Tenants))
	for k, v := range waldurCluster.Status.Tenants {
		tenants[k] = *v.DeepCopy()
	}

	for _, dc := range waldurCluster.Spec.Datacenters {
		if existing, ok := tenants[dc.OfferingSlug]; ok {
			if err := r.refreshTenant(ctx, &existing); err != nil {
				log.Error(err, "Unable to refresh tenant", "offering", dc.OfferingSlug)
			}
			tenants[dc.OfferingSlug] = existing
			continue
		}

		tenant, err := r.createTenant(ctx, dc)
		if err != nil {
			log.Error(err, "Unable to create tenant", "offering", dc.OfferingSlug)
			continue
		}
		tenants[dc.OfferingSlug] = *tenant
	}

	waldurCluster.Status.Tenants = tenants
	r.setReadyCondition(&waldurCluster)

	if err := r.Status().Patch(ctx, &waldurCluster, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "couldn't patch status for cluster %q", waldurCluster.Name)
	}

	if anyTenantPending(tenants) {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *WaldurClusterReconciler) setReadyCondition(waldurCluster *infrastructurev1beta2.WaldurCluster) {
	switch {
	case len(waldurCluster.Status.Tenants) < len(waldurCluster.Spec.Datacenters):
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "Provisioning",
			Message: "Waiting for tenants to be created",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(false)}
	case anyTenantPending(waldurCluster.Status.Tenants):
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "Provisioning",
			Message: "Waiting for tenants to be provisioned",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(false)}
	case anyTenantErred(waldurCluster.Status.Tenants):
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "ProvisioningFailed",
			Message: "One or more tenants failed to provision",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(false)}
	default:
		meta.SetStatusCondition(&waldurCluster.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "Provisioned",
			Message: "All tenants are ready",
		})
		waldurCluster.Status.Initialization = &infrastructurev1beta2.WaldurClusterInitialization{Provisioned: ptr.To(true)}
	}
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
