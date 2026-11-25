// Package machinerytest is an in-memory machinery for tests.
package machinerytest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
)

// ErrDown is what every call returns while the fake is Down.
var ErrDown = errors.New("machinery unavailable")

// Fake serves objects by kind/namespace/name. It is its own Dialer and
// Client: every endpoint sees the same objects.
type Fake struct {
	mu      sync.Mutex
	objects map[string]machinery.Resource
	// unwatched kinds answer List with machinery.ErrKindNotWatched.
	unwatched map[string]bool
	down      bool
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
	r.Name, r.Namespace = name, namespace
	f.objects[key(kind, namespace, name)] = r
}

// Delete removes an object: machinery no longer sees it.
func (f *Fake) Delete(kind, namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key(kind, namespace, name))
}

// Unwatch takes kinds out of the watch set (List answers
// machinery.ErrKindNotWatched).
func (f *Fake) Unwatch(kinds ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unwatched == nil {
		f.unwatched = map[string]bool{}
	}
	for _, k := range kinds {
		f.unwatched[k] = true
	}
}

// SetDown makes every call fail (machinery unreachable) or answer again.
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
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
	if f.down {
		return nil, ErrDown
	}
	r, ok := f.objects[key(kind, namespace, name)]
	if !ok {
		return &machinery.Resource{}, nil
	}
	cp := r
	return &cp, nil
}

// List implements machinery.Client.
func (f *Fake) List(_ context.Context, kind string) ([]machinery.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, ErrDown
	}
	if f.unwatched[kind] {
		return nil, fmt.Errorf("%s: %w", kind, machinery.ErrKindNotWatched)
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, kind+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]machinery.Resource, 0, len(keys))
	for _, k := range keys {
		out = append(out, f.objects[k])
	}
	return out, nil
}
