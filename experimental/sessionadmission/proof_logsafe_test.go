package sessionadmission

import (
	"testing"

	"github.com/sagernet/sing/common/format"
)

// TestProof_AdmissionTypesAreLogSafe reproduces the exact crash seen on the
// server: sing-box's logger routes every argument through
// format.ToString, whose type switch panics ("unknown value") on a bare named
// string type that is neither `string` nor a Stringer.
//
// box.go logs AdmissionFailureMode when the gate is enabled, and route.go logs
// AdmissionReason on every rejection — so before the String() methods were
// added, enabling the gate (or rejecting an over-limit device) panicked the
// core at startup / on the hard-block path. This test calls the SAME
// format.ToString the logger uses and asserts it no longer panics and yields
// the readable underlying values.
func TestProof_AdmissionTypesAreLogSafe(t *testing.T) {
	// Would previously panic: format.ToString(FailClose) / (ReasonDeviceLimit).
	if got := format.ToString("failure_mode=", FailClose); got != "failure_mode=reject" {
		t.Fatalf("AdmissionFailureMode not log-safe: got %q", got)
	}
	if got := format.ToString("reason=", ReasonDeviceLimit); got != "reason=DEVICE_LIMIT_REACHED" {
		t.Fatalf("AdmissionReason not log-safe: got %q", got)
	}
	// Exercise the exact box.go:295 argument shape (failure mode + duration).
	if got := format.ToString(
		"in-core session admission enabled (failure_mode=", FailOpen,
		", slot_grace_period=", (Options{}).GracePeriod(), ")",
	); got == "" {
		t.Fatal("box.go-style log line stringified to empty")
	} else {
		t.Logf("box.go log line OK: %s", got)
	}
	t.Log("PROOF OK: AdmissionFailureMode and AdmissionReason stringify via sing format.ToString — no more panic('unknown value')")
}
