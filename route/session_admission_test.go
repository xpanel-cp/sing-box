package route

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/sessionadmission"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// These tests exercise the shared admission path (Router.admitConnection)
// directly, so they cover both Router entry points (RouteConnectionEx and
// RoutePacketConnectionEx) without standing up the whole routing stack. Both
// net.Conn and N.PacketConn satisfy io.Closer, which is all admitConnection
// needs, so the same code path is validated for stream and packet connections.

// mapLimits is a trivial LimitSource backed by a map (0/absent = unlimited).
type mapLimits map[string]int

func (m mapLimits) Limit(user string) int { return m[user] }

// stubFingerprinter lets a test control device-key derivation.
type stubFingerprinter struct {
	fn func(md adapter.InboundContext) (string, bool)
}

func (s stubFingerprinter) Identify(md adapter.InboundContext) (string, bool) { return s.fn(md) }

var _ sessionadmission.Fingerprinter = stubFingerprinter{}

// recordConn is a closer that records Close() calls and any bytes written, so a
// test can assert a rejected connection was closed and transferred zero bytes.
type recordConn interface {
	io.Closer
	Closes() int
	Writes() int
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "test" }
func (dummyAddr) String() string  { return "test" }

// fakeStreamConn is a net.Conn recording Close/Write counts.
type fakeStreamConn struct {
	closes int
	writes int
}

func (c *fakeStreamConn) Read([]byte) (int, error)       { return 0, io.EOF }
func (c *fakeStreamConn) Write(b []byte) (int, error)    { c.writes += len(b); return len(b), nil }
func (c *fakeStreamConn) Close() error                   { c.closes++; return nil }
func (c *fakeStreamConn) LocalAddr() net.Addr            { return dummyAddr{} }
func (c *fakeStreamConn) RemoteAddr() net.Addr           { return dummyAddr{} }
func (c *fakeStreamConn) SetDeadline(time.Time) error    { return nil }
func (c *fakeStreamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeStreamConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeStreamConn) Closes() int                    { return c.closes }
func (c *fakeStreamConn) Writes() int                    { return c.writes }

var (
	_ net.Conn   = (*fakeStreamConn)(nil)
	_ recordConn = (*fakeStreamConn)(nil)
)

// fakePacketConn is an N.PacketConn recording Close/Write counts.
type fakePacketConn struct {
	closes int
	writes int
}

func (c *fakePacketConn) ReadPacket(*buf.Buffer) (M.Socksaddr, error) { return M.Socksaddr{}, io.EOF }
func (c *fakePacketConn) WritePacket(buffer *buf.Buffer, _ M.Socksaddr) error {
	c.writes += buffer.Len()
	return nil
}
func (c *fakePacketConn) Close() error                     { c.closes++; return nil }
func (c *fakePacketConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (c *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakePacketConn) Closes() int                      { return c.closes }
func (c *fakePacketConn) Writes() int                      { return c.writes }

var (
	_ N.PacketConn = (*fakePacketConn)(nil)
	_ recordConn   = (*fakePacketConn)(nil)
)

func nopRouter(gate admissionGate) *Router {
	return &Router{
		logger:    log.NewNOPFactory().Logger(),
		admission: gate,
	}
}

func srcAddr(ip string) M.Socksaddr {
	return M.Socksaddr{Addr: netip.MustParseAddr(ip), Port: 1000}
}

// Task 7.2 / Property 6 — No-config transparency (Validates: Requirements 9.1, 9.2).
// With admission == nil, admitConnection is a pass-through: it returns ok=true,
// leaves the connection open, and forwards the original onClose unchanged.
func TestAdmitConnectionNilGatePassthrough(t *testing.T) {
	cases := map[string]func() recordConn{
		"stream": func() recordConn { return &fakeStreamConn{} },
		"packet": func() recordConn { return &fakePacketConn{} },
	}
	for name, newConn := range cases {
		t.Run(name, func(t *testing.T) {
			r := nopRouter(nil) // no gate configured
			conn := newConn()

			var closedWith []error
			orig := N.CloseHandlerFunc(func(it error) { closedWith = append(closedWith, it) })

			handler, ok := r.admitConnection(context.Background(), conn, adapter.InboundContext{User: "u"}, orig)
			if !ok {
				t.Fatal("nil gate must admit (ok=true)")
			}
			if conn.Closes() != 0 {
				t.Fatalf("nil gate must not close the connection, got %d closes", conn.Closes())
			}
			if conn.Writes() != 0 {
				t.Fatalf("nil gate must not write to the connection, got %d bytes", conn.Writes())
			}
			// The returned handler must be the original onClose unchanged: no
			// release is chained, so invoking it fires the original exactly once.
			sentinel := io.EOF
			handler(sentinel)
			if len(closedWith) != 1 || closedWith[0] != sentinel {
				t.Fatalf("nil gate must forward onClose unchanged, got %v", closedWith)
			}
		})
	}
}

// Task 6.3 / Properties 2 + 7 — Pre-session rejection and protocol uniformity
// (Validates: Requirements 1.2, 1.3, 2.2, 3.1, 3.2, 3.3, 4.1, 4.2).
// Runs the identical reject/admit scenario for both a stream (net.Conn) and a
// packet (N.PacketConn) closer to confirm stream+packet uniformity.
func TestAdmitConnectionPreSessionRejection(t *testing.T) {
	cases := map[string]func() recordConn{
		"stream": func() recordConn { return &fakeStreamConn{} },
		"packet": func() recordConn { return &fakePacketConn{} },
	}
	for name, newConn := range cases {
		t.Run(name, func(t *testing.T) {
			store := sessionadmission.NewMemoryStore(0)
			gate := sessionadmission.NewGate(
				sessionadmission.SourceIPFingerprinter{},
				store,
				mapLimits{"u": 1},
				sessionadmission.FailOpen,
			)
			r := nopRouter(gate)
			ctx := context.Background()

			// Device A is admitted: it occupies the single slot. Its connection
			// must not be closed or written to.
			connA := newConn()
			var aClosed []error
			mdA := adapter.InboundContext{User: "u", Inbound: "vless", Source: srcAddr("1.1.1.1")}
			handlerA, okA := r.admitConnection(ctx, connA, mdA, N.CloseHandlerFunc(func(it error) {
				aClosed = append(aClosed, it)
			}))
			if !okA {
				t.Fatal("device A within limit must be admitted")
			}
			if connA.Closes() != 0 || connA.Writes() != 0 {
				t.Fatalf("admitted conn must be untouched, closes=%d writes=%d", connA.Closes(), connA.Writes())
			}

			// Device B is a new distinct device for the same user at the limit:
			// it must be rejected before routing, closed, transfer zero bytes, and
			// fire onClose exactly once.
			connB := newConn()
			var bClosed []error
			mdB := adapter.InboundContext{User: "u", Inbound: "vless", Source: srcAddr("2.2.2.2")}
			handlerB, okB := r.admitConnection(ctx, connB, mdB, N.CloseHandlerFunc(func(it error) {
				bClosed = append(bClosed, it)
			}))
			if okB {
				t.Fatal("device B at limit must be rejected")
			}
			if handlerB != nil {
				t.Fatal("a rejected connection must return a nil close handler")
			}
			if connB.Closes() == 0 {
				t.Fatal("a rejected connection must be closed")
			}
			if connB.Writes() != 0 {
				t.Fatalf("a rejected connection must transfer zero bytes, got %d", connB.Writes())
			}
			if len(bClosed) != 1 {
				t.Fatalf("reject must fire onClose exactly once, got %d", len(bClosed))
			}

			// Releasing device A's slot (via its chained close handler) must free
			// the slot so device B is subsequently admitted.
			handlerA(nil)
			connB2 := newConn()
			_, okB2 := r.admitConnection(ctx, connB2, mdB, N.CloseHandlerFunc(func(error) {}))
			if !okB2 {
				t.Fatal("device B must be admitted after device A's slot is freed")
			}
			if connB2.Closes() != 0 {
				t.Fatal("the now-admitted device B connection must not be closed")
			}
		})
	}
}

// Task 6.4 — Cross-protocol single-slot counting
// (Validates: Requirements 4.2, 4.3, 4.4).
// Uses a stub Fingerprinter that keys on source IP only (ignoring the inbound),
// so the same device reaching two different inbound protocols yields one key and
// shares a single slot, while a genuinely different key at the limit is rejected.
func TestAdmitConnectionCrossProtocolSingleSlot(t *testing.T) {
	store := sessionadmission.NewMemoryStore(0)
	fp := stubFingerprinter{fn: func(md adapter.InboundContext) (string, bool) {
		if !md.Source.Addr.IsValid() {
			return "", false
		}
		return md.Source.Addr.String(), true // constant across inbound protocols
	}}
	gate := sessionadmission.NewGate(fp, store, mapLimits{"u": 1}, sessionadmission.FailOpen)
	r := nopRouter(gate)
	ctx := context.Background()
	src := srcAddr("1.1.1.1")

	// Same user + same device key on two different inbound protocols → one slot.
	_, ok1 := r.admitConnection(ctx, &fakeStreamConn{},
		adapter.InboundContext{User: "u", Inbound: "vless", Source: src},
		N.CloseHandlerFunc(func(error) {}))
	_, ok2 := r.admitConnection(ctx, &fakeStreamConn{},
		adapter.InboundContext{User: "u", Inbound: "trojan", Source: src},
		N.CloseHandlerFunc(func(error) {}))
	if !ok1 || !ok2 {
		t.Fatalf("same device key across protocols must share one slot; ok1=%v ok2=%v", ok1, ok2)
	}
	if got := store.GetUserDeviceCount(ctx, "u"); got != 1 {
		t.Fatalf("two connections from one device must count as one slot, got %d", got)
	}

	// A genuinely different device key for the same user at limit 1 → rejected.
	_, ok3 := r.admitConnection(ctx, &fakeStreamConn{},
		adapter.InboundContext{User: "u", Inbound: "vless", Source: srcAddr("9.9.9.9")},
		N.CloseHandlerFunc(func(error) {}))
	if ok3 {
		t.Fatal("a genuinely different device key at the limit must be rejected")
	}

	// Empty user is admitted as a no-op without consulting the store (R4.4),
	// leaving the tracked user's count unchanged.
	before := store.GetUserDeviceCount(ctx, "u")
	_, ok4 := r.admitConnection(ctx, &fakeStreamConn{},
		adapter.InboundContext{User: "", Source: src},
		N.CloseHandlerFunc(func(error) {}))
	if !ok4 {
		t.Fatal("empty user must be admitted as a no-op")
	}
	if after := store.GetUserDeviceCount(ctx, "u"); after != before {
		t.Fatalf("empty-user admission must not change tracked counts, before=%d after=%d", before, after)
	}
}
