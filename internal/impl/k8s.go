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
	"golang.org/x/exp/maps"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	// The exported port by the Service Weaver services.
	servicePort = 80
)

var (
	// Start value for ports used by the weavelets to listen for internal traffic.
	internalPort = 10000

	// Start value for ports used by the public listeners.
	externalPort = 20000
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
		a, err := buildAutoscaler()
		if err != nil {
			return fmt.Errorf("unable to create k8s autoscaler for replica set %s: %w", rsc.name, err)
		}
		content, err := yaml.Marshal(a)
		if err != nil {
			return err
		}
		k8sGenerated = append(k8sGenerated, []byte(fmt.Sprintf("# Autoscaler for replica set %s\n", rsc.name))...)
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
func buildDeployment(rs *replicaSetInfo, dep *protos.Deployment, image string) (
		*v1.Deployment, error) {
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
			Replicas: ptrOf(int32(1)),
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
func buildListenerService(rs *replicaSetInfo, lis *ReplicaSetConfig_Listener, dep *protos.Deployment) (
		*corev1.Service, error) {
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

// buildService builds a container for a replica set.
func buildContainer(dockerImage string, rs *replicaSetInfo, dep *protos.Deployment) (
		corev1.Container, error) {
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
