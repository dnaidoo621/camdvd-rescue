// Package events fans job, drive and queue changes out to every open
// browser over Server-Sent Events.
package events

import (
	"sync"
	"time"
)

// Event is one message. Name is the SSE event name the page listens for,
// e.g. "job-d-7f3a9c", "jobs", "drives", "notice".
type Event struct {
	Name string
	Data string
}

// Hub broadcasts events. Slow subscribers drop events rather than block the
// engine; pages reload state from the server on each event anyway.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
	last map[string]time.Time
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}, last: map[string]time.Time{}}
}

// Subscribe returns a channel of events and a function to stop.
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

// Publish sends an event to every subscriber.
func (h *Hub) Publish(name, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- Event{name, data}:
		default:
		}
	}
}

// Throttled publishes at most once per interval per name; it reports whether
// the event went out, so callers can persist progress at the same pace.
func (h *Hub) Throttled(name, data string, every time.Duration) bool {
	h.mu.Lock()
	now := time.Now()
	if now.Sub(h.last[name]) < every {
		h.mu.Unlock()
		return false
	}
	h.last[name] = now
	h.mu.Unlock()
	h.Publish(name, data)
	return true
}
