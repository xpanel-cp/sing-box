package sessionadmission

import (
	"context"
	"testing"
	"testing/quick"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

// --- test stubs -------------------------------------------------------------

type fixedLimit int

func (f fixedLimit) Limit(string) int { return int(f) }

type stubFP struct {
	ok  bool
	key string
}

func (s stubFP) Identify(adapter.InboundContext) (string, bool) { return s.key, s.ok }

type stubStoreMode int

const (
	stubOK stubStoreMode = iota
	stubAtLimit
	stubErr
)

type stubStore struct {
	mode         stubStoreMode
	acquireCalls int
	released     int
}

func (s *stubStore) Acquire(_ context.Context, _, _ string, _ int) (func(), bool, error) {
	s.acquireCalls++
	switch s.mode {
	case stubAtLimit:
		return noopRelease, false, nil
	case stubErr:
		return noopRelease, false, context.DeadlineExceeded
	default:
		return func() { s.released++ }, true, nil
	}
}

func (s *stubStore) Snapshot(context.Context) map[string]int              { return nil }
func (s *stubStore) GetUserDeviceCount(context.Context, string) int       { return 0 }
func (s *stubStore) GetUserSessions(context.Context, string) UserSessions { return UserSessions{} }

// blockingStore blocks in Acquire until the context is cancelled, so the Gate's
// own 500ms deadline is what unblocks it.
type blockingStore struct {
	hadDeadline bool
}

func (s *blockingStore) Acquire(ctx context.Context, _, _ string, _ int) (func(), bool, error) {
	_, s.hadDeadline = ctx.Deadline()
	select {
	case <-ctx.Done():
		return noopRelease, false, ctx.Err()
	case <-time.After(5 * time.Second):
		return noopRelease, true, nil
	}
}

func (s *blockingStore) Snapshot(context.Context) map[string]int              { return nil }
func (s *blockingStore) GetUserDeviceCount(context.Context, string) int       { return 0 }
func (s *blockingStore) GetUserSessions(context.Context, string) UserSessions { return UserSessions{} }

// --- Property 5 (task 4.2): failure mode & reason-code correctness ----------

func TestProperty5_FailureModeAndReasonCodes(t *testing.T) {
	prop := func(userEmpty bool, rawLimit int, reject bool, fpOK bool, storeSel uint8) bool {
		mode := FailOpen
		if reject {
			mode = FailClose
		}
		limit := rawLimit % 5 // may be <= 0 to exercise the unlimited path
		store := &stubStore{mode: stubStoreMode(int(storeSel) % 3)}
		gate := NewGate(stubFP{ok: fpOK, key: "k"}, store, fixedLimit(limit), mode)

		user := "user-1"
		if userEmpty {
			user = "   " // whitespace-only
		}
		_, res := gate.Admit(context.Background(), adapter.InboundContext{User: user})

		// Fallback reasons must never masquerade as ADMITTED/DEVICE_LIMIT_REACHED.
		if (res.Reason == ReasonStoreTimeout || res.Reason == ReasonUnknownDevice) &&
			(res.Admit && res.Reason == ReasonAdmitted) {
			return false
		}

		switch {
		case userEmpty || limit <= 0:
			return res.Admit && res.Reason == ReasonUnlimited && store.acquireCalls == 0
		case !fpOK:
			return res.Admit == (mode != FailClose) &&
				res.Reason == ReasonUnknownDevice &&
				store.acquireCalls == 0
		default:
			switch store.mode {
			case stubOK:
				return res.Admit && res.Reason == ReasonAdmitted
			case stubAtLimit:
				return !res.Admit && res.Reason == ReasonDeviceLimit
			default: // stubErr
				return res.Admit == (mode != FailClose) && res.Reason == ReasonStoreTimeout
			}
		}
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// --- task 4.3: admit/reject, timeout, reason codes, release -----------------

func TestGate_AdmitAndReject(t *testing.T) {
	// Admit within limit against the real MemoryStore.
	store := NewMemoryStore(0)
	gate := NewGate(SourceIPFingerprinter{}, store, fixedLimit(1), FailOpen)
	md := mdWithSource("192.0.2.1", 1000, "vless", "tcp")
	md.User = "u"

	rel, res := gate.Admit(context.Background(), md)
	if !res.Admit || res.Reason != ReasonAdmitted {
		t.Fatalf("expected ADMITTED, got %+v", res)
	}
	if store.GetUserDeviceCount(bg, "u") != 1 {
		t.Fatalf("expected slot registered")
	}
	// A new distinct device at limit 1 is rejected.
	md2 := mdWithSource("192.0.2.2", 1000, "vless", "tcp")
	md2.User = "u"
	_, res2 := gate.Admit(context.Background(), md2)
	if res2.Admit || res2.Reason != ReasonDeviceLimit {
		t.Fatalf("expected DEVICE_LIMIT_REACHED, got %+v", res2)
	}
	// Releasing the first frees the slot.
	rel()
	if store.GetUserDeviceCount(bg, "u") != 0 {
		t.Fatalf("expected slot freed after release")
	}
	_, res3 := gate.Admit(context.Background(), md2)
	if !res3.Admit || res3.Reason != ReasonAdmitted {
		t.Fatalf("expected admit after release, got %+v", res3)
	}
}

func TestGate_Unlimited(t *testing.T) {
	store := &stubStore{}
	// limit <= 0
	gate := NewGate(SourceIPFingerprinter{}, store, fixedLimit(0), FailOpen)
	md := mdWithSource("192.0.2.1", 1, "in", "tcp")
	md.User = "u"
	_, res := gate.Admit(context.Background(), md)
	if !res.Admit || res.Reason != ReasonUnlimited {
		t.Fatalf("limit<=0 should be UNLIMITED, got %+v", res)
	}
	// empty/whitespace user
	md.User = "  "
	_, res = gate.Admit(context.Background(), md)
	if !res.Admit || res.Reason != ReasonUnlimited {
		t.Fatalf("empty user should be UNLIMITED, got %+v", res)
	}
	if store.acquireCalls != 0 {
		t.Fatalf("Acquire must not be called on the unlimited path")
	}
}

func TestGate_GateEnforcedTimeout(t *testing.T) {
	store := &blockingStore{}
	for _, mode := range []AdmissionFailureMode{FailOpen, FailClose} {
		gate := NewGate(stubFP{ok: true, key: "k"}, store, fixedLimit(2), mode)
		// Caller context has NO deadline; the Gate must impose one.
		start := time.Now()
		_, res := gate.Admit(context.Background(), adapter.InboundContext{User: "u"})
		elapsed := time.Since(start)

		if res.Reason != ReasonStoreTimeout {
			t.Fatalf("mode %s: expected STORE_TIMEOUT, got %+v", mode, res)
		}
		if res.Admit != (mode != FailClose) {
			t.Fatalf("mode %s: expected admit=%v, got %v", mode, mode != FailClose, res.Admit)
		}
		if !store.hadDeadline {
			t.Fatalf("Acquire must receive a context WITH a deadline even when caller ctx has none")
		}
		if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("expected ~500ms gate-enforced timeout, got %v", elapsed)
		}
	}
}

func TestGate_UnknownDeviceReasonCode(t *testing.T) {
	store := &stubStore{}
	for _, mode := range []AdmissionFailureMode{FailOpen, FailClose} {
		gate := NewGate(stubFP{ok: false}, store, fixedLimit(2), mode)
		_, res := gate.Admit(context.Background(), adapter.InboundContext{User: "u"})
		if res.Reason != ReasonUnknownDevice {
			t.Fatalf("mode %s: expected UNKNOWN_DEVICE, got %+v", mode, res)
		}
		if res.Admit != (mode != FailClose) {
			t.Fatalf("mode %s: expected admit=%v", mode, mode != FailClose)
		}
	}
	if store.acquireCalls != 0 {
		t.Fatalf("Acquire must not be called when the device is unidentifiable")
	}
}
