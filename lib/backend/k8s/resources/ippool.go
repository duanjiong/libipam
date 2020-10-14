package resources

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"kubesphere.io/libipam/lib/apis/v1alpha1"
	"reflect"
)

const (
	IPPoolResourceName = "IPPools"
	IPPoolCRDName      = "ippools.network.kubesphere.io"
)

func NewIPPoolClient(c *kubernetes.Clientset, r *rest.RESTClient) K8sResourceClient {
	return &customK8sResourceClient{
		clientSet:       c,
		restClient:      r,
		name:            IPPoolCRDName,
		resource:        IPPoolResourceName,
		description:     "Kubesphere IP Pools",
		k8sResourceType: reflect.TypeOf(v1alpha1.IPPool{}),
		k8sResourceTypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.KindIPPool,
			APIVersion: v1alpha1.GroupVersionCurrent,
		},
		k8sListType:  reflect.TypeOf(v1alpha1.IPPoolList{}),
		resourceKind: v1alpha1.KindIPPool,
	}
}
