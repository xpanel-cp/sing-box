package sessionadmission

import (
	"context"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// mdFor builds an InboundContext for a client identified by source IP on a
// given inbound (the v1 SourceIPFingerprinter keys on "<ip>|<inbound>").
func mdFor(user, inbound, ip string) adapter.InboundContext {
	return adapter.InboundContext{
		User:    user,
		Inbound: inbound,
		Source:  M.SocksaddrFrom(netip.MustParseAddr(ip), 12345),
	}
}

// TestProof_HardBlockSecondDevice is the end-to-end EVIDENCE test for the hard
// device cap, using the REAL Gate + REAL MemoryStore + REAL SourceIPFingerprinter
// (no stubs). Scenario mirrors the operator request: max_devices = 1.
//
//	device#1 connects  -> ADMITTED (Reason=ADMITTED)
//	device#2 connects  -> DENIED AT ADMISSION (Admit=false, Reason=DEVICE_LIMIT)
//	                      i.e. the decision is made BEFORE routing / before any
//	                      byte flows; the connection is never established, so it
//	                      is NOT "admitted then killed by the DeviceLimiter".
//	device#1 releases  -> device#2 retry now ADMITTED (slot freed)
func TestProof_HardBlockSecondDevice(t *testing.T) {
	const user = "11111111-1111-1111-1111-111111111111"
	opts := Options{
		Enabled: true,
		Users:   []UserLimit{{UUID: user, MaxDevices: 1}},
	}
	store := NewMemoryStore(opts.GracePeriod())
	gate := NewGate(SourceIPFingerprinter{}, store, NewLimitSource(opts), opts.FailureModeValue())
	ctx := context.Background()

	// --- device #1 ---
	rel1, res1 := gate.Admit(ctx, mdFor(user, "vless-in", "1.1.1.1"))
	t.Logf("device#1 (1.1.1.1): Admit=%v Reason=%v", res1.Admit, res1.Reason)
	if !res1.Admit {
		t.Fatalf("device#1 must be admitted, got %+v", res1)
	}

	// --- device #2 while #1 holds the only slot ---
	_, res2 := gate.Admit(ctx, mdFor(user, "vless-in", "2.2.2.2"))
	t.Logf("device#2 (2.2.2.2): Admit=%v Reason=%v  (pre-routing decision)", res2.Admit, res2.Reason)
	if res2.Admit {
		t.Fatalf("device#2 must be DENIED (over cap of 1), got admitted %+v", res2)
	}
	if res2.Reason != ReasonDeviceLimit {
		t.Fatalf("device#2 denial reason must be %v, got %v", ReasonDeviceLimit, res2.Reason)
	}
	t.Logf("live device count for user while #1 active = %d (cap=1)", store.GetUserDeviceCount(ctx, user))

	// --- #1 disconnects, freeing its slot; #2 retry must now succeed ---
	rel1()
	_, res3 := gate.Admit(ctx, mdFor(user, "vless-in", "2.2.2.2"))
	t.Logf("device#2 retry after #1 released: Admit=%v Reason=%v", res3.Admit, res3.Reason)
	if !res3.Admit {
		t.Fatalf("after #1 released, device#2 should be admitted, got %+v", res3)
	}

	t.Log("PROOF OK: the over-cap device is HARD-DENIED at admission (Reason=DEVICE_LIMIT) before any data path exists — not admitted-then-killed.")
}
