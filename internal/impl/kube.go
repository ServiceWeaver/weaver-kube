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
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ServiceWeaver/weaver-kube/internal/proto"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"golang.org/x/exp/maps"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	_ "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	deploymentId    string            // globally unique deployment id
	image           string            // Docker image URI
	traceServiceURL string            // where traces are exported to, if not empty
	config          *kubeConfig       // [kube] config from weaver.toml
	app             *protos.AppConfig // parsed weaver.toml
	nodes           []node            // nodes
}

// node contains information about a possibly replicated group of components.
type node struct {
	name       string     // node name
	components []string   // hosted components
	listeners  []listener // hosted listeners
}

// listener contains information about a listener.
type listener struct {
	name        string // listener name
	serviceName string // Kubernetes service name
	port        int32  // port on which listener listens
	public      bool   // is the listener publicly accessible
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
	return fmt.Sprintf("%s-%s-%s", shortened, deploymentId[:8], hash)
}

// buildDeployment generates a Kubernetes Deployment for a node.
//
// TODO(rgrandl): test to see if it works with an app where a component foo is
// collocated with main, and a component bar that is not collocated with main
// calls foo.
func buildDeployment(d deployment, n node) (*appsv1.Deployment, error) {
	// Create labels.
	name := deploymentName(d.app.Name, n.name, d.deploymentId)
	podLabels := map[string]string{
		"serviceweaver/name":    name,
		"serviceweaver/app":     d.app.Name,
		"serviceweaver/version": d.deploymentId[:8],
	}
	if d.config.Observability[metricsConfigKey] != disabled {
		podLabels["metrics"] = d.app.Name // Needed by Prometheus to scrape the metrics.
	}

	// Pick DNS policy.
	dnsPolicy := corev1.DNSClusterFirst
	if d.config.UseHostNetwork {
		dnsPolicy = corev1.DNSClusterFirstWithHostNet
	}

	// Create container.
	container, err := buildContainer(d, n)
	if err != nil {
		return nil, err
	}

	// Create Deployment.
	return &appsv1.Deployment{
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
				"description": fmt.Sprintf("This Deployment hosts components %v.", strings.Join(n.components, ", ")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"serviceweaver/name": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    podLabels,
					Namespace: d.config.Namespace,
					Annotations: map[string]string{
						"description": fmt.Sprintf("This Pod hosts components %v.", strings.Join(n.components, ", ")),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: d.config.ServiceAccount,
					Containers:         []corev1.Container{container},
					DNSPolicy:          dnsPolicy,
					HostNetwork:        d.config.UseHostNetwork,
				},
			},
		},
	}, nil
}

// buildListenerService generates a kubernetes service for a listener.
//
// Note that for public listeners, we generate a Load Balancer service because
// it has to be reachable from the outside; for internal listeners, we generate
// a ClusterIP service, reachable only from internal Service Weaver services.
func buildListenerService(d deployment, n node, lis listener) (*corev1.Service, error) {
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
				"serviceweaver/name": deploymentName(d.app.Name, n.name, d.deploymentId),
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

// buildAutoscaler generates a Kubernetes HorizontalPodAutoscaler for a node.
func buildAutoscaler(d deployment, n node) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	// Per deployment name that is app version specific.
	name := deploymentName(d.app.Name, n.name, d.deploymentId)
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
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name,
			},
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
		},
	}, nil
}

// buildContainer builds a container specification for a node.
func buildContainer(d deployment, n node) (corev1.Container, error) {
	// Rewrite the app config to point to the binary in the container.
	d.app.Binary = fmt.Sprintf("/weaver/%s", filepath.Base(d.app.Binary))

	// Create the ReplicaSetConfig passed to the babysitter.
	//
	// TODO(mwhittaker): We associate every listener with the first component.
	// This is technically incorrect, but doesn't affect how the babysitter
	// runs. This will get simplified when I simplify ReplicaSetConfig.
	components := make([]*ReplicaSetConfig_Component, len(n.components))
	for i, component := range n.components {
		components[i] = &ReplicaSetConfig_Component{Name: component}
	}
	components[0].Listeners = make([]*ReplicaSetConfig_Listener, len(n.listeners))
	for i, listener := range n.listeners {
		components[0].Listeners[i] = &ReplicaSetConfig_Listener{
			Name:         listener.name,
			ServiceName:  listener.serviceName,
			ExternalPort: listener.port,
			IsPublic:     listener.public,
		}
	}
	configString, err := proto.ToEnv(&ReplicaSetConfig{
		Namespace:       d.config.Namespace,
		Name:            n.name,
		DepId:           d.deploymentId,
		App:             d.app,
		TraceServiceUrl: d.traceServiceURL,
		Components:      components,
	})
	if err != nil {
		return corev1.Container{}, err
	}

	// Gather the set of ports.
	var ports []corev1.ContainerPort
	for _, l := range n.listeners {
		ports = append(ports, corev1.ContainerPort{
			Name:          l.name,
			ContainerPort: l.port,
		})
	}
	if d.config.Observability[metricsConfigKey] != disabled {
		// Expose the metrics port from the container, so it can be
		// discoverable for scraping by Prometheus.
		//
		// TODO(rgrandl): We may want to have a default metrics port that can
		// be scraped by any metrics collection system. For now, disable the
		// port if Prometheus will not collect the metrics.
		ports = append(ports, corev1.ContainerPort{
			Name:          "prometheus",
			ContainerPort: defaultMetricsPort,
		})
	}

	// Gather the set of resources.
	resources, err := computeResourceRequirements(d.config.Resources)
	if err != nil {
		return corev1.Container{}, err
	}

	c := corev1.Container{
		Name:            appContainerName,
		Image:           d.image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"babysitter"},
		Env: []corev1.EnvVar{
			{Name: kubeConfigEnvKey, Value: configString},
		},
		Resources: resources,
		Ports:     ports,

		// Enabling TTY and Stdin allows the user to run a shell inside the
		// container, for debugging.
		TTY:   true,
		Stdin: true,
	}

	createProbeFn := func(opts *probeOptions) *corev1.Probe {
		probe := &corev1.Probe{}
		if opts.TimeoutSecs > 0 {
			probe.TimeoutSeconds = opts.TimeoutSecs
		}
		if opts.PeriodSecs > 0 {
			probe.PeriodSeconds = opts.PeriodSecs
		}
		if opts.SuccessThreshold > 0 {
			probe.SuccessThreshold = opts.SuccessThreshold
		}
		if opts.FailureThreshold > 0 {
			probe.FailureThreshold = opts.FailureThreshold
		}
		if opts.Tcp != nil {
			probe.TCPSocket = &corev1.TCPSocketAction{Port: intstr.IntOrString{IntVal: opts.Tcp.Port}}
		}
		if opts.Http != nil {
			probe.HTTPGet = &corev1.HTTPGetAction{Port: intstr.IntOrString{IntVal: opts.Http.Port}}
			if opts.Http.Path != "" {
				// If no path specified, the HTTPGetAction will do health checks on "/".
				probe.HTTPGet.Path = opts.Http.Path
			}
		}
		if opts.Exec != nil {
			// Command is optional for an ExecAction. However, it's confusing why that's
			// the case, especially that this is the only parameter to configure for an
			// ExecAction.
			probe.Exec = &corev1.ExecAction{Command: opts.Exec.Cmd}
		}
		return probe
	}

	// Add probes if any.
	if cfg.LivenessProbeOpts != nil {
		c.LivenessProbe = createProbeFn(cfg.LivenessProbeOpts)
	}
	if cfg.ReadinessProbeOpts != nil {
		c.LivenessProbe = createProbeFn(cfg.ReadinessProbeOpts)
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
func generateYAMLs(image string, app *protos.AppConfig, depId string, cfg *kubeConfig) error {
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

	// Generate core YAMLs (deployments, services, autoscalers).
	if err := generateCoreYAMLs(&b, d); err != nil {
		return fmt.Errorf("unable to create kube app deployment: %w", err)
	}

	// Generate deployment info needed to get insights into the application.
	if err := generateObservabilityYAMLs(&b, app.Name, cfg); err != nil {
		return fmt.Errorf("unable to create configuration information: %w", err)
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
#     kubectl get all --selector=version={{.Version}}
#
# To view a description of every resource, run:
#
#     kubectl get all -o custom-columns=KIND:.kind,NAME:.metadata.name,APP:.metadata.labels.serviceweaver/app,VERSION:.metadata.labels.serviceweaver/version,DESCRIPTION:.metadata.annotations.description
#
# To delete the resources, run:
#
#     kubectl delete all --selector=version={{.Version}}

`))

	// Extract the tool version.
	toolVersion, _, err := ToolVersion()
	if err != nil {
		return "", err
	}

	// Compute groups.
	groups := make([][]string, len(d.nodes))
	for i, n := range d.nodes {
		groups[i] = n.components
	}

	// Compute listeners.
	var listeners []string
	for _, n := range d.nodes {
		for _, lis := range n.listeners {
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

// generateCoreYAMLs generates the core YAMLs for the given deployment.
func generateCoreYAMLs(w io.Writer, d deployment) error {
	// For each node, build a deployment and an autoscaler. If a node has any
	// listeners, build a service for each listener.
	for _, n := range d.nodes {
		// Build a Service for each listener.
		for _, lis := range n.listeners {
			service, err := buildListenerService(d, n, lis)
			if err != nil {
				return fmt.Errorf("unable to create kube listener service for %s: %w", lis.name, err)
			}
			if err := marshalResource(w, service, fmt.Sprintf("Listener Service for node %s", n.name)); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Generated kube listener service for listener %v\n", lis.name)
		}

		// Build a Deployment for the node.
		deployment, err := buildDeployment(d, n)
		if err != nil {
			return fmt.Errorf("unable to create kube deployment for node %s: %w", n.name, err)
		}
		if err := marshalResource(w, deployment, fmt.Sprintf("Deployment for node %s", n.name)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Generated kube deployment for node %v\n", n.name)

		// Build autoscaler HorizontalPodAutoscaler for the Deployment.
		autoscaler, err := buildAutoscaler(d, n)
		if err != nil {
			return fmt.Errorf("unable to create kube autoscaler for node %s: %w", n.name, err)
		}
		if err := marshalResource(w, autoscaler, fmt.Sprintf("Autoscaler for node %s", n.name)); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Generated kube autoscaler for node %v\n", n.name)
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
	groups := map[string]*protos.ComponentGroup{}
	for _, group := range app.Colocate {
		for _, component := range group.Components {
			groups[component] = group
		}
	}

	// Form nodes based on groups.
	nodesByName := map[string]node{}
	for component, listeners := range components {
		// We use the first component in a group as the name of the node.
		name := component
		if group, ok := groups[component]; ok {
			name = group.Components[0]
		}

		// Append the component and listeners to the node.
		n, ok := nodesByName[name]
		if !ok {
			n = node{name: name}
		}
		n.components = append(n.components, component)
		for _, name := range listeners {
			n.listeners = append(n.listeners, newListener(depId, cfg, name))
		}
		nodesByName[name] = n
	}

	// Sort nodes by name to ensure stable YAML.
	nodes := maps.Values(nodesByName)
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].name < nodes[j].name
	})

	// Compute the URL of the export traces service.
	var traceServiceURL string
	switch jservice := cfg.Observability[tracesConfigKey]; {
	case jservice == auto:
		// Point to the service launched by the kube deployer.
		traceServiceURL = fmt.Sprintf("http://%s:%d/api/traces", name{app.Name, jaegerAppName}.DNSLabel(), defaultJaegerCollectorPort)
	case jservice != disabled:
		// Point to the service launched by the user.
		traceServiceURL = fmt.Sprintf("http://%s:%d/api/traces", jservice, defaultJaegerCollectorPort)
	default:
		// No trace to export.
	}

	return deployment{
		deploymentId:    depId,
		image:           image,
		traceServiceURL: traceServiceURL,
		config:          cfg,
		app:             app,
		nodes:           nodes,
	}, nil

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

	opts, ok := config.Listeners[name]
	if ok && opts.ServiceName != "" {
		lis.serviceName = opts.ServiceName
	}
	if ok && opts.Public {
		lis.public = opts.Public
	}
	if ok && opts.Port != 0 {
		lis.port = opts.Port
	}
	return lis
}

// computeResourceRequirements computes resource requirements.
func computeResourceRequirements(req resourceRequirements) (corev1.ResourceRequirements, error) {
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}

	// Compute the resource requests.
	if req.RequestsMem != "" {
		reqsMem, err := resource.ParseQuantity(req.RequestsMem)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("unable to parse requests_mem: %w", err)
		}
		requests[corev1.ResourceMemory] = reqsMem
	}
	if req.RequestsCPU != "" {
		reqsCPU, err := resource.ParseQuantity(req.RequestsCPU)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("unable to parse requests_cpu: %w", err)
		}
		requests[corev1.ResourceCPU] = reqsCPU
	}

	// Compute the resource limits.
	if req.LimitsMem != "" {
		limitsMem, err := resource.ParseQuantity(req.LimitsMem)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("unable to parse limits_mem: %w", err)
		}
		limits[corev1.ResourceMemory] = limitsMem
	}
	if req.LimitsCPU != "" {
		limitsCPU, err := resource.ParseQuantity(req.LimitsCPU)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("unable to parse limits_cpu: %w", err)
		}
		limits[corev1.ResourceCPU] = limitsCPU
	}

	return corev1.ResourceRequirements{
		Requests: requests,
		Limits:   limits,
	}, nil
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
