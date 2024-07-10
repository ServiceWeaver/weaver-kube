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
	"crypto/sha256"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/google/uuid"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/types/known/durationpb"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	_ "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	// Name of the container that hosts the application binary.
	appContainerName = "serviceweaver"

	// The exported port by the Service Weaver services.
	servicePort = 80

	// Port used by the weavelets to listen for internal traffic.
	internalPort = 10000
)

// Start value for ports used by the public and private listeners.
var externalPort int32 = 20000

// deployment contains information about a deployment of a Service Weaver
// application.
//
// Note that this is different from a Kubernetes Deployment. A deployed Service
// Weaver application consists of many Kubernetes Deployments.
type deployment struct {
	deploymentId string            // globally unique deployment id
	image        string            // Docker image URI
	config       *kubeConfig       // [kube] config from weaver.toml
	app          *protos.AppConfig // parsed weaver.toml
	groups       []group           // groups
}

// listener contains information about a listener.
type listener struct {
	name        string // listener name
	serviceName string // Kubernetes service name
	port        int32  // port on which listener listens
	public      bool   // is the listener publicly accessible
}

// buildDeployment generates a Kubernetes Deployment for a group.
//
// TODO(rgrandl): test to see if it works with an app where a component foo is
// collocated with main, and a component bar that is not collocated with main
// calls foo.
func buildDeployment(d deployment, g group) (*appsv1.Deployment, error) {
	// Create labels.
	name := deploymentName(d.app.Name, g.Name, d.deploymentId)
	podLabels := map[string]string{
		"serviceweaver/name":    name,
		"serviceweaver/app":     d.app.Name,
		"serviceweaver/version": d.deploymentId[:8],
	}

	// Pick DNS policy.
	dnsPolicy := corev1.DNSClusterFirst
	if d.config.UseHostNetwork {
		dnsPolicy = corev1.DNSClusterFirstWithHostNet
	}

	matchLabels := map[string]string{
		"serviceweaver/name": name,
	}

	// Create container.
	container, err := buildContainer(d, g)
	if err != nil {
		return nil, err
	}

	// Create Deployment.
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.config.Namespace,
			Labels: map[string]string{
				"serviceweaver/app":     d.app.Name,
				"serviceweaver/version": d.deploymentId[:8],
			},
			Annotations: map[string]string{
				"description": fmt.Sprintf("This Deployment hosts components %v.", strings.Join(g.Components, ", ")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    podLabels,
					Namespace: d.config.Namespace,
					Annotations: map[string]string{
						"description": fmt.Sprintf("This Pod hosts components %v.", strings.Join(g.Components, ", ")),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: d.config.ServiceAccount,
					Containers:         []corev1.Container{container},
					DNSPolicy:          dnsPolicy,
					HostNetwork:        d.config.UseHostNetwork,
					Affinity:           updateAffinitySpec(d.config.AffinitySpec, matchLabels),
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName(d.deploymentId),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Add volume sources if any volume specified for the application or for the group.
	dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, d.config.StorageSpec.Volumes...)
	dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, g.StorageSpec.Volumes...)
	return dep, nil
}

// buildListenerService generates a kubernetes service for a listener.
//
// Note that for public listeners, we generate a Load Balancer service because
// it has to be reachable from the outside; for internal listeners, we generate
// a ClusterIP service, reachable only from internal Service Weaver services.
func buildListenerService(d deployment, g group, lis listener) (*corev1.Service, error) {
	serviceType := "ClusterIP"
	if lis.public {
		serviceType = "LoadBalancer"
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      lis.serviceName,
			Namespace: d.config.Namespace,
			Labels: map[string]string{
				"serviceweaver/app":      d.app.Name,
				"serviceweaver/listener": lis.name,
				"serviceweaver/version":  d.deploymentId[:8],
			},
			Annotations: map[string]string{
				"description": fmt.Sprintf("This Service forwards traffic to the %q listener.", lis.name),
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceType(serviceType),
			Selector: map[string]string{
				"serviceweaver/name": deploymentName(d.app.Name, g.Name, d.deploymentId),
			},
			Ports: []corev1.ServicePort{
				{
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: lis.port},
				},
			},
		},
	}, nil
}

// buildAutoscaler generates a Kubernetes HorizontalPodAutoscaler for a group.
func buildAutoscaler(d deployment, g group) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	// Per deployment name that is app version specific.
	name := deploymentName(d.app.Name, g.Name, d.deploymentId)

	var spec autoscalingv2.HorizontalPodAutoscalerSpec

	// Override the autoscaling spec if the user provides any spec. The scaling
	// spec set for the group takes priority.
	if g.ScalingSpec != nil { // Scaling spec specified for the group
		spec = *g.ScalingSpec
	} else if d.config.ScalingSpec != nil { // Scaling spec specified for the app
		spec = *d.config.ScalingSpec
	} else { // No scaling spec specified, compute default spec.
		spec = autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas: ptrOf(int32(1)),
			MaxReplicas: 10,
			Metrics: []autoscalingv2.MetricSpec{
				{
					// The pods are scaled up/down when the average CPU
					// utilization is above/below 80%.
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: ptrOf(int32(80)),
						},
					},
				},
			},
		}
	}

	spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       name,
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "autoscaling/v2",
			Kind:       "HorizontalPodAutoscaler",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.config.Namespace,
			Labels: map[string]string{
				"serviceweaver/app":     d.app.Name,
				"serviceweaver/version": d.deploymentId[:8],
			},
			Annotations: map[string]string{
				"description": fmt.Sprintf("This HorizontalPodAutoscaler scales the %q Deployment.", name),
			},
		},
		Spec: spec,
	}, nil
}

// buildContainer builds a container specification for a group.
func buildContainer(d deployment, g group) (corev1.Container, error) {
	// Gather the set of ports.
	var ports []corev1.ContainerPort
	for _, l := range g.listeners {
		ports = append(ports, corev1.ContainerPort{
			Name:          l.name,
			ContainerPort: l.port,
		})
	}

	c := corev1.Container{
		Name:            appContainerName,
		Image:           d.image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            append([]string{"babysitter", "/weaver/weaver.toml", "/weaver/config.textpb"}, g.Components...),
		Ports:           ports,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "config",
				MountPath: "/weaver/weaver.toml",
				SubPath:   "weaver.toml",
			},
			{
				Name:      "config",
				MountPath: "/weaver/config.textpb",
				SubPath:   "config.textpb",
			},
		},

		// Enabling TTY and Stdin allows the user to run a shell inside the
		// container, for debugging.
		TTY:   true,
		Stdin: true,
	}

	// Add volume mounts if any volume specified for the application or for the group.
	c.VolumeMounts = append(c.VolumeMounts, d.config.StorageSpec.VolumeMounts...)
	c.VolumeMounts = append(c.VolumeMounts, g.StorageSpec.VolumeMounts...)

	// Add custom resource requirements if any. The resource spec set for the group
	// takes priority.
	if g.ResourceSpec != nil {
		c.Resources = *g.ResourceSpec
	} else if d.config.ResourceSpec != nil {
		c.Resources = *d.config.ResourceSpec
	}

	// Add probes if any.
	if d.config.ProbeSpec.ReadinessProbe != nil {
		c.ReadinessProbe = d.config.ProbeSpec.ReadinessProbe
	}
	if d.config.ProbeSpec.LivenessProbe != nil {
		c.LivenessProbe = d.config.ProbeSpec.LivenessProbe
	}
	if d.config.ProbeSpec.StartupProbe != nil {
		c.StartupProbe = d.config.ProbeSpec.StartupProbe
	}

	return c, nil
}

// generateYAMLs generates Kubernetes YAML configurations for a given
// application version.
//
// The following Kubernetes YAML configurations will be generated:
//
//   - For replica sets that don't host any listeners, a Kubernetes Deployment
//     YAML with a unique name.
//
//   - For replica sets that do host a network listener, a Kubernetes Deployment
//     YAML with a stable name, i.e., a name that persists across application
//     versions. This crossed-version shared naming will be used to gradually
//     roll out a new application version.
//
//     For example, let's assume that we have an app v1 with two replica sets:
//     `main` and `foo`, with `main` hosting a network listener. When we deploy
//     v2 of the app, it will be rolled out as follows:
//
//     [main v1] [main v1]     [main v1] [main v2]     [main v2] [main v2]
//     |            |          |         |             |         |
//     v            |       => v         v         =>  |         v
//     [foo v1] <---|          [foo v1]  [foo v2]      |-------> [foo v2]
//
//   - For network listeners, a Kubernetes Service with a stable name, i.e.,
//     a name that persists across application versions.
//
//   - If observability services are enabled (e.g., Prometheus, Jaeger), a
//     Kubernetes Deployment and/or a Service for each observability service.
func generateYAMLs(app *protos.AppConfig, cfg *kubeConfig, depId, image string) error {
	fmt.Fprintf(os.Stderr, greenText(), "\nGenerating kube deployment info ...")

	// Form deployment.
	d, err := newDeployment(app, cfg, depId, image)
	if err != nil {
		return err
	}

	// Generate header.
	var b bytes.Buffer
	yamlFile := filepath.Join(os.TempDir(), fmt.Sprintf("kube_%s.yaml", depId[:8]))
	header, err := header(d, yamlFile)
	if err != nil {
		return err
	}
	b.WriteString(header)

	// Generate roles and role bindings.
	if err := generateRolesAndBindings(&b, cfg.Namespace, cfg.ServiceAccount); err != nil {
		return fmt.Errorf("unable to generate roles and bindings: %w", err)
	}

	// Generate configuration ConfigMap.
	if err := generateConfigMap(&b, cfg.AppConfig, d); err != nil {
		return fmt.Errorf("unable to generate configuration ConfigMap: %w", err)
	}

	// Generate core YAMLs (deployments, services, autoscalers).
	if err := generateCoreYAMLs(&b, d); err != nil {
		return fmt.Errorf("unable to create kube app deployment: %w", err)
	}

	// Write the generated kube info into a file.
	f, err := os.OpenFile(yamlFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(b.Bytes()); err != nil {
		return fmt.Errorf("unable to write the kube deployment info: %w", err)
	}
	fmt.Fprintf(os.Stderr, greenText(), "kube deployment information successfully generated")
	fmt.Println(yamlFile)
	return nil
}

// header returns the informational header at the top of a generated YAML file.
func header(d deployment, filename string) (string, error) {
	type content struct {
		ToolVersion string
		App         string
		Version     string
		Groups      [][]string
		Listeners   []string
		Filename    string
	}
	header := template.Must(template.New("header").Parse(`# This file was generated by "weaver kube" version {{.ToolVersion}} for the following
# application:
#
#     app: {{.App}}
#     version: {{.Version}}
#     components groups:
      {{- range .Groups}}
#     - {{range .}}
#       - {{.}}
		{{- end}}
      {{- end}}
#     listeners:
      {{- range .Listeners}}
#     - {{.}}
      {{- end}}
#
# This file contains the following resources:
#
#     1. A Deployment for every group of components.
#     2. A HorizontalPodAutoscaler for every Deployment.
#     3. A Service for every listener.
#     4. Some Roles and RoleBindings to configure permissions.
#
# To deploy these resources, run:
#
#     kubectl apply -f {{.Filename}}
#
# To view the deployed resources, run:
#
#     kubectl get all,configmaps --selector=serviceweaver/version={{.Version}}
#
# To view a description of every resource, run:
#
#     kubectl get all,configmaps --selector=serviceweaver/version={{.Version}} -o custom-columns=KIND:.kind,NAME:.metadata.name,APP:.metadata.labels.serviceweaver/app,VERSION:.metadata.labels.serviceweaver/version,DESCRIPTION:.metadata.annotations.description
#
# To delete the resources, run:
#
#     kubectl delete all,configmaps --selector=serviceweaver/version={{.Version}}

`))

	// Extract the tool version.
	toolVersion, _, err := ToolVersion()
	if err != nil {
		return "", err
	}

	// Compute groups.
	groups := make([][]string, len(d.groups))
	for i, g := range d.groups {
		groups[i] = g.Components
	}

	// Compute listeners.
	var listeners []string
	for _, g := range d.groups {
		for _, lis := range g.listeners {
			listeners = append(listeners, lis.name)
		}
	}

	// Execute template.
	var b strings.Builder
	err = header.Execute(&b, content{
		ToolVersion: toolVersion,
		App:         d.app.Name,
		Version:     d.deploymentId[:8],
		Groups:      groups,
		Listeners:   listeners,
		Filename:    filename,
	})
	return b.String(), err
}

// generateRolesAndBindings generates Kubernetes roles and role bindings in
// a given namespace that grant permissions to the appropriate service accounts.
func generateRolesAndBindings(w io.Writer, namespace, serviceAccount string) error {
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
			Name:      "serviceweaver-pods-getter",
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
			Name:      "serviceweaver-default-pods-getter",
			Namespace: namespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "serviceweaver-pods-getter",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccount,
				Namespace: namespace,
			},
		},
	}

	if err := marshalResource(w, role, "Roles."); err != nil {
		return err
	}
	if err := marshalResource(w, binding, "Role Bindings."); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Generated roles and bindings\n")
	return nil
}

// configMapName returns the name of the configuration ConfigMap.
func configMapName(deploymentId string) string {
	return fmt.Sprintf("config-%s", deploymentId[:8])
}

// generateConfigMap generates the ConfigMap used to configure an application.
// configFilename is the name of the weaver.toml configuration file.
func generateConfigMap(w io.Writer, configFilename string, d deployment) error {
	// Read weaver.toml.
	weaverToml, err := os.ReadFile(configFilename)
	if err != nil {
		return err
	}

	// Form config.textpb and mapping from component names to their group names.
	listeners := map[string]int32{}
	groups := map[string]string{}
	for _, g := range d.groups {
		for _, lis := range g.listeners {
			listeners[lis.name] = lis.port
		}
		for _, c := range g.Components {
			groups[c] = g.Name
		}
	}

	exportInterval, err := time.ParseDuration(d.config.Telemetry.Metrics.ExportInterval)
	if err != nil {
		return fmt.Errorf("unable to parse metrics export interval: %v", err)
	}

	babysitterConfig := &BabysitterConfig{
		Namespace:    d.config.Namespace,
		DeploymentId: d.deploymentId,
		Listeners:    listeners,
		Groups:       groups,
		Telemetry: &Telemetry{
			Metrics: &MetricOptions{
				AutoGenerateMetrics: d.config.Telemetry.Metrics.Generated,
				ExportInterval:      durationpb.New(exportInterval),
			},
		},
	}
	configTextpb, err := prototext.MarshalOptions{Multiline: true}.Marshal(babysitterConfig)
	if err != nil {
		return err
	}

	c := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(d.deploymentId),
			Namespace: d.config.Namespace,
			Labels: map[string]string{
				"serviceweaver/app":     d.app.Name,
				"serviceweaver/version": d.deploymentId[:8],
			},
			Annotations: map[string]string{
				"description": fmt.Sprintf("This ConfigMap contains config files for app %q version %q.", d.app.Name, d.deploymentId[:8]),
			},
		},
		Data: map[string]string{
			"weaver.toml":   string(weaverToml),
			"config.textpb": string(configTextpb),
		},
	}
	if err := marshalResource(w, c, "Configuration"); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Generated configuration ConfigMap\n")
	return nil
}

// generateCoreYAMLs generates the core YAMLs for the given deployment.
func generateCoreYAMLs(w io.Writer, d deployment) error {
	// For each group, build a deployment and an autoscaler. If a group has any
	// listeners, build a service for each listener.
	for _, g := range d.groups {
		// Build a Service for each listener.
		for _, lis := range g.listeners {
			service, err := buildListenerService(d, g, lis)
			if err != nil {
				return fmt.Errorf("unable to create kube listener service for %s: %w", lis.name, err)
			}
			if err := marshalResource(w, service, fmt.Sprintf("Listener Service for group %s", g.Name)); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Generated kube listener service for listener %v\n", lis.name)
		}

		// Build a Deployment for the group.
		deployment, err := buildDeployment(d, g)
		if err != nil {
			return fmt.Errorf("unable to create kube deployment for group %s: %w", g.Name, err)
		}
		if err := marshalResource(w, deployment, fmt.Sprintf("Deployment for group %s", g.Name)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Generated kube deployment for group %v\n", g.Name)

		// Build autoscaler HorizontalPodAutoscaler for the Deployment.
		autoscaler, err := buildAutoscaler(d, g)
		if err != nil {
			return fmt.Errorf("unable to create kube autoscaler for group %s: %w", g.Name, err)
		}
		if err := marshalResource(w, autoscaler, fmt.Sprintf("Autoscaler for group %s", g.Name)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Generated kube autoscaler for group %v\n", g.Name)
	}
	return nil
}

// newDeployment returns a new deployment for a Service Weaver application.
func newDeployment(app *protos.AppConfig, cfg *kubeConfig, depId, image string) (deployment, error) {
	// Read the components and listeners from the binary.
	components, err := readComponentsAndListeners(app.Binary)
	if err != nil {
		return deployment{}, err
	}

	// Map every component to its group, or nil if it's in a group by itself.
	groups := map[string]group{}
	for _, group := range cfg.Groups {
		for _, component := range group.Components {
			groups[component] = group
		}
	}

	// Form groups.
	groupsByName := map[string]group{}
	for component, listeners := range components {
		// We use the first component in a group as the name of the group.
		var gname string
		cgroup, ok := groups[component]
		if ok {
			gname = cgroup.Name
		} else {
			gname = component
		}

		// Append the component and listeners to the group.
		g, ok := groupsByName[gname]
		if !ok {
			g = group{
				Name:         gname,
				StorageSpec:  cgroup.StorageSpec,
				ResourceSpec: cgroup.ResourceSpec,
				ScalingSpec:  cgroup.ScalingSpec,
			}
		}
		g.Components = append(g.Components, component)
		for _, name := range listeners {
			g.listeners = append(g.listeners, newListener(depId, cfg, name))
		}

		groupsByName[gname] = g
	}

	// Sort groups by name to ensure stable YAML.
	sorted := maps.Values(groupsByName)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	return deployment{
		deploymentId: depId,
		image:        image,
		config:       cfg,
		app:          app,
		groups:       sorted,
	}, nil
}

// updateAffinitySpec updates an affinity spec with a rule that instructs the
// kubernetes scheduler to try its best to assign different replicas for the same
// deployment to different nodes.
//
// Note that this rule isn't necessarily enforced, and the scheduler can ignore
// it if there is no way this can be done (e.g., number of replicas is greater or
// equal to the number of nodes).
func updateAffinitySpec(spec *corev1.Affinity, labels map[string]string) *corev1.Affinity {
	updated := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
			{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: labels,
					},
					TopologyKey: corev1.LabelHostname,
				},
			},
		},
	},
	}
	if spec == nil {
		return updated
	}
	if spec.NodeAffinity != nil {
		updated.NodeAffinity = spec.NodeAffinity
	}
	if spec.PodAffinity != nil {
		updated.PodAffinity = spec.PodAffinity
	}
	if spec.PodAntiAffinity != nil {
		updated.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = spec.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		updated.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(
			updated.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, spec.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution...)
	}
	return updated
}

// newListener returns a new listener.
func newListener(depId string, config *kubeConfig, name string) listener {
	lis := listener{
		name:        name,
		serviceName: fmt.Sprintf("%s-%s", name, depId[:8]),
		public:      false,
		port:        externalPort,
	}
	externalPort++

	for _, l := range config.Listeners {
		if name != l.Name {
			continue
		}
		if l.ServiceName != "" {
			lis.serviceName = l.ServiceName
		}
		if l.Public {
			lis.public = l.Public
		}
		if l.Port != 0 {
			lis.port = l.Port
		}
		return lis
	}
	return lis
}

// readComponentsAndListeners returns a map from every component to its
// (potentially empty) set of listeners.
func readComponentsAndListeners(binary string) (map[string][]string, error) {
	// Read the components from the binary.
	names, _, err := bin.ReadComponentGraph(binary)
	if err != nil {
		return nil, err
	}
	components := map[string][]string{}
	for _, name := range names {
		components[name] = []string{}
	}

	// Read the listeners from the binary.
	ls, err := bin.ReadListeners(binary)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		components[l.Component] = l.Listeners
	}

	return components, nil
}

// hash8 computes a stable 8-byte hash over the provided strings.
func hash8(strs []string) string {
	h := sha256.New()
	var data []byte
	for _, str := range strs {
		h.Write([]byte(str))
		h.Write(data)
		data = h.Sum(data)
	}
	return uuid.NewHash(h, uuid.Nil, data, 0).String()[:8]
}

// marshalResource marshals the provided Kubernetes resource into YAML into the
// provided writer, prefixing it with the provided comment.
func marshalResource(w io.Writer, resource any, comment string) error {
	bytes, err := yaml.Marshal(resource)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\n# %s\n", comment)
	if _, err := w.Write(bytes); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n---\n")
	return nil
}

// shortenComponent shortens the given component name to be of the format
// <pkg>-<IfaceType>. (Recall that the full component name is of the format
// <path1>/<path2>/.../<pathN>/<IfaceType>.)
func shortenComponent(component string) string {
	parts := strings.Split(component, "/")
	switch len(parts) {
	case 0: // should never happen
		panic(fmt.Errorf("invalid component name: %s", component))
	case 1:
		return parts[0]
	default:
		return fmt.Sprintf("%s-%s", parts[len(parts)-2], parts[len(parts)-1])
	}
}

func deploymentName(app, component, deploymentId string) string {
	hash := hash8([]string{app, component, deploymentId})
	shortened := strings.ToLower(shortenComponent(component))
	return fmt.Sprintf("%s-%s-%s-%s", app, shortened, deploymentId[:8], hash)
}
