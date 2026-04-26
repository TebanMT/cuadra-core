// dashboardCache backs DA-33.3: cache UC-033's aggregate per gym for 60s.
// The dashboard is read constantly when a gym is busy; recomputing the half
// dozen aggregates on every poll burns DB cycles for no reason.
package reports

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// dashboardCacheEntry — one cached result.
type dashboardCacheEntry struct {
	expiresAt time.Time
	payload   *DashboardOutput
}

// dashboardCache is a per-gym TTL cache. Concurrency-safe; bounded only by the
// number of gyms (one entry per gym). At MVP scale this is fine; if the gym
// count ever grows, replace with a bounded LRU.
type dashboardCache struct {
	ttl  time.Duration
	mu   sync.Mutex
	data map[uuid.UUID]dashboardCacheEntry
	now  func() time.Time // injectable for tests
}

func newDashboardCache(ttl time.Duration) *dashboardCache {
	return &dashboardCache{
		ttl:  ttl,
		data: make(map[uuid.UUID]dashboardCacheEntry),
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// Get returns (entry, true) when fresh; (nil, false) when missing or expired.
// On expiry the entry is dropped from the map.
func (c *dashboardCache) Get(gymID uuid.UUID) (*DashboardOutput, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[gymID]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.data, gymID)
		return nil, false
	}
	return e.payload, true
}

// Put stores a fresh entry, replacing any prior one.
func (c *dashboardCache) Put(gymID uuid.UUID, out *DashboardOutput) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[gymID] = dashboardCacheEntry{
		expiresAt: c.now().Add(c.ttl),
		payload:   out,
	}
}

// Invalidate drops the entry — useful when a mutation should immediately
// reflect (e.g. operator marks "le hablé" and refreshes the dashboard).
func (c *dashboardCache) Invalidate(gymID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, gymID)
}
