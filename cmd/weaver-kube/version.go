// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"

	"github.com/ServiceWeaver/weaver-kube/internal/impl"
	"github.com/ServiceWeaver/weaver/runtime/tool"
)

var versionCmd = tool.Command{
	Name:        "version",
	Flags:       flag.NewFlagSet("version", flag.ContinueOnError),
	Description: "Show weaver kube version",
	Help:        "Usage:\n  weaver kube version",
	Fn: func(context.Context, []string) error {
		version, _, err := impl.ToolVersion()
		if err != nil {
			return err
		}
		fmt.Printf("weaver kube %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	},
}
