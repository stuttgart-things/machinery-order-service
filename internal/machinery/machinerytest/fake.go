// Package machinerytest is an in-memory machinery for tests.
package machinerytest

import (
	"context"
	"sync"

	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// Fake serves objects by kind/namespace/name. It is its own Dialer and
// Client: every endpoint sees the same objects.
type Fake struct {
	mu      sync.Mutex
	objects map[string]machinery.Resource
	// Dialed are the endpoints asked for.
	Dialed []machinery.Endpoint
}

func key(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }

// Set puts an object.
func (f *Fake) Set(kind, namespace, name string, r machinery.Resource) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = map[string]machinery.Resource{}
	}
	r.Found = true
	f.objects[key(kind, namespace, name)] = r
}

// Dial implements machinery.Dialer.
func (f *Fake) Dial(ep machinery.Endpoint) (machinery.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Dialed = append(f.Dialed, ep)
	return f, nil
}

// Get implements machinery.Client.
func (f *Fake) Get(_ context.Context, kind, name, namespace string) (*machinery.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.objects[key(kind, namespace, name)]
	if !ok {
		return &machinery.Resource{}, nil
	}
	cp := r
	return &cp, nil
}
