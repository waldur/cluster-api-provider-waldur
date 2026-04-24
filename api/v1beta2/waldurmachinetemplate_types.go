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
)

// WaldurMachineTemplateSpec defines the desired state of WaldurMachineTemplate.
type WaldurMachineTemplateSpec struct {
	Template WaldurMachineTemplateResource `json:"template"`
}

// WaldurMachineTemplateResource holds the spec that is cloned into each WaldurMachine
// when a MachineDeployment or MachineSet creates a new Machine.
type WaldurMachineTemplateResource struct {
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WaldurMachineSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta1=v1beta2"
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1beta2"

// WaldurMachineTemplate is the Schema for the waldurmachinetemplates API.
type WaldurMachineTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec WaldurMachineTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// WaldurMachineTemplateList contains a list of WaldurMachineTemplate.
type WaldurMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WaldurMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WaldurMachineTemplate{}, &WaldurMachineTemplateList{})
}
