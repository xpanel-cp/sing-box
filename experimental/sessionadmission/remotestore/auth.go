package remotestore

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Per-RPC HMAC credential metadata keys. These MUST match the manager's auth
// interceptor (session-manager/auth.go) EXACTLY — gRPC lowercases metadata keys,
// and the manager reads these same lowercase keys — so signatures verify.
const (
	mdNodeID    = "x-node-id"
	mdTimestamp = "x-timestamp"
	mdNonce     = "x-nonce"
	mdSignature = "x-signature"
)

// signingString builds the canonical string signed by HMAC-SHA256:
// method + timestamp + nonce + bodyHash. This mirrors the manager's
// signingString byte-for-byte so the recomputed signature matches.
func signingString(method, timestamp, nonce, bodyHash string) string {
	return method + timestamp + nonce + bodyHash
}

// hashBody returns the lowercase hex SHA-256 of body (mirrors manager.hashBody).
func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// SignRequest computes the credential signature a node client must send for a
// request with the given full gRPC method, timestamp, nonce, and raw
// (deterministically-marshaled) body. It is byte-identical to the manager's
// SignRequest so the manager's verifier accepts it.
func SignRequest(secret, method, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingString(method, timestamp, nonce, hashBody(body))))
	return hex.EncodeToString(mac.Sum(nil))
}

// newNonce returns a globally-unique nonce: the node id (so two nodes never
// collide in the manager's shared ReplayStore) plus a monotonic counter plus a
// random suffix.
func newNonce(nodeID string, seq uint64) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "cnonce-" + nodeID + "-" + strconv.FormatUint(seq, 10) + "-" + hex.EncodeToString(b[:])
}

// SigningClientInterceptor returns a gRPC UnaryClientInterceptor that signs
// every outgoing unary RPC with the node's HMAC credential, mirroring the
// manager's expected scheme (method + timestamp + nonce + bodyHash over the
// node secret). clock supplies the timestamp (injectable for tests).
func SigningClientInterceptor(nodeID, secret string, clock func() time.Time) grpc.UnaryClientInterceptor {
	if clock == nil {
		clock = time.Now
	}
	var seq atomic.Uint64
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var body []byte
		if msg, ok := req.(proto.Message); ok {
			body, _ = proto.MarshalOptions{Deterministic: true}.Marshal(msg)
		}
		ts := strconv.FormatInt(clock().Unix(), 10)
		nonce := newNonce(nodeID, seq.Add(1))
		sig := SignRequest(secret, method, ts, nonce, body)
		ctx = metadata.AppendToOutgoingContext(ctx,
			mdNodeID, nodeID,
			mdTimestamp, ts,
			mdNonce, nonce,
			mdSignature, sig,
		)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
