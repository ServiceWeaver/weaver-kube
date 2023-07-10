#!/usr/bin/env bash
# Copyright 2022 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Exit on command error
set -e

# Ensure image name is lowercase
IMAGE_NAME=$(echo $IMAGE_NAME | tr '[:upper:]' '[:lower:]')

echo "Generate weaver.toml"
cat <<EOT > ./weaver.toml
[serviceweaver]
binary = "./collatz"
[kube]
image = "$IMAGE_NAME"
listeners.collatz = {public = true}
EOT

# Ensure the binary is executable
chmod +x ./collatz

echo "Build the docker image and push"
weaver-kube deploy weaver.toml

echo "Deploy the application"
kubectl apply -f ./kube_*.yaml

echo "Wait for deployment to be ready"
kubectl wait --for=condition=Available=True --timeout=90s Deployment \
  -l serviceweaver/app_name=collatz

# Get the load balancer name
LOAD_BALANCER_NAME=$(kubectl get service \
  -l serviceweaver/app_name=collatz \
  -o=go-template \
  --template='{{- range .items -}}{{- if eq .spec.type "LoadBalancer" -}}{{ .metadata.name }}{{- end -}}{{- end -}}')

echo "Call the API and check the response"
kubectl run -i --rm --restart=Never --image=busybox:latest test-api \
  --command wget -- -q -O - http://$LOAD_BALANCER_NAME/\?x\=10
