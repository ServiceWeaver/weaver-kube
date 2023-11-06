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
)

// The maximum time to wait for `docker build` to finish before aborting.
const dockerBuildTimeout = time.Second * 120

// dockerOptions configure how Docker images are built and pushed.
type dockerOptions struct {
	image string // see kubeConfig.Image
	repo  string // see kubeConfig.Repo
}

// buildAndUploadDockerImage builds a Docker image and uploads it to a remote
// repo, if one is specified. It returns the image name that should be used in
// Kubernetes YAML files.
func buildAndUploadDockerImage(ctx context.Context, app *protos.AppConfig, depId string, opts dockerOptions) (string, error) {
	// Build the Docker image.
	image, err := buildImage(ctx, app, depId, opts)
	if err != nil {
		return "", fmt.Errorf("unable to create image: %w", err)
	}
	if opts.repo == "" {
		return image, nil
	}

	// Push the Docker image to the repo.
	image, err = pushImage(ctx, image, opts.repo)
	if err != nil {
		return "", fmt.Errorf("unable to push image: %w", err)
	}
	return image, nil
}

// buildImage builds a Docker image.
func buildImage(ctx context.Context, app *protos.AppConfig, depId string, opts dockerOptions) (string, error) {
	// Pick an image name.
	image := opts.image
	if image == "" {
		image = fmt.Sprintf("%s:%s", app.Name, depId[:8])
	}
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("Building image %s...", image))

	// Create:
	//  workDir/
	//    file1
	//    file2
	//    ...
	//    fileN
	//    Dockerfile   - Docker build instructions

	// Create workDir/.
	workDir, err := os.MkdirTemp("", "weaver-kube")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workDir)

	// Copy the application binary to workDir/.
	if err := cp(app.Binary, filepath.Join(workDir, filepath.Base(app.Binary))); err != nil {
		return "", err
	}

	// Copy the "weaver-kube" binary into workDir/ if the binary can run in the
	// container. Otherwise, we'll install the "weaver-kube" binary inside the
	// container.
	toolVersion, toolIsDev, err := ToolVersion()
	if err != nil {
		return "", err
	}
	tool, err := os.Executable()
	if err != nil {
		return "", err
	}
	install := "" // "weaver-kube" binary to install, if any
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		// The "weaver-kube" binary can run inside the container, so copy it.
		if err := cp(tool, filepath.Join(workDir, filepath.Base(tool))); err != nil {
			return "", err
		}
	} else if toolIsDev {
		// The "weaver-kube" binary has local modifications, but it cannot be
		// copied into the container. In this case, we install the latest
		// version of "weaver-kube" in the container, if approved by the user.
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Print(
			`The running weaver-kube binary hasn't been cross-compiled for linux/amd64 and
cannot run inside the container. Instead, the latest weaver-kube binary will be
downloaded and installed in the container. Do you want to proceed? [Y/n] `)
		scanner.Scan()
		text := scanner.Text()
		if text != "" && text != "y" && text != "Y" {
			return "", fmt.Errorf("user bailed out")
		}
		install = "github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@latest"
	} else {
		// Install the currently running version of "weaver-kube" in the
		// container.
		install = "github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@" + toolVersion
	}

	// Create a Dockerfile in workDir/.
	type content struct {
		Install    string // "weaver-kube" binary to install, if any
		Entrypoint string // container entrypoint
	}
	var template = template.Must(template.New("Dockerfile").Parse(`
{{if .Install }}
FROM golang:bullseye as builder
RUN go install "{{.Install}}"
{{end}}

FROM ubuntu:rolling
WORKDIR /weaver/
COPY . .
{{if .Install }}
COPY --from=builder /go/bin/ /weaver/
{{end}}
ENTRYPOINT ["{{.Entrypoint}}"]
`))
	dockerFile, err := os.Create(filepath.Join(workDir, "Dockerfile"))
	if err != nil {
		return "", err
	}
	defer dockerFile.Close()
	c := content{Install: install}
	if install != "" {
		c.Entrypoint = "/weaver/weaver-kube"
	} else {
		c.Entrypoint = filepath.Join("/weaver", filepath.Base(tool))
	}
	if err := template.Execute(dockerFile, c); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, dockerBuildTimeout)
	defer cancel()
	return image, dockerBuild(ctx, workDir, image)
}

// dockerBuild builds a Docker image given a directory and an image name.
func dockerBuild(ctx context.Context, dir, image string) error {
	fmt.Fprintln(os.Stderr, "Building image ", image)
	c := exec.CommandContext(ctx, "docker", "build", dir, "-t", image)
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	return c.Run()
}

// pushImage pushes the provided Docker image to the provided repo, returning
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
