package sessionadmission

import (
	"time"

	"github.com/sagernet/sing/common/json/badoption"
)

// Options is the parsed configuration for the admission gate. It mirrors the
// JSON schema of the experimental.session_admission block.
//
// # Import-cycle-avoidance note
//
// This package intentionally imports ONLY adapter (for the Fingerprinter
// signature) and badoption (for Duration parsing). It does NOT import the
// option package. sing-box's option package cannot import this package either,
// because adapter imports option and this package imports adapter — importing
// this package from option would form the cycle
// option -> sessionadmission -> adapter -> option.
//
// Therefore the tree's config registry declares its OWN mirror struct
// (option.SessionAdmissionOptions / option.SessionAdmissionUser) under the
// experimental options, and startup wiring (out of scope for this task)
// translates those parsed values into this package's Options before
// constructing the store/gate. The two structs mirror the same JSON but stay
// decoupled, keeping this package importing only adapter (+ badoption) and the
// interface seam clean for a future RemoteStore.
type Options struct {
	Enabled         bool               `json:"enabled,omitempty"`
	DefaultLimit    int                `json:"default_limit,omitempty"`
	FailureMode     string             `json:"failure_mode,omitempty"`      // "allow" (default) | "reject"
	SlotGracePeriod badoption.Duration `json:"slot_grace_period,omitempty"` // 0 = disabled
	Users           []UserLimit        `json:"users,omitempty"`
}

// UserLimit maps a user UUID to its maximum device count.
type UserLimit struct {
	UUID       string `json:"uuid"`
	MaxDevices int    `json:"max_devices"`
}

// ParseFailureMode resolves a raw failure_mode string to an AdmissionFailureMode.
// Any value other than "reject" (including empty and unknown values) resolves to
// FailOpen ("allow"), preserving the safe default.
func ParseFailureMode(raw string) AdmissionFailureMode {
	if raw == string(FailClose) {
		return FailClose
	}
	return FailOpen
}

// GracePeriod returns the configured slot grace period as a time.Duration
// (0 = disabled / immediate release).
func (o Options) GracePeriod() time.Duration {
	return time.Duration(o.SlotGracePeriod)
}

// FailureModeValue resolves the configured failure mode.
func (o Options) FailureModeValue() AdmissionFailureMode {
	return ParseFailureMode(o.FailureMode)
}

// mapLimitSource is a LimitSource backed by a per-user map plus a default limit
// for users not listed. It is immutable after construction, so it can be
// swapped atomically on config reload.
type mapLimitSource struct {
	perUser      map[string]int
	defaultLimit int
}

// NewLimitSource builds a LimitSource from the parsed Options: each listed user
// maps to its MaxDevices, and users not listed fall back to DefaultLimit.
func NewLimitSource(opts Options) LimitSource {
	perUser := make(map[string]int, len(opts.Users))
	for _, u := range opts.Users {
		perUser[u.UUID] = u.MaxDevices
	}
	return &mapLimitSource{
		perUser:      perUser,
		defaultLimit: opts.DefaultLimit,
	}
}

// Limit returns the device limit for a user, falling back to the default when
// the user is not explicitly listed. 0 or negative means unlimited.
func (m *mapLimitSource) Limit(user string) int {
	if limit, ok := m.perUser[user]; ok {
		return limit
	}
	return m.defaultLimit
}

var _ LimitSource = (*mapLimitSource)(nil)
