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
	"path"
	"path/filepath"
	"runtime"
	"text/template"
	"time"

	"github.com/ServiceWeaver/weaver-kube/internal/version"
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
	tag       string   // tag is the container build tag
	files     []string // files that should be copied to the container
	goInstall []string // binary targets that should be 'go install'-ed
}

// BuildAndUploadDockerImage builds a docker image and uploads it to a remote
// repo, if one is specified. It returns the docker image tag that should
// be used in the application containers.
func BuildAndUploadDockerImage(ctx context.Context, dep *protos.Deployment, buildTag, dockerRepo string, runInDevMode bool) (string, error) {
	// Create the build specifications.
	spec, err := dockerBuildSpec(dep, buildTag, runInDevMode)
	if err != nil {
		return "", fmt.Errorf("unable to build image spec: %w", err)
	}

	// Build the docker image.
	if err := buildImage(ctx, spec); err != nil {
		return "", fmt.Errorf("unable to create image: %w", err)
	}

	tag := spec.tag
	if dockerRepo != "" {
		// Push the docker image to the repo.
		if tag, err = pushImage(ctx, tag, dockerRepo); err != nil {
			return "", fmt.Errorf("unable to push image: %w", err)
		}
	}
	return tag, nil
}

// dockerBuildSpec creates a build specification for an app deployment.
func dockerBuildSpec(dep *protos.Deployment, buildTag string, runInDevMode bool) (*buildSpec, error) {
	files := []string{dep.App.Binary}
	toolVersion := fmt.Sprintf("v%d.%d.%d", version.Major, version.Minor, version.Patch)
	goInstall := []string{
		"github.com/ServiceWeaver/weaver-kube/cmd/weaver-kube@" + toolVersion,
	}
	if buildTag == "" {
		buildTag = fmt.Sprintf("%s:%s", dep.App.Name, dep.Id[:8])
	}

	// If we run the kube deployer in the development mode, we should copy the
	// local kube binary to the image instead.
	if runInDevMode && runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		// Use the running weaver-kube tool binary.
		toolBinPath, err := os.Executable()
		if err != nil {
			return nil, err
		}
		files = append(files, toolBinPath)
		goInstall = []string{}
	}

	return &buildSpec{
		tag:       buildTag,
		files:     files,
		goInstall: goInstall,
	}, nil
}

// buildImage builds a docker image with a given spec.
func buildImage(ctx context.Context, spec *buildSpec) error {
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("Building image %s...", spec.tag))
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
	return dockerBuild(ctx, workDir, spec.tag)
}

// dockerBuild builds a docker image given a directory and an image tag.
func dockerBuild(ctx context.Context, dir, tag string) error {
	fmt.Fprintln(os.Stderr, "Building with tag:", tag)
	c := exec.CommandContext(ctx, "docker", "build", dir, "-t", tag)
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	return c.Run()
}

// pushImage pushes a docker image with a given build tag to a docker
// repository, returning its tag in the repository.
func pushImage(ctx context.Context, tag, repo string) (string, error) {
	fmt.Fprintf(os.Stderr, greenText(), fmt.Sprintf("\nUploading image to %s...", repo))
	repoTag := path.Join(repo, tag)
	cTag := exec.CommandContext(ctx, "docker", "tag", tag, repoTag)
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
