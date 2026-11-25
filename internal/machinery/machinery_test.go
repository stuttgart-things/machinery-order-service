package machinery_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"testing"

	maschinist "github.com/stuttgart-things/machinery/resourceservice"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

type fakeServer struct {
	maschinist.UnimplementedResourceServiceServer
	auth string
}

func (f *fakeServer) GetResourceDetail(ctx context.Context, r *maschinist.ResourceDetailRequest) (*maschinist.ResourceStatus, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("authorization"); len(v) > 0 {
		f.auth = v[0]
	}
	if r.Name != "demo-vm" {
		return nil, status.Error(codes.NotFound, "no")
	}
	return &maschinist.ResourceStatus{Name: r.Name, Kind: r.Kind, Ready: true, ConnectionDetails: "10.0.0.5 | started=true, ansibleSucceeded=True", InfoFields: map[string]string{"ansible": "Ready"}}, nil
}

func (f *fakeServer) GetResources(ctx context.Context, r *maschinist.ResourceRequest) (*maschinist.ResourceListResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("authorization"); len(v) > 0 {
		f.auth = v[0]
	}
	switch r.Kind {
	case "Provider":
		return &maschinist.ResourceListResponse{Resources: []*maschinist.ResourceStatus{
			{Name: "provider-kubernetes", Kind: "Provider", Conditions: []*maschinist.Condition{
				{Type: "Installed", Status: "True"},
				{Type: "Healthy", Status: "False", Reason: "UnhealthyPackageRevision", Message: "node already exists"},
			}},
		}}, nil
	case "Function":
		// watched, but the cluster does not serve the kind
		return &maschinist.ResourceListResponse{}, nil
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported kind %q", r.Kind)
	}
}

func TestGRPCClient(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{}
	srv := grpc.NewServer()
	maschinist.RegisterResourceServiceServer(srv, fs)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	d := &machinery.GRPCDialer{}
	defer d.Close()
	c, err := d.Dial(machinery.Endpoint{Address: lis.Addr().String(), Insecure: true, Token: "t0k"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Get(context.Background(), "NativeVsphereVM", "demo-vm", "default")
	if err != nil || !r.Found || !r.Ready || r.Address != "10.0.0.5" || r.Info["ansible"] != "Ready" ||
		r.Info["started"] != "true" || r.Info["ansibleSucceeded"] != "True" {
		t.Fatalf("get: %+v %v", r, err)
	}
	if fs.auth != "Bearer t0k" {
		t.Errorf("token not sent: %q", fs.auth)
	}
	r, err = c.Get(context.Background(), "NativeVsphereVM", "other", "default")
	if err != nil || r.Found {
		t.Errorf("missing: %+v %v", r, err)
	}

	fs.auth = ""
	list, err := c.List(context.Background(), "Provider")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if fs.auth != "Bearer t0k" {
		t.Errorf("token not sent on list: %q", fs.auth)
	}
	h := list[0].Condition("Healthy")
	if list[0].Name != "provider-kubernetes" || h == nil || h.Status != "False" || h.Reason != "UnhealthyPackageRevision" || h.Message != "node already exists" {
		t.Errorf("list item: %+v", list[0])
	}
	if list[0].Condition("Ready") != nil {
		t.Error("Condition(Ready) should be nil")
	}
	if list, err := c.List(context.Background(), "Function"); err != nil || len(list) != 0 {
		t.Errorf("unserved kind: %+v %v", list, err)
	}
	if _, err := c.List(context.Background(), "Configuration"); !errors.Is(err, machinery.ErrKindNotWatched) {
		t.Errorf("unwatched kind: %v", err)
	}
}

func TestSplitConnection(t *testing.T) {
	for _, c := range []struct {
		in, addr string
		fields   map[string]string
	}{
		{"10.31.102.236 | started=true, ansibleSucceeded=True", "10.31.102.236", map[string]string{"started": "true", "ansibleSucceeded": "True"}},
		{"10.0.0.5", "10.0.0.5", map[string]string{}},
		{"", "", map[string]string{}},
		{"started=true, ansibleSucceeded=False", "", map[string]string{"started": "true", "ansibleSucceeded": "False"}},
		{"https://vm.example/?a=b", "https://vm.example/?a=b", map[string]string{}},
		{"10.0.0.5 | phase=Ready, message=a, b", "10.0.0.5", map[string]string{"phase": "Ready", "message": "a, b"}},
	} {
		addr, fields := machinery.SplitConnection(c.in)
		if addr != c.addr || !maps.Equal(fields, c.fields) {
			t.Errorf("%q: got %q %v, want %q %v", c.in, addr, fields, c.addr, c.fields)
		}
	}
}
