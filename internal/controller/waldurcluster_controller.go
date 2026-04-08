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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/pkg/errors"
	infrastructurev1alpha1 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	waldurclient "github.com/waldur/go-client"

	util "sigs.k8s.io/cluster-api/util"
	patch "sigs.k8s.io/cluster-api/util/patch"

	openapitypes "github.com/oapi-codegen/runtime/types"
)

// WaldurClusterReconciler reconciles a WaldurCluster object
type WaldurClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Waldur waldurclient.ClientWithResponses
}

func (r *WaldurClusterReconciler) getOrCreateProject(ctx context.Context, projectSlug *string) (*waldurclient.Project, error) {
	projectResponse, err := r.Waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{
		Slug: projectSlug,
	})

	if err != nil {
		return nil, err
	}

	projects := *projectResponse.JSON200

	if len(projects) < 1 {
		// create a project
		projectData := waldurclient.ProjectsCreateJSONRequestBody{
			Name:     *projectSlug,
			Customer: "",
		}
		projectResponse, err := r.Waldur.ProjectsCreateWithResponse(ctx, projectData)

		if err != nil {
			return nil, err
		}

		return projectResponse.JSON201, nil
	} else {
		return &projects[0], nil
	}

}

func (r *WaldurClusterReconciler) submitTenantCreationOrder(ctx context.Context, offering *waldurclient.PublicOfferingDetails, project *waldurclient.Project) (*waldurclient.OrderDetails, error) {
	orderType := waldurclient.Create
	rawAttrs := waldurclient.OpenStackTenantCreateOrderAttributes{}

	attrs := waldurclient.OrderCreateRequest_Attributes{}
	err := attrs.FromOpenStackTenantCreateOrderAttributes(rawAttrs)
	if err != nil {
		return nil, err
	}

	limits := map[string]int{}
	orderPayload := waldurclient.MarketplaceOrdersCreateJSONRequestBody{
		Project:    *project.Url,
		Offering:   *offering.Url,
		Type:       &orderType,
		Limits:     &limits,
		Attributes: &attrs,
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

func (r *WaldurClusterReconciler) getOffering(ctx context.Context, offeringSlug string) (*waldurclient.PublicOfferingDetails, error) {
	offeringResponse, err := r.Waldur.MarketplacePublicOfferingsListWithResponse(ctx, &waldurclient.MarketplacePublicOfferingsListParams{
		Slug: &offeringSlug,
	})
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

	var waldurCluster infrastructurev1alpha1.WaldurCluster
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

	if cluster == nil {
		log.Info("Waiting for Cluster Controller to set OwnerRef on WaldurCluster")
		return ctrl.Result{}, nil
	}

	// Tenants already created
	if waldurCluster.Status.Tenants != nil {
		// Check tenant statuses
		for _, tenant := range waldurCluster.Status.Tenants {
			if tenant.State != waldurclient.CoreStatesOK {
				return ctrl.Result{}, nil
			}
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

	projectSlug := waldurCluster.Spec.Project

	project, err := r.getOrCreateProject(ctx, projectSlug)

	if err != nil {
		return ctrl.Result{}, nil
	}

	// TODO: create tenant(s) in the projects if not created
	tenantOfferings := waldurCluster.Spec.Offerings

	createdOrders := make(map[string]infrastructurev1alpha1.WaldurOrder, len(tenantOfferings))
	createdTenants := make(map[string]infrastructurev1alpha1.OpenStackTenant, len(tenantOfferings))

	for _, offeringSlug := range tenantOfferings {
		offering, err := r.getOffering(ctx, offeringSlug)
		if err != nil {
			log.Error(err, "Unable to get offering details", "offering", offeringSlug)
			continue
		}

		order, err := r.submitTenantCreationOrder(ctx, offering, project)
		if err != nil || order == nil {
			log.Error(err, "Unable to submit order for offering", "offering", offeringSlug)
			continue
		}
		createdOrders[offeringSlug] = infrastructurev1alpha1.WaldurOrder{
			State:        *order.State,
			ResourceUuid: order.MarketplaceResourceUuid.String(),
		}

		tenant, err := r.getOpenStackTenant(ctx, order.ResourceUuid)
		if err != nil {
			log.Error(err, "Unable to get tenant", "resource_uuid", order.ResourceUuid)
			continue
		}
		createdTenants[offeringSlug] = infrastructurev1alpha1.OpenStackTenant{
			Uuid:  tenant.Uuid.String(),
			State: *tenant.State,
		}
	}

	helper, err := patch.NewHelper(&waldurCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	waldurCluster.Status.Orders = createdOrders
	waldurCluster.Status.Tenants = createdTenants
	if err := helper.Patch(ctx, &waldurCluster); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "couldn't patch cluster %q", waldurCluster.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WaldurClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.WaldurCluster{}).
		Named("waldurcluster").
		Complete(r)
}
