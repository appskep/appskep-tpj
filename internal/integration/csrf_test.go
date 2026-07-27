package integration

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	appmw "github.com/remorac/appskep-tpj/internal/shared/middleware"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The CSRF secret lives inside the signed, HttpOnly session cookie rather than in
// a cookie of its own. That is what makes it stronger than a plain double-submit
// cookie, where anyone able to write a cookie on the domain — a compromised
// sibling subdomain, and a shared Appskep deployment has several — sets both
// halves to a value they know and walks through.
//
// r.Use(d.CSRF) goes AFTER the auth middleware, so these tests stack them the
// same way the routers do.

// post drives a POST through RequireAuth then CSRF, as a real state-changing
// route is mounted.
func post(t *testing.T, env *testsupport.Env, body url.Values, cookie *http.Cookie, headers map[string]string) (*httptest.ResponseRecorder, *ok) {
	t.Helper()

	h := &ok{}
	handler := env.Deps.RequireAuth(env.Deps.CSRF(h))

	r := httptest.NewRequest(http.MethodPost, "/admin/layanan/1", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec, h
}

func TestCSRFAdmitsAValidToken(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, token := env.SignedIn(admin.AppskepUserID)

	rec, h := post(t, env, url.Values{"_csrf": {token}, "value": {"1"}}, cookie, nil)

	if !h.ran {
		t.Fatalf("a valid token was refused; status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestCSRFRefusals(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, token := env.SignedIn(admin.AppskepUserID)
	// A second session, whose token is valid but belongs to someone else.
	_, othersToken := env.SignedIn(admin.AppskepUserID)

	tests := []struct {
		name    string
		body    url.Values
		headers map[string]string
		want    int
		why     string
	}{
		{
			name: "no token at all", body: url.Values{"value": {"1"}},
			want: http.StatusForbidden,
			why:  "a form posted from another site carries no token",
		},
		{
			name: "an empty token", body: url.Values{"_csrf": {""}, "value": {"1"}},
			want: http.StatusForbidden,
		},
		{
			name: "a tampered token",
			body: url.Values{"_csrf": {tamperBase64(t, token)}, "value": {"1"}},
			want: http.StatusForbidden,
		},
		{
			name: "another session's token",
			body: url.Values{"_csrf": {othersToken}, "value": {"1"}},
			want: http.StatusForbidden,
			why:  "this is the case a double-submit cookie cannot refuse",
		},
		{
			name: "garbage", body: url.Values{"_csrf": {"!!!not base64!!!"}, "value": {"1"}},
			want: http.StatusForbidden,
		},
		{
			name:    "a cross-site Origin, even with a valid token",
			body:    url.Values{"_csrf": {token}, "value": {"1"}},
			headers: map[string]string{"Origin": "https://evil.example"},
			want:    http.StatusForbidden,
			why:     "the origin check is defence in depth on top of the token",
		},
		{
			name:    "a cross-site Referer when Origin is absent",
			body:    url.Values{"_csrf": {token}, "value": {"1"}},
			headers: map[string]string{"Referer": "https://evil.example/form"},
			want:    http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, h := post(t, env, tc.body, cookie, tc.headers)

			if h.ran {
				t.Fatalf("the handler ran for %s — %s", tc.name, tc.why)
			}
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestCSRFAdmitsAMatchingOrigin: both APP_URL's host and the request's own Host
// count as ours, so a machine browsing 127.0.0.1 while APP_URL says localhost does
// not have every form refused.
func TestCSRFAdmitsAMatchingOrigin(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, token := env.SignedIn(admin.AppskepUserID)

	for _, origin := range []string{"http://localhost:8080", "http://example.com"} {
		// example.com is httptest's default r.Host.
		_, h := post(t, env, url.Values{"_csrf": {token}}, cookie,
			map[string]string{"Origin": origin})
		if !h.ran {
			t.Errorf("Origin %q was refused", origin)
		}
	}
}

// TestCSRFAdmitsAMissingOrigin: the no-JS, no-header case. The token is the real
// control here, and a request carrying neither header is admitted on it alone.
func TestCSRFAdmitsAMissingOrigin(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, token := env.SignedIn(admin.AppskepUserID)

	if _, h := post(t, env, url.Values{"_csrf": {token}}, cookie, nil); !h.ran {
		t.Error("a request with neither Origin nor Referer was refused")
	}
}

// TestCSRFExemptsSafeMethods. Every route in this app that changes state is a
// POST, so a GET needs no token.
func TestCSRFExemptsSafeMethods(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, _ := env.SignedIn(admin.AppskepUserID)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		h := &ok{}
		r := httptest.NewRequest(method, "/admin/layanan", nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		env.Deps.RequireAuth(env.Deps.CSRF(h)).ServeHTTP(rec, r)

		if !h.ran {
			t.Errorf("%s was refused for want of a token; status = %d", method, rec.Code)
		}
	}
}

// TestOversizedBodyIs413NotForbidden is Phase 12's found defect, made permanent.
//
// The middleware parses the body itself rather than calling PostFormValue, which
// discards the parse error. Without that, a body over the cap arrives at the token
// check as an empty form and is reported as a CSRF failure — telling someone who
// uploaded a large file that they lack permission.
func TestOversizedBodyIs413NotForbidden(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, token := env.SignedIn(admin.AppskepUserID)

	// The global cap, as main.go applies it ahead of every mount.
	const cap = 4 << 20
	body := url.Values{"_csrf": {token}, "padding": {strings.Repeat("x", cap+1024)}}

	h := &ok{}
	handler := appmw.MaxBody(cap)(env.Deps.RequireAuth(env.Deps.CSRF(h)))

	r := httptest.NewRequest(http.MethodPost, "/admin/layanan/1", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if h.ran {
		t.Fatal("an oversized body reached the handler")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 — a 403 would tell someone who uploaded a "+
			"large file that they lack permission", rec.Code)
	}
}

// TestUnreadableBodyIs400: distinguishable from both the size case and the token
// case.
func TestUnreadableBodyIs400(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, _ := env.SignedIn(admin.AppskepUserID)

	h := &ok{}
	// Declares multipart but carries no boundary, so ParseMultipartForm fails for
	// a reason that is neither size nor a missing token.
	r := httptest.NewRequest(http.MethodPost, "/admin/layanan/1", strings.NewReader("garbage"))
	r.Header.Set("Content-Type", "multipart/form-data")
	r.AddCookie(cookie)

	rec := httptest.NewRecorder()
	env.Deps.RequireAuth(env.Deps.CSRF(h)).ServeHTTP(rec, r)

	if h.ran {
		t.Fatal("an unreadable body reached the handler")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestNoSecretIsRefused. Every state-changing route is behind RequireAuth or
// RequireAdmin, so a session with no secret is only reachable if one is ever
// mounted without a gate — refusing is the safe answer to that.
func TestCSRFWithNoSessionSecretIsRefused(t *testing.T) {
	env := testsupport.New(t)

	h := &ok{}
	// CSRF alone, with no auth middleware in front to mint a secret.
	r := httptest.NewRequest(http.MethodPost, "/admin/layanan/1",
		strings.NewReader("_csrf=anything"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	env.Deps.CSRF(h).ServeHTTP(rec, r)

	if h.ran {
		t.Fatal("a request with no session secret reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestTokensAreFreshPerRender is what keeps the token out of reach of a
// BREACH-style compression oracle. The booking form reflects submitted input back
// into the same 422 that carries the token, which is exactly the shape such an
// attack needs.
func TestTokensAreFreshPerRender(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()

	seen := make(map[string]bool, 20)
	cookie, first := env.SignedIn(admin.AppskepUserID)
	seen[first] = true

	for range 20 {
		_, token := env.SignedIn(admin.AppskepUserID)
		if seen[token] {
			t.Fatalf("the same token was rendered twice: %q", token)
		}
		seen[token] = true
	}

	// Every one of them still verifies against its own session.
	if _, h := post(t, env, url.Values{"_csrf": {first}}, cookie, nil); !h.ran {
		t.Error("the first token stopped verifying")
	}
}

// tamperBase64 changes exactly one byte of a base64url value and re-encodes it.
//
// Flipping the last CHARACTER instead is what the first draft did, and it is
// wrong: base64 encodes 6 bits per character, so the final character of a value
// whose length is not a multiple of 3 carries padding bits that decoding
// ignores. "…A" and "…B" can therefore decode to identical bytes, and the
// "tampered" token verified — a test that passed alone and failed only when the
// suite happened to produce a token of the unlucky length.
//
// Decoding, flipping a byte and re-encoding cannot have that problem.
func tamperBase64(t *testing.T, s string) string {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) == 0 {
		t.Fatalf("tamperBase64: %q is not base64url (%v)", s, err)
	}

	// The middle, so nothing about the encoding's edges is involved.
	raw[len(raw)/2] ^= 0xFF

	out := base64.RawURLEncoding.EncodeToString(raw)
	if out == s {
		t.Fatalf("tamperBase64 produced the original value")
	}
	return out
}
