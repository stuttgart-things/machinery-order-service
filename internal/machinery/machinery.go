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
	"errors"
	"fmt"
	"strings"
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
	Name          string
	Namespace     string
	Ready         bool
	StatusMessage string
	// Address is the connection field of machinery's connection details
	// (a VM's IP), without the status fields machinery appends to it.
	Address string
	// Info are machinery's info fields plus the status fields from the
	// connection details (e.g. started, ansibleSucceeded).
	Info map[string]string
	// Conditions are the object's status.conditions, in their order.
	Conditions []Condition
}

// Condition is one entry of status.conditions.
type Condition struct {
	Type, Status, Reason, Message string
}

// Condition returns the condition of type t, or nil.
func (r *Resource) Condition(t string) *Condition {
	for i := range r.Conditions {
		if r.Conditions[i].Type == t {
			return &r.Conditions[i]
		}
	}
	return nil
}

// ErrKindNotWatched: machinery's watch set has no such kind. machinery
// answered, so it is reachable; it just cannot tell about this kind.
var ErrKindNotWatched = errors.New("kind not in machinery's watch set")

// SplitConnection splits machinery's connection details,
// "<connection> | <label>=<value>, ...", into the connection itself and
// the status fields. machinery leaves out the connection part when the
// connection field is empty, and the status part when there are none.
func SplitConnection(details string) (address string, fields map[string]string) {
	fields = map[string]string{}
	address, extra, found := strings.Cut(details, " | ")
	if !found {
		for _, part := range strings.Split(details, ", ") {
			if k, _, ok := strings.Cut(part, "="); !ok || strings.TrimSpace(k) == "" || strings.ContainsAny(k, "/:?") {
				return strings.TrimSpace(details), fields
			}
		}
		// no connection part, only status fields
		address, extra = "", details
	}
	last := ""
	for _, part := range strings.Split(extra, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if k = strings.TrimSpace(k); ok && k != "" {
			fields[k] = strings.TrimSpace(v)
			last = k
		} else if last != "" {
			// a value that contains ", " itself
			fields[last] += ", " + part
		}
	}
	return strings.TrimSpace(address), fields
}

// Client reads objects.
type Client interface {
	// Get reads one object. A missing object is Found=false, not an error.
	Get(ctx context.Context, kind, name, namespace string) (*Resource, error)
	// List reads every object of a kind. A kind outside machinery's watch
	// set is ErrKindNotWatched. A kind the cluster does not serve (CRD
	// absent) is an empty list: machinery skips it the same way.
	List(ctx context.Context, kind string) ([]Resource, error)
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

func (c *grpcClient) callOpts() []grpc.CallOption {
	if c.token == "" {
		return nil
	}
	return []grpc.CallOption{grpc.PerRPCCredentials(bearer{token: c.token, insecure: c.insecure})}
}

// Get reads one object. A missing object is Found=false, not an error.
func (c *grpcClient) Get(ctx context.Context, kind, name, namespace string) (*Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := c.rs.GetResourceDetail(ctx, &maschinist.ResourceDetailRequest{Kind: kind, Name: name, Namespace: namespace}, c.callOpts()...)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &Resource{}, nil
		}
		return nil, fmt.Errorf("machinery: %s/%s: %w", kind, name, err)
	}
	return fromStatus(st), nil
}

// List reads every object of one kind.
func (c *grpcClient) List(ctx context.Context, kind string) ([]Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.rs.GetResources(ctx, &maschinist.ResourceRequest{Kind: kind, Count: -1}, c.callOpts()...)
	if err != nil {
		// machinery answered, it just cannot list this kind: not in its
		// watch set (InvalidArgument), or a machinery without the RPC.
		if c := status.Code(err); c == codes.InvalidArgument || c == codes.Unimplemented {
			return nil, fmt.Errorf("machinery: %s: %w", kind, ErrKindNotWatched)
		}
		return nil, fmt.Errorf("machinery: list %s: %w", kind, err)
	}
	out := make([]Resource, 0, len(resp.GetResources()))
	for _, st := range resp.GetResources() {
		out = append(out, *fromStatus(st))
	}
	return out, nil
}

func fromStatus(st *maschinist.ResourceStatus) *Resource {
	address, info := SplitConnection(st.GetConnectionDetails())
	// machinery's own info fields win over a status field of the same name
	for k, v := range st.GetInfoFields() {
		info[k] = v
	}
	if len(info) == 0 {
		info = nil
	}
	var conds []Condition
	for _, c := range st.GetConditions() {
		conds = append(conds, Condition{Type: c.GetType(), Status: c.GetStatus(), Reason: c.GetReason(), Message: c.GetMessage()})
	}
	return &Resource{
		Found:         true,
		Name:          st.GetName(),
		Namespace:     st.GetNamespace(),
		Ready:         st.GetReady(),
		StatusMessage: st.GetStatusMessage(),
		Address:       address,
		Info:          info,
		Conditions:    conds,
	}
}
