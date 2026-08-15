package remotestore

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/sessionadmission"
	M "github.com/sagernet/sing/common/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fixedLimit is a constant LimitSource for the Gate.
type fixedLimit int

func (f fixedLimit) Limit(string) int { return int(f) }

// deadRemoteStore builds a REAL RemoteStore whose dialer always fails —
// simulating a Central Session Manager that is DOWN / unreachable — so every
// AcquireSession RPC returns an error, which the Gate resolves via its
// AdmissionFailureMode.
func deadRemoteStore(t *testing.T) *RemoteStore {
	t.Helper()
	rs, err := New(Config{
		Endpoint:   "passthrough:///dead-manager",
		NodeID:     "node-a",
		NodeSecret: "secret",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return nil, errors.New("central session manager is down")
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	return rs
}

func mdFor(user, ip string) adapter.InboundContext {
	return adapter.InboundContext{
		User:    user,
		Inbound: "vless-in",
		Source:  M.SocksaddrFrom(netip.MustParseAddr(ip), 12345),
	}
}

// TestProof_ManagerDown_FailClosedRejects_FailOpenAdmits is the Production
// fail-mode proof for the operator requirement, using the REAL Gate + REAL
// RemoteStore against a DOWN manager:
//
//	failure_mode=reject (fail-CLOSED, the default) → NEW connection is REJECTED
//	                                                  when the manager is down.
//	failure_mode=allow  (fail-OPEN, opt-in)        → NEW connection is ADMITTED
//	                                                  when the manager is down.
func TestProof_ManagerDown_FailClosedRejects_FailOpenAdmits(t *testing.T) {
	const user = "u-1"

	// Fail-CLOSED (production default).
	closedGate := sessionadmission.NewGate(
		sessionadmission.SourceIPFingerprinter{}, deadRemoteStore(t), fixedLimit(1), sessionadmission.FailClose,
	)
	_, res := closedGate.Admit(context.Background(), mdFor(user, "1.1.1.1"))
	t.Logf("fail-closed + manager DOWN: Admit=%v Reason=%v", res.Admit, res.Reason)
	if res.Admit {
		t.Fatal("fail-closed MUST reject a new connection when the manager is down")
	}

	// Fail-OPEN (opt-in).
	openGate := sessionadmission.NewGate(
		sessionadmission.SourceIPFingerprinter{}, deadRemoteStore(t), fixedLimit(1), sessionadmission.FailOpen,
	)
	_, res2 := openGate.Admit(context.Background(), mdFor(user, "2.2.2.2"))
	t.Logf("fail-open + manager DOWN: Admit=%v Reason=%v", res2.Admit, res2.Reason)
	if !res2.Admit {
		t.Fatal("fail-open MUST admit a new connection when the manager is down")
	}

	t.Log("PROOF OK: manager DOWN → fail-closed(reject) denies new connections; fail-open(allow) admits")
}
