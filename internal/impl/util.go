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
	"bufio"
	"fmt"
	"io"
	"os"
)

// greenText returns the ANSI escape code for a green colored text.
func greenText() string {
	return "\033[32m%s\033[0m\n"
}

// cp copies the src file to the dst files.
//
// TODO(rgrandl): remove duplicate.
func cp(src, dst string) error {
	// Open src.
	srcf, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer srcf.Close()
	srcinfo, err := srcf.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}

	// Create or truncate dst.
	dstf, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}
	defer dstf.Close()

	// Copy src to dst.
	const bufSize = 1 << 20
	if _, err := io.Copy(dstf, bufio.NewReaderSize(srcf, bufSize)); err != nil {
		return fmt.Errorf("cp %q %q: %w", src, dst, err)
	}
	if err := os.Chmod(dst, srcinfo.Mode()); err != nil {
		return fmt.Errorf("chmod %q: %w", dst, err)
	}
	return nil
}

// TODO(rgrandl): Remove duplicate.
func ptrOf[T any](val T) *T { return &val }
