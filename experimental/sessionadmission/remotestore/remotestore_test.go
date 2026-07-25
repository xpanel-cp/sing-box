package remotestore

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/sessionadmission"
	"github.com/sagernet/sing-box/experimental/sessionadmission/remotestore/managerpb"
	M "github.com/sagernet/sing/common/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const (
	testNodeID = "nodeA"
	testSecret = "secret-aaa"
)

// stubServer is an in-process SessionManagerServer whose responses are fully
// scriptable, so RemoteStore's mapping and the Gate's failure handling can be
// driven end-to-end over a real gRPC channel (bufconn).
type stubServer struct {
	managerpb.UnimplementedSessionManagerServer

	mu sync.Mutex

	acquireResp  *managerpb.AcquireSessionResponse
	acquireErr   error
	acquireDelay time.Duration
	acquireReqs  []*managerpb.AcquireSessionRequest

	releaseCalls   int
	releasedLeases []string

	renewCalls   int
	renewReqs    []*managerpb.RenewLeaseRequest
	renewExpired []string

	snapshot *managerpb.GetSessionSnapshotResponse
}

func (s *stubServer) AcquireSession(ctx context.Context, req *managerpb.AcquireSessionRequest) (*managerpb.AcquireSessionResponse, error) {
	s.mu.Lock()
	delay := s.acquireDelay
	err := s.acquireErr
	resp := s.acquireResp
	s.acquireReqs = append(s.acquireReqs, req)
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *stubServer) ReleaseSession(_ context.Context, req *managerpb.ReleaseSessionRequest) (*managerpb.ReleaseSessionResponse, error) {
	s.mu.Lock()
	s.releaseCalls++
	s.releasedLeases = append(s.releasedLeases, req.GetLeaseId())
	s.mu.Unlock()
	return &managerpb.ReleaseSessionResponse{Released: true}, nil
}

func (s *stubServer) RenewLease(_ context.Context, req *managerpb.RenewLeaseRequest) (*managerpb.RenewLeaseResponse, error) {
	s.mu.Lock()
	s.renewCalls++
	s.renewReqs = append(s.renewReqs, req)
	expired := s.renewExpired
	s.mu.Unlock()
	valid := make([]string, 0, len(req.GetLeaseId()))
	expiredSet := map[string]struct{}{}
	for _, e := range expired {
		expiredSet[e] = struct{}{}
	}
	for _, id := range req.GetLeaseId() {
		if _, ok := expiredSet[id]; !ok {
			valid = append(valid, id)
		}
	}
	return &managerpb.RenewLeaseResponse{Valid: valid, Expired: expired}, nil
}

func (s *stubServer) GetSessionSnapshot(_ context.Context, _ *managerpb.GetSessionSnapshotRequest) (*managerpb.GetSessionSnapshotResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		return &managerpb.GetSessionSnapshotResponse{}, nil
	}
	return s.snapshot, nil
}

func (s *stubServer) releaseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseCalls
}

func (s *stubServer) renewCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewCalls
}

// verifyingServerInterceptor recomputes the per-RPC HMAC using the SAME scheme
// the RemoteStore client signs with and rejects on mismatch. Its presence in a
// test proves the client signing path (metadata + canonical string) is wired
// end-to-end; the server accepts only correctly-signed calls.
func verifyingServerInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		get := func(k string) string {
			if v := md.Get(k); len(v) > 0 {
				return v[0]
			}
			return ""
		}
		var body []byte
		if msg, ok := req.(proto.Message); ok {
			body, _ = proto.MarshalOptions{Deterministic: true}.Marshal(msg)
		}
		expected := SignRequest(secret, info.FullMethod, get(mdTimestamp), get(mdNonce), body)
		got := get(mdSignature)
		if got == "" || get(mdNodeID) == "" || got != expected {
			return nil, status.Error(codes.Unauthenticated, "bad or missing signature")
		}
		return handler(ctx, req)
	}
}

// startStub stands up the stub server on a bufconn listener and returns a client
// ClientConn that signs every call with the RemoteStore's SigningClientInterceptor.
// When verify is true, the server rejects any incorrectly-signed RPC.
func startStub(t *testing.T, stub *stubServer, verify bool) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)

	var srvOpts []grpc.ServerOption
	if verify {
		srvOpts = append(srvOpts, grpc.UnaryInterceptor(verifyingServerInterceptor(testSecret)))
	}
	srv := grpc.NewServer(srvOpts...)
	managerpb.RegisterSessionManagerServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(SigningClientInterceptor(testNodeID, testSecret, time.Now)),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newRemoteStore builds a RemoteStore over the given conn with a long heartbeat
// interval so the heartbeat goroutine does not interfere with the test.
func newRemoteStore(t *testing.T, conn *grpc.ClientConn, maxSessions int) *RemoteStore {
	t.Helper()
	rs, err := New(Config{
		Conn:              conn,
		NodeID:            testNodeID,
		MaxSessions:       maxSessions,
		HeartbeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New RemoteStore: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	return rs
}

func mdWithSource(addr string, port uint16, inbound, network string) adapter.InboundContext {
	var src M.Socksaddr
	if ip, err := netip.ParseAddr(addr); err == nil {
		src = M.Socksaddr{Addr: ip, Port: port}
	} else {
		src = M.Socksaddr{Fqdn: addr, Port: port}
	}
	return adapter.InboundContext{Inbound: inbound, Network: network, Source: src}
}

func limitsFor(user string, max int) sessionadmission.LimitSource {
	return sessionadmission.NewLimitSource(sessionadmission.Options{
		DefaultLimit: 0,
		Users:        []sessionadmission.UserLimit{{UUID: user, MaxDevices: max}},
	})
}

// routerAdmissionSeam mirrors the route package's unexported admissionGate
// interface (the single method the router calls). Asserting *sessionadmission.Gate
// satisfies it here proves that backing the Gate with a RemoteStore requires NO
// change to the router/gate call sites (Property 7 / interface parity).
type routerAdmissionSeam interface {
	Admit(ctx context.Context, md adapter.InboundContext) (func(), sessionadmission.AdmissionResult)
}

// TestProperty7_InterfaceParity asserts RemoteStore satisfies SessionStore and
// drops into the Gate with no change to the router seam.
func TestProperty7_InterfaceParity(t *testing.T) {
	// Compile-time: RemoteStore is a SessionStore.
	var _ sessionadmission.SessionStore = (*RemoteStore)(nil)

	stub := &stubServer{}
	conn := startStub(t, stub, true)
	rs := newRemoteStore(t, conn, 0)

	// A Gate constructed with the RemoteStore backend still satisfies the router
	// seam — identical construction to a MemoryStore-backed Gate.
	gate := sessionadmission.NewGate(sessionadmission.SourceIPFingerprinter{}, rs, limitsFor("u1", 2), sessionadmission.FailOpen)
	var _ routerAdmissionSeam = gate
	if gate == nil {
		t.Fatal("gate must not be nil")
	}
}

// TestProperty6_AdmittedTrueGateAdmits: a normal admitted=true response makes the
// Gate admit with reason ADMITTED, and the device_key carries the node id.
func TestProperty6_AdmittedTrueGateAdmits(t *testing.T) {
	stub := &stubServer{
		acquireResp: &managerpb.AcquireSessionResponse{Admitted: true, LeaseId: "lease-1", Ttl: 30000},
	}
	conn := startStub(t, stub, true)
	rs := newRemoteStore(t, conn, 5)
	gate := sessionadmission.NewGate(sessionadmission.SourceIPFingerprinter{}, rs, limitsFor("u1", 2), sessionadmission.FailOpen)

	md := mdWithSource("192.0.2.1", 1000, "vless", "tcp")
	md.User = "u1"
	release, res := gate.Admit(context.Background(), md)
	if !res.Admit || res.Reason != sessionadmission.ReasonAdmitted {
		t.Fatalf("expected ADMITTED admit, got %+v", res)
	}
	if release == nil {
		t.Fatal("expected non-nil release on admit")
	}

	// device_key sent to the manager must be "<sourceIP>|<inbound>|<node_id>".
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.acquireReqs) != 1 {
		t.Fatalf("expected 1 acquire, got %d", len(stub.acquireReqs))
	}
	got := stub.acquireReqs[0]
	if want := "192.0.2.1|vless|" + testNodeID; got.GetDeviceKey() != want {
		t.Fatalf("device_key = %q, want %q", got.GetDeviceKey(), want)
	}
	if got.GetMaxDevices() != 2 {
		t.Fatalf("max_devices = %d, want 2 (from Gate limit)", got.GetMaxDevices())
	}
	if got.GetMaxSessions() != 5 {
		t.Fatalf("max_sessions = %d, want 5 (from node config)", got.GetMaxSessions())
	}
	if got.GetConnId() == "" {
		t.Fatal("conn_id must be minted per Acquire")
	}
	if got.GetNodeId() != testNodeID {
		t.Fatalf("node_id = %q, want %q", got.GetNodeId(), testNodeID)
	}
}

// TestProperty6_AdmittedFalseGateRejects: a definite admitted=false is a reject
// regardless of failure mode, tagged DEVICE_LIMIT_REACHED by the Gate.
func TestProperty6_AdmittedFalseGateRejects(t *testing.T) {
	for _, mode := range []sessionadmission.AdmissionFailureMode{sessionadmission.FailOpen, sessionadmission.FailClose} {
		stub := &stubServer{
			acquireResp: &managerpb.AcquireSessionResponse{Admitted: false, Reason: "DEVICE_LIMIT_REACHED"},
		}
		conn := startStub(t, stub, true)
		rs := newRemoteStore(t, conn, 0)
		gate := sessionadmission.NewGate(sessionadmission.SourceIPFingerprinter{}, rs, limitsFor("u1", 2), mode)

		md := mdWithSource("192.0.2.2", 1000, "vless", "tcp")
		md.User = "u1"
		_, res := gate.Admit(context.Background(), md)
		if res.Admit {
			t.Fatalf("mode %s: definite reject must not admit, got %+v", mode, res)
		}
		if res.Reason != sessionadmission.ReasonDeviceLimit {
			t.Fatalf("mode %s: expected DEVICE_LIMIT_REACHED, got %+v", mode, res)
		}
	}
}

// TestProperty6_RPCErrorRoutesThroughFailureMode: any RPC error surfaces as
// err from Acquire so the Gate applies AdmissionFailureMode tagged STORE_TIMEOUT.
func TestProperty6_RPCErrorRoutesThroughFailureMode(t *testing.T) {
	cases := []struct {
		mode      sessionadmission.AdmissionFailureMode
		wantAdmit bool
	}{
		{sessionadmission.FailOpen, true},
		{sessionadmission.FailClose, false},
	}
	for _, tc := range cases {
		stub := &stubServer{acquireErr: status.Error(codes.Unavailable, "manager down")}
		conn := startStub(t, stub, true)
		rs := newRemoteStore(t, conn, 0)
		gate := sessionadmission.NewGate(sessionadmission.SourceIPFingerprinter{}, rs, limitsFor("u1", 2), tc.mode)

		md := mdWithSource("192.0.2.3", 1000, "vless", "tcp")
		md.User = "u1"
		_, res := gate.Admit(context.Background(), md)
		if res.Reason != sessionadmission.ReasonStoreTimeout {
			t.Fatalf("mode %s: expected STORE_TIMEOUT, got %+v", tc.mode, res)
		}
		if res.Admit != tc.wantAdmit {
			t.Fatalf("mode %s: admit = %v, want %v", tc.mode, res.Admit, tc.wantAdmit)
		}
	}
}

// TestProperty6_TimeoutRoutesThroughFailureMode: a manager that blocks past the
// Gate's 500ms budget yields a deadline error → AdmissionFailureMode.
func TestProperty6_TimeoutRoutesThroughFailureMode(t *testing.T) {
	stub := &stubServer{
		acquireResp:  &managerpb.AcquireSessionResponse{Admitted: true, LeaseId: "lease-x"},
		acquireDelay: 700 * time.Millisecond, // > the Gate's 500ms admission budget
	}
	conn := startStub(t, stub, true)
	rs := newRemoteStore(t, conn, 0)
	gate := sessionadmission.NewGate(sessionadmission.SourceIPFingerprinter{}, rs, limitsFor("u1", 2), sessionadmission.FailClose)

	md := mdWithSource("192.0.2.4", 1000, "vless", "tcp")
	md.User = "u1"
	start := time.Now()
	_, res := gate.Admit(context.Background(), md)
	elapsed := time.Since(start)

	if res.Reason != sessionadmission.ReasonStoreTimeout {
		t.Fatalf("expected STORE_TIMEOUT on slow manager, got %+v", res)
	}
	if res.Admit {
		t.Fatalf("FailClose must reject on timeout, got admit")
	}
	// The Gate must have enforced its own deadline well before the 700ms server delay.
	if elapsed > 650*time.Millisecond {
		t.Fatalf("admission took %v; Gate 500ms budget not enforced", elapsed)
	}
}

// TestReleaseFiresAtMostOnce: repeated/concurrent invocation of the release
// closure sends ReleaseSession at most once (sync.Once), and the lease is
// dropped from the local registry.
func TestReleaseFiresAtMostOnce(t *testing.T) {
	stub := &stubServer{
		acquireResp: &managerpb.AcquireSessionResponse{Admitted: true, LeaseId: "lease-42"},
	}
	conn := startStub(t, stub, true)
	rs := newRemoteStore(t, conn, 0)

	release, ok, err := rs.Acquire(context.Background(), "u1", "192.0.2.5|vless", 2)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if got := rs.snapshotLeases(); len(got) != 1 {
		t.Fatalf("expected 1 live lease after admit, got %d", len(got))
	}

	// Fire the release many times concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()

	// The lease must be gone from the local registry immediately (sync path).
	if got := rs.snapshotLeases(); len(got) != 0 {
		t.Fatalf("expected 0 live leases after release, got %d", len(got))
	}

	// The async ReleaseSession RPC must fire exactly once. Poll briefly for the
	// single call to land, then confirm it never exceeds one.
	deadline := time.Now().Add(2 * time.Second)
	for stub.releaseCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stub.releaseCount(); got != 1 {
		t.Fatalf("ReleaseSession call count = %d, want exactly 1", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := stub.releaseCount(); got != 1 {
		t.Fatalf("ReleaseSession fired more than once: %d", got)
	}
}

// TestHeartbeatRenewsAndDropsExpired: the heartbeat goroutine batch-renews live
// leases and drops any the manager reports expired.
func TestHeartbeatRenewsAndDropsExpired(t *testing.T) {
	stub := &stubServer{
		acquireResp:  &managerpb.AcquireSessionResponse{Admitted: true, LeaseId: "lease-live"},
		renewExpired: []string{"lease-live"}, // manager reports it expired on renew
	}
	conn := startStub(t, stub, true)
	rs, err := New(Config{
		Conn:              conn,
		NodeID:            testNodeID,
		HeartbeatInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	if _, ok, err := rs.Acquire(context.Background(), "u1", "192.0.2.6|vless", 2); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}

	// Wait for at least one heartbeat renew to run and drop the expired lease.
	deadline := time.Now().Add(2 * time.Second)
	for (stub.renewCount() == 0 || len(rs.snapshotLeases()) != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.renewCount() == 0 {
		t.Fatal("heartbeat never called RenewLease")
	}
	if got := rs.snapshotLeases(); len(got) != 0 {
		t.Fatalf("expected expired lease dropped from registry, still have %d", len(got))
	}
}

// TestObservabilityMapsSnapshot: GetUserDeviceCount/GetUserSessions map the
// manager's GetSessionSnapshot; Snapshot is a best-effort empty aggregate in v1.
func TestObservabilityMapsSnapshot(t *testing.T) {
	stub := &stubServer{
		snapshot: &managerpb.GetSessionSnapshotResponse{
			User:     "u1",
			Sessions: 3,
			Devices: []*managerpb.Device{
				{DeviceKey: "192.0.2.1|vless|nodeA", Ip: "192.0.2.1", Connections: 2, FirstSeen: 1000, LastActive: 2000},
				{DeviceKey: "192.0.2.2|vless|nodeA", Ip: "192.0.2.2", Connections: 1},
			},
		},
	}
	conn := startStub(t, stub, true)
	rs := newRemoteStore(t, conn, 0)

	if got := rs.GetUserDeviceCount(context.Background(), "u1"); got != 2 {
		t.Fatalf("GetUserDeviceCount = %d, want 2", got)
	}
	view := rs.GetUserSessions(context.Background(), "u1")
	if view.UserID != "u1" || len(view.Devices) != 2 {
		t.Fatalf("GetUserSessions = %+v, want 2 devices for u1", view)
	}
	if view.Devices[0].Connections != 2 || view.Devices[0].IP != "192.0.2.1" {
		t.Fatalf("device[0] mapped wrong: %+v", view.Devices[0])
	}
	if got := rs.Snapshot(context.Background()); len(got) != 0 {
		t.Fatalf("Snapshot must be an empty aggregate in v1, got %v", got)
	}
}
