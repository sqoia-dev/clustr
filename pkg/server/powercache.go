// Package server — powercache.go provides a short-lived in-memory cache for
// IPMI power status results. BMC queries via ipmitool take 1-5 seconds each;
// caching avoids hammering the BMC on every page load or poll cycle.
package server

import (
	"sync"
	"time"
)

// PowerCache stores the most recent power status for each node, keyed by node ID.
// Entries expire after TTL; expired entries are fetched fresh from the BMC.
type PowerCache struct {
	mu    sync.RWMutex
	cache map[string]*PowerCacheEntry
	ttl   time.Duration
}

// PowerCacheEntry holds one cached power status reading.
type PowerCacheEntry struct {
	Status      string    // "on", "off", or "unknown"
	LastChecked time.Time // wall time of the last successful or failed BMC query
	Error       string    // non-empty when the BMC was unreachable
}

// NewPowerCache returns a PowerCache with the given TTL.
// 15 seconds is appropriate for interactive use — fresh enough for the UI,
// slow enough to not stress the BMC.
func NewPowerCache(ttl time.Duration) *PowerCache {
	return &PowerCache{
		cache: make(map[string]*PowerCacheEntry),
		ttl:   ttl,
	}
}

// Get returns the cached entry for nodeID if it exists and has not expired.
// Returns (nil, false) when the cache is cold or stale.
func (c *PowerCache) Get(nodeID string) (*PowerCacheEntry, bool) {
	c.mu.RLock()
	e, ok := c.cache[nodeID]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(e.LastChecked) > c.ttl {
		return nil, false
	}
	return e, true
}

// Set stores a power status result for nodeID.
// errMsg should be the empty string on success; the BMC error message otherwise.
func (c *PowerCache) Set(nodeID, status, errMsg string) {
	e := &PowerCacheEntry{
		Status:      status,
		LastChecked: time.Now().UTC(),
		Error:       errMsg,
	}
	c.mu.Lock()
	c.cache[nodeID] = e
	c.mu.Unlock()
}

// Invalidate removes the cached entry for nodeID so the next GET fetches a
// fresh reading from the BMC. Call this immediately after every power action
// (on / off / cycle / reset) so the UI reflects the new state.
func (c *PowerCache) Invalidate(nodeID string) {
	c.mu.Lock()
	delete(c.cache, nodeID)
	c.mu.Unlock()
}

// GetFlat returns the cached power status for nodeID as flat primitive values,
// satisfying the handlers.PowerCache interface without an import cycle.
// Returns ok=false when the entry is missing or stale.
func (c *PowerCache) GetFlat(nodeID string) (status string, lastChecked time.Time, errMsg string, ok bool) {
	e, hit := c.Get(nodeID)
	if !hit {
		return "", time.Time{}, "", false
	}
	return e.Status, e.LastChecked, e.Error, true
}
