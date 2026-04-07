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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/sergei-zaiaev/cluster-api-provider-waldur/api/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	waldurclient "github.com/waldur/go-client"

	util "sigs.k8s.io/cluster-api/util"
)

// WaldurClusterReconciler reconciles a WaldurCluster object
type WaldurClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	waldur waldurclient.ClientWithResponses
}

func (r *WaldurClusterReconciler) getOrCreateProject(ctx context.Context, projectSlug *string) (*waldurclient.Project, error) {
	projectResponse, err := r.waldur.ProjectsListWithResponse(ctx, &waldurclient.ProjectsListParams{
		Slug: projectSlug,
	})

	if err != nil {
		return nil, err
	}

	projects := projectResponse.JSON200

	if len(*projects) < 1 {
		// create a project
		projectData := waldurclient.ProjectsCreateJSONRequestBody{
			Name:     *projectSlug,
			Slug:     *&projectSlug,
			Customer: "",
		}
		projectResponse, err := r.waldur.ProjectsCreateWithResponse(ctx, projectData)

		if err != nil {
			return nil, err
		}

		return projectResponse.JSON201, nil
	} else {
		return &(*projects)[0], nil
	}

}

func (r *WaldurClusterReconciler) submitTenantCreationOrder(ctx context.Context, offering *waldurclient.Offering, project *waldurclient.Project) (*waldurclient.OrderDetails, error) {
	// TODO
	return nil, nil
}

func (r *WaldurClusterReconciler) getOffering(ctx context.Context, offeringSlug *string) (*waldurclient.Offering, error) {
	// offeringResponse, err := r.waldur.MarketplacePublicOfferingsList(ctx, &waldurclient.MarketplacePublicOfferingsListParams{
	// 	Slug: offeringSlug,
	// })
	return nil, nil
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

	projectSlug := waldurCluster.Spec.Project

	project, err := r.getOrCreateProject(ctx, projectSlug)

	if err != nil {
		return ctrl.Result{}, nil
	}

	// TODO: create tenant(s) in the projects if not created
	tenantOfferings := waldurCluster.Spec.Offerings

	for _, offeringSlug := range *tenantOfferings {
		offering, err := r.getOffering(ctx, &offeringSlug)
		if err != nil {
			r.submitTenantCreationOrder(ctx, offering, project)
		}
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
