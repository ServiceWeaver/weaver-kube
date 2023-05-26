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

	"github.com/ServiceWeaver/weaver-k8s/internal/proto"
	"github.com/ServiceWeaver/weaver/runtime"
	"github.com/ServiceWeaver/weaver/runtime/colors"
	"github.com/ServiceWeaver/weaver/runtime/envelope"
	"github.com/ServiceWeaver/weaver/runtime/logging"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/exp/maps"
)

// babysitter starts and manages a weavelet inside the Pod.
type babysitter struct {
	ctx      context.Context
	cfg      *ReplicaSetConfig
	envelope *envelope.Envelope

	// printer pretty prints log entries.
	printer *logging.PrettyPrinter
}

var _ envelope.EnvelopeHandler = &babysitter{}

func RunBabysitter(ctx context.Context) error {
	// Retrieve the deployment information.
	val, ok := os.LookupEnv(k8sConfigEnvKey)
	if !ok {
		return fmt.Errorf("environment variable %q not set", k8sConfigEnvKey)
	}
	if val == "" {
		return fmt.Errorf("empty value for environment variable %q", k8sConfigEnvKey)
	}
	cfg := &ReplicaSetConfig{}
	if err := proto.FromEnv(val, cfg); err != nil {
		return err
	}

	// Create the envelope.
	wlet := &protos.EnvelopeInfo{
		App:          cfg.Deployment.App.Name,
		DeploymentId: cfg.Deployment.Id,
		Id:           uuid.New().String(),
		Sections:     cfg.Deployment.App.Sections,
		RunMain:      cfg.ReplicaSet == runtime.Main,
		InternalPort: cfg.InternalPort,
	}
	e, err := envelope.NewEnvelope(ctx, wlet, cfg.Deployment.App)
	if err != nil {
		return err
	}

	// Create the babysitter.
	b := &babysitter{
		ctx:      ctx,
		cfg:      cfg,
		envelope: e,
		printer:  logging.NewPrettyPrinter(colors.Enabled()),
	}

	// Inform the weavelet of the components it should host.
	if err := b.envelope.UpdateComponents(maps.Keys(cfg.ComponentsToStart)); err != nil {
		return err
	}

	// Run the envelope and handle messages from the weavelet.
	return e.Serve(b)
}

// ActivateComponent implements the envelope.EnvelopeHandler interface.
func (b *babysitter) ActivateComponent(_ context.Context, request *protos.ActivateComponentRequest) (*protos.ActivateComponentReply, error) {
	// Got a request from the weavelet to activate a component. However, the
	// component should be already activated.
	rs := replicaSet(request.Component, b.cfg.Deployment)

	// Tell the weavelet the address of the service that runs the component.
	name := name{b.cfg.Deployment.App.Name, rs, b.cfg.Deployment.Id[:8]}.DNSLabel()
	addr := fmt.Sprintf("tcp://%s:%d", name, servicePort)
	routingInfo := &protos.RoutingInfo{
		Component: request.Component,
		Replicas:  []string{addr},
	}
	if err := b.envelope.UpdateRoutingInfo(routingInfo); err != nil {
		return nil, err
	}
	return &protos.ActivateComponentReply{}, nil
}

// GetListenerAddress implements the envelope.EnvelopeHandler interface.
func (b *babysitter) GetListenerAddress(_ context.Context, request *protos.GetListenerAddressRequest) (
	*protos.GetListenerAddressReply, error) {
	// The external listeners are prestarted, hence it returns the address on
	// which the requested listener service is available.
	for _, listeners := range maps.Values(b.cfg.ComponentsToStart) {
		for _, lis := range listeners.Listeners {
			if lis.Name == request.Name {
				addr := fmt.Sprintf(":%d", lis.ExternalPort)
				return &protos.GetListenerAddressReply{Address: addr}, nil
			}
		}
	}
	return &protos.GetListenerAddressReply{}, nil
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
func (b *babysitter) HandleTraceSpans(_ context.Context, spans []trace.ReadOnlySpan) error {
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
func (b *babysitter) VerifyServerCertificate(ctx context.Context, request *protos.VerifyServerCertificateRequest) (*protos.VerifyServerCertificateReply, error) {
	panic("unused")
}
