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
	vaultpkg "github.com/sergei-zaiaev/cluster-api-provider-waldur/internal/vault"
	waldurclient "github.com/waldur/go-client"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	util "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const machineFinalizer = "waldurmachine.infrastructure.cluster.waldur.com/finalizer"

var errBootstrapNotReady = errors.New("bootstrap secret not ready")

// ptrStr safely dereferences a *string for logging; returns "<nil>" if nil.
func ptrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// WaldurMachineReconciler reconciles a WaldurMachine object
type WaldurMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Waldur waldurclient.ClientWithResponses
	// VaultClient is optional. When non-nil the controller strips the RKE2 join token
	// from the bootstrap cloud-init, writes it to Vault, and injects a Vault pull script
	// so the token never appears in Waldur's user_data store.
	VaultClient vaultpkg.Client
	// BaseTemplate is the static OS cloud-init base (disk setup, sysctl, packages).
	// Loaded from a ConfigMap at startup. May be nil — only bootstrap + Vault sections
	// are used in that case.
	BaseTemplate []byte
	// OperatorNamespace is the namespace the controller manager runs in, used to look up
	// the vault-config Secret.
	OperatorNamespace string
}

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
		if err := r.createVM(ctx, machine, &waldurMachine, waldurCluster); err != nil {
			if errors.Is(err, errBootstrapNotReady) {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
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

func (r *WaldurMachineReconciler) createVM(ctx context.Context, machine *clusterv1.Machine, waldurMachine *infrastructurev1beta2.WaldurMachine, waldurCluster *infrastructurev1beta2.WaldurCluster) error {
	log := logf.FromContext(ctx)

	// --- Bootstrap gate ---
	// Gate on the bootstrap Secret being ready (standard CAPI infra provider contract).
	// The CAPI Machine controller sets DataSecretName once the bootstrap provider completes.
	if machine.Spec.Bootstrap.DataSecretName == nil {
		log.Info("Waiting for bootstrap secret", "machine", waldurMachine.Name)
		return errBootstrapNotReady
	}

	// Read the bootstrap Secret produced by the RKE2 bootstrap provider.
	// secret.Data["value"] is the full cloud-init YAML including the RKE2 join token.
	bootstrapSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      *machine.Spec.Bootstrap.DataSecretName,
	}, bootstrapSecret); err != nil {
		return errors.Wrap(err, "failed to fetch bootstrap secret")
	}
	rawCloudInit := bootstrapSecret.Data["value"]

	// --- Vault integration (optional) ---
	var userData *string
	var vaultSecretID, vaultRoleName string
	if r.VaultClient != nil {
		ud, sid, roleName, err := r.buildUserDataWithVault(ctx, machine, rawCloudInit)
		if err != nil {
			return err
		}
		userData = &ud
		vaultSecretID = sid
		vaultRoleName = roleName
	} else {
		// No Vault: pass bootstrap cloud-init verbatim as user_data.
		// Note: the RKE2 join token will be visible in Waldur's user_data store.
		ud := string(rawCloudInit)
		userData = &ud
	}

	orderPayload, err := r.buildOrderPayload(ctx, waldurMachine, waldurCluster, userData)
	if err != nil {
		return err
	}

	log.Info("Submitting VM creation order", "machine", waldurMachine.Name, "offering", waldurMachine.Spec.OfferingSlug)
	orderResp, err := r.Waldur.MarketplaceOrdersCreateWithResponse(ctx, orderPayload)
	if err != nil {
		return errors.Wrap(err, "unable to submit order")
	}
	if orderResp.StatusCode() != 201 {
		if r.VaultClient != nil && vaultSecretID != "" {
			_ = r.VaultClient.RevokeSecretID(ctx, vaultRoleName, vaultSecretID)
		}
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

// buildOrderPayload resolves all Waldur API references (project, offering, flavor, image,
// security groups, subnets, volume types) and assembles the marketplace order payload.
// Extracted from createVM to keep cyclomatic complexity in check.
func (r *WaldurMachineReconciler) buildOrderPayload(
	ctx context.Context,
	waldurMachine *infrastructurev1beta2.WaldurMachine,
	waldurCluster *infrastructurev1beta2.WaldurCluster,
	userData *string,
) (waldurclient.MarketplaceOrdersCreateJSONRequestBody, error) {
	log := logf.FromContext(ctx)
	tenant, ok := waldurCluster.Status.Tenants[waldurMachine.Spec.OfferingSlug]
	if !ok {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Errorf("tenant for offering %q not found in WaldurCluster status", waldurMachine.Spec.OfferingSlug)
	}
	if tenant.MarketplaceResourceState != waldurclient.ResourceStateOK {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Errorf("tenant for offering %q is not ready (state: %s)", waldurMachine.Spec.OfferingSlug, tenant.MarketplaceResourceState)
	}
	if tenant.ProjectSlug == nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Errorf("tenant for offering %q has no project slug", waldurMachine.Spec.OfferingSlug)
	}
	if tenant.Uuid == nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Errorf("tenant for offering %q has no UUID yet", waldurMachine.Spec.OfferingSlug)
	}

	projectResp, err := r.Waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{Slug: tenant.ProjectSlug})
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to list projects")
	}
	projects := *projectResp.JSON200
	if len(projects) == 0 {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Errorf("project with slug %q not found", *tenant.ProjectSlug)
	}

	parentOffering, err := getOffering(ctx, r.Waldur, waldurMachine.Spec.OfferingSlug)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to get parent offering")
	}
	log.Info("[DEV] tenant offering", "name", ptrStr(parentOffering.Name), "uuid", parentOffering.Uuid, "url", ptrStr(parentOffering.Url))

	subresourceOffering, err := r.getVMOffering(ctx, tenant.MarketplaceResourceUuid)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to get VM subresource offering")
	}

	log.Info("[DEV] Subresource VM offering", "uuid", subresourceOffering.Uuid)

	offering, err := getOfferingByUUID(ctx, r.Waldur, *subresourceOffering.Uuid)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to get VM offering")
	}

	log.Info("[DEV] VM offering", "URL", offering.Url)

	tenantUuid, err := uuid.Parse(*tenant.Uuid)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to parse tenant UUID")
	}

	flavor, err := getFlavor(ctx, r.Waldur, parentOffering, waldurMachine.Spec.Flavor)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrapf(err, "unable to get flavor %q", waldurMachine.Spec.Flavor)
	}

	image, err := getImage(ctx, r.Waldur, parentOffering, waldurMachine.Spec.Image)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrapf(err, "unable to get image %q", waldurMachine.Spec.Image)
	}

	securityGroups, err := getTenantSecurityGroups(ctx, r.Waldur, tenantUuid)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to get security groups")
	}
	log.Info("[DEV] security groups", "count", len(securityGroups))
	sgRequests := make([]waldurclient.OpenStackSecurityGroupHyperlinkRequest, 0, len(securityGroups))

	defaultSecGroupName := "default"
	for _, sg := range securityGroups {
		log.Info("[DEV] security group", "name", ptrStr(sg.Name), "url", ptrStr(sg.Url), "tenantName", ptrStr(sg.TenantName), "tenantUuid", sg.TenantUuid)
		if sg.Url != nil && *sg.Name == defaultSecGroupName {
			sgRequests = append(sgRequests, waldurclient.OpenStackSecurityGroupHyperlinkRequest{Url: *sg.Url})
		}
	}

	subnets, err := getTenantSubnets(ctx, r.Waldur, tenantUuid)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to get subnets")
	}
	portRequests := make([]waldurclient.OpenStackCreateInstancePortRequest, 0, len(subnets))
	for _, sn := range subnets {
		if sn.Url != nil {
			portRequests = append(portRequests, waldurclient.OpenStackCreateInstancePortRequest{Subnet: sn.Url})
		}
	}

	volumeTypes, err := getTenantVolumeTypes(ctx, r.Waldur, tenantUuid)
	if err != nil {
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to get volume types")
	}

	floatingIps := []waldurclient.OpenStackCreateFloatingIPRequest{}
	rawAttrs := waldurclient.OpenStackInstanceCreateOrderAttributes{
		Name:           waldurMachine.Name,
		Flavor:         flavor.Url,
		Image:          image.Url,
		SecurityGroups: &sgRequests,
		Ports:          &portRequests,
		FloatingIps:    &floatingIps,
		UserData:       userData,
	}

	log.Info("[DEV] instance cloud-init", "user-data", userData)
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
		return waldurclient.MarketplaceOrdersCreateJSONRequestBody{}, errors.Wrap(err, "unable to build order attributes")
	}

	orderType := waldurclient.Create
	acceptingTermsOfService := true
	payload := waldurclient.MarketplaceOrdersCreateJSONRequestBody{
		Type:                    &orderType,
		Offering:                *offering.Url,
		Project:                 *projects[0].Url,
		Attributes:              &attrs,
		AcceptingTermsOfService: &acceptingTermsOfService,
	}
	return payload, nil
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
		waldurMachine.Status.Initialization = &infrastructurev1beta2.WaldurMachineInitialization{Provisioned: new(true)}
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

// buildUserDataWithVault strips the RKE2 join token from the bootstrap cloud-init,
// stores it in Vault (idempotent — first node writes, rest skip), generates a
// single-use AppRole secret_id for this node, and returns the merged cloud-init
// that fetches the token at boot via a Vault pull script.
//
// Returns (userData, secretID, roleName, err). The caller must call
// VaultClient.RevokeSecretID(roleName, secretID) if the VM creation order fails,
// to avoid leaving dangling credentials in Vault.
func (r *WaldurMachineReconciler) buildUserDataWithVault(ctx context.Context, machine *clusterv1.Machine, rawCloudInit []byte) (userData, secretID, roleName string, err error) {
	log := logf.FromContext(ctx)

	sanitisedCI, rke2Token, err := stripRKE2Token(rawCloudInit)
	if err != nil {
		return "", "", "", errors.Wrap(err, "failed to strip RKE2 token from bootstrap cloud-init")
	}

	// Read the vault-config-<clusterName> Secret written by Crossplane at cluster creation.
	vaultConfigSecret := &corev1.Secret{}
	secretName := fmt.Sprintf("vault-config-%s", machine.Spec.ClusterName)
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: r.OperatorNamespace,
		Name:      secretName,
	}, vaultConfigSecret); err != nil {
		return "", "", "", errors.Wrapf(err, "failed to fetch vault config secret %q", secretName)
	}

	vaultAddr := string(vaultConfigSecret.Data["vault_addr"])
	secretPath := string(vaultConfigSecret.Data["vault_secret_path"])
	roleName = string(vaultConfigSecret.Data["role_name"])
	roleID := string(vaultConfigSecret.Data["role_id"])

	// Idempotent: the first node writes the token; subsequent nodes skip the write.
	// secretPath is the full KV v2 path (e.g. "secret/data/rke2/<clusterName>/join-token").
	exists, err := r.VaultClient.SecretExists(ctx, secretPath)
	if err != nil {
		return "", "", "", errors.Wrap(err, "failed to check if RKE2 token exists in Vault")
	}
	if !exists {
		log.Info("Writing RKE2 token to Vault", "path", secretPath)
		if err := r.VaultClient.WriteSecret(ctx, secretPath, map[string]string{"token": rke2Token}); err != nil {
			return "", "", "", errors.Wrap(err, "failed to write RKE2 token to Vault")
		}
	} else {
		log.Info("RKE2 token already in Vault, skipping write", "path", secretPath)
	}

	// Generate a single-use secret_id for this node's boot-time Vault login.
	secretID, err = r.VaultClient.GenerateSecretID(ctx, roleName)
	if err != nil {
		return "", "", "", errors.Wrap(err, "failed to generate Vault secret_id")
	}

	userData, err = mergeCloudInit(MergeInput{
		BootstrapCloudInit: sanitisedCI,
		StaticCloudInit:    r.BaseTemplate,
		VaultParams: VaultParams{
			Addr:       vaultAddr,
			SecretPath: secretPath,
			RoleID:     roleID,
			SecretID:   secretID,
		},
	})
	if err != nil {
		// Revoke unused secret_id — don't leave it dangling in Vault until TTL expiry.
		_ = r.VaultClient.RevokeSecretID(ctx, roleName, secretID)
		return "", "", "", errors.Wrap(err, "failed to merge cloud-init")
	}

	return userData, secretID, roleName, nil
}

func (r *WaldurMachineReconciler) getVMOffering(ctx context.Context, tenantMarketplaceUuid string) (*waldurclient.SubresourceOffering, error) {
	tenantMPUuid, err := uuid.Parse(tenantMarketplaceUuid)
	if err != nil {
		return nil, err
	}

	offeringsResponse, err := r.Waldur.MarketplaceResourcesOfferingForSubresourcesListWithResponse(ctx, tenantMPUuid)

	if err != nil {
		return nil, err
	}

	if offeringsResponse.StatusCode() != 200 {
		return nil, errors.Errorf("Unable to list subresource offerings for the marketplace resource %s, code %d, reason %s", tenantMPUuid, offeringsResponse.StatusCode(), string(offeringsResponse.Body))
	}

	offerings := *offeringsResponse.JSON200

	for _, offering := range offerings {
		if *offering.Type == "OpenStack.Instance" {
			return &offering, nil
		}
	}

	return nil, errors.Errorf("Unable to find a VM offering for the marketplace resource offering %s, list is empty", tenantMPUuid)
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
