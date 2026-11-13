package machinery_test

import (
	"context"
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
	return &maschinist.ResourceStatus{Name: r.Name, Kind: r.Kind, Ready: true, ConnectionDetails: "10.0.0.5", InfoFields: map[string]string{"ansible": "Ready"}}, nil
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
	if err != nil || !r.Found || !r.Ready || r.Address != "10.0.0.5" || r.Info["ansible"] != "Ready" {
		t.Fatalf("get: %+v %v", r, err)
	}
	if fs.auth != "Bearer t0k" {
		t.Errorf("token not sent: %q", fs.auth)
	}
	r, err = c.Get(context.Background(), "NativeVsphereVM", "other", "default")
	if err != nil || r.Found {
		t.Errorf("missing: %+v %v", r, err)
	}
}
