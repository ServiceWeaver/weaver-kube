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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/ServiceWeaver/weaver-kube/internal/impl"
	swruntime "github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/codegen"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/ServiceWeaver/weaver/runtime/tool"
	"github.com/google/uuid"
)

const (
	configKey      = "github.com/ServiceWeaver/weaver/kube"
	shortConfigKey = "kube"
)

var (
	flags        = flag.NewFlagSet("deploy", flag.ContinueOnError)
	runInDevMode = flags.Bool("runInDevMode", false, "Whether deploy in development mode.")

	deployCmd = tool.Command{
		Name:        "deploy",
		Description: "Deploy a Service Weaver app",
		Help: `Usage:
  weaver kube deploy <configfile>

Flags:
  -h, --help	Print this help message.

Container Image Names:
  "weaver kube deploy" builds a container image locally, and optionally uploads
  it to a container repository. The repository can be specified using the
  "repo" field inside the "kube" section of the config file. For example,
  consider the following config file:

      [serviceweaver]
      binary = "./foo"

      [kube]
      tag  = "foo/foo:0.0.1"
      repo = "docker.io/my_docker_hub_username/my_repo"

  Using this config file, "weaver kube deploy" will build a container
  with a local build tag [1] "foo/foo:0.0.1", and upload it to the
  "docker.io/my_docker_hub_username/my_repo" repository. If the "tag" is not
  specified, it defaults to "<app_name>:<app_version>. If the "repo"
  is not specified, the container is not uploaded and must be pushed
  manually to the Kubernetes environments.
  
  Example repositories are:
      - Docker Hub:                docker.io/USERNAME/REPO_NAME
      - Google Artifact Registry:  LOCATION-docker.pkg.dev/PROJECT-ID/REPO_NAME
      - GitHub Container Registry: ghcr.io/NAMESPACE

  Note that the final image tag for the application container will
  be a concatenation of repo and tag fields, i.e., "repo/tag".
  
  [1]: https://docs.docker.com/engine/reference/commandline/tag/`,
		Flags: flags,
		Fn:    deploy,
	}
)

func deploy(ctx context.Context, args []string) error {
	// Validate command line arguments.
	if len(args) == 0 {
		return fmt.Errorf("no config file provided")
	}
	if len(args) > 1 {
		return fmt.Errorf("too many arguments")
	}

	// Load the config file.
	cfgFile := args[0]
	cfg, err := os.ReadFile(cfgFile)
	if err != nil {
		return fmt.Errorf("load config file %q: %w", cfgFile, err)
	}

	// Parse and validate the app config.
	app, err := swruntime.ParseConfig(cfgFile, string(cfg), codegen.ComponentConfigValidator)
	if err != nil {
		return fmt.Errorf("load config file %q: %w", cfgFile, err)
	}
	if _, err := os.Stat(app.Binary); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("binary %q doesn't exist", app.Binary)
	}

	// Parse and validate the kube section of the config.
	config := &impl.KubeConfig{}
	if err := swruntime.ParseConfigSection(configKey, shortConfigKey, app.Sections, config); err != nil {
		return fmt.Errorf("parse kube config: %w", err)
	}
	if config.Repo == "" {
		fmt.Fprintln(os.Stderr, "No container repo specified in the config file. The container image will only be accessible locally. See `weaver kube deploy --help` for details.")
	}
	if config.Namespace == "" {
		config.Namespace = "default"
	}
	binListeners, err := bin.ReadListeners(app.Binary)
	if err != nil {
		return fmt.Errorf("cannot read listeners from binary %s: %w", app.Binary, err)
	}
	allListeners := make(map[string]struct{})
	for _, c := range binListeners {
		for _, l := range c.Listeners {
			allListeners[l] = struct{}{}
		}
	}
	for lis := range config.Listeners {
		if _, ok := allListeners[lis]; !ok {
			return fmt.Errorf("listener %s specified in the config not found in the binary", lis)
		}
	}

	// Create a deployment.
	dep := &protos.Deployment{
		Id:  uuid.New().String(),
		App: app,
	}

	// Build the docker image for the deployment.
	image, err := impl.BuildAndUploadDockerImage(ctx, dep, config.LocalTag, config.Repo, *runInDevMode)
	if err != nil {
		return err
	}

	// Generate the kube deployment information.
	return impl.GenerateKubeDeployment(image, dep, config)
}
