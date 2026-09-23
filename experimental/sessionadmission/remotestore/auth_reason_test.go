package remotestore

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/sessionadmission"
	"github.com/sagernet/sing-box/experimental/sessionadmission/remotestore/managerpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// A node whose secret has drifted from the manager's gets Unauthenticated on
// every AcquireSession, in about a millisecond. Acquire used to hand that up
// indistinguishable from an unanswered call, so the Gate logged STORE_TIMEOUT
// and the operator went looking for a network fault that did not exist.
//
// These tests drive the real signing path over a real gRPC channel: the client
// signs with the WRONG secret, the manager's verifier refuses it, and the error
// must arrive classified.

// startMismatchedStub stands the stub server up with a verifier expecting
// serverSecret while the client signs with clientSecret — the exact shape of a
// drifted node credential.
func startMismatchedStub(t *testing.T, stub *stubServer, serverSecret, clientSecret string) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnaryInterceptor(verifyingServerInterceptor(serverSecret)))
	managerpb.RegisterSessionManagerServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(SigningClientInterceptor(testNodeID, clientSecret, time.Now)),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestAcquireClassifiesRefusedCredential is the end-to-end proof: a node signing
// with a stale secret gets an error the Gate can recognise as an auth failure.
func TestAcquireClassifiesRefusedCredential(t *testing.T) {
	stub := &stubServer{}
	conn := startMismatchedStub(t, stub, "manager-side-secret", "node-side-STALE-secret")
	rs := newRemoteStore(t, conn, 0)

	_, ok, err := rs.Acquire(context.Background(), "user-1", "203.0.113.5|vless-21907", 2)
	if ok {
		t.Fatal("a refused credential must not admit")
	}
	if err == nil {
		t.Fatal("expected an error from a refused credential")
	}

	// The error must reach the Gate's seam, and the Gate must read it as such.
	var authErr sessionadmission.StoreAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error does not implement StoreAuthError: %v (%T)", err, err)
	}
	if !authErr.StoreUnauthenticated() {
		t.Error("StoreUnauthenticated() = false; want true")
	}
	// The underlying status must still be reachable for anyone who wants it.
	if s, okStatus := status.FromError(errors.Unwrap(err)); !okStatus || s.Code() != codes.Unauthenticated {
		t.Errorf("unwrapped error lost its gRPC status: %v", errors.Unwrap(err))
	}
}

// TestAcquireLeavesOtherErrorsUnclassified guards the other half: a genuine
// transport failure must NOT be dressed up as an auth failure.
func TestAcquireLeavesOtherErrorsUnclassified(t *testing.T) {
	stub := &stubServer{acquireErr: status.Error(codes.Unavailable, "manager is down")}
	conn := startStub(t, stub, false)
	rs := newRemoteStore(t, conn, 0)

	_, ok, err := rs.Acquire(context.Background(), "user-1", "203.0.113.5|vless-21907", 2)
	if ok {
		t.Fatal("an unavailable manager must not admit")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	var authErr sessionadmission.StoreAuthError
	if errors.As(err, &authErr) {
		t.Errorf("an Unavailable error must not be classified as an auth failure: %v", err)
	}
}

// TestClassifyRPCError covers the mapping directly, including the codes that
// must keep their old treatment.
func TestClassifyRPCError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantAuth bool
	}{
		{"nil stays nil", nil, false},
		{"Unauthenticated is classified", status.Error(codes.Unauthenticated, "authentication failed"), true},
		{"PermissionDenied is not an auth-credential failure", status.Error(codes.PermissionDenied, "role"), false},
		{"DeadlineExceeded stays a timeout", status.Error(codes.DeadlineExceeded, "slow"), false},
		{"Unavailable stays a timeout", status.Error(codes.Unavailable, "down"), false},
		{"Unimplemented stays a timeout", status.Error(codes.Unimplemented, "no such method"), false},
		{"a non-status error stays a timeout", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRPCError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("classifyRPCError(nil) = %v; want nil", got)
				}
				return
			}
			var authErr sessionadmission.StoreAuthError
			if errors.As(got, &authErr) != tc.wantAuth {
				t.Errorf("classified=%v; want %v (err=%v)", !tc.wantAuth, tc.wantAuth, got)
			}
		})
	}
}
