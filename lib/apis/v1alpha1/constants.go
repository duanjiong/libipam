/*
Copyright 2020 The KubeSphere authors.

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

package v1alpha1

const (
	Group               = "network.kubesphere.io"
	VersionCurrent      = "v1alpha1"
	GroupVersionCurrent = Group + "/" + VersionCurrent

	// Known orchestrators.  Orchestrators are not limited to this list.
	OrchestratorKubernetes = "k8s"
	OrchestratorCNI        = "cni"
	OrchestratorDocker     = "libnetwork"
	OrchestratorOpenStack  = "openstack"

	// Enum options for enable/disable fields
	Enabled  = "Enabled"
	Disabled = "Disabled"
)
