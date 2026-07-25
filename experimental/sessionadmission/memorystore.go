package sessionadmission

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MemoryStore is the default single-node SessionStore backend for this fork. It
// tracks live device slots per user under a single mutex, with refcounting so a
// device opening many concurrent connections consumes exactly one slot, freed
// only when its last connection closes.
//
// MemoryStore is intentionally just ONE implementation of SessionStore. The
// admission Gate never references it directly; a future cross-node RemoteStore
// implements the same interface and drops in with no Gate/router change.
type MemoryStore struct {
	mu    sync.Mutex
	users map[string]*userSlots
	// graceAfter is the optional slot grace period; 0 disables it (immediate
	// release, the default).
	graceAfter time.Duration
	// clock is injectable so time-dependent grace behavior is testable without
	// wall-clock sleeps.
	clock func() time.Time
}

type userSlots struct {
	devices map[string]*deviceEntry
}

type deviceEntry struct {
	refcount   int
	firstSeen  time.Time
	lastActive time.Time
	// graceUntil is set when refcount hits 0 while a grace period is configured;
	// the slot is retained (but reclaimable) until this instant.
	graceUntil time.Time
}

// NewMemoryStore creates a MemoryStore with the given slot grace period (0 =
// disabled = immediate release) using the real wall clock.
func NewMemoryStore(graceAfter time.Duration) *MemoryStore {
	return NewMemoryStoreWithClock(graceAfter, time.Now)
}

// NewMemoryStoreWithClock is like NewMemoryStore but with an injectable clock
// for deterministic tests of the grace period.
func NewMemoryStoreWithClock(graceAfter time.Duration, clock func() time.Time) *MemoryStore {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryStore{
		users:      make(map[string]*userSlots),
		graceAfter: graceAfter,
		clock:      clock,
	}
}

// Acquire attempts to claim a slot for (user, deviceKey). The context is
// accepted for interface compatibility but ignored by this local store, and the
// returned error is always nil. The returned release func is sync.Once-wrapped
// so it is idempotent.
func (s *MemoryStore) Acquire(_ context.Context, user, deviceKey string, limit int) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	slots := s.users[user]
	if slots == nil {
		slots = &userSlots{devices: make(map[string]*deviceEntry)}
		s.users[user] = slots
	}
	// Prune expired grace entries so occupancy is computed accurately.
	s.pruneExpiredLocked(slots, now)

	if entry, ok := slots.devices[deviceKey]; ok {
		// Existing device (live or within its grace window): re-attach. Clear any
		// grace hold, bump the refcount and last-activity. No fresh slot consumed.
		entry.graceUntil = time.Time{}
		entry.refcount++
		entry.lastActive = now
		return s.makeRelease(user, deviceKey), true, nil
	}

	// New device. Occupied slots = all remaining (live or grace-held) entries.
	if len(slots.devices) >= limit {
		// At limit: try to reclaim a grace-held slot so live occupancy never
		// exceeds the limit. Grace-held entries have refcount 0.
		if reclaimed := s.reclaimGraceHeldLocked(slots); !reclaimed {
			// All slots are live: definite at-limit rejection, no-op release.
			return noopRelease, false, nil
		}
	}
	slots.devices[deviceKey] = &deviceEntry{
		refcount:   1,
		firstSeen:  now,
		lastActive: now,
	}
	return s.makeRelease(user, deviceKey), true, nil
}

// makeRelease returns an idempotent (sync.Once-wrapped) release closure.
func (s *MemoryStore) makeRelease(user, deviceKey string) func() {
	var once sync.Once
	return func() {
		once.Do(func() { s.decrement(user, deviceKey) })
	}
}

// noopRelease is handed back on a rejected Acquire; it never mutates state.
var noopRelease = func() {}

// decrement is the underlying, once-guarded release action. It decrements the
// device refcount (clamped at 0 as a defensive backstop) and, when the refcount
// reaches 0, either deletes the device immediately (grace disabled) or retains
// it as reclaimable until its grace window expires.
func (s *MemoryStore) decrement(user, deviceKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	slots := s.users[user]
	if slots == nil {
		return
	}
	entry := slots.devices[deviceKey]
	if entry == nil {
		return
	}
	if entry.refcount > 0 {
		entry.refcount--
	}
	if entry.refcount == 0 {
		if s.graceAfter <= 0 {
			delete(slots.devices, deviceKey)
		} else {
			entry.graceUntil = s.clock().Add(s.graceAfter)
		}
	}
	if len(slots.devices) == 0 {
		delete(s.users, user)
	}
}

// pruneExpiredLocked removes grace-held entries whose window has passed. Caller
// must hold s.mu.
func (s *MemoryStore) pruneExpiredLocked(slots *userSlots, now time.Time) {
	for key, entry := range slots.devices {
		if entry.refcount == 0 && !entry.graceUntil.IsZero() && !now.Before(entry.graceUntil) {
			delete(slots.devices, key)
		}
	}
}

// reclaimGraceHeldLocked deletes one grace-held (refcount 0) entry so a new
// device can take its place without exceeding the limit. Returns true if a slot
// was reclaimed. Caller must hold s.mu.
func (s *MemoryStore) reclaimGraceHeldLocked(slots *userSlots) bool {
	for key, entry := range slots.devices {
		if entry.refcount == 0 {
			delete(slots.devices, key)
			return true
		}
	}
	return false
}

// Snapshot returns live device counts per user. Grace-held (non-expired)
// entries are counted, consistent with GetUserDeviceCount and GetUserSessions.
func (s *MemoryStore) Snapshot(_ context.Context) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	out := make(map[string]int, len(s.users))
	for user, slots := range s.users {
		s.pruneExpiredLocked(slots, now)
		if len(slots.devices) == 0 {
			delete(s.users, user)
			continue
		}
		out[user] = len(slots.devices)
	}
	return out
}

// GetUserDeviceCount returns the user's current live device count directly
// (0 when the user is absent), counting grace-held entries exactly as Snapshot
// does so the two never disagree.
func (s *MemoryStore) GetUserDeviceCount(_ context.Context, userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	slots := s.users[userID]
	if slots == nil {
		return 0
	}
	s.pruneExpiredLocked(slots, s.clock())
	if len(slots.devices) == 0 {
		delete(s.users, userID)
		return 0
	}
	return len(slots.devices)
}

// GetUserSessions returns the detailed live sessions for a user: each active
// device with its key, best-effort source IP, connection count, first-seen, and
// last-activity timestamps. The device set is consistent with Snapshot and
// GetUserDeviceCount for the same user.
func (s *MemoryStore) GetUserSessions(_ context.Context, userID string) UserSessions {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := UserSessions{UserID: userID}
	slots := s.users[userID]
	if slots == nil {
		return result
	}
	s.pruneExpiredLocked(slots, s.clock())
	if len(slots.devices) == 0 {
		delete(s.users, userID)
		return result
	}
	result.Devices = make([]DeviceSession, 0, len(slots.devices))
	for key, entry := range slots.devices {
		result.Devices = append(result.Devices, DeviceSession{
			DeviceKey:   key,
			IP:          ipFromDeviceKey(key),
			Connections: entry.refcount,
			FirstSeen:   entry.firstSeen,
			LastActive:  entry.lastActive,
		})
	}
	return result
}

// ipFromDeviceKey extracts the best-effort source IP from a v1 composite device
// key ("<sourceIP>|<inbound>"). For non-composite keys it returns the key as-is.
func ipFromDeviceKey(deviceKey string) string {
	ip, _, _ := strings.Cut(deviceKey, "|")
	return ip
}

// Compile-time assertion that MemoryStore satisfies SessionStore.
var _ SessionStore = (*MemoryStore)(nil)
