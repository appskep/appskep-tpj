package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/middleware"
)

func prefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()

	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			t.Fatalf("bad test prefix %q: %v", v, err)
		}
		out = append(out, p)
	}
	return out
}

func request(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		xff     string
		trusted []string
		want    string
		why     string
	}{
		{
			// The default. TRUSTED_PROXIES is empty out of the box, so a header
			// anyone can send decides nothing at all.
			name:   "no trusted proxies ignores XFF entirely",
			remote: "203.0.113.7:54321",
			xff:    "1.2.3.4",
			want:   "203.0.113.7",
			why:    "an unconditionally trusted header is no limit at all",
		},
		{
			// The header is only believed when the connection itself came from a
			// proxy we were told to trust.
			name:    "an untrusted connection cannot claim an address",
			remote:  "203.0.113.7:54321",
			xff:     "1.2.3.4",
			trusted: []string{"10.0.0.0/8"},
			want:    "203.0.113.7",
		},
		{
			name:    "behind a trusted proxy",
			remote:  "10.0.0.5:44444",
			xff:     "203.0.113.7",
			trusted: []string{"10.0.0.0/8"},
			want:    "203.0.113.7",
		},
		{
			// Right to left: the client can prepend anything, but cannot remove what
			// the trusted hops appended after it.
			name:    "a spoofed prefix is skipped",
			remote:  "10.0.0.5:44444",
			xff:     "9.9.9.9, 203.0.113.7",
			trusted: []string{"10.0.0.0/8"},
			want:    "203.0.113.7",
			why:     "the rightmost non-trusted entry is the one the chain observed",
		},
		{
			name:    "two trusted hops are walked through",
			remote:  "10.0.0.5:44444",
			xff:     "203.0.113.7, 10.0.0.9, 10.0.0.5",
			trusted: []string{"10.0.0.0/8"},
			want:    "203.0.113.7",
		},
		{
			name:    "a client claiming to be a trusted proxy is still skipped",
			remote:  "10.0.0.5:44444",
			xff:     "10.0.0.1, 203.0.113.7",
			trusted: []string{"10.0.0.0/8"},
			want:    "203.0.113.7",
		},
		{
			name:    "no XFF behind a trusted proxy",
			remote:  "10.0.0.5:44444",
			trusted: []string{"10.0.0.0/8"},
			want:    "10.0.0.5",
		},
		{
			// The chain cannot be trusted past a value that does not parse.
			name:    "a malformed entry stops the walk",
			remote:  "10.0.0.5:44444",
			xff:     "203.0.113.7, not-an-ip",
			trusted: []string{"10.0.0.0/8"},
			want:    "10.0.0.5",
		},
		{
			name:    "every entry is a trusted proxy",
			remote:  "10.0.0.5:44444",
			xff:     "10.0.0.1, 10.0.0.2",
			trusted: []string{"10.0.0.0/8"},
			want:    "10.0.0.5",
		},
		{
			name:    "empty XFF header",
			remote:  "10.0.0.5:44444",
			xff:     "",
			trusted: []string{"10.0.0.0/8"},
			want:    "10.0.0.5",
		},
		{
			name:    "IPv6 client behind an IPv6 proxy",
			remote:  "[2001:db8::5]:44444",
			xff:     "2001:db8:1::7",
			trusted: []string{"2001:db8::/64"},
			want:    "2001:db8:1::7",
		},
		{
			// httptest and some listeners produce a RemoteAddr with no port; it is
			// returned as is rather than discarded.
			name:   "RemoteAddr without a port",
			remote: "203.0.113.7",
			want:   "203.0.113.7",
		},
		{
			name:    "unparseable RemoteAddr falls through",
			remote:  "garbage",
			xff:     "203.0.113.7",
			trusted: []string{"10.0.0.0/8"},
			want:    "garbage",
		},
		{
			name:    "loopback proxy",
			remote:  "127.0.0.1:44444",
			xff:     "203.0.113.7",
			trusted: []string{"127.0.0.1/32"},
			want:    "203.0.113.7",
		},
		{
			// The proxy sends the client mapped into IPv6; Unmap is what makes the
			// IPv4 prefix still match.
			name:    "IPv4-mapped IPv6 proxy address",
			remote:  "[::ffff:10.0.0.5]:44444",
			xff:     "203.0.113.7",
			trusted: []string{"10.0.0.0/8"},
			want:    "203.0.113.7",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := middleware.ClientIP(request(tc.remote, tc.xff), prefixes(t, tc.trusted...))
			if got != tc.want {
				t.Errorf("ClientIP(remote=%q, xff=%q, trusted=%v) = %q, want %q — %s",
					tc.remote, tc.xff, tc.trusted, got, tc.want, tc.why)
			}
		})
	}
}

// TestClientIPCannotBeSpoofedFromOutside is the property the whole design exists
// for, stated as one test: no header a client controls changes the answer when
// the connection did not come from a trusted proxy.
func TestClientIPCannotBeSpoofedFromOutside(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8")

	attempts := []string{
		"1.2.3.4",
		"1.2.3.4, 5.6.7.8",
		"10.0.0.1",                     // claiming to be the proxy
		"10.0.0.1, 10.0.0.2, 10.0.0.3", // claiming to be the whole chain
		"",
		"   ",
		"not-an-ip",
	}

	for _, xff := range attempts {
		got := middleware.ClientIP(request("203.0.113.7:1234", xff), trusted)
		if got != "203.0.113.7" {
			t.Errorf("X-Forwarded-For: %q changed the client to %q from an untrusted connection",
				xff, got)
		}
	}
}
