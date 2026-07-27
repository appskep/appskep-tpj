package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP resolves who is calling, for rate limiting and for the request log.
//
// chi's RealIP is deliberately not used anywhere in this app: it believes
// X-Forwarded-For unconditionally, so any client can claim any address — which
// turns a per-IP rate limit into no limit at all and a log field into fiction.
//
// The rule here is that a forwarded header is read only when the connection
// itself came from a proxy we were told to trust. TRUSTED_PROXIES is empty by
// default, so out of the box XFF is ignored entirely and RemoteAddr is the
// answer. Behind nginx, set it to the proxy's address.
//
// Walking right-to-left is what makes the result unspoofable: a client can
// prepend anything it likes to XFF, but it cannot remove the entries the trusted
// hops appended after it. The first address from the right that is not itself a
// trusted proxy is the closest one the chain actually observed.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := hostOnly(r.RemoteAddr)

	if len(trusted) == 0 {
		return remote
	}

	addr, err := netip.ParseAddr(remote)
	if err != nil || !inAny(addr, trusted) {
		return remote
	}

	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return remote
	}

	parts := strings.Split(fwd, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			// A malformed entry means the chain cannot be trusted past this point.
			break
		}
		if !inAny(candidate, trusted) {
			return candidate.String()
		}
	}
	return remote
}

func inAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// hostOnly strips the port RemoteAddr always carries. A value without one is
// returned as is rather than discarded — httptest and some listeners produce it.
func hostOnly(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}
