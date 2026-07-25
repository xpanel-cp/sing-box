package sessionadmission

import (
	"context"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

// admissionTimeout is the admission budget enforced by the Gate itself (not the
// caller/router) so every backend — the local MemoryStore and any future remote
// store — behaves uniformly under a slow or unreachable store.
const admissionTimeout = 500 * time.Millisecond

// Gate is the admission core. It depends ONLY on the Fingerprinter,
// SessionStore, and LimitSource interfaces, never on a concrete store, so the
// identity scheme, storage backend, and failure policy never leak into the
// router call sites.
type Gate struct {
	fp          Fingerprinter
	store       SessionStore
	limits      LimitSource
	failureMode AdmissionFailureMode
}

// NewGate constructs a Gate. failureMode is normalized: any value other than
// FailClose ("reject") resolves to FailOpen ("allow"), preserving the safe
// fail-open default.
func NewGate(fp Fingerprinter, store SessionStore, limits LimitSource, failureMode AdmissionFailureMode) *Gate {
	if failureMode != FailClose {
		failureMode = FailOpen
	}
	return &Gate{
		fp:          fp,
		store:       store,
		limits:      limits,
		failureMode: failureMode,
	}
}

// Admit is called by the router before routing. It returns a release func to be
// chained onto the connection's onClose, plus an AdmissionResult carrying the
// admit decision and exactly one reason code.
//
// The Gate owns the admission deadline: it wraps the caller's ctx with a 500ms
// timeout and passes THAT to store.Acquire, so the router never has to set a
// timeout and every backend times out uniformly.
func (g *Gate) Admit(ctx context.Context, md adapter.InboundContext) (func(), AdmissionResult) {
	// Empty / whitespace-only user: anonymous inbound, no enforcement.
	if strings.TrimSpace(md.User) == "" {
		return noopRelease, AdmissionResult{Admit: true, Reason: ReasonUnlimited}
	}
	// Unlimited user (limit <= 0): no-op, no Acquire call.
	limit := g.limits.Limit(md.User)
	if limit <= 0 {
		return noopRelease, AdmissionResult{Admit: true, Reason: ReasonUnlimited}
	}

	device, identified := g.fp.Identify(md)
	if !identified {
		// Cannot identify the device: apply the failure mode, tagged UNKNOWN_DEVICE.
		return noopRelease, AdmissionResult{Admit: g.onUncertain(), Reason: ReasonUnknownDevice}
	}

	// Gate-enforced admission deadline: derived from the caller's ctx so a remote
	// store honors cancellation/tracing, but capped at 500ms uniformly here.
	actx, cancel := context.WithTimeout(ctx, admissionTimeout)
	defer cancel()

	release, ok, err := g.store.Acquire(actx, md.User, device, limit)
	if err != nil {
		// Store error or timeout (e.g. a remote agent unreachable/slow). Apply the
		// failure mode, tagged STORE_TIMEOUT; no tracked count changed.
		return noopRelease, AdmissionResult{Admit: g.onUncertain(), Reason: ReasonStoreTimeout}
	}
	if !ok {
		// Definite at-limit decision: reject regardless of failure mode.
		return noopRelease, AdmissionResult{Admit: false, Reason: ReasonDeviceLimit}
	}
	return release, AdmissionResult{Admit: true, Reason: ReasonAdmitted}
}

// onUncertain returns the admit decision dictated by the configured failure
// mode: FailOpen ("allow") admits, FailClose ("reject") rejects.
func (g *Gate) onUncertain() bool {
	return g.failureMode != FailClose
}
