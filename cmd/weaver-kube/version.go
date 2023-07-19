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

	"github.com/ServiceWeaver/weaver-kube/internal/version"
	"github.com/ServiceWeaver/weaver/runtime/tool"
)

var versionCmd = tool.Command{
	Name:        "version",
	Flags:       flag.NewFlagSet("version", flag.ContinueOnError),
	Description: "Show weaver kube version",
	Help:        "Usage:\n  weaver kube version",
	Fn: func(context.Context, []string) error {
		fmt.Printf("weaver kube v%d.%d.%d %s/%s\n", version.Major, version.Minor, version.Patch, runtime.GOOS, runtime.GOARCH)
		return nil
	},
}
