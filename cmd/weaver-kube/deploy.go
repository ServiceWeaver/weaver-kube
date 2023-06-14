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
	"github.com/ServiceWeaver/weaver/runtime"
	swruntime "github.com/ServiceWeaver/weaver/runtime"
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
	deployCmd = tool.Command{
		Name:        "deploy",
		Description: "Deploy a Service Weaver app",
		Help:        "Usage:\n  weaver kube deploy <configfile>",
		Flags:       flag.NewFlagSet("deploy", flag.ContinueOnError),
		Fn:          deploy,
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

	// Parse the kube section of the config.
	config := &impl.KubeConfig{}
	if err := runtime.ParseConfigSection(configKey, shortConfigKey, app.Sections, config); err != nil {
		return fmt.Errorf("parse kube config: %w", err)
	}

	// Create a deployment.
	dep := &protos.Deployment{
		Id:  uuid.New().String(),
		App: app,
	}

	// Build the docker image for the deployment, and upload it to docker hub.
	image, err := impl.BuildAndUploadDockerImage(ctx, dep)
	if err != nil {
		return err
	}

	// Generate the kube deployment information.
	return impl.GenerateKubeDeployment(image, dep, config)
}
