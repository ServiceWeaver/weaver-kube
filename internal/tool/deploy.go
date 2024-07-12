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

package tool

import (
	"context"
	"flag"
	"fmt"

	"github.com/ServiceWeaver/weaver-kube/internal/impl"
	"github.com/ServiceWeaver/weaver/runtime/tool"
)

var (
	flags     = flag.NewFlagSet("deploy", flag.ContinueOnError)
	deployCmd = tool.Command{
		Name:        "deploy",
		Description: "Deploy a Service Weaver app",
		Help: `Usage:
  weaver kube deploy <config file>

Flags:
  -h, --help	Print this help message.

  "weaver kube deploy" builds a container image locally for a given app binary,
  and optionally uploads it to a container repository. The app specification is
  provided in a ".toml" file using the "appConfig" field. The name of the image
  and the repository to which the image is uploaded are specified using the
  "image" and "repo" fields. For example, consider the
  following config file:
      "---- config.yaml ----"
      appConfig: "weaver.toml"

      image: "foo:0.0.1"
      repo: "docker.io/my_docker_hub_username"

  Using this config file, "weaver kube deploy" will build an image named
  "foo:0.0.1" and upload it to "docker.io/my_docker_hub_username/foo:0.0.1". If
  the "image" field is not specified, the image name defaults to
  "<app_name>:<app_version>". If the "repo" field is not specified, the
  container is not uploaded.

  Example repositories are:

      - Docker Hub:                docker.io/USERNAME
      - Google Artifact Registry:  LOCATION-docker.pkg.dev/PROJECT-ID
      - GitHub Container Registry: ghcr.io/NAMESPACE

  You can set more advanced knobs:
    a) Namespace [1] - where the application should be deployed.
       namespace: "your_namespace"

    b) Service account [2] - specify the service account to use when running your
       application pods.
       serviceAccount: "your_service_account""

    c) Configure listeners
      1. Whether your listener should be public, i.e., should it receive ingress
         traffic from the public internet. If false, the listener is configured
         only for cluster-internal access. To make a listener "foo" public you
         should set:
         listeners:
           - name: foo
             public: true

      2. Whether you want your listener to listen on a particular port:
         listeners:
           - name: foo
             port: 1234

      3. Whether the listener service should have the same name across multiple
         application versions:
         listeners:
           - name: foo
             serviceName: "unique_name"

      You can specify any combination of the various options or none. E.g.,
         listeners:
           - name: foo
             public: true
             serviceName: "unique_name"

    d) Configure resource requirements for the pods [3]. E.g.,
       resourceSpec:
         requests:
           memory: "64Mi"
           cpu: "250m"
         limits:
           memory: "128Mi"
           cpu: "500m"

    e) Configure scaling specs [4] for the pods. E.g.,
       scalingSpec:
         minReplicas: 2
         maxReplicas: 5
         metrics:
           - type: Resource
             resource:
               name: cpu
               target:
                 type: Utilization
                 averageUtilization: 50

    f) Configure container probes [5]. E.g.,
       probeSpec:
         livenessProbe:
           httpGet:
             path: /healthz
             port: 80
          initialDelaySeconds: 15
          periodSeconds: 10

    g) Specify volumes and how to mount volumes [6] (e.g., pvc, config maps, secrets). E.g.,
       storageSpec:
         volumes:
           - name: my-secret
             volumeSource:
                secret: 
                  secretName: my-secret-name
         volumeMounts:
           - name: my-secret
             mountPath: /etc/secret

    h) Configure groups of collocated components. E.g., if you want two components
       "Foo" and "Bar" to run in the same OS process, you can put them in the
       same group. E.g.,
       groups:
         - name: group1
           components: 
             - Foo
             - Bar

       For each group, you can also configure the resource, scaling and the storage
       specs as shown in e), f) and h).

    i) Whether the application container should be built using Docker or Podman [7]. E.g.,
       buildTool: podman

    [1] https://kubernetes.io/docs/concepts/overview/working-with-objects/namespaces/
    [2] https://kubernetes.io/docs/concepts/security/service-accounts/ 
    [3] https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/
    [4] https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/
    [5] https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
    [6] https://kubernetes.io/docs/concepts/storage/volumes/
    [7] https://podman.io/
`,
		Flags: flags,
		Fn: func(ctx context.Context, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: weaver kube deploy <config file>")
			}
			return impl.Deploy(ctx, args[0])
		},
	}
)
