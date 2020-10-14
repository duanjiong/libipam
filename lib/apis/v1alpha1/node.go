// Copyright (c) 2017,2020 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	KindNode     = "Node"
	KindNodeList = "NodeList"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient:nonNamespaced
// +kubebuilder:resource:categories="network",scope="Cluster"
// Node contains information about a Node resource.
type Node struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Specification of the Node.
	Spec NodeSpec `json:"spec,omitempty"`
	// Status of the Node.
	Status NodeStatus `json:"status,omitempty"`
}

// NodeSpec contains the specification for a Node resource.
type NodeSpec struct {
	// OrchRefs for this node.
	OrchRefs []OrchRef `json:"orchRefs,omitempty" validate:"omitempty"`
}

type NodeStatus struct {
	// PodCIDR is a reflection of the Kubernetes node's spec.PodCIDRs field.
	PodCIDRs []string `json:"podCIDRs,omitempty" validate:"omitempty"`
}

// OrchRef is used to correlate a Calico node to its corresponding representation in a given orchestrator
type OrchRef struct {
	// NodeName represents the name for this node according to the orchestrator.
	NodeName string `json:"nodeName,omitempty" validate:"omitempty"`
	// Orchestrator represents the orchestrator using this node.
	Orchestrator string `json:"orchestrator"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// NodeList contains a list of Node resources.
type NodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []Node `json:"items"`
}

// NewNode creates a new (zeroed) Node struct with the TypeMetadata initialised to the current
// version.
func NewNode() *Node {
	return &Node{
		TypeMeta: metav1.TypeMeta{
			Kind:       KindNode,
			APIVersion: GroupVersionCurrent,
		},
	}
}

// NewNodeList creates a new (zeroed) NodeList struct with the TypeMetadata initialised to the current
// version.
func NewNodeList() *NodeList {
	return &NodeList{
		TypeMeta: metav1.TypeMeta{
			Kind:       KindNodeList,
			APIVersion: GroupVersionCurrent,
		},
	}
}
