package app

import "testing"

// In-package: safePath is unexported. It guards the two places a caller-supplied
// value decides where an authenticated user lands — the `pass` parameter Appskep
// sends back on the SSO callback, and the `next` query on /login. Both are
// attacker-supplied, and a bare leading slash is not enough to make one safe.

func TestSafePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The ordinary cases: a site-relative path is passed through untouched.
		{name: "root", in: "/", want: "/"},
		{name: "a page", in: "/booking", want: "/booking"},
		{name: "with a query", in: "/booking?layanan=urut", want: "/booking?layanan=urut"},
		{name: "with a fragment", in: "/riwayat#atas", want: "/riwayat#atas"},
		{name: "admin", in: "/admin/jadwal", want: "/admin/jadwal"},
		{name: "an encoded path", in: "/layanan/urut%20therapeutic", want: "/layanan/urut%20therapeutic"},

		{name: "empty", in: "", want: "/"},
		{name: "relative", in: "booking", want: "/"},
		{name: "absolute http", in: "http://evil.example/", want: "/"},
		{name: "absolute https", in: "https://evil.example/", want: "/"},
		{name: "a scheme with no host", in: "javascript:alert(1)", want: "/"},
		{name: "a data URL", in: "data:text/html,<script>", want: "/"},

		// A bare leading slash is not enough. Browsers read both of these as
		// protocol-relative URLs pointing at another host, which is exactly the
		// open redirect the guard exists for.
		{name: "protocol-relative", in: "//evil.example", want: "/"},
		{name: "protocol-relative with a path", in: "//evil.example/booking", want: "/"},
		{name: "backslash variant", in: "/\\evil.example", want: "/"},
		{name: "backslash with a path", in: "/\\evil.example/booking", want: "/"},

		// A single slash followed by a real path segment is fine, even when the
		// segment itself looks like a host.
		{name: "a path that looks like a host", in: "/evil.example", want: "/evil.example"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := safePath(tc.in); got != tc.want {
				t.Errorf("safePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSafePathAlwaysReturnsASitePath states the property rather than the cases:
// whatever goes in, what comes out starts with exactly one slash and can only
// address this site.
func TestSafePathAlwaysReturnsASitePath(t *testing.T) {
	inputs := []string{
		"", "x", "//x", "/\\x", "http://x", "https://x", "///x", "/\\/x",
		"//", "/\\", "\\\\x", "/..//x", "//evil.example@good.example/",
		"/%2f%2fevil.example", "\t//evil.example",
	}

	for _, in := range inputs {
		got := safePath(in)

		if len(got) == 0 || got[0] != '/' {
			t.Errorf("safePath(%q) = %q, which is not site-relative", in, got)
			continue
		}
		if len(got) > 1 && (got[1] == '/' || got[1] == '\\') {
			t.Errorf("safePath(%q) = %q, which a browser reads as protocol-relative", in, got)
		}
	}
}
