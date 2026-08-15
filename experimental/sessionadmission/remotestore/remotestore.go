// Package remotestore implements a sessionadmission.SessionStore backed by the
// central X-NET Session Manager over gRPC.
//
// # Strict isolation
//
// gRPC and protobuf imports are confined to THIS package (and its generated
// managerpb subpackage). The parent sessionadmission package — the SessionStore
// interface and the Gate — never imports any grpc/proto package, so a build
// that does not wire a RemoteStore pulls in no gRPC. RemoteStore drops into the
// Gate exactly like MemoryStore (Property 7 / interface parity) with no change
// to the Gate or the router.
//
// # Contract
//
// The behavior here follows the design's authoritative "Phase 3 Preparation —
// RemoteStore Contract Review":
//
//   - Acquire → AcquireSession. The single interface limit maps to max_devices;
//     max_sessions comes from this store's own node-side config (default 0 =
//     session cap disabled in v1). device_key is composed as
//     "<deviceKey>|<node_id>" so the manager keys on
//     "<sourceIP>|<inbound>|<node_id>". Acquire uses the Gate-supplied ctx (the
//     500ms admission budget) directly and adds NO longer timeout.
//   - admitted=true → record the lease locally and return a sync.Once release;
//     admitted=false → definite reject (noop release, ok=false, nil err);
//     any RPC error/timeout → (noop release, ok=false, err) so the Gate applies
//     AdmissionFailureMode. v1 keeps NO cached decision.
//   - release → ReleaseSession on a FRESH background ctx (~5s), best-effort with
//     limited retry, fired at most once (sync.Once).
//   - a single heartbeat goroutine batch-renews ALL live leases every 10s
//     (≈ TTL/3) via one RenewLease; leases the manager reports expired are
//     dropped from the local registry.
//   - Snapshot is a best-effort empty aggregate in v1 (not the hot path);
//     GetUserDeviceCount / GetUserSessions map GetSessionSnapshot.
package remotestore

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/experimental/sessionadmission"
	"github.com/sagernet/sing-box/experimental/sessionadmission/remotestore/managerpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// defaultHeartbeatInterval batch-renews all live leases at ≈ Lease_TTL/3.
	defaultHeartbeatInterval = 10 * time.Second
	// renewTimeout bounds one batch RenewLease call on a fresh context.
	renewTimeout = 5 * time.Second
	// releaseTimeout bounds one ReleaseSession call on a fresh background context.
	releaseTimeout = 5 * time.Second
	// releaseRetries is the best-effort retry budget for a release.
	releaseRetries = 3
	// releaseRetryDelay is the backoff between release retries.
	releaseRetryDelay = 200 * time.Millisecond
)

// Config configures a RemoteStore.
type Config struct {
	// Endpoint is the manager's gRPC target (e.g. "host:port"). Used to dial a
	// persistent connection when Conn is nil.
	Endpoint string
	// NodeID identifies this node to the manager. Required.
	NodeID string
	// NodeSecret is the shared HMAC secret used to sign RPCs. Required when
	// dialing (Conn == nil).
	NodeSecret string
	// MaxSessions is the node-side concurrent-session cap sent as max_sessions.
	// 0 (default) disables the session cap in v1.
	MaxSessions int

	// Conn, when non-nil, is used directly instead of dialing Endpoint. The
	// caller is then responsible for attaching a signing interceptor (see
	// SigningClientInterceptor); primarily for tests (bufconn). RemoteStore
	// closes it on Close.
	Conn *grpc.ClientConn
	// DialOptions are appended to the default insecure dial options when
	// RemoteStore dials Endpoint itself. This is the extension hook for mTLS
	// (transport credentials) and for tests (a bufconn context dialer).
	DialOptions []grpc.DialOption

	// Clock is injectable for deterministic tests (defaults to time.Now).
	Clock func() time.Time
	// HeartbeatInterval overrides the default 10s renew cadence (tests).
	HeartbeatInterval time.Duration
}

// RemoteStore is a sessionadmission.SessionStore backed by the central manager.
type RemoteStore struct {
	client      managerpb.SessionManagerClient
	conn        *grpc.ClientConn
	nodeID      string
	maxSessions int32
	clock       func() time.Time

	connSeq atomic.Uint64

	mu     sync.Mutex
	leases map[string]struct{} // local live-lease registry (lease_id set)

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New constructs a RemoteStore, establishes/holds a persistent gRPC connection
// (insecure credentials in v1; mTLS is a future transport-only change via
// Config.DialOptions), and starts the single lease-renew heartbeat goroutine.
func New(cfg Config) (*RemoteStore, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("remotestore: NodeID is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	conn := cfg.Conn
	if conn == nil {
		if cfg.Endpoint == "" {
			return nil, errors.New("remotestore: Endpoint is required when no Conn is supplied")
		}
		// v1 transport: insecure credentials. mTLS later is a pure transport
		// change appended via cfg.DialOptions — Acquire/Release/Renew and the
		// HMAC signing are unchanged.
		opts := []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(SigningClientInterceptor(cfg.NodeID, cfg.NodeSecret, clock)),
		}
		opts = append(opts, cfg.DialOptions...)
		c, err := grpc.NewClient(cfg.Endpoint, opts...)
		if err != nil {
			return nil, err
		}
		conn = c
	}

	rs := &RemoteStore{
		client:      managerpb.NewSessionManagerClient(conn),
		conn:        conn,
		nodeID:      cfg.NodeID,
		maxSessions: int32(cfg.MaxSessions),
		clock:       clock,
		leases:      make(map[string]struct{}),
		stopCh:      make(chan struct{}),
	}

	interval := cfg.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	rs.wg.Add(1)
	go rs.heartbeatLoop(interval)
	return rs, nil
}

// noopRelease is returned on a rejected or errored Acquire; it never mutates
// state (mirrors sessionadmission.noopRelease, kept local to preserve isolation).
var noopRelease = func() {}

// Acquire issues one AcquireSession RPC using the Gate-supplied ctx directly
// (the 500ms admission budget — no longer timeout is added). See the package
// doc for the full contract.
func (r *RemoteStore) Acquire(ctx context.Context, user, deviceKey string, limit int) (func(), bool, error) {
	req := &managerpb.AcquireSessionRequest{
		User: user,
		// Append node_id so the manager keys on "<sourceIP>|<inbound>|<node_id>".
		// The caller's deviceKey is already the opaque "<sourceIP>|<inbound>".
		DeviceKey:   deviceKey + "|" + r.nodeID,
		MaxDevices:  int32(limit),
		MaxSessions: r.maxSessions,
		NodeId:      r.nodeID,
		ConnId:      r.newConnID(),
	}
	resp, err := r.client.AcquireSession(ctx, req)
	if err != nil {
		// ANY gRPC error/timeout → Gate applies AdmissionFailureMode (STORE_TIMEOUT).
		return noopRelease, false, err
	}
	if !resp.GetAdmitted() {
		// Definite reject (reason carried in resp); NOT an uncertain outcome.
		return noopRelease, false, nil
	}
	leaseID := resp.GetLeaseId()
	r.addLease(leaseID)
	return r.makeRelease(leaseID), true, nil
}

// makeRelease returns an idempotent (sync.Once-wrapped) release closure.
func (r *RemoteStore) makeRelease(leaseID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() { r.sendRelease(leaseID) })
	}
}

// sendRelease removes the lease locally and sends ReleaseSession best-effort on
// a FRESH background context (never the connection ctx, which is cancelled on
// close), with a limited retry.
func (r *RemoteStore) sendRelease(leaseID string) {
	r.removeLease(leaseID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		for attempt := 0; attempt < releaseRetries; attempt++ {
			_, err := r.client.ReleaseSession(ctx, &managerpb.ReleaseSessionRequest{
				LeaseId: leaseID,
				NodeId:  r.nodeID,
			})
			if err == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(releaseRetryDelay):
			}
		}
	}()
}

// heartbeatLoop is the single background goroutine that batch-renews all live
// leases on the heartbeat interval until Close.
func (r *RemoteStore) heartbeatLoop(interval time.Duration) {
	defer r.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			// Renew all live leases; when the node holds NONE, send a standalone
			// idle liveness heartbeat so the manager keeps this node marked online
			// (Requirement 17.3) and never sweeps it offline while it is healthy
			// but simply carrying no sessions.
			if !r.renewAll() {
				r.sendNodeHeartbeat()
			}
		}
	}
}

// renewAll batch-renews every live lease in one RenewLease call on a fresh
// context and drops any lease the manager reports expired. It returns true when
// there was at least one lease to renew (so the caller can fall back to a
// standalone NodeHeartbeat when the node is idle), false when there were none.
func (r *RemoteStore) renewAll() bool {
	ids := r.snapshotLeases()
	if len(ids) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), renewTimeout)
	defer cancel()
	resp, err := r.client.RenewLease(ctx, &managerpb.RenewLeaseRequest{
		LeaseId: ids,
		NodeId:  r.nodeID,
	})
	if err != nil {
		return true // best-effort; a missed heartbeat is recovered next tick
	}
	for _, expired := range resp.GetExpired() {
		r.removeLease(expired)
	}
	return true
}

// sendNodeHeartbeat sends one idle-node liveness NodeHeartbeat on a fresh
// context, best-effort (a missed beat is recovered on the next tick). It is only
// called when the node holds zero live leases; a node with leases refreshes its
// liveness implicitly through RenewLease.
func (r *RemoteStore) sendNodeHeartbeat() {
	ctx, cancel := context.WithTimeout(context.Background(), renewTimeout)
	defer cancel()
	_, _ = r.client.NodeHeartbeat(ctx, &managerpb.NodeHeartbeatRequest{NodeId: r.nodeID})
}

// Snapshot returns a best-effort empty aggregate in v1 (documented; not the hot
// path). A global aggregate is the manager's own observability job.
func (r *RemoteStore) Snapshot(_ context.Context) map[string]int {
	return map[string]int{}
}

// GetUserDeviceCount returns the user's live device count via GetSessionSnapshot
// (0 on error or when absent). Not on the hot path.
func (r *RemoteStore) GetUserDeviceCount(ctx context.Context, userID string) int {
	resp, err := r.client.GetSessionSnapshot(ctx, &managerpb.GetSessionSnapshotRequest{User: userID})
	if err != nil {
		return 0
	}
	return len(resp.GetDevices())
}

// GetUserSessions returns the detailed live sessions for a user via
// GetSessionSnapshot. On error it returns an empty view.
func (r *RemoteStore) GetUserSessions(ctx context.Context, userID string) sessionadmission.UserSessions {
	result := sessionadmission.UserSessions{UserID: userID}
	resp, err := r.client.GetSessionSnapshot(ctx, &managerpb.GetSessionSnapshotRequest{User: userID})
	if err != nil {
		return result
	}
	devices := resp.GetDevices()
	result.Devices = make([]sessionadmission.DeviceSession, 0, len(devices))
	for _, d := range devices {
		result.Devices = append(result.Devices, sessionadmission.DeviceSession{
			DeviceKey:   d.GetDeviceKey(),
			IP:          d.GetIp(),
			Connections: int(d.GetConnections()),
			FirstSeen:   time.UnixMilli(d.GetFirstSeen()),
			LastActive:  time.UnixMilli(d.GetLastActive()),
		})
	}
	return result
}

// Start is a no-op: the persistent connection and the heartbeat goroutine are
// already established in New. It exists so RemoteStore structurally satisfies
// sing-box's adapter.SimpleLifecycle (Start/Close), letting box startup tie the
// Close into the normal box shutdown without this package importing adapter.
func (r *RemoteStore) Start() error { return nil }

// Close stops the heartbeat goroutine and closes the gRPC connection. It is
// safe to call more than once.
func (r *RemoteStore) Close() error {
	r.closeOnce.Do(func() { close(r.stopCh) })
	r.wg.Wait()
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// newConnID mints a unique per-connection idempotency key for an Acquire.
func (r *RemoteStore) newConnID() string {
	return r.nodeID + "-" + strconv.FormatInt(r.clock().UnixNano(), 10) + "-" + strconv.FormatUint(r.connSeq.Add(1), 10)
}

func (r *RemoteStore) addLease(leaseID string) {
	if leaseID == "" {
		return
	}
	r.mu.Lock()
	r.leases[leaseID] = struct{}{}
	r.mu.Unlock()
}

func (r *RemoteStore) removeLease(leaseID string) {
	r.mu.Lock()
	delete(r.leases, leaseID)
	r.mu.Unlock()
}

func (r *RemoteStore) snapshotLeases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.leases))
	for id := range r.leases {
		ids = append(ids, id)
	}
	return ids
}

// Compile-time assertion that RemoteStore satisfies sessionadmission.SessionStore.
var _ sessionadmission.SessionStore = (*RemoteStore)(nil)
