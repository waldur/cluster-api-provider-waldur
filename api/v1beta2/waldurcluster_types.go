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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	waldurclient "github.com/waldur/go-client"
)

// ClusterTopology defines the datacenter distribution strategy.
// +kubebuilder:validation:Enum=single;multi
type ClusterTopology string

const (
	// SingleDatacenter concentrates all resources in one datacenter.
	SingleDatacenter ClusterTopology = "single"
	// MultiDatacenter distributes resources across three datacenters for high availability.
	MultiDatacenter ClusterTopology = "multi"
)

// NodeType classifies a node group by its workload role.
// +kubebuilder:validation:Enum=worker;storage
type NodeType string

const (
	// WorkerNode runs application workloads.
	WorkerNode NodeType = "worker"
	// StorageNode provides persistent volume capabilities via Longhorn vSAN.
	StorageNode NodeType = "storage"
)

// WaldurClusterSpec defines the desired state of WaldurCluster.
type WaldurClusterSpec struct {
	// KubernetesVersion is the target Kubernetes version in semver format (e.g. v1.29.0).
	// +kubebuilder:validation:Pattern=`^v\d+\.\d+\.\d+$`
	KubernetesVersion string `json:"kubernetesVersion"`

	// Topology defines whether resources are concentrated in a single datacenter
	// or distributed across three datacenters for high availability.
	Topology ClusterTopology `json:"topology"`

	// Datacenters defines per-datacenter configuration. Each entry maps to one
	// Waldur offering (OpenStack deployment). Single topology requires exactly 1;
	// multi topology requires exactly 3.
	// +kubebuilder:validation:MinItems=1
	Datacenters []DatacenterSpec `json:"datacenters"`

	// SecurityRules defines CIDR-based network ingress/egress policies applied to all tenants.
	// +optional
	SecurityRules []SecurityRule `json:"securityRules,omitempty"`
}

// DatacenterSpec defines the configuration for a single datacenter (Waldur offering).
type DatacenterSpec struct {
	// OfferingSlug is the Waldur marketplace offering slug identifying this datacenter.
	OfferingSlug string `json:"offeringSlug"`

	// Name is a human-readable label for this datacenter, used as the Waldur project name.
	Name string `json:"name"`

	// OpenstackInfrastructure identifies the OpenStack tenant backing this datacenter.
	OpenstackInfrastructure OpenstackInfrastructure `json:"openstackInfrastructure"`

	// NodeGroups defines the groups of nodes to provision in this datacenter.
	// +kubebuilder:validation:MinItems=1
	NodeGroups []NodeGroupSpec `json:"nodeGroups"`
}

// OpenstackInfrastructure identifies the OpenStack tenant that backs a datacenter.
type OpenstackInfrastructure struct {
	// Name is the name of the OpenStack tenant.
	Name string `json:"name"`

	// CustomerName is the Waldur customer (organization) slug that owns the tenant.
	CustomerName string `json:"customerName"`

	// Uuid is the UUID of a pre-existing OpenStack tenant. When set, the controller
	// uses this tenant directly instead of creating a new one.
	// +optional
	Uuid *string `json:"uuid,omitempty"`
}

// NodeGroupSpec defines a homogeneous group of nodes within a datacenter.
type NodeGroupSpec struct {
	// Type classifies the node group as worker or storage.
	Type NodeType `json:"type"`

	// Flavor is the OpenStack flavor name for nodes in this group.
	Flavor string `json:"flavor"`

	// Count is the number of nodes to provision.
	// +kubebuilder:validation:Minimum=1
	Count int `json:"count"`

	// DataDiskSize is the size in GB of the data disk attached to each worker node.
	// Minimum 10 GB. Applicable to worker nodes.
	// +kubebuilder:validation:Minimum=10
	// +optional
	DataDiskSize *int `json:"dataDiskSize,omitempty"`

	// VsanDiskSize is the size in GB of the vSAN disk attached to each storage node.
	// Minimum 100 GB. Required for storage nodes (Longhorn).
	// +kubebuilder:validation:Minimum=100
	// +optional
	VsanDiskSize *int `json:"vsanDiskSize,omitempty"`
}

// SecurityRule defines a CIDR-based network access policy.
type SecurityRule struct {
	// Direction specifies whether this rule applies to incoming or outgoing traffic.
	// +kubebuilder:validation:Enum=ingress;egress
	Direction string `json:"direction"`

	// CIDR is the IPv4 address range in CIDR notation.
	// +kubebuilder:validation:Pattern=`^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$`
	CIDR string `json:"cidr"`

	// Port is the target port or port range (e.g. "443" or "8000-9000").
	// +optional
	Port *string `json:"port,omitempty"`

	// Protocol is the network protocol.
	// +kubebuilder:validation:Enum=tcp;udp;icmp
	// +optional
	Protocol *string `json:"protocol,omitempty"`
}

// WaldurClusterStatus defines the observed state of WaldurCluster.
type WaldurClusterStatus struct {
	// conditions represent the current state of the WaldurCluster resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Initialization provides information about infrastructure initialization.
	// Required by the CAPI v1beta2 contract — the core Cluster controller reads
	// status.initialization.provisioned to determine when to advance the cluster phase.
	// +optional
	Initialization *WaldurClusterInitialization `json:"initialization,omitempty"`

	// Tenants tracks the OpenStack tenant provisioned per offering slug.
	// +optional
	Tenants map[string]OpenStackTenant `json:"tenants,omitempty"`
}

// WaldurClusterInitialization holds provisioning completion state for the CAPI v1beta2 contract.
type WaldurClusterInitialization struct {
	// Provisioned indicates the infrastructure has been provisioned.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1beta2"

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

// WaldurOrder tracks a Waldur marketplace order.
type WaldurOrder struct {
	Uuid                    string                    `json:"uuid,omitempty"`
	Type                    waldurclient.RequestTypes `json:"type,omitempty"`
	State                   waldurclient.OrderState   `json:"state,omitempty"`
	MarketplaceResourceUuid string                    `json:"marketplaceResourceUuid,omitempty"`
	ResourceUuid            *string                   `json:"resourceUuid,omitempty"`
}

// OpenStackTenant tracks the OpenStack tenant provisioned for one offering.
type OpenStackTenant struct {
	State waldurclient.CoreStates `json:"state,omitempty"`
	Uuid  *string                 `json:"uuid,omitempty"`
	Name  string                  `json:"name,omitempty"`
	// MarketplaceResourceUuid is the UUID of the marketplace resource backing this tenant.
	// Required to submit a termination order.
	// +optional
	MarketplaceResourceUuid string `json:"marketplaceResourceUuid,omitempty"`
	// MarketplaceResourceState is the lifecycle state of the marketplace resource
	// (e.g. Creating, OK, Erred, Terminating, Terminated).
	// +optional
	MarketplaceResourceState waldurclient.ResourceState `json:"marketplaceResourceState,omitempty"`
	// ProjectSlug is the slug of the Waldur project created for this datacenter.
	// +optional
	ProjectSlug *string `json:"projectSlug,omitempty"`
	// Order is the currently executing or most recent Waldur order for this tenant.
	Order *WaldurOrder `json:"order,omitempty"`
}

func init() {
	SchemeBuilder.Register(&WaldurCluster{}, &WaldurClusterList{})
}
