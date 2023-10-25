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

package tool

import (
	"context"

	"github.com/ServiceWeaver/weaver-kube/internal/impl"
	"github.com/ServiceWeaver/weaver-kube/internal/tool"
	"github.com/ServiceWeaver/weaver/runtime/metrics"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	swtool "github.com/ServiceWeaver/weaver/runtime/tool"
	"go.opentelemetry.io/otel/sdk/trace"
)

// Plugins configure a Service Weaver application deployed on Kubernetes.
//
// Note that the handlers inside a Plugins struct are invoked on every replica
// of a Service Weaver application binary on locally produced logs, metrics,
// and traces. Handlers may be called concurrently from multiple goroutines.
type Plugins struct {
	// HandleLogEntry handles log entries produced by a component logger.
	HandleLogEntry func(context.Context, *protos.LogEntry) error
	// HandleTraceSpans handles spans produced by inter-component communication.
	HandleTraceSpans func(context.Context, []trace.ReadOnlySpan) error
	// HandleMetrics is called periodically on a snapshot of all metric values.
	HandleMetrics func(context.Context, []*metrics.MetricSnapshot) error
}

// Run runs the "weaver-kube" binary. You can provide Run a set of Plugins to
// customize the behavior of "weaver-kube". The provided name is the name of
// the custom binary.
func Run(name string, plugins Plugins) {
	swtool.Run(name, tool.Commands(impl.BabysitterOptions(plugins)))
}
