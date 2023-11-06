// Copyright 2023 Google LLC
//
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

package impl

// kubeConfig contains configuration for a Service Weaver application deployed
// with `weaver kube deploy`. The contents of a kubeConfig are parsed from the
// [kube] section of a weaver.toml file.
type kubeConfig struct {
	// Image is the name of the container image hosting the Service Weaver
	// application.
	//
	// If empty, the image name defaults to "<app_name>:<app_version>", where
	// <app_name> is the name of the app, and <app_version> is the unique
	// version id of the application deployment.
	Image string

	// Repo is the name of the repository where the container image is uploaded.
	//
	// For example, if Image is "mycontainer:v1" and Repo is
	// "docker.io/alanturing", then "weaver kube deploy" will build the image
	// locally as "mycontainer:v1" and then push it to
	// "docker.io/alanturing/mycontainer:v1".
	//
	// If empty, the image is not pushed to a repository.
	//
	// Example repositories are:
	//   - Docker Hub                : docker.io/USERNAME
	//   - Google Artifact Registry  : LOCATION-docker.pkg.dev/PROJECT-ID
	//   - GitHub Container Registry : ghcr.io/NAMESPACE
	Repo string

	// Namespace is the name of the Kubernetes namespace where the application
	// should be deployed. If not specified, the application will be deployed in
	// the default namespace.
	Namespace string

	// If set, it specifies the service account [1] under which to run the pods.
	// Otherwise, the application is being deployed using the default service
	// account for your namespace.
	//
	// [1] https://kubernetes.io/docs/concepts/security/service-accounts/
	ServiceAccount string `toml:"service_account"`

	// If true, application listeners will use the underlying nodes' network.
	// This behavior is generally discouraged, but it may be useful when running
	// the application in a minikube environment, where using the underlying
	// nodes' network may make it easier to access the listeners directly from
	// the host machine.
	UseHostNetwork bool `toml:"use_host_network"`

	// Options for the application listeners, keyed by listener name.
	// If a listener isn't specified in the map, default options will be used.
	Listeners map[string]*listenerConfig

	// Resources needed to run the pods. Note that the resources should satisfy
	// the format specified in [1].
	//
	// [1] https://pkg.go.dev/k8s.io/apimachinery/pkg/api/resource#example-MustParse.
	Resources resourceRequirements

	// Options for probes to check the readiness/liveness of the pods.
	LivenessProbeOpts  *probeOptions `toml:"liveness_probe"`
	ReadinessProbeOpts *probeOptions `toml:"readiness_probe"`
}

// listenerConfig stores configuration options for a listener.
type listenerConfig struct {
	// If specified, the listener service will have the name set to this value.
	// Otherwise, we will generate a unique name for each app version.
	ServiceName string `toml:"service_name"`

	// Is the listener public, i.e., should it receive ingress traffic
	// from the public internet. If false, the listener is configured only
	// for cluster-internal access.
	Public bool

	// If specified, the port inside the container on which the listener
	// is reachable. If zero or not specified, the first available port
	// is used.
	Port int32
}

// resourceRequirements stores the resource requirements configuration for running pods.
type resourceRequirements struct {
	// Describes the minimum amount of CPU required to run the pod.
	RequestsCPU string `toml:"requests_cpu"`
	// Describes the minimum amount of memory required to run the pod.
	RequestsMem string `toml:"requests_mem"`
	// Describes the maximum amount of CPU allowed to run the pod.
	LimitsCPU string `toml:"limits_cpu"`
	// Describes the maximum amount of memory allowed to run the pod.
	LimitsMem string `toml:"limits_mem"`
}

// probeOptions stores the probes [1] configuration for the pods. These options
// mirror the Kubernetes probe options available in [2].
//
// [1] https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
// [2] https://github.com/kubernetes/api/blob/v0.28.3/core/v1/types.go#L2277
//
// TODO(rgrandl): There are a few more knobs available in the kubernetes probe
// definition. We can enable more knobs if really needed.
type probeOptions struct {
	// How often to perform the probe.
	PeriodSecs int32 `toml:"period_secs"`
	// Number of seconds after which the probe times out.
	TimeoutSecs int32 `toml:"timeout_secs"`
	// Minimum consecutive successes for the probe to be considered successful after having failed.
	SuccessThreshold int32 `toml:"success_threshold"`
	// Minimum consecutive failures for the probe to be considered failed after having succeeded.
	FailureThreshold int32 `toml:"failure_threshold"`

	// Probe behavior. Note that only one of the following should be set by the user.

	// The probe action is taken by executing commands.
	Exec *execAction
	// The probe action is taken by executing HTTP GET requests.
	Http *httpAction
	// The probe action is taken by executing TCP requests.
	Tcp *tcpAction
}

// execAction describes the probe action when using a list of commands. It mirrors
// Kubernetes ExecAction [1].
//
// [1] https://github.com/kubernetes/api/blob/v0.28.3/core/v1/types.go#L2265
type execAction struct {
	Cmd []string // List of commands to execute inside the container.
}

// httpAction describes the probe action when using HTTP. It mirrors Kubernetes
// HTTPGetAction [1].
//
// [1] https://github.com/kubernetes/api/blob/v0.28.3/core/v1/types.go#L2208
type httpAction struct {
	Path string // Path to access on the HTTP server.
	Port int32  // Port number to access on the container.
}

// tcpAction describes the probe action when using TCP. It mirrors Kubernetes
// TCPSocketAction [1].
//
// [1] https://github.com/kubernetes/api/blob/v0.28.3/core/v1/types.go#L2241
type tcpAction struct {
	Port int32 // Port number to access on the container.
}
