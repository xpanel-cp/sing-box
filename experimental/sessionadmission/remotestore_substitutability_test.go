package sessionadmission

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/adapter"
)

// Property 8 — Backend substitutability (task 11.2).
//
// These tests prove that the admission Gate depends ONLY on the SessionStore
// interface: a completely different, context-aware, error-injecting backend
// (remoteStore, standing in for a future centralized X-NET Agent) drops in with
// NO change to Gate construction and NO change to the router call sites. The
// router seam is mirrored locally (routerAdmissionSeam) so we assert it without
// importing the route package.

// routerAdmissionSeam mirrors the route package's unexported admissionGate
// interface (the single method the router calls). Asserting *Gate satisfies it
// here proves substituting the backend never changes what the router sees.
type routerAdmissionSeam interface {
	Admit(ctx context.Context, md adapter.InboundContext) (func(), AdmissionResult)
}

// The Gate satisfies the router seam regardless of which SessionStore backs it.
var _ routerAdmissionSeam = (*Gate)(nil)

// remoteStore is a stub SessionStore standing in for a remote/centralized
// backend. It delegates to an in-memory store for the happy path and supports
// an injectable error to simulate a remote/timeout failure. It is fully
// context-aware, exactly like a real remote implementation would be.
type remoteStore struct {
	delegate *MemoryStore

	mu           sync.Mutex
	failErr      error
	acquireCalls int
}

func newRemoteStore() *remoteStore {
	return &remoteStore{delegate: NewMemoryStore(0)}
}

func (r *remoteStore) injectError(err error) {
	r.mu.Lock()
	r.failErr = err
	r.mu.Unlock()
}

func (r *remoteStore) Acquire(ctx context.Context, user, deviceKey string, limit int) (func(), bool, error) {
	r.mu.Lock()
	err := r.failErr
	r.acquireCalls++
	r.mu.Unlock()
	if err != nil {
		// Simulate a remote failure/timeout: no slot claimed, count untouched.
		return noopRelease, false, err
	}
	return r.delegate.Acquire(ctx, user, deviceKey, limit)
}

func (r *remoteStore) Snapshot(ctx context.Context) map[string]int {
	return r.delegate.Snapshot(ctx)
}

func (r *remoteStore) GetUserDeviceCount(ctx context.Context, userID string) int {
	return r.delegate.GetUserDeviceCount(ctx, userID)
}

func (r *remoteStore) GetUserSessions(ctx context.Context, userID string) UserSessions {
	return r.delegate.GetUserSessions(ctx, userID)
}

// Compile-time proof the stub satisfies the same interface as MemoryStore.
var _ SessionStore = (*remoteStore)(nil)

// admitStep is one scripted admission in the substitutability comparison.
type admitStep struct {
	user string
	ip   string
}

// runScript drives a Gate through a fixed sequence and records the reason codes
// and admit flags, so two backends can be compared for identical behavior.
func runScript(gate *Gate, steps []admitStep) []AdmissionResult {
	results := make([]AdmissionResult, 0, len(steps))
	for _, step := range steps {
		md := mdWithSource(step.ip, 1000, "vless", "tcp")
		md.User = step.user
		_, res := gate.Admit(context.Background(), md)
		results = append(results, res)
	}
	return results
}

// limitsForSubstitutability: user "u2" is limited to 2 devices; every other
// user (e.g. "unl") falls back to the default limit of 0 = unlimited.
func limitsForSubstitutability() LimitSource {
	return NewLimitSource(Options{
		DefaultLimit: 0,
		Users:        []UserLimit{{UUID: "u2", MaxDevices: 2}},
	})
}

// TestProperty8_HappyPathIdenticalToMemoryStore asserts the remote-backed Gate
// admits/rejects identically to the MemoryStore-backed Gate on the happy path:
// within-limit -> ADMITTED, at-limit new device -> DEVICE_LIMIT_REACHED,
// unlimited user -> UNLIMITED, and a repeat device re-attaches (no new slot).
func TestProperty8_HappyPathIdenticalToMemoryStore(t *testing.T) {
	script := []admitStep{
		{user: "u2", ip: "192.0.2.1"},  // ADMITTED (1/2)
		{user: "u2", ip: "192.0.2.2"},  // ADMITTED (2/2)
		{user: "u2", ip: "192.0.2.3"},  // DEVICE_LIMIT_REACHED (new device at limit)
		{user: "unl", ip: "192.0.2.9"}, // UNLIMITED (limit 0)
		{user: "u2", ip: "192.0.2.1"},  // ADMITTED (re-attach existing device)
	}
	want := []AdmissionReason{
		ReasonAdmitted, ReasonAdmitted, ReasonDeviceLimit, ReasonUnlimited, ReasonAdmitted,
	}

	// Backend A: the default MemoryStore.
	memGate := NewGate(SourceIPFingerprinter{}, NewMemoryStore(0), limitsForSubstitutability(), FailOpen)
	// Backend B: the substituted remote store — identical Gate construction.
	remote := newRemoteStore()
	remoteGate := NewGate(SourceIPFingerprinter{}, remote, limitsForSubstitutability(), FailOpen)

	memResults := runScript(memGate, script)
	remoteResults := runScript(remoteGate, script)

	for i := range want {
		if memResults[i].Reason != want[i] {
			t.Fatalf("step %d: MemoryStore reason = %s, want %s", i, memResults[i].Reason, want[i])
		}
		if remoteResults[i] != memResults[i] {
			t.Fatalf("step %d: remote result %+v != MemoryStore result %+v (backend not substitutable)",
				i, remoteResults[i], memResults[i])
		}
	}

	// Both backends must report the same live device count for the limited user.
	if got := remote.GetUserDeviceCount(bg, "u2"); got != 2 {
		t.Fatalf("remote store device count = %d, want 2", got)
	}
	// The unlimited user was a no-op: it must never have hit the store.
	// (Two admitted + one rejected + one re-attach for u2 = 4 Acquire calls.)
	if remote.acquireCalls != 4 {
		t.Fatalf("remote Acquire calls = %d, want 4 (unlimited user must not call Acquire)", remote.acquireCalls)
	}
}

// TestProperty8_StoreErrorRoutesThroughFailureMode asserts that when the
// substituted backend returns an error, the Gate applies the configured failure
// mode (FailOpen -> admit, FailClose -> reject), tags the result STORE_TIMEOUT,
// and leaves the tracked device count unchanged (Requirements 8.2, 8.3).
func TestProperty8_StoreErrorRoutesThroughFailureMode(t *testing.T) {
	cases := []struct {
		mode      AdmissionFailureMode
		wantAdmit bool
	}{
		{FailOpen, true},
		{FailClose, false},
	}
	for _, tc := range cases {
		remote := newRemoteStore()
		gate := NewGate(SourceIPFingerprinter{}, remote, limitsForSubstitutability(), tc.mode)

		// Establish one healthy slot for u2 before injecting the failure.
		md1 := mdWithSource("192.0.2.1", 1000, "vless", "tcp")
		md1.User = "u2"
		if _, res := gate.Admit(context.Background(), md1); !res.Admit || res.Reason != ReasonAdmitted {
			t.Fatalf("mode %s: setup admit failed: %+v", tc.mode, res)
		}
		countBefore := remote.GetUserDeviceCount(bg, "u2")

		// Now the remote backend fails for a NEW device.
		remote.injectError(errors.New("remote agent unreachable"))
		md2 := mdWithSource("192.0.2.2", 1000, "vless", "tcp")
		md2.User = "u2"
		_, res := gate.Admit(context.Background(), md2)

		if res.Reason != ReasonStoreTimeout {
			t.Fatalf("mode %s: expected STORE_TIMEOUT, got %+v", tc.mode, res)
		}
		if res.Admit != tc.wantAdmit {
			t.Fatalf("mode %s: expected admit=%v, got %v", tc.mode, tc.wantAdmit, res.Admit)
		}
		// Tracked count must be unchanged by the failure-mode fallback.
		if countAfter := remote.GetUserDeviceCount(bg, "u2"); countAfter != countBefore {
			t.Fatalf("mode %s: device count changed by fallback (%d -> %d)", tc.mode, countBefore, countAfter)
		}
	}
}
