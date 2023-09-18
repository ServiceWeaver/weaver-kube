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
	"fmt"
	"runtime/debug"
)

// ToolVersion returns the version of the running tool binary, along with
// an indication whether the tool was built manually, i.e., not via go install.
func ToolVersion() (string, bool, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		// Should never happen.
		return "", false, fmt.Errorf("tool binary must be built from a module")
	}
	dev := info.Main.Version == "(devel)"
	return info.Main.Version, dev, nil
}
