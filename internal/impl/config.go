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

	// Observability controls how the deployer will export observability information
	// such as logs, metrics and traces, keyed by service. If no options are
	// specified, the deployer will launch corresponding services for exporting logs,
	// metrics and traces automatically.
	//
	// The key must be one of the following strings:
	// "prometheus_service" - to export metrics to Prometheus [1]
	// "jaeger_service"     - to export traces to Jaeger [2]
	// "loki_service"       - to export logs to Grafana Loki [3]
	// "grafana_service"    - to visualize/manipulate observability information [4]
	//
	// Possible values for each service:
	// 1) do not specify a value at all; leave it empty
	// this is the default behavior; kube deployer will automatically create the
	// observability service for you.
	//
	// 2) "none"
	// kube deployer will not export the corresponding observability information to
	// any service. E.g., prometheus_service = "none" means that the user will not
	// be able to see any metrics at all. This can be useful for testing or
	// benchmarking the performance of your application.
	//
	// 3) "your_observability_service_name"
	// if you already have a running service to collect metrics, traces or logs,
	// then you can simply specify the service name, and your application will
	// automatically export the corresponding information to your service. E.g.,
	// jaeger_service = "jaeger-all-in-one" will enable your running Jaeger
	// "service/jaeger-all-in-one" to capture all the app traces.
	//
	// [1] - https://prometheus.io/
	// [2] - https://www.jaegertracing.io/
	// [3] - https://grafana.com/oss/loki/
	// [4] - https://grafana.com/
	Observability map[string]string

	// Resources needed to run the pods. Note that the resources should satisfy
	// the format specified in [1].
	//
	// [1] https://pkg.go.dev/k8s.io/apimachinery/pkg/api/resource#example-MustParse.
	Resources resourceRequirements
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
