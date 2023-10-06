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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"text/template"
	"time"

	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/google/uuid"
)

// dockerfileTmpl contains the templatized content of the Dockerfile.
//
// TODO(rgrandl): See if we can use a much simpler image. Previously we've been
// using gcr.io/distroless/base-debian11, but it lacks libraries that can lead to
// runtime errors (e.g., glibc).
var dockerfileTmpl = template.Must(template.New("Dockerfile").Parse(`
{{if . }}
FROM golang:bullseye as builder
RUN echo ""{{range .}} && go install {{.}}{{end}}
{{end}}
FROM ubuntu:rolling
WORKDIR /weaver/
COPY . .
{{if . }}
COPY --from=builder /go/bin/ /weaver/
{{end}}
ENTRYPOINT ["/weaver/weaver-kube"]
`))

// buildSpec holds information about a container image build.
type buildSpec struct {
	image     string   // container image name
	files     []string // files that should be copied to the container
	goInstall []string // binary targets that should be 'go install'-ed
}

// BuildAndUploadDockerImage builds a docker image and uploads it to a remote
// repo, if one is specified. It returns the image name that should be used in
// Kubernetes YAML files.
func BuildAndUploadDockerImage(ctx context.Context, app *protos.AppConfig, depId string,
	image, repo string) (string, error) {
	// Create the build specifications.
	spec, err := dockerBuildSpec(app, depId, image)
	if err != nil {
		return "", fmt.Errorf("unable to build image spec: %w", err)
	}

	// Build the docker image.
	if err := buildImage(ctx, spec); err != nil {
		return "", fmt.Errorf("unable to create image: %w", err)
	}

	image = spec.image
	if repo != "" {
		// Push the docker image to the repo.
		if image, err = pushImage(ctx, image, repo); err != nil {
			return "", fmt.Errorf("unable to push image: %w", err)
		}
	}
	return image, nil
}

// dockerBuildSpec creates a build specification for an app deployment.
func dockerBuildSpec(app *protos.AppConfig, depId string, image string) (*buildSpec, error) {
	// Figure out which tool binary will run inside the container.
	toolVersion, toolIsDev, err := ToolVersion()
	if err != nil {
		return nil, err
	}
	toCopy := []string{app.Binary}
	var toInstall []string
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		// The running tool binary can run inside the container: copy it.
		toolBinPath, err := os.Executable()
		if err != nil {
			return nil, err
		}
		toCopy = append(toCopy, toolBinPath)
	} else if toolIsDev {
		// Devel tool binary that's not linux/amd64: prompt the user.
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Print(
			`The running weaver-kube binary hasn't been cross-compiled for linux/amd64 and
cannot run inside the container. Instead, the latest weaver-kube binary will be
downloaded and installed in the container. Do you want to proceed? [Y/n] `)
		scanner.Scan()
		text := scanner.Text()
		if text != "" && text != "y" && text != "Y" {
			return nil, fmt.Errorf("user bailed out")
		}
		toInstall = append(toInstall, "github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@latest")
	} else {
		// Released tool binary that's not compiled to linux/amd64. Re-install
		// it inside the container.
		toInstall = append(toInstall, "github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@"+toolVersion)
	}

	if image == "" {
		image = fmt.Sprintf("%s:%s", app.Name, depId[:8])
	}

	return &buildSpec{
		image:     image,
		files:     toCopy,
		goInstall: toInstall,
	}, nil
}

// buildImage builds a docker image with a given spec.
func buildImage(ctx context.Context, spec *buildSpec) error {
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("Building image %s...", spec.image))
	// Create:
	//  workDir/
	//    file1
	//    file2
	//    ...
	//    fileN
	//    Dockerfile   - docker build instructions
	ctx, cancel := context.WithTimeout(ctx, time.Second*120)
	defer cancel()

	// Create workDir/.
	workDir := filepath.Join(os.TempDir(), fmt.Sprintf("weaver%s", uuid.New().String()))
	if err := os.Mkdir(workDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	// Copy the files from spec.files to workDir/.
	for _, file := range spec.files {
		workDirFile := filepath.Join(workDir, filepath.Base(filepath.Clean(file)))
		if err := cp(file, workDirFile); err != nil {
			return err
		}
	}

	// Create a Dockerfile in workDir/.
	dockerFile, err := os.Create(filepath.Join(workDir, dockerfileTmpl.Name()))
	if err != nil {
		return err
	}
	if err := dockerfileTmpl.Execute(dockerFile, spec.goInstall); err != nil {
		dockerFile.Close()
		return err
	}
	if err := dockerFile.Close(); err != nil {
		return err
	}
	return dockerBuild(ctx, workDir, spec.image)
}

// dockerBuild builds a docker image given a directory and an image name.
func dockerBuild(ctx context.Context, dir, image string) error {
	fmt.Fprintln(os.Stderr, "Building image ", image)
	c := exec.CommandContext(ctx, "docker", "build", dir, "-t", image)
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	return c.Run()
}

// pushImage pushes the provided docker image to the provided repo, returning
// the repo-qualified image name.
func pushImage(ctx context.Context, image, repo string) (string, error) {
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("\nUploading image to %s...", repo))
	repoTag := path.Join(repo, image)
	cTag := exec.CommandContext(ctx, "docker", "tag", image, repoTag)
	cTag.Stdout = os.Stderr
	cTag.Stderr = os.Stderr
	if err := cTag.Run(); err != nil {
		return "", err
	}

	cPush := exec.CommandContext(ctx, "docker", "push", repoTag)
	cPush.Stdout = os.Stderr
	cPush.Stderr = os.Stderr
	if err := cPush.Run(); err != nil {
		return "", err
	}
	return repoTag, nil
}
