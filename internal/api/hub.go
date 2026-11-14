package api

import (
	"sync"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
)

// Event is one change of an order, as the watch streams it.
type Event struct {
	// Type is ADDED, MODIFIED or DELETED.
	Type  string          `json:"type"`
	Order *v1alpha1.Order `json:"order"`
}

// Hub fans order events out to the SSE watchers. A slow watcher loses
// events rather than stalling the others; it reloads on the next one.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Subscribe returns a channel of events and its cancel function.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish sends e to every watcher.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// OnAdd, OnUpdate and OnDelete make the hub an informer event handler.
func (h *Hub) OnAdd(obj any, _ bool) { h.publish("ADDED", obj) }

// OnUpdate publishes a modified order.
func (h *Hub) OnUpdate(_, obj any) { h.publish("MODIFIED", obj) }

// OnDelete publishes a deleted order.
func (h *Hub) OnDelete(obj any) { h.publish("DELETED", obj) }

func (h *Hub) publish(t string, obj any) {
	if o, ok := obj.(*v1alpha1.Order); ok {
		h.Publish(Event{Type: t, Order: o.DeepCopy()})
	}
}
