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

// This file contains an example of how to use the plugin API to configure how
// metrics and traces are exported. We register a plugin to export traces to
// Jaeger and a plugin to export metrics to Prometheus. Compile the telemetry
// binary and use it as you would "weaver kube". Use "prometheus.yaml" and
// "jaeger.yaml" to deploy Prometheus and Jaeger to a Kubernetes cluster.
//
//     $ kubectl apply \
//         -f jaeger.yaml \
//         -f prometheus.yaml \
//         -f $(telemetry deploy kube_deploy.yaml)

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/ServiceWeaver/weaver-kube/tool"
	"github.com/ServiceWeaver/weaver/runtime/metrics"
	"github.com/ServiceWeaver/weaver/runtime/prometheus"
	"go.opentelemetry.io/otel/exporters/jaeger" //lint:ignore SA1019 TODO: Update
	"go.opentelemetry.io/otel/sdk/trace"
)

const (
	// The Jaeger and Prometheus ports. These values should be the same as the
	// ones in jaeger.yaml and prometheus.yaml.
	jaegerPort     = 14268
	prometheusPort = 9090
)

// prometheusExporter exports metrics via HTTP in Prometheus text format.
type prometheusExporter struct {
	mu      sync.Mutex
	metrics []*metrics.MetricSnapshot
}

// serve runs an HTTP server that exports metrics in Prometheus text format.
func (p *prometheusExporter) serve(lis net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var b bytes.Buffer
		prometheus.TranslateMetricsToPrometheusTextFormat(&b, p.metrics, r.Host, "/metrics")
		w.Write(b.Bytes()) //nolint:errcheck // response write error
	})
	return http.Serve(lis, mux)
}

// handleMetrics implements tool.Plugins.HandleMetrics.
func (p *prometheusExporter) handleMetrics(_ context.Context, metrics []*metrics.MetricSnapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.metrics = metrics
	return nil
}

func main() {
	// Export traces to Jaegar.
	jaegerURL := fmt.Sprintf("http://jaeger:%d/api/traces", jaegerPort)
	endpoint := jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(jaegerURL))
	traceExporter, err := jaeger.New(endpoint)
	if err != nil {
		panic(err)
	}
	handleTraceSpans := func(ctx context.Context, spans []trace.ReadOnlySpan) error {
		return traceExporter.ExportSpans(ctx, spans)
	}

	// Export metrics to Prometheus.
	p := &prometheusExporter{}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", prometheusPort))
	if err != nil {
		panic(err)
	}
	go func() {
		if err := p.serve(lis); err != nil {
			panic(err)
		}
	}()

	tool.Run("telemetry", tool.Plugins{
		HandleTraceSpans: handleTraceSpans,
		HandleMetrics:    p.handleMetrics,
	})
}
