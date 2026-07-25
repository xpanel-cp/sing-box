package sessionadmission

import "github.com/sagernet/sing-box/adapter"

// Device identity roadmap
//
// Device identity accuracy improves in stages. Each stage is a drop-in
// Fingerprinter implementation; the admission core (Gate), the SessionStore,
// and the router call sites are unchanged across stages. Earlier stages remain
// available as fallbacks so a richer scheme never loses the ability to derive
// SOME key.
//
//   - v1 — source IP + inbound composite (SourceIPFingerprinter, this type).
//     The device key is a COMPOSITE of the client source IP and the inbound
//     tag/protocol the connection arrived on ("<sourceIP>|<inbound>"). It does
//     NOT use the raw source IP alone: keying on IP-only would collapse the
//     same public IP arriving on different inbound protocols into one device
//     key, so v1 intentionally distinguishes protocols. NAT/CGNAT collisions
//     (several physical devices sharing one public IP on the same inbound) and
//     mobile IP changes (one device appearing as several) are accepted, known
//     limitations of v1 — and the reason identity is an interface, not a
//     hardcoded field.
//
//   - v2 — connection fingerprint. Derives the key from source IP + ALPN + SNI
//     + uTLS fingerprint (whatever the transport exposes), falling back to the
//     v1 composite key when those attributes are unavailable.
//
//   - v3 — X-NET client device ID. Reads an app-injected device identifier,
//     giving a stable per-device key that survives IP changes; falls back to
//     v2, then to the v1 composite key.
//
// Cross-protocol single-device recognition (the same physical device reaching
// two inbounds counted as one slot) is a v2/v3 property, NOT a v1 guarantee:
// v1's composite key deliberately embeds the inbound identifier. Requirement
// 4.3's single-slot outcome applies precisely when the Fingerprinter yields an
// equal Device_Key, which for v1 does not hold across protocols by design.
//
// No v2/v3 implementation exists yet; only the interface-readiness/fallback
// contract is captured here so the seam can absorb them without touching the
// admission core, store, or router.

// SourceIPFingerprinter is the v1 default/fallback identity for this fork. The
// device key is a COMPOSITE of source IP + inbound tag/protocol, not the raw IP
// alone, so the same public IP arriving on different inbounds does not collapse
// into one device key. The key is deterministic and opaque.
type SourceIPFingerprinter struct{}

// Identify returns the composite "<sourceIP>|<inbound>" device key. The IP
// portion has the transport port and any zone identifier removed. It returns
// ("", false) when the source address is absent or is not a valid IP, so an
// unidentifiable connection is resolved by the configured AdmissionFailureMode
// rather than silently admitted.
func (SourceIPFingerprinter) Identify(md adapter.InboundContext) (string, bool) {
	// Require a valid IP source. Socksaddr.IsValid() is true for domains too,
	// so check the IP field directly to satisfy "valid IPv4/IPv6" (R7.3).
	if !md.Source.Addr.IsValid() {
		return "", false
	}
	// IP only: Unmap() collapses 4-in-6 to IPv4; WithZone("") drops any zone so
	// no zone identifier leaks into the key. The port is naturally excluded
	// because we read the Addr field, not AddrPort.
	ip := md.Source.Addr.Unmap().WithZone("").String()

	// Inbound discriminator: prefer the configured inbound tag, fall back to the
	// network/protocol when no tag is set. Either way the same IP on a different
	// inbound yields a different key.
	inbound := md.Inbound
	if inbound == "" {
		inbound = md.Network
	}
	return ip + "|" + inbound, true
}

// Compile-time assertion that the v1 fingerprinter satisfies the interface the
// gate depends on, so substitution never touches the admission core.
var _ Fingerprinter = SourceIPFingerprinter{}
