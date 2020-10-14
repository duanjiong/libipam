// Copyright (c) 2016-2020 Tigera, Inc. All rights reserved.

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

package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	kapiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	"kubesphere.io/libipam/lib/apis/v1alpha1"
	"kubesphere.io/libipam/lib/backend/api"
	"kubesphere.io/libipam/lib/backend/model"
)

const (
	nodeBgpIpv4AddrAnnotation            = "projectcalico.org/IPv4Address"
	nodeBgpIpv4IPIPTunnelAddrAnnotation  = "projectcalico.org/IPv4IPIPTunnelAddr"
	nodeBgpIpv4VXLANTunnelAddrAnnotation = "projectcalico.org/IPv4VXLANTunnelAddr"
	nodeBgpVXLANTunnelMACAddrAnnotation  = "projectcalico.org/VXLANTunnelMACAddr"
	nodeBgpIpv6AddrAnnotation            = "projectcalico.org/IPv6Address"
	nodeBgpAsnAnnotation                 = "projectcalico.org/ASNumber"
	nodeBgpCIDAnnotation                 = "projectcalico.org/RouteReflectorClusterID"
	nodeK8sLabelAnnotation               = "projectcalico.org/kube-labels"
	nodeWireguardIpv4IfaceAddrAnnotation = "projectcalico.org/IPv4WireguardInterfaceAddr"
	nodeWireguardPublicKeyAnnotation     = "projectcalico.org/WireguardPublicKey"
)

func NewNodeClient(c *kubernetes.Clientset, usePodCIDR bool) K8sResourceClient {
	return &nodeClient{
		clientSet:  c,
		usePodCIDR: usePodCIDR,
	}
}

// Implements the api.Client interface for Nodes.
type nodeClient struct {
	clientSet  *kubernetes.Clientset
	usePodCIDR bool
}

func (c *nodeClient) Create(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	log.Warn("Operation Create is not supported on Node type")
	return nil, cerrors.ErrorOperationNotSupported{
		Identifier: kvp.Key,
		Operation:  "Create",
	}
}

func (c *nodeClient) Update(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	log.Debug("Received Update request on Node type")
	// Get a current copy of the node to fill in fields we don't track.
	oldNode, err := c.clientSet.CoreV1().Nodes().Get(kvp.Key.(model.ResourceKey).Name, metav1.GetOptions{})
	if err != nil {
		return nil, K8sErrorToCalico(err, kvp.Key)
	}

	node, err := mergeCalicoNodeIntoK8sNode(kvp.Value.(*v1alpha1.Node), oldNode)
	if err != nil {
		return nil, err
	}

	newNode, err := c.clientSet.CoreV1().Nodes().UpdateStatus(node)
	if err != nil {
		log.WithError(err).Info("Error updating Node resource")
		return nil, K8sErrorToCalico(err, kvp.Key)
	}

	newCalicoNode, err := K8sNodeToCalico(newNode, c.usePodCIDR)
	if err != nil {
		log.Errorf("Failed to parse returned Node after call to update %+v", newNode)
		return nil, err
	}

	return newCalicoNode, nil
}

func (c *nodeClient) DeleteKVP(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	return c.Delete(ctx, kvp.Key, kvp.Revision, kvp.UID)
}

func (c *nodeClient) Delete(ctx context.Context, key model.Key, revision string, uid *types.UID) (*model.KVPair, error) {
	log.Warn("Operation Delete is not supported on Node type")
	return nil, cerrors.ErrorOperationNotSupported{
		Identifier: key,
		Operation:  "Delete",
	}
}

func (c *nodeClient) Get(ctx context.Context, key model.Key, revision string) (*model.KVPair, error) {
	log.Debug("Received Get request on Node type")
	node, err := c.clientSet.CoreV1().Nodes().Get(key.(model.ResourceKey).Name, metav1.GetOptions{ResourceVersion: revision})
	if err != nil {
		return nil, K8sErrorToCalico(err, key)
	}

	kvp, err := K8sNodeToCalico(node, c.usePodCIDR)
	if err != nil {
		log.WithError(err).Error("Couldn't convert k8s node.")
		return nil, err
	}

	return kvp, nil
}

func (c *nodeClient) List(ctx context.Context, list model.ListInterface, revision string) (*model.KVPairList, error) {
	log.Debug("Received List request on Node type")
	nl := list.(model.ResourceListOptions)
	kvps := []*model.KVPair{}

	if nl.Name != "" {
		// The node is already fully qualified, so perform a Get instead.
		// If the entry does not exist then we just return an empty list.
		kvp, err := c.Get(ctx, model.ResourceKey{Name: nl.Name, Kind: v1alpha1.KindNode}, revision)
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
				return nil, err
			}
			return &model.KVPairList{
				KVPairs:  kvps,
				Revision: revision,
			}, nil
		}

		kvps = append(kvps, kvp)
		return &model.KVPairList{
			KVPairs:  kvps,
			Revision: revision,
		}, nil
	}

	// Listing all nodes.
	nodes, err := c.clientSet.CoreV1().Nodes().List(metav1.ListOptions{ResourceVersion: revision})
	if err != nil {
		return nil, K8sErrorToCalico(err, list)
	}

	for _, node := range nodes.Items {
		kvp, err := K8sNodeToCalico(&node, c.usePodCIDR)
		if err != nil {
			log.Errorf("Unable to convert k8s node to Calico node: node=%s: %v", node.Name, err)
			continue
		}
		kvps = append(kvps, kvp)
	}

	return &model.KVPairList{
		KVPairs:  kvps,
		Revision: revision,
	}, nil
}

func (c *nodeClient) EnsureInitialized() error {
	return nil
}

func (c *nodeClient) Watch(ctx context.Context, list model.ListInterface, revision string) (api.WatchInterface, error) {
	// Build watch options to pass to k8s.
	opts := metav1.ListOptions{ResourceVersion: revision, Watch: true}
	rlo, ok := list.(model.ResourceListOptions)
	if !ok {
		return nil, fmt.Errorf("ListInterface is not a ResourceListOptions: %s", list)
	}
	if len(rlo.Name) != 0 {
		// We've been asked to watch a specific node resource.
		log.WithField("name", rlo.Name).Debug("Watching a single node")
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", rlo.Name).String()
	}

	k8sWatch, err := c.clientSet.CoreV1().Nodes().Watch(opts)
	if err != nil {
		return nil, K8sErrorToCalico(err, list)
	}
	converter := func(r Resource) (*model.KVPair, error) {
		k8sNode, ok := r.(*kapiv1.Node)
		if !ok {
			return nil, errors.New("node conversion with incorrect k8s resource type")
		}
		return K8sNodeToCalico(k8sNode, c.usePodCIDR)
	}
	return newK8sWatcherConverter(ctx, "Node", converter, k8sWatch), nil
}

// K8sNodeToCalico converts a Kubernetes format node, with Calico annotations, to a Calico Node.
func K8sNodeToCalico(k8sNode *kapiv1.Node, usePodCIDR bool) (*model.KVPair, error) {
	// Create a new CalicoNode resource and copy the settings across from the k8s Node.
	calicoNode := v1alpha1.NewNode()
	calicoNode.ObjectMeta.Name = k8sNode.Name
	SetCalicoMetadataFromK8sAnnotations(calicoNode, k8sNode)

	// Calico Nodes inherit labels from Kubernetes nodes, do that merge.
	err := mergeCalicoAndK8sLabels(calicoNode, k8sNode)
	if err != nil {
		log.WithError(err).Error("Failed to merge Calico and Kubernetes labels.")
		return nil, err
	}

	// Add in an orchestrator reference back to the Kubernetes node name.
	calicoNode.Spec.OrchRefs = []v1alpha1.OrchRef{{NodeName: k8sNode.Name, Orchestrator: v1alpha1.OrchestratorKubernetes}}

	// Fill in status with Kubernetes pod CIDRs.
	if len(k8sNode.Spec.PodCIDRs) > 0 {
		calicoNode.Status.PodCIDRs = make([]string, len(k8sNode.Spec.PodCIDRs))
		for i, c := range k8sNode.Spec.PodCIDRs {
			calicoNode.Status.PodCIDRs[i] = c
		}
	}

	// Create the resource key from the node name.
	return &model.KVPair{
		Key: model.ResourceKey{
			Name: k8sNode.Name,
			Kind: v1alpha1.KindNode,
		},
		Value:    calicoNode,
		Revision: k8sNode.ObjectMeta.ResourceVersion,
	}, nil
}

// mergeCalicoNodeIntoK8sNode takes a k8s node and a Calico node and puts the values from the Calico
// node into the k8s node.
func mergeCalicoNodeIntoK8sNode(calicoNode *v1alpha1.Node, k8sNode *kapiv1.Node) (*kapiv1.Node, error) {
	// Nodes inherit labels from Kubernetes, but we also have our own set of labels that are stored in an annotation.
	// For nodes that are being updated, we want to avoid writing k8s labels that we inherited into our annotation
	// and we don't want to touch the k8s labels directly.  Take a copy of the node resource and update its labels
	// to match what we want to store in our annotation only.
	calicoNode, err := restoreCalicoLabels(calicoNode)
	if err != nil {
		return nil, err
	}

	// Set the k8s annotations from the Calico node metadata.
	SetK8sAnnotationsFromCalicoMetadata(k8sNode, calicoNode)

	return k8sNode, nil
}

// mergeCalicoAndK8sLabels merges the Kubernetes labels (from k8sNode.Labels) with those that are already present in
// calicoNode (which were loaded from our annotation).  Kubernetes labels take precedence.  To make the operation
// reversible (so that we can support write back of a Calico node that was read from Kubernetes), we also store the
// complete set of Kubernetes labels in an annotation.
//
// Note: if a Kubernetes label shadows a Calico label, the Calico label will be lost when the resource is written
// back to the datastore.  This is consistent with kube-controllers' behavior.
func mergeCalicoAndK8sLabels(calicoNode *v1alpha1.Node, k8sNode *kapiv1.Node) error {
	// Now, copy the Kubernetes Node labels over.  Note: this may overwrite Calico labels of the same name, but that's
	// consistent with the kube-controllers behavior.
	for k, v := range k8sNode.Labels {
		if calicoNode.Labels == nil {
			calicoNode.Labels = map[string]string{}
		}
		calicoNode.Labels[k] = v
	}

	// For consistency with kube-controllers, and so we can correctly round-trip labels, we stash the kubernetes labels
	// in an annotation.
	if calicoNode.Annotations == nil {
		calicoNode.Annotations = map[string]string{}
	}
	bytes, err := json.Marshal(k8sNode.Labels)
	if err != nil {
		log.WithError(err).Errorf("Error marshalling node labels")
		return err
	}
	calicoNode.Annotations[nodeK8sLabelAnnotation] = string(bytes)
	return nil
}

// restoreCalicoLabels tries to undo the transformation done by mergeCalicoLabels.  If no changes are needed, it
// returns the input value; otherwise, it returns a copy.
func restoreCalicoLabels(calicoNode *v1alpha1.Node) (*v1alpha1.Node, error) {
	rawLabels := calicoNode.Annotations[nodeK8sLabelAnnotation]
	if rawLabels == "" {
		return calicoNode, nil
	}

	// We're about to update the labels and annotations on the node, take a copy.
	calicoNode = calicoNode.DeepCopy()

	// We stashed the k8s labels in an annotation, extract them so we can compare with the combined labels.
	k8sLabels := map[string]string{}
	if err := json.Unmarshal([]byte(rawLabels), &k8sLabels); err != nil {
		log.WithError(err).Error("Failed to unmarshal k8s node labels from " +
			nodeK8sLabelAnnotation + " annotation")
		return nil, err
	}

	// Now remove any labels that match the k8s ones.
	if log.GetLevel() >= log.DebugLevel {
		log.WithField("k8s", k8sLabels).Debug("Loaded label annotations")
	}
	for k, k8sVal := range k8sLabels {
		if calVal, ok := calicoNode.Labels[k]; ok && calVal != k8sVal {
			log.WithFields(log.Fields{
				"label":    k,
				"newValue": calVal,
				"k8sValue": k8sVal,
			}).Warn("Update to label that is shadowed by a Kubernetes label will be ignored.")
		}

		// The k8s value was inherited and there was no old Calico value, drop the label so that we don't copy
		// it to the Calico annotation.
		if log.GetLevel() >= log.DebugLevel {
			log.WithField("key", k).Debug("Removing inherited k8s label")
		}
		delete(calicoNode.Labels, k)
	}

	// Filter out our bookkeeping annotation, which is only used for round-tripping labels correctly.
	delete(calicoNode.Annotations, nodeK8sLabelAnnotation)
	if len(calicoNode.Annotations) == 0 {
		calicoNode.Annotations = nil
	}

	return calicoNode, nil
}