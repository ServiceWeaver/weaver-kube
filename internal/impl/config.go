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

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

// kubeConfig contains the kubernetes configuration for a Service Weaver
// application deployed with `weaver kube deploy`.
type kubeConfig struct {
	// Path to the app config file.
	AppConfig string

	// BaseImage is the base docker image used to build the container.
	//
	// If empty, Service Weaver will pick 'ubuntu:rolling' as the base image.
	BaseImage string

	// Image is the name of the container image hosting the Service Weaver
	// application.
	//
	// If empty, the image name defaults to "<app_name>:<app_version>", where
	// <app_name> is the name of the app, and <app_version> is the unique
	// version id of the application deployment.
	Image string

	// Build tool is the name of the container build tool to use.
	//
	// If empty, Service Weaver will pick default build tool as `docker`.
	// Supported values are `docker` and `podman`.
	BuildTool string

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
	ServiceAccount string

	// If true, application listeners will use the underlying nodes' network.
	// This behavior is generally discouraged, but it may be useful when running
	// the application in a minikube environment, where using the underlying
	// nodes' network may make it easier to access the listeners directly from
	// the host machine.
	UseHostNetwork bool

	// Options for the application listeners. If a listener isn't specified in the
	// config, default options will be used.
	Listeners []listenerSpec

	// Resource requirements needed to run the pods. Note that the resources should
	// satisfy the format specified in [1].
	//
	// [1] https://pkg.go.dev/k8s.io/api/core/v1#ResourceRequirements.
	ResourceSpec *corev1.ResourceRequirements

	// Specs on how to scale the pods. Note that the scaling specs should satisfy
	// the format specified in [1].
	//
	// [1] https://pkg.go.dev/k8s.io/kubernetes/pkg/apis/autoscaling#HorizontalPodAutoscalerSpec.
	ScalingSpec *autoscalingv2.HorizontalPodAutoscalerSpec

	// Specs for pod affinity. Note that the affinity specs should satisfy
	// the format specified in [1].
	//
	// [1] https://pkg.go.dev/k8s.io/api/core/v1#Affinity
	AffinitySpec *corev1.Affinity

	// Volumes that should be provided to all the running components.
	StorageSpec volumeSpecs

	// Options for probes to check the readiness/liveness/startup of the pods.
	// Note that the scaling specs should satisfy the format specified in [1].
	//
	// [1] https://pkg.go.dev/k8s.io/api/core/v1#Probe.
	ProbeSpec probes

	// Groups contains kubernetes configuration for groups of collocated components.
	// Note that some knobs if specified for a group will override the corresponding
	// knob set for all the groups (e.g., ScalingSpec, ResourceSpec); for knobs like
	// Volumes, each group will contain the sum of the volumes specified for all
	// groups and the ones set for the group.
	Groups []group

	// TelemetrySpec contains options to control how the telemetry is being manipulated.
	Telemetry telemetry
}

// listenerSpec stores configuration options for a listener.
type listenerSpec struct {
	// Listener name.
	Name string

	// If specified, the listener service will have the name set to this value.
	// Otherwise, we will generate a unique name for each app version.
	ServiceName string

	// Is the listener public, i.e., should it receive ingress traffic
	// from the public internet. If false, the listener is configured only
	// for cluster-internal access.
	Public bool

	// If specified, the port inside the container on which the listener
	// is reachable. If zero or not specified, the first available port
	// is used.
	Port int32
}

// Encapsulates probe specs as defined by the user in the kubernetes config.
//
// Note that we create this struct, so we can group the probe specs under a single
// section in the yaml config file.
type probes struct {
	ReadinessProbe *corev1.Probe // Periodic probe of container service readiness.
	LivenessProbe  *corev1.Probe // Periodic probe of container liveness.
	StartupProbe   *corev1.Probe // Indicates that the pod has successfully initialized.
}

// group contains kubernetes configuration for a group of colocated components.
type group struct {
	Name         string      // name of the group
	Components   []string    // list of components in the group
	StorageSpec  volumeSpecs // list of volumes and volume mounts
	ResourceSpec *corev1.ResourceRequirements
	ScalingSpec  *autoscalingv2.HorizontalPodAutoscalerSpec
	listeners    []listener // hosted listeners, populated by the kube deployer.
}

// volumeSpecs encapsulates volumes and volume mounts specs as defined by the
// user in the kubernetes config.
type volumeSpecs struct {
	Volumes      []corev1.Volume
	VolumeMounts []corev1.VolumeMount
}

// telemetry contains options to control how the telemetry is being manipulated.
type telemetry struct {
	Metrics metricOpts
}

// metricOpts contains options to configure how the metrics are handled by the deployer.
type metricOpts struct {
	// If true, the deployer also exports metrics created by the framework; e.g.,
	// autogenerated metrics, http metrics.
	Generated bool

	// How often to export the metrics. By default, we export metrics every 30 seconds.
	ExportInterval string
}
