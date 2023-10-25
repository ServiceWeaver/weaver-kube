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

Container Image Names:
  "weaver kube deploy" builds a container image locally, and optionally uploads
  it to a container repository. The name of the image and the repository to
  which the image is uploaded are specified using the "image" and "repo" fields
  inside the "kube" section of the config file. For example, consider the
  following config file:

      [serviceweaver]
      binary = "./foo"

      [kube]
      image = "foo:0.0.1"
      repo  = "docker.io/my_docker_hub_username"

  Using this config file, "weaver kube deploy" will build an image named
  "foo:0.0.1" and upload it to "docker.io/my_docker_hub_username/foo:0.0.1". If
  the "image" field is not specified, the image name defaults to
  "<app_name>:<app_version>". If the "repo" field is not specified, the
  container is not uploaded.

  Example repositories are:

      - Docker Hub:                docker.io/USERNAME
      - Google Artifact Registry:  LOCATION-docker.pkg.dev/PROJECT-ID
      - GitHub Container Registry: ghcr.io/NAMESPACE

  You can set more advanced knobs in the "kube" section:
    a) Namespace - where the application should be deployed.
       namespace = "your_namespace"

    b) Service account - specify the service account to use when running your
       application pods.
       service_account = "your_service_account""

    b) Configure listeners
      1. Whether your listener should be public, i.e., should it receive ingress
         traffic from the public internet. If false, the listener is configured
         only for cluster-internal access. To make a listener "foo" public you
         should set:
         listeners.foo = {public = true}
      2. Whether you want your listener to listen on a particular port:
         listeners.foo = {port = 1234}
      3. Whether the listener service should have the same name across multiple
         application versions:
         listeners.foo = {service_name = "unique_name"}

      You can specify any combination of the various options or none. E.g.,
         listeners.foo = {public = true, serice_name = "unique_name"}

    c) Configure resource requirements for the pods [1]. E.g.,
      [kube.resources]
      requests_cpu = "200m"
      requests_mem = "256Mi"
      limits_cpu = "400m"
      limits_mem = "512Mi"

      You can also specify any combination of the various options or none.

      [1] https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/

    d) Configure probes [1]. The kube deployer allows you to configure readiness
       and liveness probes. For each probe, you can configure:
         - how often to perform the probe "period_secs"
         - how long to wait for a probe to respond before declaring a timeout "timeout_secs"
         - minimum consecutive successes for the probe to be successful "success_threshold"
         - minimum consecutive failures for the probe to be considered failed "failure_threshold"
         - a probe handler that describes the probe behavior. You can use a TCP,
           HTTP or a custom commands probe handler.

       E.g.,
       [kube.readiness_probe]
       period_secs = 2
       [kube.readiness_probe.http]
       path = "/health"
       port = 8081

      [1] https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
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
