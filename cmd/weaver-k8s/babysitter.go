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
	"flag"
	"fmt"

	"github.com/ServiceWeaver/weaver-k8s/internal/impl"
	"github.com/ServiceWeaver/weaver/runtime/tool"
)

var babysitterFlags = flag.NewFlagSet("babysitter", flag.ContinueOnError)

var babysitterCmd = tool.Command{
	Name:        "babysitter",
	Flags:       babysitterFlags,
	Description: "The weaver kubernetes babysitter",
	Help: `Usage:
  weaver k8s babysitter

Flags:
  -h, --help   Print this help message.`,
	Fn: func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("usage: weaver k8s babysitter")
		}
		return impl.RunBabysitter(ctx)
	},
	Hidden: true,
}
