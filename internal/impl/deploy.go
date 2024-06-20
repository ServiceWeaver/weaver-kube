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
	"sigs.k8s.io/yaml"
)

const (
	defaultNamespace                = "default"
	defaultServiceAccount           = "default"
	defaultBaseImage                = "ubuntu:rolling"
	defaultMinExportMetricsInterval = "30s"
)

// Deploy generates a Kubernetes YAML file and corresponding Docker image to
// deploy the Service Weaver application specified by the provided kube.yaml
// config file.
func Deploy(ctx context.Context, configFilename string) error {
	// Read the config file.
	contents, err := os.ReadFile(configFilename)
	if err != nil {
		return fmt.Errorf("read deployment config file %q: %w", configFilename, err)
	}

	// Parse and validate the deployment config.
	config := &kubeConfig{}
	if err := yaml.Unmarshal(contents, config); err != nil {
		return fmt.Errorf("parse deployment config file %q: %w", configFilename, err)
	}

	if config.AppConfig == "" {
		return fmt.Errorf("app config file not specified")
	}
	// Ensure that the app config path is resolved relative to the deployment config path.
	dir := filepath.Dir(configFilename)
	if !filepath.IsAbs(config.AppConfig) {
		appConfigPath, err := filepath.Abs(filepath.Join(dir, config.AppConfig))
		if err != nil {
			return err
		}
		config.AppConfig = appConfigPath
	}

	// Read the app config file.
	contents, err = os.ReadFile(config.AppConfig)
	if err != nil {
		return fmt.Errorf("read app config file %q: %w", config.AppConfig, err)
	}

	// Parse and validate the app config.
	app, err := runtime.ParseConfig(config.AppConfig, string(contents), codegen.ComponentConfigValidator)
	if err != nil {
		return fmt.Errorf("parse app config file %q: %w", config.AppConfig, err)
	}
	if _, err := os.Stat(app.Binary); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("binary %q doesn't exist", app.Binary)
	}
	if err := checkVersionCompatibility(app.Binary); err != nil {
		return err
	}

	if config.Repo == "" {
		fmt.Fprintln(os.Stderr, "No container repo specified in the config file. The container image will only be accessible locally. See `weaver kube deploy --help` for details.")
	}
	if config.Namespace == "" {
		config.Namespace = defaultNamespace
	}
	if config.ServiceAccount == "" {
		config.ServiceAccount = defaultServiceAccount
	}
	if config.BaseImage == "" {
		config.BaseImage = defaultBaseImage
	}
	if config.Telemetry.Metrics.ExportInterval == "" {
		config.Telemetry.Metrics.ExportInterval = defaultMinExportMetricsInterval
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
	for _, lis := range config.Listeners {
		if _, ok := allListeners[lis.Name]; !ok {
			return fmt.Errorf("listener %s specified in the config not found in the binary", lis.Name)
		}
	}

	// Unique app deployment identifier.
	depId := uuid.New().String()

	// Build the docker image for the deployment.
	opts := dockerOptions{image: config.Image, repo: config.Repo, baseImage: config.BaseImage}
	image, err := buildAndUploadDockerImage(ctx, app, depId, opts)
	if err != nil {
		return err
	}

	// Generate the kube deployment information.
	return generateYAMLs(app, config, depId, image)
}

// checkVersionCompatibility checks that the `weaver kube` binary is compatible
// with the application binary being deployed.
func checkVersionCompatibility(appBinary string) error {
	// Read the versions of the application binary.
	appBinaryVersions, err := bin.ReadVersions(appBinary)
	if err != nil {
		return fmt.Errorf("read versions: %w", err)
	}

	// Read the versions of the 'weaver kube' binary (i.e. the currently
	// running binary).
	tool, err := os.Executable()
	if err != nil {
		return err
	}
	weaverKubeVersions, err := bin.ReadVersions(tool)
	if err != nil {
		return fmt.Errorf("read versions: %w", err)
	}
	selfVersion, _, err := ToolVersion()
	if err != nil {
		return fmt.Errorf("read weaver-kube version: %w", err)
	}

	// Try to relativize the binary, defaulting to the absolute path if there
	// are any errors.
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

	if appBinaryVersions.DeployerVersion != version.DeployerVersion {
		return fmt.Errorf(`
ERROR: The binary you're trying to deploy (%q) was built with
github.com/ServiceWeaver/weaver module version %s (internal version %s).
However, the 'weaver-kube' binary you're using (%s) was built with weaver
module version %s (internal version %s). These versions are incompatible.

We recommend updating both the weaver module your application is built with and
updating the 'weaver-kube' command by running the following.

	go get github.com/ServiceWeaver/weaver@latest
	go install github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@latest

Then, re-build your code and re-run 'weaver-kube deploy'. If the problem
persists, please file an issue at https://github.com/ServiceWeaver/weaver/issues`,
			relativize(appBinary), appBinaryVersions.ModuleVersion, appBinaryVersions.DeployerVersion, selfVersion, weaverKubeVersions.ModuleVersion, version.DeployerVersion)
	}
	return nil
}
