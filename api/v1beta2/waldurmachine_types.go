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

package v1beta2

import (
	waldurclient "github.com/waldur/go-client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WaldurMachineSpec defines the desired state of WaldurMachine.
type WaldurMachineSpec struct {
	// ProviderID is the identifier of the VM in the cloud provider, set after provisioning.
	// Follows the format "waldur://<vm-uuid>". Required by the CAPI Machine controller.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// OfferingSlug identifies which datacenter offering (OpenStack deployment) this
	// machine should be provisioned in. Must match one of the offering slugs in the
	// parent WaldurCluster's spec.datacenters.
	OfferingSlug string `json:"offeringSlug"`

	// NodeType classifies this machine as a worker or storage node.
	NodeType NodeType `json:"nodeType"`

	// Flavor is the OpenStack flavor name for this machine.
	Flavor string `json:"flavor"`

	// Image is the name or UUID of the OS image to use for this VM.
	Image string `json:"image"`

	// SystemDiskSize is the size in GB of the system (root) disk.
	// +kubebuilder:validation:Minimum=10
	// +optional
	SystemDiskSize *int `json:"systemDiskSize,omitempty"`

	// DataDiskSize is the size in GB of the data disk. Applicable to worker nodes.
	// +kubebuilder:validation:Minimum=10
	// +optional
	DataDiskSize *int `json:"dataDiskSize,omitempty"`

	// VsanDiskSize is the size in GB of the vSAN disk. Required for storage nodes.
	// +kubebuilder:validation:Minimum=100
	// +optional
	VsanDiskSize *int `json:"vsanDiskSize,omitempty"`
}

// WaldurMachineStatus defines the observed state of WaldurMachine.
type WaldurMachineStatus struct {
	// conditions represent the current state of the WaldurMachine resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Initialization provides information about machine provisioning completion.
	// Required by the CAPI v1beta2 contract.
	// +optional
	Initialization *WaldurMachineInitialization `json:"initialization,omitempty"`

	// VmUuid is the UUID of the VM in Waldur/OpenStack, set after provisioning.
	// +optional
	VmUuid *string `json:"vmUuid,omitempty"`

	// State is the current Waldur resource lifecycle state of the VM.
	// +optional
	State waldurclient.CoreStates `json:"state,omitempty"`

	// MarketplaceResourceUuid is the UUID of the marketplace resource backing this VM.
	// Required to submit a termination order.
	// +optional
	MarketplaceResourceUuid string `json:"marketplaceResourceUuid,omitempty"`

	// MarketplaceResourceState is the lifecycle state of the marketplace resource.
	// +optional
	MarketplaceResourceState waldurclient.ResourceState `json:"marketplaceResourceState,omitempty"`

	// Order is the currently executing or most recent Waldur order for this VM.
	// +optional
	Order *WaldurOrder `json:"order,omitempty"`
}

// WaldurMachineInitialization holds provisioning completion state for the CAPI v1beta2 contract.
type WaldurMachineInitialization struct {
	// Provisioned indicates the machine infrastructure has been provisioned.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1beta2"

// WaldurMachine is the Schema for the waldurmachines API
type WaldurMachine struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WaldurMachine
	// +required
	Spec WaldurMachineSpec `json:"spec"`

	// status defines the observed state of WaldurMachine
	// +optional
	Status WaldurMachineStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WaldurMachineList contains a list of WaldurMachine
type WaldurMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WaldurMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WaldurMachine{}, &WaldurMachineList{})
}
