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
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ServiceWeaver/weaver-kube/internal/proto"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"golang.org/x/exp/maps"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	v2 "k8s.io/api/autoscaling/v2"
	_ "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	// Name of the container that hosts the application binary.
	appContainerName = "serviceweaver"

	// kubeConfigEnvKey is the name of the env variable that contains deployment
	// information for a babysitter deployed using kube.
	kubeConfigEnvKey = "SERVICEWEAVER_DEPLOYMENT_CONFIG"

	// The exported port by the Service Weaver services.
	servicePort = 80

	// Port used by the weavelets to listen for internal traffic.
	//
	// TODO(mwhittaker): Remove internal port from kube.proto.
	internalPort = 10000
)

var (
	// Start value for ports used by the public and private listeners.
	externalPort = 20000

	// Resource allocation units for "cpu" and "memory" resources.
	//
	// TODO(rgrandl): Should we allow the user to customize how many
	// resources each pod starts with?
	cpuUnit    = resource.MustParse("100m")
	memoryUnit = resource.MustParse("128Mi")
)

// replicaSetInfo contains information associated with a replica set.
type replicaSetInfo struct {
	name      string             // name of the replica set
	image     string             // name of the image to be deployed
	namespace string             // namespace where the replica set will be deployed
	dep       *protos.Deployment // deployment info

	// set of the components hosted by the replica set and their listeners,
	// keyed by component name.
	components map[string]*ReplicaSetConfig_Listeners

	// port used by the weavelets that are part of the replica set to listen on
	// for internal traffic.
	internalPort int

	traceServiceURL string // trace exporter URL
}

// ListenerOptions stores configuration options for a listener.
type ListenerOptions struct {
	// Is the listener public, i.e., should it receive ingress traffic
	// from the public internet. If false, the listener is configured only
	// for cluster-internal access.
	Public bool
}

// KubeConfig stores the configuration information for one execution of a
// Service Weaver application deployed using the Kube deployer.
type KubeConfig struct {
	// Image is the name of the container image that "weaver kube deploy"
	// builds and uploads. For example, if Image is "docker.io/alanturing/foo",
	// then "weaver kube deploy" will build a container called
	// "docker.io/alanturing/foo" and upload it to Docker Hub.
	//
	// The format of Image depends on the registry being used. For example:
	//
	// - Docker Hub: USERNAME/NAME or docker.io/USERNAME/NAME
	// - Google Artifact Registry: LOCATION-docker.pkg.dev/PROJECT-ID/REPOSITORY/NAME
	// - GitHub Container Registry: ghcr.io/NAMESPACE/NAME
	//
	// Note that "weaver kube deploy" will automatically append a unique tag to
	// Image, so Image should not already contain a tag.
	Image string

	// Namespace is the name of the Kubernetes namespace where the application
	// should be deployed. If not specified, the application will be deployed in
	// the default namespace.
	Namespace string

	// Options for the application listeners, keyed by listener name.
	// If a listener isn't specified in the map, default options will be used.
	Listeners map[string]*ListenerOptions

	// Observability controls how the deployer will export observability information
	// such as logs, metrics and traces, keyed by service. If no options are
	// specified, the deployer will launch corresponding services for exporting logs,
	// metrics and traces automatically.
	//
	// We support the following observability services:
	// prometheus_service - to export metrics to Prometheus [1]
	// jaeger_service     - to export traces to Jaeger [2]
	// loki_service       - to export logs to Grafana Loki [3]
	// grafana_service    - to visualize/manipulate observability information [4]
	//
	// Possible values for each service:
	// 1) do not specify a value at all; leave it empty
	// this is the default value; kube deployer will automatically create the
	// observability service for you.
	//
	// 2) "none"
	// kube deployer will not export the corresponding observability information to
	// any service. E.g., prometheus_service = "none", it means that the user will
	// not be able to see any metrics at all. This can be useful for testing or
	// benchmarking the performance of your application.
	//
	// 3) "your_observability_service_name"
	//  if you already have a running service to collect metrics, traces or logs,
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
}

// globalName returns an unique name that persists across app versions.
func (r *replicaSetInfo) globalName() string {
	return name{r.dep.App.Name, r.name}.DNSLabel()
}

// deploymentName returns a name that is version specific.
func (r *replicaSetInfo) deploymentName() string {
	return name{r.dep.App.Name, r.name, r.dep.Id[:8]}.DNSLabel()
}

// buildDeployment generates a kubernetes deployment for a replica set.
//
// TODO(rgrandl): test to see if it works with an app where a component foo is
// collocated with main, and a component bar that is not collocated with main
// calls foo.
func (r *replicaSetInfo) buildDeployment() (*v1.Deployment, error) {
	matchLabels := map[string]string{}
	podLabels := map[string]string{
		"appName": r.dep.App.Name,
		"depName": r.deploymentName(),
		"metrics": r.dep.App.Name, // Needed by Prometheus to scrape the metrics.
	}
	name := r.deploymentName()
	if r.hasListeners() {
		name = r.globalName()

		// Set the match and the pod labels, so they can be reachable across
		// multiple app versions.
		matchLabels["globalName"] = r.globalName()
		podLabels["globalName"] = r.globalName()
	} else {
		matchLabels["depName"] = r.deploymentName()
	}

	container, err := r.buildContainer()
	if err != nil {
		return nil, err
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    podLabels,
					Namespace: r.namespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
			Strategy: v1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &v1.RollingUpdateDeployment{},
			},
			// Number of old ReplicaSets to retain to allow rollback.
			RevisionHistoryLimit: ptrOf(int32(1)),
			MinReadySeconds:      int32(5),
		},
	}, nil
}

// buildListenerService generates a kubernetes service for a listener.
//
// Note that for public listeners, we generate a Load Balancer service because
// it has to be reachable from the outside; for internal listeners, we generate
// a ClusterIP service, reachable only from internal Service Weaver services.
func (r *replicaSetInfo) buildListenerService(lis *ReplicaSetConfig_Listener) (*corev1.Service, error) {
	// Unique name that persists across app versions.
	// TODO(rgrandl): Specify whether the listener is public in the name.
	globalLisName := name{r.dep.App.Name, "lis", lis.Name}.DNSLabel()

	var serviceType string
	if lis.IsPublic {
		serviceType = "LoadBalancer"
	} else {
		serviceType = "ClusterIP"
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      globalLisName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"lisName": lis.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceType(serviceType),
			Selector: map[string]string{
				"globalName": r.globalName(),
			},
			Ports: []corev1.ServicePort{
				{
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: lis.ExternalPort},
				},
			},
		},
	}, nil
}

// buildAutoscaler generates a kubernetes horizontal pod autoscaler for a replica set.
func (r *replicaSetInfo) buildAutoscaler() (*v2.HorizontalPodAutoscaler, error) {
	// Per deployment name that is app version specific.
	aname := name{r.dep.App.Name, "hpa", r.name, r.dep.Id[:8]}.DNSLabel()

	var depName string
	if r.hasListeners() {
		depName = r.globalName()
	} else {
		depName = r.deploymentName()
	}
	return &v2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "autoscaling/v2",
			Kind:       "HorizontalPodAutoscaler",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      aname,
			Namespace: r.namespace,
		},
		Spec: v2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: v2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       depName,
			},
			MinReplicas: ptrOf(int32(1)),
			MaxReplicas: 10,
			Metrics: []v2.MetricSpec{
				{
					// The pods are scaled up/down when the average CPU
					// utilization is above/below 80%.
					Type: v2.ResourceMetricSourceType,
					Resource: &v2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: v2.MetricTarget{
							Type:               v2.UtilizationMetricType,
							AverageUtilization: ptrOf(int32(80)),
						},
					},
				},
			},
		},
	}, nil
}

// buildContainer builds a container for a replica set.
func (r *replicaSetInfo) buildContainer() (corev1.Container, error) {
	// Set the binary path in the deployment w.r.t. to the binary path in the
	// docker image.
	r.dep.App.Binary = fmt.Sprintf("/weaver/%s", filepath.Base(r.dep.App.Binary))
	kubeCfgStr, err := proto.ToEnv(&ReplicaSetConfig{
		Namespace:            r.namespace,
		Deployment:           r.dep,
		ReplicaSet:           r.name,
		ComponentsToStart:    r.components,
		InternalPort:         int32(r.internalPort),
		TraceExporterService: r.traceServiceURL,
	})
	if err != nil {
		return corev1.Container{}, err
	}
	return corev1.Container{
		Name:            appContainerName,
		Image:           r.image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"babysitter"},
		Env: []corev1.EnvVar{
			{Name: kubeConfigEnvKey, Value: kubeCfgStr},
		},
		Resources: corev1.ResourceRequirements{
			// NOTE: start with the smallest allowed limits, and count on autoscalers
			// doing the rest.
			//
			// NOTE: if we don't specify the minimum amount of compute resources
			// required, the autoscaler doesn't work properly, because the metric
			// server is not able to report the resource usage of the container.
			Requests: corev1.ResourceList{
				"memory": memoryUnit,
				"cpu":    cpuUnit,
			},
			// NOTE: we don't specify any limits, allowing all available node
			// resources to be used, if needed. Note that in practice, we
			// attach autoscalers to all of our containers, so the extra-usage
			// should be only for a short period of time.
		},

		// Expose the metrics port from the container, so it can be discoverable for
		// scraping by Prometheus.
		Ports: []corev1.ContainerPort{{ContainerPort: metricsPort}},

		// Enabling TTY and Stdin allows the user to run a shell inside the container,
		// for debugging.
		TTY:   true,
		Stdin: true,
	}, nil
}

// hasListeners returns whether a given replica set exports any listeners.
func (r *replicaSetInfo) hasListeners() bool {
	for _, listeners := range r.components {
		if listeners.Listeners != nil {
			return true
		}
	}
	return false
}

// GenerateKubeDeployment generates the kubernetes deployment and service
// information for a given app deployment.
//
// Note that for each application, we generate the following Kubernetes topology:
//
//   - for each replica set that has at least a listener, we generate a deployment
//     whose name is persistent across new app versions rollouts. Note that in
//     general this should only be the replica set that contains the main component.
//     We do this because by default we rely on RollingUpdate as a deployment
//     strategy to rollout new versions of the app. RollingUpdate will update the
//     existing pods for the replica set with the new version one by one, hence
//     the new application version is being deployed.
//     For example, let's assume that we have an app v1 with 2 replica sets main
//     and foo. main is the replica set that contains the public listener, and it
//     has 2 replicas. Next, we deploy the version v2 of the app. v2 will be
//     rolled out as follows:
//
//     [main v1] [main v1]     [main v1] [main v2]     [main v2] [main v2]
//     |            |          |         |             |         |
//     v            |       => v         v         =>  |         v
//     [foo v1] <---|          [foo v1]  [foo v2]      |-------> [foo v2]
//
//   - for all the replica sets, we create a per app version service so components
//     within an app version can communicate with each other.
//
//   - for all the listeners we create a load balancer or a cluster IP with an
//     unique name that is persistent across new app version rollouts.
//
//   - if Prometheus/Jaeger are enabled, we deploy corresponding services using
//     unique names as well s.t., we don't rollout new instances of Prometheus/Jaeger,
//     when we rollout new versions of the app.
func GenerateKubeDeployment(image string, dep *protos.Deployment, cfg *KubeConfig) error {
	fmt.Fprintf(os.Stderr, greenText(), "\nGenerating kube deployment info ...")

	// Generate roles and role bindings.
	var generated []byte
	content, err := generateRolesAndBindings(cfg.Namespace)
	if err != nil {
		return fmt.Errorf("unable to generate roles and bindings: %w", err)
	}
	generated = append(generated, content...)

	// Generate the kubernetes replica sets for the deployment.
	replicaSets, err := buildReplicaSetSpecs(dep, image, cfg)
	if err != nil {
		return fmt.Errorf("unable to create replica sets: %w", err)
	}

	// Generate the app deployment info.
	content, err = generateAppDeployment(replicaSets)
	if err != nil {
		return fmt.Errorf("unable to create kube app deployment: %w", err)
	}
	generated = append(generated, content...)

	// Generate deployment info needed to get insights into the application.
	content, err = generateObservabilityInfo(dep, cfg)
	if err != nil {
		return fmt.Errorf("unable to create observability information: %w", err)
	}
	generated = append(generated, content...)

	// Write the generated kube info into a file.
	yamlFile := fmt.Sprintf("kube_%s.yaml", dep.Id)
	f, err := os.OpenFile(yamlFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(generated); err != nil {
		return fmt.Errorf("unable to write the kube deployment info: %w", err)
	}
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("kube deployment information successfully generated in %s", yamlFile))
	return nil
}

// generateRolesAndBindings generates Kubernetes roles and role bindings in
// namespace that grant permissions to the appropriate service accounts.
func generateRolesAndBindings(namespace string) ([]byte, error) {
	// Grant the default service account the permission to get, list, and watch
	// pods. The babysitter watches pods to generate routing info.
	//
	// TODO(mwhittaker): This leaks permissions to the user's code. We should
	// avoid that. We might have to run the babysitter and weavelet in separate
	// containers or pods.

	role := rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pods-getter",
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}

	binding := rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-pods-getter",
			Namespace: namespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "pods-getter",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "default",
				Namespace: namespace,
			},
		},
	}

	var b bytes.Buffer
	fmt.Fprintln(&b, "# Roles and bindings.")

	bytes, err := yaml.Marshal(role)
	if err != nil {
		return nil, err
	}
	b.Write(bytes)
	fmt.Fprintf(&b, "\n---\n\n")

	bytes, err = yaml.Marshal(binding)
	if err != nil {
		return nil, err
	}
	b.Write(bytes)
	fmt.Fprintf(&b, "\n---\n\n")

	fmt.Fprintf(os.Stderr, "Generated roles and bindings\n")
	return b.Bytes(), nil
}

// generateAppDeployment generates the kubernetes deployment and service
// information for a given app deployment.
func generateAppDeployment(replicaSets map[string]*replicaSetInfo) ([]byte, error) {
	var generated []byte

	// For each replica set, build a deployment and a service. If a replica set
	// has any listeners, build a service for each listener.
	for _, rs := range replicaSets {
		// Build a deployment.
		d, err := rs.buildDeployment()
		if err != nil {
			return nil, fmt.Errorf("unable to create kube deployment for replica set %s: %w", rs.name, err)
		}
		content, err := yaml.Marshal(d)
		if err != nil {
			return nil, err
		}
		generated = append(generated, []byte(fmt.Sprintf("# Deployment for replica set %s\n", rs.name))...)
		generated = append(generated, content...)
		generated = append(generated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated kube deployment for replica set %v\n", rs.name)

		// Build a horizontal pod autoscaler for the deployment.
		a, err := rs.buildAutoscaler()
		if err != nil {
			return nil, fmt.Errorf("unable to create kube autoscaler for replica set %s: %w", rs.name, err)
		}
		content, err = yaml.Marshal(a)
		if err != nil {
			return nil, err
		}
		generated = append(generated, []byte(fmt.Sprintf("\n# Autoscaler for replica set %s\n", rs.name))...)
		generated = append(generated, content...)
		generated = append(generated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated kube autoscaler for replica set %v\n", rs.name)

		// Build a service for each listener.
		for _, listeners := range rs.components {
			for _, lis := range listeners.Listeners {
				ls, err := rs.buildListenerService(lis)
				if err != nil {
					return nil, fmt.Errorf("unable to create kube listener service for %s: %w", lis.Name, err)
				}
				content, err = yaml.Marshal(ls)
				if err != nil {
					return nil, err
				}
				generated = append(generated, []byte(fmt.Sprintf("\n# Listener Service for replica set %s\n", rs.name))...)
				generated = append(generated, content...)
				generated = append(generated, []byte("\n---\n")...)
				fmt.Fprintf(os.Stderr, "Generated kube listener service for listener %v\n", lis.Name)
			}
		}
		generated = append(generated, []byte("\n")...)
	}
	return generated, nil
}

// buildReplicaSetSpecs returns the replica sets specs for the deployment dep
// keyed by the replica set.
func buildReplicaSetSpecs(dep *protos.Deployment, image string, cfg *KubeConfig) (
	map[string]*replicaSetInfo, error) {
	rsets := map[string]*replicaSetInfo{}

	// Retrieve the components from the binary.
	components, err := getComponents(dep, cfg)
	if err != nil {
		return nil, err
	}

	// Compute the URL of the export traces service.
	var exportTracesURLInfo string
	val := cfg.Observability[exportTracesURL]
	exportTracesURLIsSet := val != "" && val != "none"
	if exportTracesURLIsSet {
		exportTracesURLInfo = fmt.Sprintf("http://%s:%d/api/traces", val, jaegerCollectorPort)
	} else {
		if val != "none" {
			exportTracesURLInfo = fmt.Sprintf("http://%s:%d/api/traces", name{dep.App.Name, jaegerAppName}.DNSLabel(), jaegerCollectorPort)
		}
	}

	// Build the replica sets.
	for c, listeners := range components {
		rsName := replicaSet(c, dep)
		if _, found := rsets[rsName]; !found {
			rsets[rsName] = &replicaSetInfo{
				name:            rsName,
				image:           image,
				namespace:       cfg.Namespace,
				dep:             dep,
				components:      map[string]*ReplicaSetConfig_Listeners{},
				internalPort:    internalPort,
				traceServiceURL: exportTracesURLInfo,
			}
		}

		rsets[rsName].components[c] = listeners
	}
	fmt.Fprintf(os.Stderr, "Replica sets generated successfully %v\n", maps.Keys(rsets))
	return rsets, nil
}

// replicaSet returns the name of the replica set that hosts the given component.
func replicaSet(component string, dep *protos.Deployment) string {
	for _, group := range dep.App.Colocate {
		for _, c := range group.Components {
			if c == component {
				return group.Components[0]
			}
		}
	}
	return component
}

// getComponents returns the list of components from a binary.
func getComponents(dep *protos.Deployment, cfg *KubeConfig) (map[string]*ReplicaSetConfig_Listeners, error) {
	// Get components.
	components := map[string]*ReplicaSetConfig_Listeners{}
	callGraph, err := bin.ReadComponentGraph(dep.App.Binary)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve the call graph for binary %s: %w", dep.App.Binary, err)
	}
	for _, edge := range callGraph {
		src, dst := edge[0], edge[1]
		if _, found := components[src]; !found {
			components[src] = &ReplicaSetConfig_Listeners{}
		}
		if _, found := components[dst]; !found {
			components[dst] = &ReplicaSetConfig_Listeners{}
		}
	}

	// Get listeners.
	listenersToComponent, err := bin.ReadListeners(dep.App.Binary)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve the listeners for binary %s: %w", dep.App.Binary, err)
	}

	for _, c := range listenersToComponent {
		if _, found := components[c.Component]; !found {
			return nil, fmt.Errorf("listeners mapped to unknown component: %s", c.Component)
		}
		for _, lis := range c.Listeners {
			public := false
			if opts := cfg.Listeners[lis]; opts != nil && opts.Public {
				public = true
			}
			components[c.Component].Listeners = append(components[c.Component].Listeners,
				&ReplicaSetConfig_Listener{
					Name:         lis,
					ExternalPort: int32(externalPort),
					IsPublic:     public,
				})
			externalPort++
		}
	}
	return components, nil
}
