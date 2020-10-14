// Copyright (c) 2019 Tigera, Inc. All rights reserved.

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
	"fmt"
	"reflect"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/names"
	"github.com/projectcalico/libcalico-go/lib/net"
	"kubesphere.io/libipam/lib/apis/v1alpha1"
	"kubesphere.io/libipam/lib/backend/api"
	"kubesphere.io/libipam/lib/backend/model"
)

const (
	IPAMBlockResourceName = "IPAMBlocks"
	IPAMBlockCRDName      = "ipamblocks.network.kubesphere.io"
)

func NewIPAMBlockClient(c *kubernetes.Clientset, r *rest.RESTClient) K8sResourceClient {
	// Create a resource client which manages k8s CRDs.
	rc := customK8sResourceClient{
		clientSet:       c,
		restClient:      r,
		name:            IPAMBlockCRDName,
		resource:        IPAMBlockResourceName,
		description:     "Calico IPAM blocks",
		k8sResourceType: reflect.TypeOf(v1alpha1.IPAMBlock{}),
		k8sResourceTypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.KindIPAMBlock,
			APIVersion: v1alpha1.GroupVersionCurrent,
		},
		k8sListType:  reflect.TypeOf(v1alpha1.IPAMBlockList{}),
		resourceKind: v1alpha1.KindIPAMBlock,
	}

	return &ipamBlockClient{rc: rc}
}

// ipamBlockClient implements the api.Client interface for IPAMBlocks. It handles the translation between
// v1 objects understood by the IPAM codebase in lib/ipam, and the CRDs which are used
// to actually store the data in the Kubernetes API. It uses a customK8sResourceClient under
// the covers to perform CRUD operations on kubernetes CRDs.
type ipamBlockClient struct {
	rc customK8sResourceClient
}

func (c ipamBlockClient) toV1(kvpv3 *model.KVPair) (*model.KVPair, error) {
	cidrStr := kvpv3.Value.(*v1alpha1.IPAMBlock).Spec.CIDR
	id := kvpv3.Value.(*v1alpha1.IPAMBlock).Spec.ID
	_, cidr, err := net.ParseCIDR(cidrStr)
	if err != nil {
		return nil, err
	}

	ab := kvpv3.Value.(*v1alpha1.IPAMBlock)

	// Convert attributes.
	attrs := []model.AllocationAttribute{}
	for _, a := range ab.Spec.Attributes {
		attrs = append(attrs, model.AllocationAttribute{
			AttrPrimary:   a.AttrPrimary,
			AttrSecondary: a.AttrSecondary,
		})
	}

	return &model.KVPair{
		Key: model.BlockKey{
			CIDR: *cidr,
			ID:   id,
		},
		Value: &model.AllocationBlock{
			CIDR:        *cidr,
			Affinity:    ab.Spec.Affinity,
			Allocations: ab.Spec.Allocations,
			Unallocated: ab.Spec.Unallocated,
			Attributes:  attrs,
			Deleted:     ab.Spec.Deleted,
		},
		Revision: kvpv3.Revision,
		UID:      &ab.UID,
	}, nil
}

func (c ipamBlockClient) parseKey(k model.Key) (name, cidr string, id uint32) {
	id = k.(model.BlockKey).ID
	cidr = fmt.Sprintf("%s", k.(model.BlockKey).CIDR)
	name = fmt.Sprintf("%d-%s", id, names.CIDRToName(k.(model.BlockKey).CIDR))
	return
}

func (c ipamBlockClient) toV3(kvpv1 *model.KVPair) *model.KVPair {
	name, cidr, id := c.parseKey(kvpv1.Key)

	ab := kvpv1.Value.(*model.AllocationBlock)

	// Convert attributes.
	attrs := []v1alpha1.AllocationAttribute{}
	for _, a := range ab.Attributes {
		attrs = append(attrs, v1alpha1.AllocationAttribute{
			AttrPrimary:   a.AttrPrimary,
			AttrSecondary: a.AttrSecondary,
		})
	}

	return &model.KVPair{
		Key: model.ResourceKey{
			Name: name,
			Kind: v1alpha1.KindIPAMBlock,
		},
		Value: &v1alpha1.IPAMBlock{
			TypeMeta: metav1.TypeMeta{
				Kind:       v1alpha1.KindIPAMBlock,
				APIVersion: "network.kubesphere.io/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				ResourceVersion: kvpv1.Revision,
			},
			Spec: v1alpha1.IPAMBlockSpec{
				CIDR:        cidr,
				ID:          id,
				Allocations: ab.Allocations,
				Unallocated: ab.Unallocated,
				Affinity:    ab.Affinity,
				Attributes:  attrs,
				Deleted:     ab.Deleted,
			},
		},
		Revision: kvpv1.Revision,
	}
}

func (c *ipamBlockClient) Create(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	nkvp := c.toV3(kvp)
	b, err := c.rc.Create(ctx, nkvp)
	if err != nil {
		return nil, err
	}
	v1kvp, err := c.toV1(b)
	if err != nil {
		return nil, err
	}
	return v1kvp, nil
}

func (c *ipamBlockClient) Update(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	nkvp := c.toV3(kvp)
	b, err := c.rc.Update(ctx, nkvp)
	if err != nil {
		return nil, err
	}
	v1kvp, err := c.toV1(b)
	if err != nil {
		return nil, err
	}
	return v1kvp, nil
}

func (c *ipamBlockClient) DeleteKVP(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	// We need to mark as deleted first, since the Kubernetes API doesn't support
	// compare-and-delete. This update operation allows us to eliminate races with other clients.
	name, _, _ := c.parseKey(kvp.Key)
	kvp.Value.(*model.AllocationBlock).Deleted = true
	v1kvp, err := c.Update(ctx, kvp)
	if err != nil {
		return nil, err
	}

	// Now actually delete the object.
	k := model.ResourceKey{Name: name, Kind: v1alpha1.KindIPAMBlock}
	kvp, err = c.rc.Delete(ctx, k, v1kvp.Revision, kvp.UID)
	if err != nil {
		return nil, err
	}
	return c.toV1(kvp)
}

func (c *ipamBlockClient) Delete(ctx context.Context, key model.Key, revision string, uid *types.UID) (*model.KVPair, error) {
	// Delete should not be used for blocks, since we need the object UID for correctness.
	log.Warn("Operation Delete is not supported on IPAMBlock type - use DeleteKVP")
	return nil, cerrors.ErrorOperationNotSupported{
		Identifier: key,
		Operation:  "Delete",
	}
}

func (c *ipamBlockClient) Get(ctx context.Context, key model.Key, revision string) (*model.KVPair, error) {
	// Get the object.
	name, _, _ := c.parseKey(key)
	k := model.ResourceKey{Name: name, Kind: v1alpha1.KindIPAMBlock}
	kvp, err := c.rc.Get(ctx, k, revision)
	if err != nil {
		return nil, err
	}

	// Convert it back to V1 format.
	v1kvp, err := c.toV1(kvp)
	if err != nil {
		return nil, err
	}

	// If this object has been marked as deleted, then we need to clean it up and
	// return not found.
	if v1kvp.Value.(*model.AllocationBlock).Deleted {
		if _, err := c.DeleteKVP(ctx, v1kvp); err != nil {
			return nil, err
		}
		return nil, cerrors.ErrorResourceDoesNotExist{fmt.Errorf("Resource was deleted"), key}
	}

	return v1kvp, nil
}

func (c *ipamBlockClient) List(ctx context.Context, list model.ListInterface, revision string) (*model.KVPairList, error) {
	l := model.ResourceListOptions{Kind: v1alpha1.KindIPAMBlock}
	v3list, err := c.rc.List(ctx, l, revision)
	if err != nil {
		return nil, err
	}

	kvpl := &model.KVPairList{KVPairs: []*model.KVPair{}}
	for _, i := range v3list.KVPairs {
		v1kvp, err := c.toV1(i)
		if err != nil {
			return nil, err
		}
		kvpl.KVPairs = append(kvpl.KVPairs, v1kvp)
	}
	return kvpl, nil
}

func (c *ipamBlockClient) Watch(ctx context.Context, list model.ListInterface, revision string) (api.WatchInterface, error) {
	resl := model.ResourceListOptions{Kind: v1alpha1.KindIPAMBlock}
	k8sWatchClient := cache.NewListWatchFromClient(c.rc.restClient, c.rc.resource, "", fields.Everything())
	k8sWatch, err := k8sWatchClient.WatchFunc(metav1.ListOptions{ResourceVersion: revision})
	if err != nil {
		return nil, K8sErrorToCalico(err, list)
	}
	toKVPair := func(r Resource) (*model.KVPair, error) {
		conv, err := c.rc.convertResourceToKVPair(r)
		if err != nil {
			return nil, err
		}
		return c.toV1(conv)
	}

	return newK8sWatcherConverter(ctx, resl.Kind+" (custom)", toKVPair, k8sWatch), nil
}

// EnsureInitialized is a no-op since the CRD should be
// initialized in advance.
func (c *ipamBlockClient) EnsureInitialized() error {
	return nil
}
