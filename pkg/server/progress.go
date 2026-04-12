package server

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sqoia-dev/clonr/pkg/api"
)

// ProgressStore holds the latest DeployProgress for each active node and fans
// updates out to SSE subscribers. It is safe for concurrent use.
type ProgressStore struct {
	mu          sync.RWMutex
	states      map[string]*api.DeployProgress // keyed by node MAC
	subsMu      sync.RWMutex
	subscribers map[string]chan api.DeployProgress
}

// NewProgressStore creates a ProgressStore and starts the background cleanup goroutine.
func NewProgressStore() *ProgressStore {
	ps := &ProgressStore{
		states:      make(map[string]*api.DeployProgress),
		subscribers: make(map[string]chan api.DeployProgress),
	}
	go ps.cleanupLoop()
	return ps
}

// Update stores the latest progress for the node and publishes it to all SSE subscribers.
func (ps *ProgressStore) Update(entry api.DeployProgress) {
	ps.mu.Lock()
	copy := entry // avoid aliasing
	ps.states[entry.NodeMAC] = &copy
	ps.mu.Unlock()

	ps.publish(entry)
}

// Get returns the latest progress for a node MAC. Returns false if not found.
func (ps *ProgressStore) Get(nodeMAC string) (*api.DeployProgress, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	e, ok := ps.states[nodeMAC]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// List returns a snapshot of all tracked deployments.
func (ps *ProgressStore) List() []api.DeployProgress {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]api.DeployProgress, 0, len(ps.states))
	for _, e := range ps.states {
		out = append(out, *e)
	}
	return out
}

// Subscribe registers a new SSE subscriber. Returns a read-only channel of
// progress updates and a cancel function to remove the subscription.
func (ps *ProgressStore) Subscribe() (ch <-chan api.DeployProgress, cancel func()) {
	id := newSubID()
	internal := make(chan api.DeployProgress, 64)

	ps.subsMu.Lock()
	ps.subscribers[id] = internal
	ps.subsMu.Unlock()

	cancel = func() {
		ps.subsMu.Lock()
		delete(ps.subscribers, id)
		ps.subsMu.Unlock()
		close(internal)
	}
	return internal, cancel
}

// Cleanup removes entries that have not been updated within olderThan.
func (ps *ProgressStore) Cleanup(olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for mac, e := range ps.states {
		if e.UpdatedAt.Before(cutoff) {
			delete(ps.states, mac)
		}
	}
}

// publish fans out the entry to all subscribers. Non-blocking: slow consumers
// are dropped rather than blocking the caller.
func (ps *ProgressStore) publish(entry api.DeployProgress) {
	ps.subsMu.RLock()
	defer ps.subsMu.RUnlock()
	for _, ch := range ps.subscribers {
		select {
		case ch <- entry:
		default:
		}
	}
}

// cleanupLoop removes stale entries every 5 minutes, retaining data for 30 minutes.
func (ps *ProgressStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ps.Cleanup(30 * time.Minute)
	}
}

// newSubID generates a unique subscriber ID.
func newSubID() string {
	return uuid.New().String()
}
