// Package events is a small fan-out bus for agent events.
package events

import (
	"sync"
	"time"
)

type Event struct {
	Time    time.Time      `json:"time"`
	Source  string         `json:"source"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

const historySize = 500

type Bus struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	history []Event
}

func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

// Publish delivers e to every subscriber; slow subscribers drop events
// rather than block protocol goroutines.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.history = append(b.history, e)
	if len(b.history) > historySize {
		b.history = b.history[len(b.history)-historySize:]
	}
	for c := range b.subs {
		select {
		case c <- e:
		default:
		}
	}
}

// Subscribe returns a channel of events and a cancel func. With replay,
// buffered history is returned first.
func (b *Bus) Subscribe(replay bool) ([]Event, <-chan Event, func()) {
	c := make(chan Event, 256)
	b.mu.Lock()
	var hist []Event
	if replay {
		hist = append(hist, b.history...)
	}
	b.subs[c] = struct{}{}
	b.mu.Unlock()
	return hist, c, func() {
		b.mu.Lock()
		delete(b.subs, c)
		b.mu.Unlock()
	}
}
