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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	waldurclient "github.com/waldur/go-client"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// WaldurClusterSpec defines the desired state of WaldurCluster
type WaldurClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// Organization slug for project creation
	Organization *string `json:"org,omitempty"`

	// Slug of project containing tenants
	Project *string `json:"project,omitempty"`

	// List of slugs for tenant offerings
	Offerings []string `json:"offerings,omitempty"`
}

// WaldurClusterStatus defines the observed state of WaldurCluster.
type WaldurClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the WaldurCluster resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// List of created tenants
	Tenants map[string]OpenStackTenant `json:"tenants,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta1=v1beta1"

// WaldurCluster is the Schema for the waldurclusters API
type WaldurCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WaldurCluster
	// +required
	Spec WaldurClusterSpec `json:"spec"`

	// status defines the observed state of WaldurCluster
	// +optional
	Status WaldurClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WaldurClusterList contains a list of WaldurCluster
type WaldurClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WaldurCluster `json:"items"`
}

type WaldurOrder struct {
	Uuid                    string                    `json:"uuid,omitempty"`
	Type                    waldurclient.RequestTypes `json:"type,omitempty"`
	State                   waldurclient.OrderState   `json:"state,omitempty"`
	MarketplaceResourceUuid string                    `json:"resource_uuid,omitempty"`
	ResourceUuid            *string                   `json:"tenant_uuid,omitempty"`
}

type OpenStackTenant struct {
	State waldurclient.CoreStates `json:"state,omitempty"`
	Uuid  *string                 `json:"resource_uuid,omitempty"`
	Name  string                  `json:"name,omitempty"`
	// The currently executing order
	Order *WaldurOrder `json:"orders,omitempty"`
}

func init() {
	SchemeBuilder.Register(&WaldurCluster{}, &WaldurClusterList{})
}
