// Package sessionadmission implements a pre-connection session admission gate
// for sing-box. It enforces a per-user device limit AFTER authentication but
// BEFORE routing/dialing, so an over-limit device is rejected before any proxy
// session is created and before any traffic flows.
//
// # Architecture boundary
//
// The admission core (Gate) depends ONLY on the Fingerprinter, SessionStore,
// and LimitSource interfaces defined in this file. It never depends on the
// concrete MemoryStore. MemoryStore is merely the default single-node backend;
// a future centralized/remote store (the "X-NET Agent") can be swapped in with
// zero changes to the Gate or the router by implementing SessionStore. This
// keeps the interface seam clean for cross-node enforcement later.
//
// # Device identity roadmap (see fingerprint.go)
//
//   - v1 — source IP + inbound composite (SourceIPFingerprinter, the default).
//   - v2 — connection fingerprint (source IP + TLS/transport attributes).
//   - v3 — X-NET client device ID.
//
// Each stage is a drop-in Fingerprinter; later stages compose over and fall
// back to the v1 composite key, so the admission core, store, and router call
// sites never change across stages.
package sessionadmission

import (
	"context"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

// Fingerprinter derives a device identity from connection metadata. Different
// implementations trade accuracy for information availability; the admission
// core never assumes a specific scheme.
type Fingerprinter interface {
	// Identify returns a stable, opaque device key for this connection, and
	// false when no identity can be derived (the caller then consults the
	// configured AdmissionFailureMode). The returned key is opaque: callers
	// must not parse or interpret its structure.
	Identify(md adapter.InboundContext) (deviceKey string, ok bool)
}

// SessionStore admits or rejects a device for a user, tracks live slots, and
// exposes observability. It is implemented by both a local in-memory store
// (MemoryStore) and a future remote/centralized backend, so all methods are
// context-aware and may report errors. The admission Gate interacts with
// storage exclusively through this interface, so substituting an alternative
// backend requires no change to the Gate or the router entry points.
type SessionStore interface {
	// Acquire attempts to claim a slot for (user, deviceKey). It returns a
	// release func and ok=true when admitted. When the user is already at the
	// limit with this being a NEW device, it returns ok=false and a no-op
	// release. Re-acquiring an already-admitted device for the same user always
	// succeeds (multiple connections from one device share one slot).
	//
	// A non-nil error signals the store could not make a confident decision
	// (e.g. a remote timeout); the Gate then applies the AdmissionFailureMode.
	//
	// The returned release func is IDEMPOTENT: the store wraps it in sync.Once
	// before handing it to the caller, so a double, repeated, or concurrent
	// invocation frees the slot / decrements the refcount exactly once and never
	// drives the count below zero.
	Acquire(ctx context.Context, user, deviceKey string, limit int) (release func(), ok bool, err error)

	// Snapshot returns live device counts per user (aggregate observability).
	Snapshot(ctx context.Context) map[string]int

	// GetUserDeviceCount returns the user's current live device count directly,
	// without materializing the full session list (0 when the user is absent).
	// It MUST agree with Snapshot()[userID] and with the number of devices
	// reported by GetUserSessions(userID) for the same user (three-way count
	// consistency), including while grace-held slots are retained.
	GetUserDeviceCount(ctx context.Context, userID string) int

	// GetUserSessions returns the detailed live sessions for one user: the
	// active devices with their key/IP, per-device connection count, and
	// first-seen / last-activity timestamps. This is the authoritative
	// online-device view for the panel.
	GetUserSessions(ctx context.Context, userID string) UserSessions
}

// UserSessions is the observability view for a single user.
type UserSessions struct {
	UserID  string
	Devices []DeviceSession
}

// DeviceSession describes one live device for a user.
type DeviceSession struct {
	DeviceKey   string    // opaque key from the Fingerprinter
	IP          string    // best-effort source IP (v1) for display
	Connections int       // active connection count (refcount) for this device
	FirstSeen   time.Time // when the slot was first acquired
	LastActive  time.Time // most recent acquire/activity on this device
}

// AdmissionFailureMode is the policy the Gate applies when it cannot make a
// confident admission decision (unidentifiable device, store error, store
// timeout).
type AdmissionFailureMode string

const (
	// FailOpen admits on uncertainty (default, current fail-open behavior).
	FailOpen AdmissionFailureMode = "allow"
	// FailClose rejects on uncertainty (fail-closed).
	FailClose AdmissionFailureMode = "reject"
)

// AdmissionReason is a machine-readable code explaining an admission decision,
// so both rejections and fallback admissions are observable and loggable.
type AdmissionReason string

const (
	// ReasonUnlimited: admitted with no enforcement (empty user or limit <= 0).
	ReasonUnlimited AdmissionReason = "UNLIMITED"
	// ReasonAdmitted: admitted within the device limit.
	ReasonAdmitted AdmissionReason = "ADMITTED"
	// ReasonDeviceLimit: rejected because the user is at the limit for a new device.
	ReasonDeviceLimit AdmissionReason = "DEVICE_LIMIT_REACHED"
	// ReasonStoreTimeout: store error/timeout; outcome resolved per failure mode.
	ReasonStoreTimeout AdmissionReason = "STORE_TIMEOUT"
	// ReasonUnknownDevice: unidentifiable device; outcome resolved per failure mode.
	ReasonUnknownDevice AdmissionReason = "UNKNOWN_DEVICE"
)

// AdmissionResult is the outcome of a Gate decision. A bare bool is not enough:
// callers need the reason to log rejections and to distinguish a real rejection
// from a fail-open/fail-closed fallback.
type AdmissionResult struct {
	Admit  bool            // whether the connection may proceed
	Reason AdmissionReason // why (feeds the rejection log record and metrics)
}

// LimitSource resolves a user's device limit. A value of 0 (or negative) means
// unlimited. It is swappable atomically on config reload.
type LimitSource interface {
	// Limit returns the device limit for a user. 0 or negative means unlimited.
	Limit(user string) int
}
