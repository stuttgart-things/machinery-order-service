// Package machinery reads the live side of an order from machinery on the
// management cluster (gRPC, stuttgart-things/machinery resourceservice). The
// service has no kubeconfig for the management clusters: machinery already
// watches the XRs (and, if configured, the Flux Kustomization).
//
// The XR is looked up by kind and name, which the service knows from the
// rendered order. Looking it up by order-id needs a label selector in
// machinery (stuttgart-things/stuttgart-things#4557); that is only needed to
// recover Orders, which comes later.
package machinery

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	maschinist "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Resource is one object as machinery reports it.
type Resource struct {
	Found         bool
	Ready         bool
	StatusMessage string
	// Address is machinery's connection details (a VM's IP).
	Address string
	Info    map[string]string
}

// Client reads one object.
type Client interface {
	Get(ctx context.Context, kind, name, namespace string) (*Resource, error)
}

// Endpoint is how to reach one machinery.
type Endpoint struct {
	Address  string
	Insecure bool
	Token    string
}

// Dialer hands out a Client per endpoint. *GRPCDialer implements it; tests
// use a fake.
type Dialer interface {
	Dial(ep Endpoint) (Client, error)
}

// GRPCDialer keeps one connection per endpoint.
type GRPCDialer struct {
	mu    sync.Mutex
	conns map[Endpoint]*grpc.ClientConn
}

// Dial returns a client for ep, reusing its connection.
func (d *GRPCDialer) Dial(ep Endpoint) (Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conns == nil {
		d.conns = map[Endpoint]*grpc.ClientConn{}
	}
	if c, ok := d.conns[ep]; ok {
		return &grpcClient{rs: maschinist.NewResourceServiceClient(c), token: ep.Token, insecure: ep.Insecure}, nil
	}
	var creds credentials.TransportCredentials
	if ep.Insecure {
		creds = insecure.NewCredentials()
	} else {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(ep.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("machinery %s: %w", ep.Address, err)
	}
	d.conns[ep] = conn
	return &grpcClient{rs: maschinist.NewResourceServiceClient(conn), token: ep.Token, insecure: ep.Insecure}, nil
}

// Close closes every connection.
func (d *GRPCDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.Close()
	}
	d.conns = nil
}

type grpcClient struct {
	rs       maschinist.ResourceServiceClient
	token    string
	insecure bool
}

type bearer struct {
	token    string
	insecure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return !b.insecure }

// Get reads one object. A missing object is Found=false, not an error.
func (c *grpcClient) Get(ctx context.Context, kind, name, namespace string) (*Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var opts []grpc.CallOption
	if c.token != "" {
		opts = append(opts, grpc.PerRPCCredentials(bearer{token: c.token, insecure: c.insecure}))
	}
	st, err := c.rs.GetResourceDetail(ctx, &maschinist.ResourceDetailRequest{Kind: kind, Name: name, Namespace: namespace}, opts...)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &Resource{}, nil
		}
		return nil, fmt.Errorf("machinery: %s/%s: %w", kind, name, err)
	}
	return &Resource{
		Found:         true,
		Ready:         st.GetReady(),
		StatusMessage: st.GetStatusMessage(),
		Address:       st.GetConnectionDetails(),
		Info:          st.GetInfoFields(),
	}, nil
}
