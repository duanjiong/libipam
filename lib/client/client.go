// Copyright (c) 2017-2020 Tigera, Inc. All rights reserved.

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

package client

import (
	"context"
	"kubesphere.io/libipam/lib/apis/v1alpha1"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/libcalico-go/lib/apiconfig"
	"github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/options"
	"kubesphere.io/libipam/lib/backend"
	bapi "kubesphere.io/libipam/lib/backend/api"
	"kubesphere.io/libipam/lib/ipam"
)

// client implements the client.Interface.
type client struct {
	// The config we were created with.
	config apiconfig.CalicoAPIConfig

	// The backend client.
	backend bapi.Client

	// The resources client used internally.
	resources resourceInterface
}

// New returns a connected client. The ClientConfig can either be created explicitly,
// or can be loaded from a config file or environment variables using the LoadClientConfig() function.
func New(config apiconfig.CalicoAPIConfig) (Interface, error) {
	be, err := backend.NewClient(config)
	if err != nil {
		return nil, err
	}
	return client{
		config:    config,
		backend:   be,
		resources: &resources{backend: be},
	}, nil
}

// NewFromEnv loads the config from ENV variables and returns a connected client.
func NewFromEnv() (Interface, error) {

	config, err := apiconfig.LoadClientConfigFromEnvironment()
	if err != nil {
		return nil, err
	}

	return New(*config)
}

// IPPools returns an interface for managing IP pool resources.
func (c client) IPPools() IPPoolInterface {
	return ipPools{client: c}
}

// IPAM returns an interface for managing IP address assignment and releasing.
func (c client) IPAM() ipam.Interface {
	return ipam.NewIPAMClient(c.backend, poolAccessor{client: &c})
}

type poolAccessor struct {
	client *client
}

func (p poolAccessor) GetEnabledPools(ipVersion int) ([]v1alpha1.IPPool, error) {
	return p.getPools(func(pool *v1alpha1.IPPool) bool {
		if pool.Spec.Disabled {
			log.Debugf("Skipping disabled IP pool (%s)", pool.Name)
			return false
		}
		if _, cidr, err := net.ParseCIDR(pool.Spec.CIDR); err == nil && cidr.Version() == ipVersion {
			log.Debugf("Adding pool (%s) to the IPPool list", cidr.String())
			return true
		} else if err != nil {
			log.Warnf("Failed to parse the IPPool: %s. Ignoring that IPPool", pool.Spec.CIDR)
		} else {
			log.Debugf("Ignoring IPPool: %s. IP version is different.", pool.Spec.CIDR)
		}
		return false
	})
}

func (p poolAccessor) getPools(filter func(pool *v1alpha1.IPPool) bool) ([]v1alpha1.IPPool, error) {
	pools, err := p.client.IPPools().List(context.Background(), options.ListOptions{})
	if err != nil {
		return nil, err
	}
	log.Debugf("Got list of all IPPools: %v", pools)
	var filtered []v1alpha1.IPPool
	for _, pool := range pools.Items {
		if filter(&pool) {
			filtered = append(filtered, pool)
		}
	}
	return filtered, nil
}

func (p poolAccessor) GetAllPools() ([]v1alpha1.IPPool, error) {
	return p.getPools(func(pool *v1alpha1.IPPool) bool {
		return true
	})
}

// EnsureInitialized is used to ensure the backend datastore is correctly
// initialized for use by Calico.  This method may be called multiple times, and
// will have no effect if the datastore is already correctly initialized.
//
// Most Calico deployment scenarios will automatically implicitly invoke this
// method and so a general consumer of this API can assume that the datastore
// is already initialized.
func (c client) EnsureInitialized(ctx context.Context, calicoVersion, clusterType string) error {
	// Perform datastore specific initialization first.
	if err := c.backend.EnsureInitialized(); err != nil {
		return err
	}

	return nil
}

// Backend returns the backend client used by the v1alpha1 client.  Not exposed on the main
// client API, but available publicly for consumers that require access to the backend
// client (e.g. for syncer support).
func (c client) Backend() bapi.Client {
	return c.backend
}
