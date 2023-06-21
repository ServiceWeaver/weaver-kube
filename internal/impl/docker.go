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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
	"time"

	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/google/uuid"
)

// dockerHubIDEnvKey is the name of the env variable that contains the docker hub id.
//
// Note that w/o a docker hub id, we cannot push the docker image to docker hub.
const dockerHubIDEnvKey = "SERVICEWEAVER_DOCKER_HUB_ID"

// dockerfileTmpl contains the templatized content of the Dockerfile.
var dockerfileTmpl = template.Must(template.New("Dockerfile").Parse(`
{{if . }}
FROM golang:1.20-bullseye as builder
RUN echo ""{{range .}} && go install {{.}}{{end}}
{{end}}
FROM gcr.io/distroless/base-debian11
WORKDIR /weaver/
COPY . .
{{if . }}
COPY --from=builder /go/bin/ /weaver/
{{end}}
ENTRYPOINT ["/weaver/weaver-kube"]
`))

// imageSpecs holds information about a container image build.
//
// Note that GoInstall has to be exported, because it's used in the docker image
// template.
type imageSpecs struct {
	name      string   // Name is the name of the image to build
	files     []string // Files that should be copied to the container
	goInstall []string // Binary targets that should be 'go install'-ed
}

// BuildAndUploadDockerImage builds a docker image and upload it to docker hub.
func BuildAndUploadDockerImage(ctx context.Context, dep *protos.Deployment) (string, error) {
	// Create the docker image specifications.
	specs, err := buildImageSpecs(dep)
	if err != nil {
		return "", fmt.Errorf("unable to build image specs: %w", err)
	}

	// Build the docker image.
	if err := buildImage(ctx, specs); err != nil {
		return "", fmt.Errorf("unable to create image: %w", err)
	}

	// Upload the docker image to docker hub.
	if err := uploadImage(ctx, specs.name); err != nil {
		return "", fmt.Errorf("unable to upload image: %w", err)
	}
	return specs.name, nil
}

// buildImage creates a docker image with specs.
func buildImage(ctx context.Context, specs *imageSpecs) error {
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("Building Image %s ...", specs.name))
	// Create:
	//  workDir/
	//    file1
	//    file2
	//    ...
	//    fileN
	//    Dockerfile   - docker build instructions
	//    tool binary
	ctx, cancel := context.WithTimeout(ctx, time.Second*120)
	defer cancel()

	// Create workDir/.
	workDir := filepath.Join(os.TempDir(), fmt.Sprintf("weaver%s", uuid.New().String()))
	if err := os.Mkdir(workDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	// Copy the files from specs to workDir/.
	for _, file := range specs.files {
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
	if err := dockerfileTmpl.Execute(dockerFile, specs.goInstall); err != nil {
		dockerFile.Close()
		return err
	}
	if err := dockerFile.Close(); err != nil {
		return err
	}
	return dockerBuild(ctx, workDir, specs.name)
}

// Use docker-cli to build the docker image.
func dockerBuild(ctx context.Context, buildContext, tag string) error {
	c := exec.CommandContext(ctx, "docker", "build", buildContext, "-t", tag)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// uploadImage upload image appImage to docker hub.
func uploadImage(ctx context.Context, appImage string) error {
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("\nUploading Image %s to Docker Hub ...", appImage))

	c := exec.CommandContext(ctx, "docker", "push", appImage)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// buildImageSpecs build the docker image specs for an app deployment.
func buildImageSpecs(dep *protos.Deployment) (*imageSpecs, error) {
	// Get the docker hub id.
	dockerID, ok := os.LookupEnv(dockerHubIDEnvKey)
	if !ok {
		return nil, fmt.Errorf("unable to get the docker hub id; env variable %q not set", dockerHubIDEnvKey)
	}
	if dockerID == "" {
		return nil, fmt.Errorf("unable to get the docker hub id; empty value for env variable %q", dockerHubIDEnvKey)
	}

	// Copy the app binary and the tool that starts the babysitter into the image.
	files := []string{dep.App.Binary}
	var goInstall []string
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		// Use the running weaver-kube tool binary.
		toolBinPath, err := os.Executable()
		if err != nil {
			return nil, err
		}
		files = append(files, toolBinPath)
	} else {
		// Cross-compile the weaver-kube tool binary inside the container.
		goInstall = append(goInstall, "github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@latest")
	}
	return &imageSpecs{
		name:      fmt.Sprintf("%s/weaver-%s:tag%s", dockerID, dep.App.Name, dep.Id[:8]),
		files:     files,
		goInstall: goInstall,
	}, nil
}
