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

// Hub broadcasts events. Each subscriber keeps a set of pending events keyed
// by name: a burst of progress updates for one job collapses into a single
// refresh, and no distinct event is ever dropped. Pages reload state from
// the server on each event, so only the latest data per name matters.
type Hub struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
	last map[string]time.Time
}

type sub struct {
	mu      sync.Mutex
	order   []string
	pending map[string]Event
	wake    chan struct{}
	out     chan Event
	done    chan struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[*sub]struct{}{}, last: map[string]time.Time{}}
}

// Subscribe returns a channel of events and a function to stop.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	s := &sub{pending: map[string]Event{}, wake: make(chan struct{}, 1), out: make(chan Event), done: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	go s.pump()
	var once sync.Once
	return s.out, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			close(s.done)
		})
	}
}

func (s *sub) pump() {
	defer close(s.out)
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if len(s.order) == 0 {
				s.mu.Unlock()
				break
			}
			key := s.order[0]
			s.order = s.order[1:]
			ev := s.pending[key]
			delete(s.pending, key)
			s.mu.Unlock()
			select {
			case s.out <- ev:
			case <-s.done:
				return
			}
		}
	}
}

func (s *sub) add(name, data string) {
	s.mu.Lock()
	// Notices are messages, not "something changed" signals: keep each.
	key := name
	if name == "notice" {
		key = name + "\x00" + data
	}
	if _, ok := s.pending[key]; !ok {
		s.order = append(s.order, key)
	}
	s.pending[key] = Event{name, data}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Publish sends an event to every subscriber.
func (h *Hub) Publish(name, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.add(name, data)
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
