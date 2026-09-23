package sessionadmission

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A node whose HMAC secret has drifted from the manager's is refused on every
// single connection. Before STORE_UNAUTHENTICATED existed, the log for that read
// "reason=STORE_TIMEOUT" — identical to an unreachable manager — and a
// production outage was diagnosed by probing a manager that was healthy,
// answering in ~3ms, and rejecting on credentials the whole time.
//
// These tests pin the distinction: the ADMIT decision stays governed by the
// failure mode (nothing about enforcement changes), while the REASON now names
// which of the two faults occurred.

// authErrStub is a store error reporting a refused credential, standing in for
// what remotestore wraps an Unauthenticated gRPC status in.
type authErrStub struct{ msg string }

func (e *authErrStub) Error() string              { return e.msg }
func (e *authErrStub) StoreUnauthenticated() bool { return true }

// notAuthErrStub implements the method but answers false: a store that
// classified its error and found it was NOT an auth failure.
type notAuthErrStub struct{ msg string }

func (e *notAuthErrStub) Error() string              { return e.msg }
func (e *notAuthErrStub) StoreUnauthenticated() bool { return false }

// erroringStore fails every Acquire with one fixed error.
type erroringStore struct{ err error }

func (s *erroringStore) Acquire(context.Context, string, string, int) (func(), bool, error) {
	return noopRelease, false, s.err
}
func (s *erroringStore) Snapshot(context.Context) map[string]int              { return nil }
func (s *erroringStore) GetUserDeviceCount(context.Context, string) int       { return 0 }
func (s *erroringStore) GetUserSessions(context.Context, string) UserSessions { return UserSessions{} }

func TestClassifyStoreError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want AdmissionReason
	}{
		{
			name: "a refused credential is reported as STORE_UNAUTHENTICATED",
			err:  &authErrStub{"rpc error: code = Unauthenticated desc = authentication failed"},
			want: ReasonStoreUnauthenticated,
		},
		{
			name: "a refused credential is found through a wrapping error",
			err:  fmt.Errorf("acquire: %w", &authErrStub{"unauthenticated"}),
			want: ReasonStoreUnauthenticated,
		},
		{
			name: "a plain transport error stays a STORE_TIMEOUT",
			err:  errors.New("connection refused"),
			want: ReasonStoreTimeout,
		},
		{
			name: "a deadline stays a STORE_TIMEOUT",
			err:  context.DeadlineExceeded,
			want: ReasonStoreTimeout,
		},
		{
			// The seam is the method's ANSWER, not merely its presence.
			name: "an error that classified itself as non-auth stays a STORE_TIMEOUT",
			err:  &notAuthErrStub{"unavailable"},
			want: ReasonStoreTimeout,
		},
		{
			name: "no error yields the timeout default and never panics",
			err:  nil,
			want: ReasonStoreTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStoreError(tc.err); got != tc.want {
				t.Errorf("classifyStoreError(%v) = %q; want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestGateReportsAuthFailureDistinctly runs the REAL Gate against a store that
// refuses credentials, under BOTH failure modes, and asserts the split reason
// with no change to the admit decision.
func TestGateReportsAuthFailureDistinctly(t *testing.T) {
	for _, tc := range []struct {
		mode      AdmissionFailureMode
		wantAdmit bool
	}{
		{FailClose, false},
		{FailOpen, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			md := mdWithSource("198.51.100.7", 40000, "vless", "tcp")
			md.User = "user-1"

			authStore := &erroringStore{err: &authErrStub{"unauthenticated"}}
			g := NewGate(SourceIPFingerprinter{}, authStore, fixedLimit(2), tc.mode)
			_, res := g.Admit(context.Background(), md)
			if res.Admit != tc.wantAdmit {
				t.Errorf("admit = %v; want %v (the failure mode must still decide)", res.Admit, tc.wantAdmit)
			}
			if res.Reason != ReasonStoreUnauthenticated {
				t.Errorf("reason = %q; want %q", res.Reason, ReasonStoreUnauthenticated)
			}

			// The same gate and mode with an ordinary transport failure must keep
			// the old reason — the split must not swallow real timeouts.
			downStore := &erroringStore{err: errors.New("connection refused")}
			g2 := NewGate(SourceIPFingerprinter{}, downStore, fixedLimit(2), tc.mode)
			_, res2 := g2.Admit(context.Background(), md)
			if res2.Admit != tc.wantAdmit {
				t.Errorf("admit = %v; want %v", res2.Admit, tc.wantAdmit)
			}
			if res2.Reason != ReasonStoreTimeout {
				t.Errorf("reason = %q; want %q", res2.Reason, ReasonStoreTimeout)
			}
		})
	}
}

// TestStoreUnauthenticatedReasonIsLoggable guards the Stringer the router needs:
// sing's format.ToString panics on a bare named string type, so a reason without
// one would panic the core on the very path that reports it.
func TestStoreUnauthenticatedReasonIsLoggable(t *testing.T) {
	if got := ReasonStoreUnauthenticated.String(); got != "STORE_UNAUTHENTICATED" {
		t.Errorf("String() = %q; want %q", got, "STORE_UNAUTHENTICATED")
	}
	if ReasonStoreUnauthenticated == ReasonStoreTimeout {
		t.Error("the two reasons must be distinct codes")
	}
}
