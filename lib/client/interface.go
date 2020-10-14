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

	"kubesphere.io/libipam/lib/ipam"
)

type Interface interface {
	// IPPools returns an interface for managing IP pool resources.
	IPPools() IPPoolInterface
	// IPAM returns an interface for managing IP address assignment and releasing.
	IPAM() ipam.Interface

	// EnsureInitialized is used to ensure the backend datastore is correctly
	// initialized for use by Calico.  This method may be called multiple times, and
	// will have no effect if the datastore is already correctly initialized.
	// Most Calico deployment scenarios will automatically implicitly invoke this
	// method and so a general consumer of this API can assume that the datastore
	// is already initialized.
	EnsureInitialized(ctx context.Context, calicoVersion, clusterType string) error
}

// Compile-time assertion that our client implements its interface.
var _ Interface = (*client)(nil)
