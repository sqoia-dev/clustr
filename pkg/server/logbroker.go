package server

import (
	"sync"

	"github.com/google/uuid"
	"github.com/sqoia-dev/clonr/pkg/api"
)

// LogBroker is a simple in-process pub/sub bus for log entries.
// POST /api/v1/logs publishes to it; GET /api/v1/logs/stream subscribes.
type LogBroker struct {
	mu          sync.RWMutex
	subscribers map[string]*logSubscriber
}

type logSubscriber struct {
	filter api.LogFilter
	ch     chan api.LogEntry
}

// NewLogBroker creates an initialised LogBroker.
func NewLogBroker() *LogBroker {
	return &LogBroker{
		subscribers: make(map[string]*logSubscriber),
	}
}

// Subscribe registers a new subscriber with an optional filter.
// Returns a unique ID, a read-only channel of matching log entries, and a
// cancel function that removes the subscription and closes the channel.
func (b *LogBroker) Subscribe(filter api.LogFilter) (id string, ch <-chan api.LogEntry, cancel func()) {
	id = uuid.New().String()
	sub := &logSubscriber{
		filter: filter,
		ch:     make(chan api.LogEntry, 64), // buffered so Publish never blocks
	}

	b.mu.Lock()
	b.subscribers[id] = sub
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
		close(sub.ch)
	}
	return id, sub.ch, cancel
}

// Publish fans out entries to all subscribers whose filter matches.
// It never blocks — entries are dropped for a subscriber whose buffer is full.
func (b *LogBroker) Publish(entries []api.LogEntry) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, entry := range entries {
		for _, sub := range b.subscribers {
			if !matchesFilter(entry, sub.filter) {
				continue
			}
			// Non-blocking send: slow consumers are dropped, not blocked.
			select {
			case sub.ch <- entry:
			default:
			}
		}
	}
}

// matchesFilter returns true if entry satisfies all non-empty filter fields.
func matchesFilter(e api.LogEntry, f api.LogFilter) bool {
	if f.NodeMAC != "" && e.NodeMAC != f.NodeMAC {
		return false
	}
	if f.Hostname != "" && e.Hostname != f.Hostname {
		return false
	}
	if f.Level != "" && e.Level != f.Level {
		return false
	}
	if f.Component != "" && e.Component != f.Component {
		return false
	}
	if f.Since != nil && e.Timestamp.Before(*f.Since) {
		return false
	}
	return true
}
