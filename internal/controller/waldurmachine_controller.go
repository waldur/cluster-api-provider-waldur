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

	"github.com/google/uuid"
	"github.com/pkg/errors"
	infrastructurev1beta2 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1beta2"
	waldurclient "github.com/waldur/go-client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	util "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const machineFinalizer = "waldurmachine.infrastructure.cluster.waldur.com/finalizer"

// WaldurMachineReconciler reconciles a WaldurMachine object
type WaldurMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Waldur waldurclient.ClientWithResponses
}

// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.waldur.com,resources=waldurmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
func (r *WaldurMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var waldurMachine infrastructurev1beta2.WaldurMachine
	if err := r.Get(ctx, req.NamespacedName, &waldurMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling WaldurMachine", "machine", waldurMachine.Name, "deleting", !waldurMachine.DeletionTimestamp.IsZero())

	if !waldurMachine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &waldurMachine)
	}

	if !controllerutil.ContainsFinalizer(&waldurMachine, machineFinalizer) {
		log.Info("Adding finalizer", "machine", waldurMachine.Name)
		controllerutil.AddFinalizer(&waldurMachine, machineFinalizer)
		if err := r.Update(ctx, &waldurMachine); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to add finalizer")
		}
		return ctrl.Result{}, nil
	}

	// Get the owner Machine
	machine, err := util.GetOwnerMachine(ctx, r.Client, waldurMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on WaldurMachine")
		return ctrl.Result{}, nil
	}

	// Get the owner Cluster from the Machine
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Waiting for Cluster to be set on Machine")
		return ctrl.Result{}, nil
	}

	// Get the WaldurCluster from the Cluster's infrastructureRef
	waldurCluster := &infrastructurev1beta2.WaldurCluster{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cluster.Spec.InfrastructureRef.Name,
		Namespace: cluster.Namespace,
	}, waldurCluster); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to get WaldurCluster")
	}

	base := waldurMachine.DeepCopy()

	if waldurMachine.Status.MarketplaceResourceUuid == "" {
		if err := r.createVM(ctx, &waldurMachine, waldurCluster); err != nil {
			log.Error(err, "Unable to create VM", "machine", waldurMachine.Name)
		}
	} else {
		if err := r.refreshVM(ctx, &waldurMachine); err != nil {
			log.Error(err, "Unable to refresh VM", "machine", waldurMachine.Name)
		}
	}

	// Set ProviderID on spec once VM is running
	if waldurMachine.Status.State == waldurclient.CoreStatesOK &&
		waldurMachine.Spec.ProviderID == nil &&
		waldurMachine.Status.VmUuid != nil {
		providerID := fmt.Sprintf("waldur://%s", *waldurMachine.Status.VmUuid)
		waldurMachine.Spec.ProviderID = &providerID
		if err := r.Patch(ctx, &waldurMachine, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to patch providerID")
		}
		base = waldurMachine.DeepCopy()
	}

	r.setMachineReadyCondition(&waldurMachine)

	if err := r.Status().Patch(ctx, &waldurMachine, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "couldn't patch status for machine %q", waldurMachine.Name)
	}

	if waldurMachine.Status.State == waldurclient.CoreStatesOK {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *WaldurMachineReconciler) createVM(ctx context.Context, waldurMachine *infrastructurev1beta2.WaldurMachine, waldurCluster *infrastructurev1beta2.WaldurCluster) error {
	log := logf.FromContext(ctx)

	tenant, ok := waldurCluster.Status.Tenants[waldurMachine.Spec.OfferingSlug]
	if !ok {
		return errors.Errorf("tenant for offering %q not found in WaldurCluster status", waldurMachine.Spec.OfferingSlug)
	}
	if tenant.MarketplaceResourceState != waldurclient.ResourceStateOK {
		return errors.Errorf("tenant for offering %q is not ready (state: %s)", waldurMachine.Spec.OfferingSlug, tenant.MarketplaceResourceState)
	}
	if tenant.ProjectSlug == nil {
		return errors.Errorf("tenant for offering %q has no project slug", waldurMachine.Spec.OfferingSlug)
	}

	// Look up the project by slug to get its URL
	projectResp, err := r.Waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{
		Slug: tenant.ProjectSlug,
	})
	if err != nil {
		return errors.Wrap(err, "unable to list projects")
	}
	projects := *projectResp.JSON200
	if len(projects) == 0 {
		return errors.Errorf("project with slug %q not found", *tenant.ProjectSlug)
	}
	project := &projects[0]

	// Get the parentOffering
	parentOffering, err := getOffering(ctx, r.Waldur, waldurMachine.Spec.OfferingSlug)
	if err != nil {
		return errors.Wrap(err, "unable to get parent offering")
	}

	// Get the VM offering
	offering, err := r.getVMOffering(ctx, parentOffering)

	if err != nil {
		return errors.Wrap(err, "unable to get VM offering")
	}

	// Parse tenant UUID for resource lookups scoped to the OpenStack tenant
	if tenant.Uuid == nil {
		return errors.Errorf("tenant for offering %q has no UUID yet", waldurMachine.Spec.OfferingSlug)
	}
	tenantUuid, err := uuid.Parse(*tenant.Uuid)
	if err != nil {
		return errors.Wrap(err, "unable to parse tenant UUID")
	}

	// Resolve flavor name → URL
	flavor, err := getFlavor(ctx, r.Waldur, parentOffering, waldurMachine.Spec.Flavor)
	if err != nil {
		return errors.Wrapf(err, "unable to get flavor %q", waldurMachine.Spec.Flavor)
	}

	// Resolve image name → URL
	image, err := getImage(ctx, r.Waldur, parentOffering, waldurMachine.Spec.Image)
	if err != nil {
		return errors.Wrapf(err, "unable to get image %q", waldurMachine.Spec.Image)
	}

	// Look up security groups in the tenant
	securityGroups, err := getTenantSecurityGroups(ctx, r.Waldur, tenantUuid)
	if err != nil {
		return errors.Wrap(err, "unable to get security groups")
	}
	sgRequests := make([]waldurclient.OpenStackSecurityGroupHyperlinkRequest, 0, len(securityGroups))
	for _, sg := range securityGroups {
		if sg.Url != nil {
			sgRequests = append(sgRequests, waldurclient.OpenStackSecurityGroupHyperlinkRequest{Url: *sg.Url})
		}
	}

	// Look up subnets in the tenant (one port per internal subnet)
	subnets, err := getTenantSubnets(ctx, r.Waldur, tenantUuid)
	if err != nil {
		return errors.Wrap(err, "unable to get subnets")
	}
	portRequests := make([]waldurclient.OpenStackCreateInstancePortRequest, 0, len(subnets))
	for _, sn := range subnets {
		if sn.Url != nil {
			portRequests = append(portRequests, waldurclient.OpenStackCreateInstancePortRequest{Subnet: sn.Url})
		}
	}

	// Look up volume types in the tenant (use first available for both system and data)
	volumeTypes, err := getTenantVolumeTypes(ctx, r.Waldur, tenantUuid)
	if err != nil {
		return errors.Wrap(err, "unable to get volume types")
	}

	orderType := waldurclient.Create
	floatingIps := []waldurclient.OpenStackCreateFloatingIPRequest{}
	rawAttrs := waldurclient.OpenStackInstanceCreateOrderAttributes{
		Name:           waldurMachine.Name,
		Flavor:         flavor.Url,
		Image:          image.Url,
		SecurityGroups: &sgRequests,
		Ports:          &portRequests,
		FloatingIps:    &floatingIps,
	}

	if waldurMachine.Spec.SystemDiskSize != nil {
		systemSizeMiB := *waldurMachine.Spec.SystemDiskSize * 1024
		rawAttrs.SystemVolumeSize = &systemSizeMiB
	}
	if waldurMachine.Spec.DataDiskSize != nil {
		dataSizeMiB := *waldurMachine.Spec.DataDiskSize * 1024
		rawAttrs.DataVolumeSize = &dataSizeMiB
	}
	if len(volumeTypes) > 0 && volumeTypes[0].Url != nil {
		rawAttrs.SystemVolumeType = volumeTypes[0].Url
		rawAttrs.DataVolumeType = volumeTypes[0].Url
	}

	attrs := waldurclient.OrderCreateRequest_Attributes{}
	if err := attrs.FromOpenStackInstanceCreateOrderAttributes(rawAttrs); err != nil {
		return errors.Wrap(err, "unable to build order attributes")
	}

	acceptingTermsOfService := true

	orderPayload := waldurclient.MarketplaceOrdersCreateJSONRequestBody{
		Type:                    &orderType,
		Offering:                *offering.Url,
		Project:                 *project.Url,
		Attributes:              &attrs,
		AcceptingTermsOfService: &acceptingTermsOfService,
	}

	log.Info("Submitting VM creation order", "machine", waldurMachine.Name, "offering", waldurMachine.Spec.OfferingSlug)
	orderResp, err := r.Waldur.MarketplaceOrdersCreateWithResponse(ctx, orderPayload)
	if err != nil {
		return errors.Wrap(err, "unable to submit order")
	}
	if orderResp.StatusCode() != 201 {
		return errors.Errorf("unable to submit VM creation order, status %d: %s", orderResp.StatusCode(), string(orderResp.Body))
	}

	order := orderResp.JSON201
	log.Info("VM creation order submitted", "machine", waldurMachine.Name, "order", order.Uuid.String(), "state", order.State)

	waldurMachine.Status.Order = &infrastructurev1beta2.WaldurOrder{
		Uuid:                    order.Uuid.String(),
		Type:                    *order.Type,
		State:                   *order.State,
		MarketplaceResourceUuid: order.MarketplaceResourceUuid.String(),
	}
	waldurMachine.Status.MarketplaceResourceUuid = order.MarketplaceResourceUuid.String()
	waldurMachine.Status.State = waldurclient.CoreStatesCREATING

	return nil
}

func (r *WaldurMachineReconciler) refreshVM(ctx context.Context, waldurMachine *infrastructurev1beta2.WaldurMachine) error {
	log := logf.FromContext(ctx)

	if waldurMachine.Status.Order != nil && !isOrderTerminal(waldurMachine.Status.Order.State) {
		prevOrderState := waldurMachine.Status.Order.State
		if err := refreshOrder(ctx, r.Waldur, waldurMachine.Status.Order); err != nil {
			return errors.Wrap(err, "unable to refresh order")
		}
		if waldurMachine.Status.Order.State != prevOrderState {
			log.Info("VM order state changed", "machine", waldurMachine.Name, "order", waldurMachine.Status.Order.Uuid, "state", waldurMachine.Status.Order.State)
		}
	}

	resource, err := getMarketplaceResource(ctx, r.Waldur, waldurMachine.Status.MarketplaceResourceUuid)
	if err != nil {
		return err
	}
	if resource.State != nil {
		waldurMachine.Status.MarketplaceResourceState = *resource.State
	}

	// Scope is the URL of the backend VM. nil means not yet created or already terminated.
	if resource.Scope == nil || resource.ResourceUuid == nil {
		return nil
	}

	instanceResp, err := r.Waldur.OpenstackInstancesRetrieveWithResponse(ctx, *resource.ResourceUuid, &waldurclient.OpenstackInstancesRetrieveParams{
		Field: &[]waldurclient.OpenStackInstanceFieldEnum{
			waldurclient.OpenStackInstanceFieldEnumState,
			waldurclient.OpenStackInstanceFieldEnumUuid,
		},
	})
	if err != nil {
		return errors.Wrap(err, "unable to retrieve OpenStack instance")
	}
	if instanceResp.JSON200 == nil {
		return errors.Errorf("unexpected nil instance in response (status %d)", instanceResp.StatusCode())
	}

	instance := instanceResp.JSON200
	prevState := waldurMachine.Status.State

	if instance.State != nil {
		waldurMachine.Status.State = *instance.State
	}
	if instance.Uuid != nil {
		vmUuid := instance.Uuid.String()
		waldurMachine.Status.VmUuid = &vmUuid
	}

	if waldurMachine.Status.State != prevState {
		log.Info("VM state changed", "machine", waldurMachine.Name, "state", waldurMachine.Status.State, "marketplaceResourceState", waldurMachine.Status.MarketplaceResourceState)
	}

	if waldurMachine.Status.State == waldurclient.CoreStatesOK {
		waldurMachine.Status.Initialization = &infrastructurev1beta2.WaldurMachineInitialization{Provisioned: ptr.To(true)}
	}

	return nil
}

func (r *WaldurMachineReconciler) reconcileDelete(ctx context.Context, waldurMachine *infrastructurev1beta2.WaldurMachine) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Never provisioned — nothing to clean up
	if waldurMachine.Status.MarketplaceResourceUuid == "" {
		controllerutil.RemoveFinalizer(waldurMachine, machineFinalizer)
		if err := r.Update(ctx, waldurMachine); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to remove finalizer")
		}
		return ctrl.Result{}, nil
	}

	base := waldurMachine.DeepCopy()

	// Termination in flight — poll
	if waldurMachine.Status.Order != nil &&
		waldurMachine.Status.Order.Type == waldurclient.Terminate &&
		!isOrderTerminal(waldurMachine.Status.Order.State) {
		if err := r.refreshVM(ctx, waldurMachine); err != nil {
			log.Error(err, "Unable to refresh VM during termination", "machine", waldurMachine.Name)
		}
		if err := r.Status().Patch(ctx, waldurMachine, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to patch status")
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Termination done — remove finalizer
	if waldurMachine.Status.Order != nil &&
		waldurMachine.Status.Order.Type == waldurclient.Terminate &&
		waldurMachine.Status.Order.State == waldurclient.OrderStateDone {
		log.Info("VM terminated, removing finalizer", "machine", waldurMachine.Name)
		controllerutil.RemoveFinalizer(waldurMachine, machineFinalizer)
		if err := r.Update(ctx, waldurMachine); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "unable to remove finalizer")
		}
		return ctrl.Result{}, nil
	}

	// Submit termination order
	log.Info("Submitting VM termination order", "machine", waldurMachine.Name)
	order, err := submitTerminationOrder(ctx, r.Waldur, waldurMachine.Status.MarketplaceResourceUuid)
	if err != nil {
		log.Error(err, "Unable to submit VM termination order", "machine", waldurMachine.Name)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	log.Info("VM termination order submitted", "machine", waldurMachine.Name, "order", order.Uuid)

	waldurMachine.Status.Order = order
	if err := r.Status().Patch(ctx, waldurMachine, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to patch status")
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *WaldurMachineReconciler) getVMOffering(ctx context.Context, parentOffering *waldurclient.PublicOfferingDetails) (*waldurclient.PublicOfferingDetails, error) {
	params := waldurclient.MarketplacePublicOfferingsListParams{
		Type:       &[]string{"OpenStack.Instance"},
		ParentUuid: parentOffering.Uuid,
	}
	offeringsResponse, err := r.Waldur.MarketplacePublicOfferingsListWithResponse(ctx, &params)

	if err != nil {
		return nil, err
	}

	if offeringsResponse.StatusCode() != 200 {
		return nil, errors.Errorf("Unable to list VM offerings for the parent offering %s, code %d, reason %s", *parentOffering.Name, offeringsResponse.StatusCode, string(offeringsResponse.Body))
	}

	offerings := *offeringsResponse.JSON200

	if len(offerings) == 0 {
		return nil, errors.Errorf("Unable to find a VM offering for the parent offering %s, list is empty", *parentOffering.Name)
	} else {
		return &offerings[0], nil
	}
}

func (r *WaldurMachineReconciler) setMachineReadyCondition(waldurMachine *infrastructurev1beta2.WaldurMachine) {
	switch waldurMachine.Status.State {
	case waldurclient.CoreStatesOK:
		meta.SetStatusCondition(&waldurMachine.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "Provisioned",
			Message: "VM is running",
		})
	case waldurclient.CoreStatesERRED:
		meta.SetStatusCondition(&waldurMachine.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "ProvisioningFailed",
			Message: "VM provisioning failed",
		})
	default:
		meta.SetStatusCondition(&waldurMachine.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "Provisioning",
			Message: "VM is being provisioned",
		})
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *WaldurMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta2.WaldurMachine{}).
		Named("waldurmachine").
		Complete(r)
}
