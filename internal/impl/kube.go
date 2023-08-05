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
	"time"

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

	// The name of the Jaeger application.
	jaegerAppName = "jaeger"

	// The name of the jaeger image used to handle the traces.
	//
	// all-in-one[1] combines the three Jaeger components: agent, collector, and
	// query service/UI in a single binary, which is enough for handling the traces
	// in a kubernetes deployment. However, we don't really need an agent. Also,
	// we may want to launch separate collector and query services later on. Or,
	// we may want to launch an otel collector service as well, to ensure that the
	// traces are available, even if the deployment is deleted.
	//
	// [1] https://www.jaegertracing.io/docs/1.45/deployment/#all-in-one
	jaegerImageName = "jaegertracing/all-in-one"

	// The name of the Prometheus [1] image used to handle the metrics.
	//
	// [1] https://prometheus.io/
	prometheusImageName = "prom/prometheus:v2.30.3"

	// The name of the Loki [1] image used to handle the logs.
	//
	// [1] https://grafana.com/oss/loki/
	lokiImageName = "grafana/loki"

	// The name of the Promtail [1] image used to scrape the logs.
	//
	// [1] https://grafana.com/docs/loki/latest/clients/promtail/
	promtailImageName = "grafana/promtail"

	// The name of the Grafana [1] image used to display metrics, traces, and logs.
	//
	// [1] https://grafana.com/
	grafanaImageName = "grafana/grafana"

	// The exported port by the Service Weaver services.
	servicePort = 80

	// The port on which the Jaeger UI is listening on.
	jaegerUIPort = 16686

	// The port on which the Jaeger collector is receiving traces from the
	// clients when using the Jaeger exporter.
	jaegerCollectorPort = 14268

	// The port on which the weavelets are exporting the metrics.
	metricsPort = 9090

	// The port on which Loki is exporting the logs.
	lokiPort = 3100

	// The default Grafana web server port.
	grafanaPort = 3000

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

//go:embed dashboard.txt
var dashboardContent string

// replicaSetInfo contains information associated with a replica set.
type replicaSetInfo struct {
	name  string             // name of the replica set
	image string             // name of the image to be deployed
	dep   *protos.Deployment // deployment info

	// set of the components hosted by the replica set and their listeners,
	// keyed by component name.
	components map[string]*ReplicaSetConfig_Listeners

	// port used by the weavelets that are part of the replica set to listen on
	// for internal traffic.
	internalPort int
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

	// Options for the application listeners, keyed by listener name.
	// If a listener isn't specified in the map, default options will be used.
	Listeners map[string]*ListenerOptions
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
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
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
			Name: globalLisName,
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
		ObjectMeta: metav1.ObjectMeta{Name: aname},
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
		Deployment:        r.dep,
		ReplicaSet:        r.name,
		ComponentsToStart: r.components,
		InternalPort:      int32(r.internalPort),
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
	content, err := generateRolesAndBindings()
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

	// Generate the Jaeger deployment info.
	content, err = generateJaegerDeployment(dep)
	if err != nil {
		return fmt.Errorf("unable to create kube jaeger deployment: %w", err)
	}
	generated = append(generated, content...)

	// Generate the Prometheus deployment info.
	content, err = generatePrometheusDeployment(dep)
	if err != nil {
		return fmt.Errorf("unable to create kube deployment for the Prometheus service: %w", err)
	}
	generated = append(generated, content...)

	// Generate the Loki deployment info.
	content, err = generateLokiDeployment(dep)
	if err != nil {
		return fmt.Errorf("unable to create kube deployment for the Loki service: %w", err)
	}
	generated = append(generated, content...)

	// Generate the Promtail deployment info.
	content, err = generatePromtailDeployment(dep)
	if err != nil {
		return fmt.Errorf("unable to create kube deployment for Promtail: %w", err)
	}
	generated = append(generated, content...)

	// Generate the Grafana deployment info.
	content, err = generateGrafanaDeployment(dep)
	if err != nil {
		return fmt.Errorf("unable to create kube deployment for the Grafana service: %w", err)
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

// generateRolesAndBindings generates Kubernetes roles and role bindings that
// grant permissions to the appropriate service accounts.
func generateRolesAndBindings() ([]byte, error) {
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
			Name: "pods-getter",
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
			Name: "default-pods-getter",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "pods-getter",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: "ServiceAccount",
				Name: "default",
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

// generateJaegerDeployment generates the Jaeger kubernetes deployment and service
// information for a given app.
//
// Note that we run a single instance of Jaeger. This is because we are using
// a Jaeger image that combines three Jaeger components, agent, collector, and
// query service/UI in a single image.
// TODO(rgrandl): If the trace volume can't be handled by a single instance, we
// should scale these components independently, and use different image(s).
func generateJaegerDeployment(dep *protos.Deployment) ([]byte, error) {
	jname := name{dep.App.Name, jaegerAppName}.DNSLabel()

	// Generate the Jaeger deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{Name: jname},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"jaeger": jname,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"jaeger": jname,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            jname,
							Image:           fmt.Sprintf("%s:latest", jaegerImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
						},
					},
				},
			},
			Strategy: v1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &v1.RollingUpdateDeployment{},
			},
		},
	}
	content, err := yaml.Marshal(d)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte("# Jaeger Deployment\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Jaeger deployment\n")

	// Generate the Jaeger service.
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{Name: jname},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"jaeger": jname},
			Ports: []corev1.ServicePort{
				{
					Name:       "ui-port",
					Port:       jaegerUIPort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(jaegerUIPort)},
				},
				{
					Name:       "collector-port",
					Port:       jaegerCollectorPort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(jaegerCollectorPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte("\n# Jaeger Service\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Jaeger service\n")

	return generated, nil
}

// generatePrometheusDeployment generates the kubernetes configurations to deploy
// a Prometheus service for a given app.
//
// TODO(rgrandl): check if we can simplify the config map, and the deployment info.
//
// TODO(rgrandl): We run a single instance of Prometheus for now. We might want
// to scale it up if it becomes a bottleneck.
func generatePrometheusDeployment(dep *protos.Deployment) ([]byte, error) {
	cname := name{dep.App.Name, "prometheus", "config"}.DNSLabel()
	pname := name{dep.App.Name, "prometheus"}.DNSLabel()

	// Build the config map that holds the prometheus configuration file. In the
	// config we specify how to scrape the app pods for the metrics.
	config := fmt.Sprintf(`
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: "%s"
    metrics_path: %s
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - default
    scheme: http
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_metrics]
        regex: "%s"
        action: keep
`, pname, prometheusEndpoint, dep.App.Name)

	// Create a config map to store the prometheus config.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{Name: cname},
		Data: map[string]string{
			"prometheus.yaml": config,
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cname)

	// Build the kubernetes Prometheus deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{Name: pname},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"prometheus": pname},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"prometheus": pname},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            pname,
							Image:           prometheusImageName,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								fmt.Sprintf("--config.file=/etc/%s/prometheus.yaml", pname),
								fmt.Sprintf("--storage.tsdb.path=/%s", pname),
							},
							Ports: []corev1.ContainerPort{{ContainerPort: metricsPort}},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      cname,
									MountPath: fmt.Sprintf("/etc/%s/prometheus.yaml", pname),
									SubPath:   "prometheus.yaml",
								},
								{
									Name:      fmt.Sprintf("%s-data", pname),
									MountPath: fmt.Sprintf("/%s", pname),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: cname,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "prometheus.yaml",
											Path: "prometheus.yaml",
										},
									},
								},
							},
						},
						{
							Name: fmt.Sprintf("%s-data", pname),
						},
					},
				},
			},
			Strategy: v1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &v1.RollingUpdateDeployment{},
			},
		},
	}
	content, err = yaml.Marshal(d)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Prometheus Deployment %s\n", pname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for Prometheus %s\n", pname)

	// Build the kubernetes Prometheus service.
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{Name: pname},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"prometheus": pname},
			Ports: []corev1.ServicePort{
				{
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(metricsPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Prometheus Service %s\n", pname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube service for Prometheus %s\n", pname)
	return generated, nil
}

// generateLokiDeployment generates the kubernetes configurations to deploy
// a Loki service for a given app.
//
// TODO(rgrandl): check if we can simplify the config map, and the deployment info.
//
// TODO(rgrandl): We run a single instance of Loki for now. We might want
// to scale it up if it becomes a bottleneck.
func generateLokiDeployment(dep *protos.Deployment) ([]byte, error) {
	cname := name{dep.App.Name, "loki", "config"}.DNSLabel()
	lname := name{dep.App.Name, "loki"}.DNSLabel()

	timeSchemaEnabledFromFn := func() string {
		current := time.Now()
		year, month, day := current.Date()
		return fmt.Sprintf("%d-%02d-%02d", year, month, day)
	}

	// Build the config map that holds the Loki configuration file. In the
	// config we specify how to store the logs and the schema. Right now we have
	// a very simple in-memory store [1].
	//
	// TODO(rgrandl): There are millions of knobs to tune the config. We might revisit
	// this in the future.
	//
	// [1] https://grafana.com/docs/loki/latest/operations/storage/boltdb-shipper/
	config := fmt.Sprintf(`
auth_enabled: false
server:
  http_listen_port: %d

common:
  instance_addr: 127.0.0.1
  path_prefix: /tmp/%s
  storage:
    filesystem:
      chunks_directory: /tmp/%s/chunks
      rules_directory: /tmp/%s/rules
  replication_factor: 1
  ring:
    kvstore:
      store: inmemory

schema_config:
  configs:
    - from: %s  # Marks the starting point of this schema
      store: boltdb-shipper
      object_store: filesystem
      schema: v11
      index:
        prefix: index_
        period: 24h
`, lokiPort, lname, lname, lname, timeSchemaEnabledFromFn())

	// Create a config map to store the Loki config.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{Name: cname},
		Data: map[string]string{
			"loki.yaml": config,
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cname)

	// Build the kubernetes Loki deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{Name: lname},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"loki": lname},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"loki": lname},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            lname,
							Image:           fmt.Sprintf("%s:latest", lokiImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								fmt.Sprintf("--config.file=/etc/%s/loki.yaml", lname),
							},
							Ports: []corev1.ContainerPort{{ContainerPort: lokiPort}},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      cname,
									MountPath: fmt.Sprintf("/etc/%s/", lname),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: cname,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "loki.yaml",
											Path: "loki.yaml",
										},
									},
								},
							},
						},
					},
				},
			},
			Strategy: v1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &v1.RollingUpdateDeployment{},
			},
		},
	}
	content, err = yaml.Marshal(d)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Loki Deployment %s\n", lname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for Loki %s\n", lname)

	// Build the kubernetes Loki service.
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{Name: lname},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"loki": lname},
			Ports: []corev1.ServicePort{
				{
					Port:       lokiPort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(lokiPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Loki Service %s\n", lname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube service for Loki %s\n", lname)
	return generated, nil
}

// generatePromtailDeployment generates information that is needed to run a
// Promtail agent on each node in order to scrape the logs.
func generatePromtailDeployment(dep *protos.Deployment) ([]byte, error) {
	promName := name{dep.App.Name, "promtail"}.DNSLabel()
	lname := name{dep.App.Name, "loki"}.DNSLabel()

	// This configuration is a simplified version of the Promtail config generated
	// by helm [1]. Right now we scrape only logs from the pods. We may want to
	// scrape system information and nodes info as well.
	//
	// The scraped logs are sent to Loki for indexing and being stored.
	//
	// [1] https://helm.sh/docs/topics/charts/.
	config := fmt.Sprintf(`
server:
  log_format: logfmt
  http_listen_port: 3101

clients:
  - url: http://%s:%d/loki/api/v1/push

positions:
  filename: /run/promtail/positions.yaml

scrape_configs:
  - job_name: kubernetes-pods
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - default
    relabel_configs:
      - source_labels:
          - __meta_kubernetes_pod_label_appName
        regex: ^.*%s.*$
        action: keep
      - source_labels:
          - __meta_kubernetes_pod_label_appName
        action: replace
        target_label: app
      - source_labels:
          - __meta_kubernetes_pod_name
        action: replace
        target_label: pod
      - action: replace
        replacement: /var/log/pods/*$1/*.log
        separator: /
        source_labels:
        - __meta_kubernetes_pod_uid
        - __meta_kubernetes_pod_container_name
        target_label: __path__
`, lname, lokiPort, dep.App.Name)

	// Config is stored as a config map in the daemonset.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{Name: promName},
		Data: map[string]string{
			"promtail.yaml": config,
		},
	}

	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cm.Name))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cm.Name)

	// Create a daemonset that will run on each node. The daemonset will run promtail
	// in order to scrape the pods running on each node.
	dset := appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: promName,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"promtail": promName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"promtail": promName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "default",
					Containers: []corev1.Container{
						{
							Name:            promName,
							Image:           fmt.Sprintf("%s:latest", promtailImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args: []string{
								fmt.Sprintf("--config.file=/etc/%s/promtail.yaml", promName),
							},
							Ports: []corev1.ContainerPort{{ContainerPort: 3101}},
							Env: []corev1.EnvVar{
								{
									Name: "HOSTNAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: fmt.Sprintf("/etc/%s", promName),
								},
								{
									Name:      "run",
									MountPath: "/run/promtail",
								},
								{
									Name:      "containers",
									MountPath: "/var/lib/docker/containers",
									ReadOnly:  true,
								},
								{
									Name:      "pods",
									MountPath: "/var/log/pods",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cm.Name,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "promtail.yaml",
											Path: "promtail.yaml",
										},
									},
								},
							},
						},
						{
							Name: "run",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/run/promtail",
								},
							},
						},
						{
							Name: "containers",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/docker/containers",
								},
							},
						},
						{
							Name: "pods",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/log/pods",
								},
							},
						},
					},
				},
			},
			UpdateStrategy: v1.DaemonSetUpdateStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &v1.RollingUpdateDaemonSet{},
			},
		},
	}
	content, err = yaml.Marshal(dset)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte(fmt.Sprintf("\n# Promtail DaemonSet %s\n", promName))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube daemonset for Promtail %s\n", promName)
	return generated, nil
}

// generateGrafanaDeployment generates the kubernetes configurations to deploy
// a Grafana service for a given app.
//
// TODO(rgrandl): We run a single instance of Grafana for now. We might want
// to scale it up if it becomes a bottleneck.
func generateGrafanaDeployment(dep *protos.Deployment) ([]byte, error) {
	cname := name{dep.App.Name, "grafana", "config"}.DNSLabel()
	gname := name{dep.App.Name, "grafana"}.DNSLabel()
	pname := name{dep.App.Name, "prometheus"}.DNSLabel()
	jname := name{dep.App.Name, jaegerAppName}.DNSLabel()
	lname := name{dep.App.Name, "loki"}.DNSLabel()

	// Build the config map that holds the Grafana configuration file. In the
	// config we specify which data source connections the Grafana service should
	// export. By default, we export the Prometheus and Jaeger services in order to
	// have a single dashboard where we can visualize the metrics and the traces of
	// the app.
	//
	// TODO(rgrandl): add a data source connection for Loki, to export logs as well.
	config := fmt.Sprintf(`
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://%s
    isDefault: true
  - name: Jaeger
    type: jaeger
    url: http://%s:%d
  - name: Loki
    type: loki
    access: proxy
    url: http://%s:%d
`, pname, jname, jaegerUIPort, lname, lokiPort)

	// It contains the list of dashboard providers that load dashboards into
	// Grafana from the local filesystem [1].
	//
	// https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards
	const dashboard = `
apiVersion: 1
providers:
 - name: 'Service Weaver Dashboard'
   options:
     path: /etc/grafana/dashboards/default-dashboard.json
`

	// Create a config map to store the Grafana configs and the default dashboards.
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{Name: cname},
		Data: map[string]string{
			"grafana.yaml":           config,
			"dashboard-config.yaml":  dashboard,
			"default-dashboard.json": fmt.Sprintf(dashboardContent, dep.App.Name),
		},
	}
	content, err := yaml.Marshal(cm)
	if err != nil {
		return nil, err
	}
	var generated []byte
	generated = append(generated, []byte(fmt.Sprintf("\n# Config Map %s\n", cname))...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated kube deployment for config map %s\n", cname)

	// Generate the Grafana deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{Name: gname},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"grafana": gname},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"grafana": gname},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            gname,
							Image:           fmt.Sprintf("%s:latest", grafanaImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports:           []corev1.ContainerPort{{ContainerPort: grafanaPort}},
							VolumeMounts: []corev1.VolumeMount{
								{
									// By default, we have to store any data source connection that
									// should be exported by Grafana under provisioning/datasources.
									Name:      "datasource-volume",
									MountPath: "/etc/grafana/provisioning/datasources/",
								},
								{
									// By default, we need to store the dashboards config files under
									// provisioning/dashboards directory. Each config file can contain
									// a list of dashboards providers that load dashboards into Grafana
									// from the local filesystem. More info here [1].
									//
									// [1] https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards
									Name:      "dashboards-config",
									MountPath: "/etc/grafana/provisioning/dashboards/",
								},
								{
									// Mount the volume that stores the predefined dashboards.
									Name:      "dashboards",
									MountPath: "/etc/grafana/dashboards/",
								},
							},
							Env: []corev1.EnvVar{
								// TODO(rgrandl): we may want to enable the user to specify their
								// credentials in a different way.
								{
									Name:  "GF_SECURITY_ADMIN_USER",
									Value: "admin",
								},
								{
									Name:  "GF_SECURITY_ADMIN_PASSWORD",
									Value: "admin",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "datasource-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "grafana.yaml",
											Path: "grafana.yaml",
										},
									},
								},
							},
						},
						{
							Name: "dashboards-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "dashboard-config.yaml",
											Path: "dashboard-config.yaml",
										},
									},
								},
							},
						},
						{
							Name: "dashboards",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: cname,
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "default-dashboard.json",
											Path: "default-dashboard.json",
										},
									},
								},
							},
						},
					},
				},
			},
			Strategy: v1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &v1.RollingUpdateDeployment{},
			},
		},
	}
	content, err = yaml.Marshal(d)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte("# Grafana Deployment\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Grafana deployment\n")

	// Generate the Grafana service.
	//
	// TODO(rgrandl): should we create a load balancer instead of a cluster ip?
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{Name: gname},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"grafana": gname},
			Ports: []corev1.ServicePort{
				{
					Name:       "ui-port",
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(grafanaPort)},
				},
			},
		},
	}
	content, err = yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	generated = append(generated, []byte("\n# Grafana Service\n")...)
	generated = append(generated, content...)
	generated = append(generated, []byte("\n---\n")...)
	fmt.Fprintf(os.Stderr, "Generated Grafana service\n")

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

	// Build the replica sets.
	for c, listeners := range components {
		rsName := replicaSet(c, dep)
		if _, found := rsets[rsName]; !found {
			rsets[rsName] = &replicaSetInfo{
				name:         rsName,
				image:        image,
				dep:          dep,
				components:   map[string]*ReplicaSetConfig_Listeners{},
				internalPort: internalPort,
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
