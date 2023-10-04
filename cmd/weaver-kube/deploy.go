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
	"path/filepath"

	"github.com/ServiceWeaver/weaver-kube/internal/impl"
	swruntime "github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/codegen"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/ServiceWeaver/weaver/runtime/tool"
	"github.com/ServiceWeaver/weaver/runtime/version"
	"github.com/google/uuid"
)

const (
	configKey      = "github.com/ServiceWeaver/weaver/kube"
	shortConfigKey = "kube"
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
      - GitHub Container Registry: ghcr.io/NAMESPACE`,
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
	if err := checkVersionCompatibility(app.Binary); err != nil {
		return err
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
	image, err := impl.BuildAndUploadDockerImage(ctx, dep, config.Image, config.Repo)
	if err != nil {
		return err
	}

	// Generate the kube deployment information.
	return impl.GenerateYAMLs(image, dep, config)
}

// checkVersionCompatibility checks that the tool binary is compatible with
// the application binary being deployed.
func checkVersionCompatibility(appBinary string) error {
	versions, err := bin.ReadVersions(appBinary)
	if err != nil {
		return fmt.Errorf("read versions: %w", err)
	}
	selfVersion, _, err := impl.ToolVersion()
	if err != nil {
		return fmt.Errorf("read weaver-kube version: %w", err)
	}
	relativize := func(bin string) string {
		cwd, err := os.Getwd()
		if err != nil {
			return bin
		}
		rel, err := filepath.Rel(cwd, bin)
		if err != nil {
			return bin
		}
		return rel
	}
	if versions.DeployerVersion != version.DeployerVersion {
		// Try to relativize the binary, defaulting to the absolute path if
		// there are any errors..
		return fmt.Errorf(`
	ERROR: The binary you're trying to deploy (%q) was built with
	github.com/ServiceWeaver/weaver module version %s. However, the 'weaver-kube'
	binary you're using was built with weaver module version %s. These versions are
	incompatible.

	We recommend updating both the weaver module your application is built with and
	updating the 'weaver-kube' command by running the following.

		go get github.com/ServiceWeaver/weaver@latest
		go install github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@latest

	Then, re-build your code and re-run 'weaver-kube deploy'. If the problem
	persists, please file an issue at https://github.com/ServiceWeaver/weaver/issues`,
			relativize(appBinary), versions.ModuleVersion, selfVersion)
	}
	return nil
}
