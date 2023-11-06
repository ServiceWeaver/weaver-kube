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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ServiceWeaver/weaver-kube/internal/impl"
	"github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/codegen"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/ServiceWeaver/weaver/runtime/tool"
	"google.golang.org/protobuf/encoding/prototext"
)

func babysitterCmd(opts impl.BabysitterOptions) *tool.Command {
	return &tool.Command{
		Name:        "babysitter",
		Flags:       flag.NewFlagSet("babysitter", flag.ContinueOnError),
		Description: "The weaver kubernetes babysitter",
		Help: `Usage:
  weaver kube babysitter <weaver config file> <babysitter config file> <component>...

Flags:
  -h, --help   Print this help message.`,
		Fn: func(ctx context.Context, args []string) error {
			// Parse command line arguments.
			if len(args) < 3 {
				return fmt.Errorf("want >= 3 arguments, got %d", len(args))
			}
			app, err := parseWeaverConfig(args[0])
			if err != nil {
				return err
			}
			config, err := parseBabysitterConfig(args[1])
			if err != nil {
				return err
			}
			components := args[2:]

			// Create the babysitter.
			b, err := impl.NewBabysitter(ctx, app, config, components, opts)
			if err != nil {
				return err
			}

			// Run the babysitter.
			return b.Serve()
		},
		Hidden: true,
	}
}

// parseWeaverConfig parses a weaver.toml config file.
func parseWeaverConfig(filename string) (*protos.AppConfig, error) {
	contents, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", filename, err)
	}
	app, err := runtime.ParseConfig(filename, string(contents), codegen.ComponentConfigValidator)
	if err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", filename, err)
	}
	// Rewrite the app config to point to the binary in the container.
	app.Binary = fmt.Sprintf("/weaver/%s", filepath.Base(app.Binary))
	if _, err := os.Stat(app.Binary); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("binary %q doesn't exist", app.Binary)
	}
	return app, nil
}

// parseBabysitterConfig parses a config.textpb config file containing a
// BabysitterConfig.
func parseBabysitterConfig(filename string) (*impl.BabysitterConfig, error) {
	contents, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", filename, err)
	}
	var config impl.BabysitterConfig
	err = prototext.Unmarshal(contents, &config)
	return &config, err
}
