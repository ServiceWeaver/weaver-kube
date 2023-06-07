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
	"fmt"
	"os"
	"path/filepath"

	"github.com/ServiceWeaver/weaver-k8s/internal/proto"
	swruntime "github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	_ "go.opentelemetry.io/otel/exporters/jaeger"
	"golang.org/x/exp/maps"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	v2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	// Name of the container that hosts the application binary.
	appContainerName = "serviceweaver"

	// Key in a Kubernetes resource's label map that corresponds to the
	// application name that the resource is associated with. Used when looking
	// up resources that belong to a particular application.
	appNameKey = "serviceweaver/app_name"

	// Key in Kubernetes resource's label map that corresponds to the
	// application's deployment version. Used when looking up resources that
	// belong to a particular application version.
	deploymentIDKey = "serviceweaver/deployment_id"

	// k8sConfigEnvKey is the name of the env variable that contains deployment
	// information for a babysitter deployed using k8s.
	k8sConfigEnvKey = "SERVICEWEAVER_DEPLOYMENT_CONFIG"

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

	// The exported port by the Service Weaver services.
	servicePort = 80

	// The port on which the Jaeger UI is listening on.
	jaegerUIPort = 16686

	// The port on which the Jaeger collector is receiving traces from the
	// clients when using the Jaeger exporter.
	jaegerCollectorPort = 14268
)

var (
	// Start value for ports used by the weavelets to listen for internal traffic.
	internalPort = 10000

	// Start value for ports used by the public listeners.
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
	name string // name of the replica set

	// set of the components hosted by the replica set and their listeners,
	// keyed by component name.
	components map[string]*ReplicaSetConfig_Listeners

	// port used by the weavelets that are part of the replica set to listen on
	// for internal traffic.
	internalPort int
}

// GenerateK8sDeployment generates the k8s deployment and service information for
// a given app deployment.
func GenerateK8sDeployment(image string, dep *protos.Deployment) error {
	fmt.Fprintf(os.Stderr, greenText(), "\nGenerating k8s deployment info ...")

	// Generate the k8s replica sets for the deployment.
	replicaSets, err := buildReplicaSetSpecs(dep)
	if err != nil {
		return err
	}

	var k8sGenerated []byte

	// For each replica set, build a deployment and a service. If a replica set
	// has any listeners, build a service for each listener.
	for _, rsc := range replicaSets {
		// Build a deployment.
		d, err := buildDeployment(rsc, dep, image)
		if err != nil {
			return fmt.Errorf("unable to create k8s deployment for replica set %s: %w", rsc.name, err)
		}
		content, err := yaml.Marshal(d)
		if err != nil {
			return err
		}
		k8sGenerated = append(k8sGenerated, []byte(fmt.Sprintf("# Deployment for replica set %s\n", rsc.name))...)
		k8sGenerated = append(k8sGenerated, content...)
		k8sGenerated = append(k8sGenerated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated k8s deployment for replica set %v\n", rsc.name)

		// Build a horizontal pod autoscaler for the deployment.
		a, err := buildAutoscaler(rsc, dep)
		if err != nil {
			return fmt.Errorf("unable to create k8s autoscaler for replica set %s: %w", rsc.name, err)
		}
		content, err = yaml.Marshal(a)
		if err != nil {
			return err
		}
		k8sGenerated = append(k8sGenerated, []byte(fmt.Sprintf("\n# Autoscaler for replica set %s\n", rsc.name))...)
		k8sGenerated = append(k8sGenerated, content...)
		k8sGenerated = append(k8sGenerated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated k8s autoscaler for replica set %v\n", rsc.name)

		// Build a service.
		s, err := buildService(rsc, dep)
		if err != nil {
			return fmt.Errorf("unable to create k8s service for replica set %s: %w", rsc.name, err)
		}
		content, err = yaml.Marshal(s)
		if err != nil {
			return err
		}
		k8sGenerated = append(k8sGenerated, []byte(fmt.Sprintf("\n# Service for replica set %s\n", rsc.name))...)
		k8sGenerated = append(k8sGenerated, content...)
		k8sGenerated = append(k8sGenerated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated k8s service for replica set %v\n", rsc.name)

		// Build a service for each listener.
		for _, listeners := range rsc.components {
			for _, lis := range listeners.Listeners {
				ls, err := buildListenerService(rsc, lis, dep)
				if err != nil {
					return fmt.Errorf("unable to create k8s listener service for %s: %w", lis.Name, err)
				}
				content, err = yaml.Marshal(ls)
				if err != nil {
					return err
				}
				k8sGenerated = append(k8sGenerated, []byte(fmt.Sprintf("\n# Listener Service for replica set %s\n", rsc.name))...)
				k8sGenerated = append(k8sGenerated, content...)
				k8sGenerated = append(k8sGenerated, []byte("\n---\n")...)
				fmt.Fprintf(os.Stderr, "Generated k8s listener service for listener %v\n", lis.Name)
			}
		}
		k8sGenerated = append(k8sGenerated, []byte("\n")...)
	}

	// Generate the jaeger deployment info.
	jaegerGen, err := generateJaegerDeployment(dep)
	if err != nil {
		return fmt.Errorf("unable to create k8s jaeger deployment: %w", err)
	}
	k8sGenerated = append(k8sGenerated, jaegerGen...)

	// Write the k8s deployment info into a file.
	yamlFile := fmt.Sprintf("kube_%s.yaml", dep.Id)
	f, err := os.OpenFile(yamlFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(k8sGenerated); err != nil {
		return fmt.Errorf("unable to write the k8s deployment info: %w", err)
	}
	fmt.Fprintf(os.Stderr, redText(), fmt.Sprintf("k8s deployment information successfully generated in %s", yamlFile))
	return nil
}

// buildDeployment generates a k8s deployment for a replica set.
func buildDeployment(rs *replicaSetInfo, dep *protos.Deployment, image string) (*v1.Deployment, error) {
	name := name{dep.App.Name, rs.name, dep.Id[:8]}.DNSLabel()
	container, err := buildContainer(image, rs, dep)
	if err != nil {
		return nil, err
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				appNameKey:      dep.App.Name,
				deploymentIDKey: dep.Id,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":      name,
					"app_name": dep.App.Name,
					"dep_id":   dep.Id,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":      name,
						"app_name": dep.App.Name,
						"dep_id":   dep.Id,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
		},
	}, nil
}

// buildService generates a k8s service for a replica set.
func buildService(rs *replicaSetInfo, dep *protos.Deployment) (*corev1.Service, error) {
	name := name{dep.App.Name, rs.name, dep.Id[:8]}.DNSLabel()
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				appNameKey:      dep.App.Name,
				deploymentIDKey: dep.Id,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":      name,
				"app_name": dep.App.Name,
				"dep_id":   dep.Id,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: int32(rs.internalPort)},
				},
			},
		},
	}, nil
}

// buildService generates a k8s service for a listener.
func buildListenerService(rs *replicaSetInfo, lis *ReplicaSetConfig_Listener, dep *protos.Deployment) (*corev1.Service, error) {
	appName := name{dep.App.Name, rs.name, dep.Id[:8]}.DNSLabel()
	lisName := name{dep.App.Name, "lis", rs.name, dep.Id[:8]}.DNSLabel()
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name: lisName,
			Labels: map[string]string{
				appNameKey:      dep.App.Name,
				deploymentIDKey: dep.Id,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "LoadBalancer",
			Selector: map[string]string{
				"app":      appName,
				"app_name": dep.App.Name,
				"dep_id":   dep.Id,
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

// buildAutoscaler generates a k8s horizontal pod autoscaler for a replica set.
func buildAutoscaler(rs *replicaSetInfo, dep *protos.Deployment) (*v2.HorizontalPodAutoscaler, error) {
	aname := name{dep.App.Name, "hpa", rs.name, dep.Id[:8]}.DNSLabel()
	depName := name{dep.App.Name, rs.name, dep.Id[:8]}.DNSLabel()
	return &v2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "autoscaling/v2",
			Kind:       "HorizontalPodAutoscaler",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: aname,
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
func buildContainer(dockerImage string, rs *replicaSetInfo, dep *protos.Deployment) (corev1.Container, error) {
	// Set the binary path in the deployment w.r.t. to the binary path in the
	// docker image.
	dep.App.Binary = fmt.Sprintf("/weaver/%s", filepath.Base(dep.App.Binary))
	k8sCfgStr, err := proto.ToEnv(&ReplicaSetConfig{
		Deployment:        dep,
		ReplicaSet:        rs.name,
		ComponentsToStart: rs.components,
		InternalPort:      int32(rs.internalPort),
	})
	if err != nil {
		return corev1.Container{}, err
	}
	return corev1.Container{
		Name:            appContainerName,
		Image:           dockerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			fmt.Sprintf("/weaver/weaver-k8s babysitter"),
		},
		Env: []corev1.EnvVar{
			{Name: k8sConfigEnvKey, Value: k8sCfgStr},
		},
		Resources: corev1.ResourceRequirements{
			// NOTE: start with smallest allowed limits, and count on autoscalers
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

		// Enabling TTY and Stdin allows the user to run a shell inside the container,
		// for debugging.
		TTY:   true,
		Stdin: true,
	}, nil
}

// buildReplicaSetSpecs returns the replica sets specs for the deployment dep
// keyed by the replica set.
func buildReplicaSetSpecs(dep *protos.Deployment) (map[string]*replicaSetInfo, error) {
	rsets := map[string]*replicaSetInfo{}

	// Retrieve the components from the binary.
	components, err := getComponents(dep)
	if err != nil {
		return nil, err
	}

	// Build the replica sets.
	for c, listeners := range components {
		rsName := replicaSet(c, dep)
		if _, found := rsets[rsName]; !found {
			rsets[rsName] = &replicaSetInfo{
				name:         rsName,
				components:   map[string]*ReplicaSetConfig_Listeners{},
				internalPort: internalPort,
			}
			internalPort++
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
func getComponents(dep *protos.Deployment) (map[string]*ReplicaSetConfig_Listeners, error) {
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
	const k8sKey = "github.com/ServiceWeaver/weaver/k8s"
	const shortK8sKey = "k8s"

	type k8sConfigSchema struct {
		Listener []struct{ Name, Component string } `toml:"listeners"`
	}
	parsed := &k8sConfigSchema{}
	if err := swruntime.ParseConfigSection(k8sKey, shortK8sKey, dep.App.Sections, parsed); err != nil {
		return nil, fmt.Errorf("unable to parse k8s config: %w", err)
	}

	for _, lis := range parsed.Listener {
		if lis.Name == "" {
			return nil, fmt.Errorf("empty name for listener")
		}
		if lis.Component == "" {
			return nil, fmt.Errorf("empty component for listener %q", lis.Name)
		}

		if _, found := components[lis.Component]; !found {
			return nil, fmt.Errorf("listener mapped to unknown component: %s", lis.Component)
		}

		components[lis.Component].Listeners = append(components[lis.Component].Listeners,
			&ReplicaSetConfig_Listener{
				Name:         lis.Name,
				ExternalPort: int32(externalPort),
			})
		externalPort++
	}
	return components, nil
}

// generateJaegerDeployment generates the Jaeger k8s deployment and service
// information for a given app deployment.
func generateJaegerDeployment(dep *protos.Deployment) ([]byte, error) {
	name := name{dep.App.Name, jaegerAppName, dep.Id[:8]}.DNSLabel()

	// Generate the Jaeger deployment.
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{deploymentIDKey: dep.Id},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrOf(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":    name,
					"dep_id": dep.Id,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":    name,
						"dep_id": dep.Id,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            name,
							Image:           fmt.Sprintf("%s:latest", jaegerImageName),
							ImagePullPolicy: corev1.PullIfNotPresent,
						},
					},
				},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{deploymentIDKey: dep.Id},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":    name,
				"dep_id": dep.Id,
			},
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
	generated = append(generated, []byte("\n")...)
	fmt.Fprintf(os.Stderr, "Generated Jaeger service\n")
	return generated, nil
}
