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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/envelope"
	"github.com/ServiceWeaver/weaver/runtime/logging"
	"github.com/ServiceWeaver/weaver/runtime/metrics"
	"github.com/ServiceWeaver/weaver/runtime/prometheus"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Endpoint scraped by Prometheus [1] to pull the metrics.
//
// [1] https://prometheus.io
const prometheusEndpoint = "/metrics"

// The directory where "weaver kube" stores data.
var logDir = filepath.Join(runtime.LogsDir(), "kube")

// babysitter starts and manages a weavelet inside the Pod.
type babysitter struct {
	ctx       context.Context
	cfg       *BabysitterConfig
	app       *protos.AppConfig
	envelope  *envelope.Envelope
	logger    *slog.Logger
	clientset *kubernetes.Clientset
	printer   *logging.PrettyPrinter

	mu       sync.Mutex
	watching map[string]struct{} // components being watched
}

func NewBabysitter(ctx context.Context, app *protos.AppConfig, config *BabysitterConfig, components []string) (*babysitter, error) {
	// Create the envelope.
	wlet := &protos.EnvelopeInfo{
		App:             app.Name,
		DeploymentId:    config.DeploymentId,
		Id:              uuid.New().String(),
		Sections:        app.Sections,
		RunMain:         slices.Contains(components, runtime.Main),
		InternalAddress: fmt.Sprintf(":%d", internalPort),
	}
	e, err := envelope.NewEnvelope(ctx, wlet, app)
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: create envelope: %w", err)
	}

	// Create the logger.
	fs, err := logging.NewFileStore(logDir)
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: create logger: %w", err)
	}
	logSaver := fs.Add
	logger := slog.New(&logging.LogHandler{
		Opts: logging.Options{
			App:       app.Name,
			Component: "deployer",
			Weavelet:  uuid.NewString(),
			Attrs:     []string{"serviceweaver/system", ""},
		},
		Write: logSaver,
	})

	// Create a Kubernetes config.
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: get kube config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("NewBabysitter: get kube client set: %w", err)
	}

	// Create the babysitter.
	b := &babysitter{
		ctx:       ctx,
		cfg:       config,
		app:       app,
		envelope:  e,
		logger:    logger,
		clientset: clientset,
		printer:   logging.NewPrettyPrinter(false /*colors disabled*/),
		watching:  map[string]struct{}{},
	}

	// Inform the weavelet of the components it should host.
	if err := b.envelope.UpdateComponents(components); err != nil {
		return nil, fmt.Errorf("NewBabysitter: update components: %w", err)
	}

	return b, nil
}

func (b *babysitter) Serve() error {
	// Run an HTTP server that exports metrics.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", prometheusPort))
	if err != nil {
		return fmt.Errorf("Babysitter.Serve: listen on port %d: %w", prometheusPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(prometheusEndpoint, func(w http.ResponseWriter, r *http.Request) {
		// Read the metrics.
		metrics := b.readMetrics()
		var b bytes.Buffer
		prometheus.TranslateMetricsToPrometheusTextFormat(&b, metrics, r.Host, prometheusEndpoint)
		w.Write(b.Bytes()) //nolint:errcheck // response write error
	})
	var group errgroup.Group
	group.Go(func() error {
		return serveHTTP(b.ctx, lis, mux)
	})

	// Run the envelope and handle messages from the weavelet.
	group.Go(func() error {
		return b.envelope.Serve(b)
	})

	return group.Wait()
}

// ActivateComponent implements the envelope.EnvelopeHandler interface.
func (b *babysitter) ActivateComponent(_ context.Context, request *protos.ActivateComponentRequest) (*protos.ActivateComponentReply, error) {
	go func() {
		if err := b.watchPods(b.ctx, request.Component); err != nil {
			// TODO(mwhittaker): Log this error.
			fmt.Fprintf(os.Stderr, "watchPods(%q): %v", request.Component, err)
		}
	}()
	return &protos.ActivateComponentReply{}, nil
}

// watchPods watches the pods hosting the provided component, updating the
// routing info whenever the set of pods changes.
func (b *babysitter) watchPods(ctx context.Context, component string) error {
	b.mu.Lock()
	if _, ok := b.watching[component]; ok {
		// The pods for this component are already being watched.
		b.mu.Unlock()
		return nil
	}
	b.watching[component] = struct{}{}
	b.mu.Unlock()

	// Watch the pods running the requested component.
	rs := replicaSetName(component, b.app)
	name := deploymentName(b.app.Name, rs, b.cfg.DeploymentId)
	opts := metav1.ListOptions{LabelSelector: fmt.Sprintf("serviceweaver/name=%s", name)}
	watcher, err := b.clientset.CoreV1().Pods(b.cfg.Namespace).Watch(ctx, opts)
	if err != nil {
		return fmt.Errorf("watch pods for component %s: %w", component, err)
	}

	// Repeatedly receive events from Kubernetes, updating the set of pod
	// addresses appropriately. Abort when the channel is closed or the context
	// is canceled.
	addrs := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			watcher.Stop()
			return ctx.Err()

		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil
			}

			changed := false
			switch event.Type {
			case watch.Added, watch.Modified:
				pod := event.Object.(*v1.Pod)
				if pod.Status.PodIP != "" && addrs[pod.Name] != pod.Status.PodIP {
					addrs[pod.Name] = pod.Status.PodIP
					changed = true
				}
			case watch.Deleted:
				pod := event.Object.(*v1.Pod)
				if _, ok := addrs[pod.Name]; ok {
					delete(addrs, pod.Name)
					changed = true
				}
			}
			if !changed {
				continue
			}

			replicas := []string{}
			for _, addr := range addrs {
				replicas = append(replicas, fmt.Sprintf("tcp://%s:%d", addr, internalPort))
			}
			routingInfo := &protos.RoutingInfo{
				Component: component,
				Replicas:  replicas,
			}
			if err := b.envelope.UpdateRoutingInfo(routingInfo); err != nil {
				// TODO(mwhittaker): Log this error.
				fmt.Fprintf(os.Stderr, "UpdateRoutingInfo(%v): %v", routingInfo, err)
			}
		}
	}
}

// GetListenerAddress implements the envelope.EnvelopeHandler interface.
func (b *babysitter) GetListenerAddress(_ context.Context, request *protos.GetListenerAddressRequest) (*protos.GetListenerAddressReply, error) {
	port, ok := b.cfg.Listeners[request.Name]
	if !ok {
		return nil, fmt.Errorf("listener %q not found", request.Name)
	}
	return &protos.GetListenerAddressReply{Address: fmt.Sprintf(":%d", port)}, nil
}

// ExportListener implements the envelope.EnvelopeHandler interface.
func (b *babysitter) ExportListener(context.Context, *protos.ExportListenerRequest) (*protos.ExportListenerReply, error) {
	return &protos.ExportListenerReply{ProxyAddress: ""}, nil
}

// HandleLogEntry implements the envelope.EnvelopeHandler interface.
func (b *babysitter) HandleLogEntry(_ context.Context, entry *protos.LogEntry) error {
	fmt.Println(b.printer.Format(entry))
	return nil
}

// HandleTraceSpans implements the envelope.EnvelopeHandler interface.
func (b *babysitter) HandleTraceSpans(ctx context.Context, spans *protos.TraceSpans) error {
	// TODO(mwhittaker): Implement with plugins.
	return nil
}

// GetSelfCertificate implements the envelope.EnvelopeHandler interface.
func (b *babysitter) GetSelfCertificate(context.Context, *protos.GetSelfCertificateRequest) (*protos.GetSelfCertificateReply, error) {
	panic("unused")
}

// VerifyClientCertificate implements the envelope.EnvelopeHandler interface.
func (b *babysitter) VerifyClientCertificate(context.Context, *protos.VerifyClientCertificateRequest) (*protos.VerifyClientCertificateReply, error) {
	panic("unused")
}

// VerifyServerCertificate implements the envelope.EnvelopeHandler interface.
func (b *babysitter) VerifyServerCertificate(context.Context, *protos.VerifyServerCertificateRequest) (*protos.VerifyServerCertificateReply, error) {
	panic("unused")
}

// readMetrics returns the latest metrics from the weavelet.
func (b *babysitter) readMetrics() []*metrics.MetricSnapshot {
	var ms []*metrics.MetricSnapshot
	ms = append(ms, metrics.Snapshot()...)
	m, err := b.envelope.GetMetrics()
	if err != nil {
		return ms
	}
	return append(ms, m...)
}

// serveHTTP serves HTTP traffic on the provided listener using the provided
// handler. The server is shut down when then provided context is cancelled.
func serveHTTP(ctx context.Context, lis net.Listener, handler http.Handler) error {
	server := http.Server{Handler: handler}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(lis) }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return server.Shutdown(ctx)
	}
}

// replicaSetName returns the name of the replica set that hosts a given
// component.
func replicaSetName(component string, app *protos.AppConfig) string {
	for _, group := range app.Colocate {
		for _, c := range group.Components {
			if c == component {
				return group.Components[0]
			}
		}
	}
	return component
}
