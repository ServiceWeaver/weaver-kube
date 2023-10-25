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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/codegen"
	"github.com/ServiceWeaver/weaver/runtime/version"
	"github.com/google/uuid"
)

// Deploy generates a Kubernetes YAML file and corresponding Docker image to
// deploy the Service Weaver application specified by the provided weaver.toml
// config file.
func Deploy(ctx context.Context, configFilename string) error {
	// Read the config file.
	contents, err := os.ReadFile(configFilename)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", configFilename, err)
	}

	// Parse and validate the app config.
	app, err := runtime.ParseConfig(configFilename, string(contents), codegen.ComponentConfigValidator)
	if err != nil {
		return fmt.Errorf("parse config file %q: %w", configFilename, err)
	}
	if _, err := os.Stat(app.Binary); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("binary %q doesn't exist", app.Binary)
	}
	if err := checkVersionCompatibility(app.Binary); err != nil {
		return err
	}

	// Parse and validate the [kube] section of the app config.
	config := &kubeConfig{}
	if err := runtime.ParseConfigSection("kube", "github.com/ServiceWeaver/weaver/kube", app.Sections, config); err != nil {
		return fmt.Errorf("parse [kube] section of config: %w", err)
	}
	if config.Repo == "" {
		fmt.Fprintln(os.Stderr, "No container repo specified in the config file. The container image will only be accessible locally. See `weaver kube deploy --help` for details.")
	}
	if config.Namespace == "" {
		config.Namespace = "default"
	}
	if config.ServiceAccount == "" {
		config.ServiceAccount = "default"
	}

	// Validate the probe options.
	if err := checkProbeOptions(config.LivenessProbeOpts); err != nil {
		return fmt.Errorf("invalid liveness probe spec: %w", err)
	}
	if err := checkProbeOptions(config.ReadinessProbeOpts); err != nil {
		return fmt.Errorf("invalid readiness probe spec: %w", err)
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

	// Unique app deployment identifier.
	depId := uuid.New().String()

	// Build the docker image for the deployment.
	opts := dockerOptions{image: config.Image, repo: config.Repo}
	image, err := buildAndUploadDockerImage(ctx, app, depId, opts)
	if err != nil {
		return err
	}

	// Generate the kube deployment information.
	return generateYAMLs(configFilename, app, config, depId, image)
}

// checkVersionCompatibility checks that the `weaver kube` binary is compatible
// with the application binary being deployed.
func checkVersionCompatibility(appBinary string) error {
	versions, err := bin.ReadVersions(appBinary)
	if err != nil {
		return fmt.Errorf("read versions: %w", err)
	}
	selfVersion, _, err := ToolVersion()
	if err != nil {
		return fmt.Errorf("read weaver-kube version: %w", err)
	}

	// Try to relativize the binary, defaulting to the absolute path if there
	// are any errors..
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

// checkProbeOptions validates the configuration options for the probes.
func checkProbeOptions(opts *probeOptions) error {
	if opts == nil {
		return nil
	}
	// Check that exactly one of the probe handlers is set.
	phSet := 0
	if opts.Http != nil {
		phSet++
	}
	if opts.Tcp != nil {
		phSet++
	}
	if opts.Exec != nil {
		phSet++
	}
	if phSet != 1 {
		return fmt.Errorf("exactly one probe handler should be specified; %d provided", phSet)
	}

	// Validate the handlers.
	if opts.Http != nil && (opts.Http.Port < 1 || opts.Http.Port > 65535) {
		return fmt.Errorf("http handler: invalid port %d", opts.Http.Port)
	}
	if opts.Tcp != nil && (opts.Tcp.Port < 1 || opts.Tcp.Port > 65535) {
		return fmt.Errorf("tcp handler: invalid port %d", opts.Tcp.Port)
	}
	if opts.Exec != nil && len(opts.Exec.Cmd) == 0 {
		return fmt.Errorf("exec handler: no commands specified")
	}
	return nil
}
