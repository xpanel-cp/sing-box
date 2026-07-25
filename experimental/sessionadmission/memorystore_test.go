package sessionadmission

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

var bg = context.Background()

func countLive(model map[string]int) int {
	n := 0
	for _, c := range model {
		if c > 0 {
			n++
		}
	}
	return n
}

// heldConn tracks a live connection and its release func in the model.
type heldConn struct {
	user string
	dev  string
	rel  func()
}

// --- Property 1 (task 3.2): limit is never exceeded -------------------------

func TestProperty1_LimitNeverExceeded(t *testing.T) {
	prop := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		N := rng.Intn(4) + 1 // limit 1..4
		store := NewMemoryStore(0)
		const user = "u"
		model := map[string]int{} // dev -> live conn count
		var held []heldConn

		for step := 0; step < 200; step++ {
			if len(held) > 0 && rng.Intn(2) == 0 {
				// release a random held connection
				i := rng.Intn(len(held))
				h := held[i]
				h.rel()
				held = append(held[:i], held[i+1:]...)
				model[h.dev]--
				if model[h.dev] == 0 {
					delete(model, h.dev)
				}
			} else {
				dev := fmt.Sprintf("d%d", rng.Intn(N+2)) // device space slightly larger than N
				prevLive := model[dev] > 0
				occupancy := countLive(model)
				rel, ok, err := store.Acquire(bg, user, dev, N)
				if err != nil {
					return false
				}
				expectOK := prevLive || occupancy < N
				if ok != expectOK {
					t.Logf("acquire dev=%s prevLive=%v occ=%d N=%d got ok=%v want %v", dev, prevLive, occupancy, N, ok, expectOK)
					return false
				}
				if ok {
					model[dev]++
					held = append(held, heldConn{user, dev, rel})
				}
			}
			// invariant: live device count never exceeds N and matches the model.
			cnt := store.GetUserDeviceCount(bg, user)
			if cnt > N {
				return false
			}
			if cnt != countLive(model) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

func TestNPlusOneDeviceRejected(t *testing.T) {
	store := NewMemoryStore(0)
	const user = "u"
	const N = 2
	var rels []func()
	for i := 0; i < N; i++ {
		rel, ok, err := store.Acquire(bg, user, fmt.Sprintf("d%d", i), N)
		if err != nil || !ok {
			t.Fatalf("device %d should be admitted, ok=%v err=%v", i, ok, err)
		}
		rels = append(rels, rel)
	}
	// (N+1)th distinct device must be rejected with a no-op release.
	_, ok, err := store.Acquire(bg, user, "dX", N)
	if err != nil || ok {
		t.Fatalf("(N+1)th device must be rejected, ok=%v err=%v", ok, err)
	}
	// Freeing one slot lets a new device in.
	rels[0]()
	if _, ok, _ := store.Acquire(bg, user, "dX", N); !ok {
		t.Fatalf("after release, new device should be admitted")
	}
}

// --- Property 3 (task 3.3): slot conservation & pruning ---------------------

func TestProperty3_SlotConservationAndPruning(t *testing.T) {
	prop := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		store := NewMemoryStore(0) // grace disabled
		users := []string{"a", "b", "c"}
		var held []heldConn

		for step := 0; step < 150; step++ {
			if len(held) > 0 && rng.Intn(2) == 0 {
				i := rng.Intn(len(held))
				held[i].rel()
				held = append(held[:i], held[i+1:]...)
			} else {
				u := users[rng.Intn(len(users))]
				dev := fmt.Sprintf("d%d", rng.Intn(3))
				rel, ok, _ := store.Acquire(bg, u, dev, 5)
				if ok {
					held = append(held, heldConn{u, dev, rel})
				}
			}
		}
		// Release everything remaining.
		for _, h := range held {
			h.rel()
		}
		// All counts must return to 0 and the internal maps must be pruned.
		for _, u := range users {
			if c := store.GetUserDeviceCount(bg, u); c != 0 {
				return false
			}
		}
		store.mu.Lock()
		empty := len(store.users) == 0
		store.mu.Unlock()
		return empty
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

// --- Property 4 (task 3.4): same-device sharing -----------------------------

func TestProperty4_SameDeviceSharing(t *testing.T) {
	store := NewMemoryStore(0)
	const user = "u"
	const dev = "dev"
	const conns = 5

	var rels []func()
	for i := 0; i < conns; i++ {
		rel, ok, err := store.Acquire(bg, user, dev, 1) // limit 1, but same device
		if err != nil || !ok {
			t.Fatalf("conn %d of same device must be admitted, ok=%v err=%v", i, ok, err)
		}
		rels = append(rels, rel)
		if got := store.GetUserDeviceCount(bg, user); got != 1 {
			t.Fatalf("same device must consume exactly one slot, got %d", got)
		}
	}
	// A different device is rejected (the one slot is occupied).
	if _, ok, _ := store.Acquire(bg, user, "other", 1); ok {
		t.Fatalf("second distinct device must be rejected at limit 1")
	}
	// Release all but the last: slot stays occupied.
	for i := 0; i < conns-1; i++ {
		rels[i]()
		if got := store.GetUserDeviceCount(bg, user); got != 1 {
			t.Fatalf("slot must remain until last conn closes, got %d after %d releases", got, i+1)
		}
	}
	// Release the last: slot frees.
	rels[conns-1]()
	if got := store.GetUserDeviceCount(bg, user); got != 0 {
		t.Fatalf("slot must free after last conn closes, got %d", got)
	}
}

// --- task 3.5: release idempotency (sync.Once) & race stress ----------------

func TestReleaseIdempotent(t *testing.T) {
	store := NewMemoryStore(0)
	const user = "u"
	const dev = "d"
	rel, ok, _ := store.Acquire(bg, user, dev, 3)
	if !ok {
		t.Fatal("expected admit")
	}
	if got := store.GetUserDeviceCount(bg, user); got != 1 {
		t.Fatalf("expected 1 device, got %d", got)
	}
	// Repeated release must decrement exactly once.
	rel()
	rel()
	rel()
	if got := store.GetUserDeviceCount(bg, user); got != 0 {
		t.Fatalf("expected 0 after repeated release, got %d", got)
	}
	// Never below zero: acquire another device, ensure counts stay sane.
	rel2, ok, _ := store.Acquire(bg, user, "d2", 3)
	if !ok {
		t.Fatal("expected admit for d2")
	}
	rel2()
	rel2()
	if got := store.GetUserDeviceCount(bg, user); got != 0 {
		t.Fatalf("count must not go negative / stay at 0, got %d", got)
	}
}

func TestReleaseIdempotentConcurrent(t *testing.T) {
	store := NewMemoryStore(0)
	const user = "u"
	rel, ok, _ := store.Acquire(bg, user, "d", 1)
	if !ok {
		t.Fatal("expected admit")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel()
		}()
	}
	wg.Wait()
	if got := store.GetUserDeviceCount(bg, user); got != 0 {
		t.Fatalf("concurrent release must decrement exactly once, got %d", got)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	store := NewMemoryStore(0)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			user := fmt.Sprintf("u%d", g%4)
			for i := 0; i < 500; i++ {
				dev := fmt.Sprintf("d%d", i%6)
				rel, ok, _ := store.Acquire(bg, user, dev, 3)
				if ok {
					_ = store.GetUserDeviceCount(bg, user)
					_ = store.Snapshot(bg)
					rel()
				}
			}
		}(g)
	}
	wg.Wait()
	// After all goroutines finish, everything should be released and pruned.
	if s := store.Snapshot(bg); len(s) != 0 {
		t.Fatalf("expected empty snapshot after all releases, got %v", s)
	}
}

// --- Property 9 (task 3.7): three-way count consistency ---------------------

func TestProperty9_ThreeWayConsistency(t *testing.T) {
	prop := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		now := time.Unix(1_700_000_000, 0)
		clock := func() time.Time { return now }
		// Randomly enable grace so consistency holds while grace slots are held.
		grace := time.Duration(0)
		if rng.Intn(2) == 0 {
			grace = time.Minute
		}
		store := NewMemoryStoreWithClock(grace, clock)
		users := []string{"a", "b"}
		var held []heldConn

		check := func() bool {
			snap := store.Snapshot(bg)
			for _, u := range users {
				c := store.GetUserDeviceCount(bg, u)
				sessions := store.GetUserSessions(bg, u)
				if c != snap[u] {
					t.Logf("count %d != snapshot %d for %s", c, snap[u], u)
					return false
				}
				if c != len(sessions.Devices) {
					t.Logf("count %d != sessions %d for %s", c, len(sessions.Devices), u)
					return false
				}
			}
			return true
		}

		for step := 0; step < 150; step++ {
			if len(held) > 0 && rng.Intn(2) == 0 {
				i := rng.Intn(len(held))
				held[i].rel()
				held = append(held[:i], held[i+1:]...)
			} else {
				u := users[rng.Intn(len(users))]
				dev := fmt.Sprintf("d%d", rng.Intn(4))
				rel, ok, _ := store.Acquire(bg, u, dev, 3)
				if ok {
					held = append(held, heldConn{u, dev, rel})
				}
			}
			if !check() {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}

func TestObservabilityFields(t *testing.T) {
	now := time.Unix(1_000, 0)
	clock := func() time.Time { return now }
	store := NewMemoryStoreWithClock(0, clock)
	const user = "u"
	dev := "203.0.113.9|vless"

	rel1, _, _ := store.Acquire(bg, user, dev, 3)
	now = now.Add(30 * time.Second)
	rel2, _, _ := store.Acquire(bg, user, dev, 3)

	sessions := store.GetUserSessions(bg, user)
	if len(sessions.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(sessions.Devices))
	}
	d := sessions.Devices[0]
	if d.DeviceKey != dev {
		t.Fatalf("unexpected device key %q", d.DeviceKey)
	}
	if d.IP != "203.0.113.9" {
		t.Fatalf("expected IP derived from key prefix, got %q", d.IP)
	}
	if d.Connections != 2 {
		t.Fatalf("expected 2 connections, got %d", d.Connections)
	}
	if !d.FirstSeen.Equal(time.Unix(1_000, 0)) {
		t.Fatalf("unexpected FirstSeen %v", d.FirstSeen)
	}
	if !d.LastActive.After(d.FirstSeen) {
		t.Fatalf("LastActive %v should be after FirstSeen %v", d.LastActive, d.FirstSeen)
	}
	rel1()
	rel2()
}

// --- task 3.9: grace period -------------------------------------------------

func TestGrace_ReattachWithinWindowNoFreshSlot(t *testing.T) {
	now := time.Unix(5_000, 0)
	clock := func() time.Time { return now }
	store := NewMemoryStoreWithClock(time.Minute, clock)
	const user = "u"
	const N = 1

	rel, ok, _ := store.Acquire(bg, user, "dev", N)
	if !ok {
		t.Fatal("expected admit")
	}
	rel() // last connection closes; slot grace-held
	if got := store.GetUserDeviceCount(bg, user); got != 1 {
		t.Fatalf("grace-held slot should still count, got %d", got)
	}
	// Same device reconnects within the window: re-attaches, no fresh slot.
	now = now.Add(10 * time.Second)
	rel2, ok, _ := store.Acquire(bg, user, "dev", N)
	if !ok {
		t.Fatal("same device within grace window must be admitted")
	}
	if got := store.GetUserDeviceCount(bg, user); got != 1 {
		t.Fatalf("re-attach must not consume a fresh slot, got %d", got)
	}
	rel2()
}

func TestGrace_ZeroImmediateRelease(t *testing.T) {
	store := NewMemoryStore(0)
	const user = "u"
	rel, _, _ := store.Acquire(bg, user, "dev", 1)
	rel()
	if got := store.GetUserDeviceCount(bg, user); got != 0 {
		t.Fatalf("grace=0 must release immediately, got %d", got)
	}
	store.mu.Lock()
	_, exists := store.users[user]
	store.mu.Unlock()
	if exists {
		t.Fatalf("user entry must be pruned immediately with grace=0")
	}
}

func TestGrace_NewDeviceReclaimsGraceHeldSlot(t *testing.T) {
	now := time.Unix(9_000, 0)
	clock := func() time.Time { return now }
	store := NewMemoryStoreWithClock(time.Minute, clock)
	const user = "u"
	const N = 1

	rel, _, _ := store.Acquire(bg, user, "old", N)
	rel() // "old" is now grace-held (occupies the one slot)
	if got := store.GetUserDeviceCount(bg, user); got != 1 {
		t.Fatalf("grace-held slot should count, got %d", got)
	}
	// A genuinely new device at limit reclaims the grace-held slot.
	now = now.Add(5 * time.Second)
	_, ok, _ := store.Acquire(bg, user, "new", N)
	if !ok {
		t.Fatal("new device must reclaim a grace-held slot")
	}
	// Live occupancy never exceeds the limit.
	if got := store.GetUserDeviceCount(bg, user); got > N {
		t.Fatalf("occupancy must not exceed limit, got %d", got)
	}
	sessions := store.GetUserSessions(bg, user)
	if len(sessions.Devices) != 1 || sessions.Devices[0].DeviceKey != "new" {
		t.Fatalf("expected only 'new' device, got %+v", sessions.Devices)
	}
}

func TestGrace_PruneOnExpiry(t *testing.T) {
	now := time.Unix(2_000, 0)
	clock := func() time.Time { return now }
	store := NewMemoryStoreWithClock(time.Minute, clock)
	const user = "u"

	rel, _, _ := store.Acquire(bg, user, "dev", 3)
	rel() // grace-held
	if got := store.GetUserDeviceCount(bg, user); got != 1 {
		t.Fatalf("grace-held slot should count before expiry, got %d", got)
	}
	// Advance past the grace window: entry must be pruned on next access.
	now = now.Add(2 * time.Minute)
	if got := store.GetUserDeviceCount(bg, user); got != 0 {
		t.Fatalf("expired grace slot must be pruned, got %d", got)
	}
	store.mu.Lock()
	_, exists := store.users[user]
	store.mu.Unlock()
	if exists {
		t.Fatalf("user entry must be pruned after grace expiry")
	}
}

// TestProperty1_GraceLimitNeverExceeded (task 3.9 extended): even while grace
// slots are held, a new device at limit reclaims a grace-held slot so live
// occupancy stays <= N.
func TestProperty1_GraceLimitNeverExceeded(t *testing.T) {
	prop := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		base := time.Unix(1_000_000, 0)
		var offset time.Duration
		clock := func() time.Time { return base.Add(offset) }
		N := rng.Intn(3) + 1
		store := NewMemoryStoreWithClock(30*time.Second, clock)
		const user = "u"
		var held []heldConn

		for step := 0; step < 200; step++ {
			switch rng.Intn(3) {
			case 0: // advance clock (may expire grace slots)
				offset += time.Duration(rng.Intn(20)) * time.Second
			case 1: // release
				if len(held) > 0 {
					i := rng.Intn(len(held))
					held[i].rel()
					held = append(held[:i], held[i+1:]...)
				}
			default: // acquire
				dev := fmt.Sprintf("d%d", rng.Intn(N+3))
				rel, ok, _ := store.Acquire(bg, user, dev, N)
				if ok {
					held = append(held, heldConn{user, dev, rel})
				}
			}
			// Occupancy (live + grace-held, counted by the store) must never exceed N.
			if store.GetUserDeviceCount(bg, user) > N {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
