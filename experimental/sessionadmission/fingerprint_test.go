package sessionadmission

import (
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

func mdWithSource(addr string, port uint16, inbound, network string) adapter.InboundContext {
	var src M.Socksaddr
	if addr != "" {
		if ip, err := netip.ParseAddr(addr); err == nil {
			src = M.Socksaddr{Addr: ip, Port: port}
		} else {
			src = M.Socksaddr{Fqdn: addr, Port: port}
		}
	}
	return adapter.InboundContext{
		Inbound: inbound,
		Network: network,
		Source:  src,
	}
}

func TestSourceIPFingerprinter_CompositeIncludesInbound(t *testing.T) {
	fp := SourceIPFingerprinter{}

	k1, ok1 := fp.Identify(mdWithSource("203.0.113.7", 12345, "vless-tls", "tcp"))
	k2, ok2 := fp.Identify(mdWithSource("203.0.113.7", 54321, "hysteria2-udp", "udp"))
	if !ok1 || !ok2 {
		t.Fatalf("expected both identifiable, got ok1=%v ok2=%v", ok1, ok2)
	}
	if k1 == k2 {
		t.Fatalf("same IP on different inbounds must yield distinct keys, both = %q", k1)
	}
	if k1 != "203.0.113.7|vless-tls" {
		t.Fatalf("unexpected key1 %q", k1)
	}
	if k2 != "203.0.113.7|hysteria2-udp" {
		t.Fatalf("unexpected key2 %q", k2)
	}
}

func TestSourceIPFingerprinter_Deterministic(t *testing.T) {
	fp := SourceIPFingerprinter{}
	// Same IP + same inbound, different ephemeral ports, must be identical.
	k1, _ := fp.Identify(mdWithSource("198.51.100.5", 1111, "vless-tls", "tcp"))
	k2, _ := fp.Identify(mdWithSource("198.51.100.5", 2222, "vless-tls", "tcp"))
	if k1 != k2 {
		t.Fatalf("expected deterministic key, got %q and %q", k1, k2)
	}
}

func TestSourceIPFingerprinter_PortAndZoneStripped(t *testing.T) {
	fp := SourceIPFingerprinter{}

	// IPv4: port must not appear.
	k4, ok := fp.Identify(mdWithSource("192.0.2.10", 8080, "in", "tcp"))
	if !ok || k4 != "192.0.2.10|in" {
		t.Fatalf("ipv4 key should exclude port, got %q ok=%v", k4, ok)
	}

	// IPv6: port must not appear.
	k6, ok := fp.Identify(mdWithSource("2001:db8::1", 8080, "in", "tcp"))
	if !ok || k6 != "2001:db8::1|in" {
		t.Fatalf("ipv6 key should exclude port, got %q ok=%v", k6, ok)
	}

	// IPv6 with zone: zone must be stripped from the IP portion.
	zoned := netip.MustParseAddr("fe80::1").WithZone("eth0")
	kz, ok := fp.Identify(adapter.InboundContext{
		Inbound: "in",
		Network: "tcp",
		Source:  M.Socksaddr{Addr: zoned, Port: 9090},
	})
	if !ok {
		t.Fatalf("zoned ipv6 should be identifiable")
	}
	if kz != "fe80::1|in" {
		t.Fatalf("zone must be stripped, got %q", kz)
	}

	// 4-in-6 mapped address should be unmapped to plain IPv4.
	mapped := netip.MustParseAddr("::ffff:192.0.2.20")
	km, ok := fp.Identify(adapter.InboundContext{
		Inbound: "in",
		Network: "tcp",
		Source:  M.Socksaddr{Addr: mapped, Port: 1},
	})
	if !ok || km != "192.0.2.20|in" {
		t.Fatalf("4-in-6 should unmap to ipv4, got %q ok=%v", km, ok)
	}
}

func TestSourceIPFingerprinter_InboundFallbackToNetwork(t *testing.T) {
	fp := SourceIPFingerprinter{}
	k, ok := fp.Identify(mdWithSource("192.0.2.30", 1, "", "udp"))
	if !ok || k != "192.0.2.30|udp" {
		t.Fatalf("expected fallback to network, got %q ok=%v", k, ok)
	}
}

func TestSourceIPFingerprinter_InvalidSource(t *testing.T) {
	fp := SourceIPFingerprinter{}

	// Absent source (zero Socksaddr).
	if k, ok := fp.Identify(adapter.InboundContext{Inbound: "in"}); ok {
		t.Fatalf("absent source must yield ok=false, got key %q", k)
	}

	// Domain-only source is not a valid IP, so no key can be derived.
	if k, ok := fp.Identify(mdWithSource("example.com", 443, "in", "tcp")); ok {
		t.Fatalf("domain source must yield ok=false, got key %q", k)
	}
}

// TestSourceIPFingerprinter_SatisfiesInterface asserts the value is usable as a
// Fingerprinter without any change to the admission core.
func TestSourceIPFingerprinter_SatisfiesInterface(t *testing.T) {
	var _ Fingerprinter = SourceIPFingerprinter{}
	var fp Fingerprinter = SourceIPFingerprinter{}
	if _, ok := fp.Identify(mdWithSource("192.0.2.1", 1, "in", "tcp")); !ok {
		t.Fatalf("expected identify via interface to succeed")
	}
}
