package integration

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The gate is three *app.Deps methods, applied per subsystem. A new admin route
// is admin-only by construction, so these tests drive the middleware directly
// rather than a mounted router: what is under test is the resolver, not chi.

// ok is the handler behind a guard. It records whether it ran and what user the
// request carried.
type ok struct {
	ran  bool
	user string
}

func (h *ok) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.ran = true
	if u := auth.UserFrom(r.Context()); u != nil {
		h.user = u.Email
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("kena"))
}

// rewireAuth returns a copy of Deps whose auth.Service points at authURL.
//
// A shallow copy of the struct, with one field replaced: auth.New reads
// cfg.Auth.URL when it builds its HTTPRefresher, so the config has to be changed
// before the service is constructed. Everything else — store, renderer, session
// manager — is shared with the original.
func rewireAuth(t *testing.T, env *testsupport.Env, authURL string) *app.Deps {
	t.Helper()

	cfg := *env.Cfg
	cfg.Auth.URL = strings.TrimRight(authURL, "/")

	deps := *env.Deps
	deps.Cfg = &cfg
	deps.Auth = auth.New(&cfg, env.Store, env.Deps.Log)
	return &deps
}

// get drives one request through a guard.
func get(t *testing.T, guard func(http.Handler) http.Handler, path string, cookies ...*http.Cookie) (*httptest.ResponseRecorder, *ok) {
	t.Helper()

	h := &ok{}
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	guard(h).ServeHTTP(rec, r)
	return rec, h
}

// TestRequireAuthRedirectsAnonymously, and the redirect target is what makes the
// whole flow work: client_base_url decides where Appskep delivers a valid token,
// so it comes from APP_URL and never from a request header.
func TestRequireAuthRedirectsAnonymously(t *testing.T) {
	env := testsupport.New(t)

	rec, h := get(t, env.Deps.RequireAuth, "/booking?layanan=urut&tanggal=2026-07-30")

	if h.ran {
		t.Error("the handler ran for an anonymous request")
	}
	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", rec.Code)
	}

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://dev-auth.example/v2/auth/login?client_base_url=") {
		t.Fatalf("Location = %q", loc)
	}

	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location is not a URL: %v", err)
	}
	want := "http://localhost:8080/booking?layanan=urut&tanggal=2026-07-30"
	if got := parsed.Query().Get("client_base_url"); got != want {
		t.Errorf("client_base_url = %q, want %q — it must be built from APP_URL, "+
			"never from r.Host, because it decides where a valid token is delivered",
			got, want)
	}
}

// TestRequireAuthAdmitsASignedInUser.
func TestRequireAuthAdmitsASignedInUser(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(8100)
	cookie, _ := env.SignedIn(user.AppskepUserID)

	rec, h := get(t, env.Deps.RequireAuth, "/riwayat", cookie)

	if !h.ran {
		t.Fatalf("the handler did not run; status = %d, location = %q",
			rec.Code, rec.Header().Get("Location"))
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if h.user != user.Email {
		t.Errorf("the request carried %q, want %q", h.user, user.Email)
	}
}

// TestRequireAdmin. A non-admin is forbidden rather than redirected: sending them
// to SSO would be a loop, since logging in again changes nothing.
func TestRequireAdmin(t *testing.T) {
	env := testsupport.New(t)

	t.Run("an admin is admitted", func(t *testing.T) {
		admin := env.Admin()
		cookie, _ := env.SignedIn(admin.AppskepUserID)

		_, h := get(t, env.Deps.RequireAdmin, "/admin/jadwal", cookie)
		if !h.ran {
			t.Error("the seeded admin was refused /admin")
		}
	})

	t.Run("a signed-in non-admin gets 403", func(t *testing.T) {
		user := env.User(8110)
		cookie, _ := env.SignedIn(user.AppskepUserID)

		rec, h := get(t, env.Deps.RequireAdmin, "/admin/jadwal", cookie)
		if h.ran {
			t.Error("a non-admin reached an admin handler")
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
		// The styled page, not a bare string — which is why these middlewares live
		// in internal/app and take Deps.
		if !strings.Contains(rec.Body.String(), "<html") {
			t.Error("the 403 is not the styled error page")
		}
	})

	t.Run("an anonymous request is redirected, not forbidden", func(t *testing.T) {
		rec, _ := get(t, env.Deps.RequireAdmin, "/admin/jadwal")
		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want 302", rec.Code)
		}
	})
}

// TestOptionalAuthStaysReachableAnonymously: the public pages render differently
// when signed in but must never bounce a visitor to SSO.
func TestOptionalAuthStaysReachableAnonymously(t *testing.T) {
	env := testsupport.New(t)

	rec, h := get(t, env.Deps.OptionalAuth, "/layanan")
	if !h.ran {
		t.Fatal("OptionalAuth denied an anonymous request")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if h.user != "" {
		t.Errorf("an anonymous request carried a user: %q", h.user)
	}

	user := env.User(8120)
	cookie, _ := env.SignedIn(user.AppskepUserID)
	_, h = get(t, env.Deps.OptionalAuth, "/layanan", cookie)
	if h.user != user.Email {
		t.Errorf("a signed-in request carried %q, want %q", h.user, user.Email)
	}
}

// TestATamperedCookieIsAnonymousNotAnError. Tampering is indistinguishable from
// having no cookie, which is the correct response — never a 500.
func TestATamperedCookieIsAnonymousNotAnError(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(8130)
	cookie, _ := env.SignedIn(user.AppskepUserID)

	// The cookie is base64url(payload) + "." + base64url(HMAC). Each half is
	// tampered with separately, and through tamperBase64 rather than by flipping
	// the last character — the final character of a base64 value carries padding
	// bits that decoding ignores, so "…A" and "…B" can decode to the same bytes
	// and the "tampered" cookie verifies.
	payload, sig, found := strings.Cut(cookie.Value, ".")
	if !found {
		t.Fatalf("the session cookie has no signature separator: %q", cookie.Value)
	}

	tests := map[string]string{
		"payload changed":   tamperBase64(t, payload) + "." + sig,
		"signature changed": payload + "." + tamperBase64(t, sig),
		"signature dropped": payload,
		"both replaced":     "eyJ0IjoiZm9yZ2VkIn0.AAAA",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			rec, h := get(t, env.Deps.RequireAuth, "/riwayat",
				&http.Cookie{Name: cookie.Name, Value: value})

			if h.ran {
				t.Error("a tampered cookie was accepted")
			}
			if rec.Code != http.StatusFound {
				t.Errorf("status = %d, want a redirect to SSO rather than an error page", rec.Code)
			}
		})
	}
}

// TestADeactivatedUserLosesAccessImmediately. LoadUser runs on every request
// rather than caching the row in the session, because role and is_active are
// precisely the two things an admin changes expecting an immediate effect.
//
// The refusal lands on "/" carrying a message rather than bouncing to SSO. Two
// reasons, and the test asserts both: SSO is a loop, because logging in again
// changes nothing about is_active; and a silent sign-out is indistinguishable
// from never having logged in, which sends the user round that loop forever.
// UpsertUserFromSSO deliberately leaves is_active alone on its ON DUPLICATE KEY
// branch, so without the message this is a permanent, unexplained lockout.
func TestADeactivatedUserLosesAccessImmediately(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(8140)
	cookie, _ := env.SignedIn(user.AppskepUserID)

	if _, h := get(t, env.Deps.RequireAuth, "/riwayat", cookie); !h.ran {
		t.Fatal("the user was refused before being deactivated")
	}

	env.Exec("UPDATE users SET is_active = 0 WHERE id = ?", user.ID)

	rec, h := get(t, env.Deps.RequireAuth, "/riwayat", cookie)
	if h.ran {
		t.Error("a deactivated user still reached the handler on the very next request")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want %q — SSO would be a redirect loop", got, "/")
	}

	sess := sessionFrom(t, env, rec)
	if sess.Token != "" {
		t.Error("the session still carries a token after the user was deactivated")
	}
	if len(sess.Flash) == 0 {
		t.Error("the user was signed out with no explanation")
	}
}

// sessionFrom decodes the session the response wrote.
//
// The payload half of the cookie is plain base64url JSON — it is signed, not
// encrypted, because it holds a JWT the bearer already has. So a test can read
// it without the key, and asserting on it is how "was the user told why" becomes
// a check rather than a thing someone has to open a browser to see.
func sessionFrom(t *testing.T, env *testsupport.Env, rec *httptest.ResponseRecorder) auth.Session {
	t.Helper()

	var raw string
	for _, c := range rec.Result().Cookies() {
		if c.Name == env.Cfg.Session.Name {
			raw = c.Value
		}
	}
	if raw == "" {
		t.Fatal("no session cookie was written")
	}

	payload, _, found := strings.Cut(raw, ".")
	if !found {
		t.Fatalf("session cookie is not payload.signature: %q", raw)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decoding session payload: %v", err)
	}

	var s auth.Session
	if err := json.Unmarshal(decoded, &s); err != nil {
		t.Fatalf("unmarshalling session payload: %v", err)
	}
	return s
}

// TestADemotedAdminLosesAdminImmediately, same reason.
func TestADemotedAdminLosesAdminImmediately(t *testing.T) {
	env := testsupport.New(t)

	admin := env.Admin()
	cookie, _ := env.SignedIn(admin.AppskepUserID)

	if _, h := get(t, env.Deps.RequireAdmin, "/admin", cookie); !h.ran {
		t.Fatal("the admin was refused before being demoted")
	}

	env.Exec("UPDATE users SET role = 'user' WHERE id = ?", admin.ID)

	rec, h := get(t, env.Deps.RequireAdmin, "/admin", cookie)
	if h.ran {
		t.Error("a demoted admin still reached /admin")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestCallbackConsumesTheToken. The token arrives as a query parameter, so it is
// in the browser history, in any Referer and in the access log of anything in
// front of this server. It cannot be removed from the inbound request; it must
// not survive it.
func TestCallbackConsumesTheToken(t *testing.T) {
	env := testsupport.New(t)
	env.ClearLogs()

	token := testsupport.MintToken(t, env.Cfg.Auth.Secret, 8200)

	rec, h := get(t, env.Deps.RequireAuth,
		"/?access_token="+url.QueryEscape(token)+"&pass=%2Friwayat")

	if h.ran {
		t.Error("the handler ran on the callback request rather than after the redirect")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/riwayat" {
		t.Errorf("Location = %q, want the sanitised pass", got)
	}
	// So the redirect carrying a freshly-minted session is never held by a shared
	// cache.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	// And the clean path carries no token.
	if strings.Contains(rec.Header().Get("Location"), "access_token") {
		t.Error("the redirect target still carries the token")
	}

	// The local mirror was created from the claims.
	created := env.User(8200)
	if created.Email != "user8200@example.test" {
		t.Errorf("email = %q, want it taken from the token claims", created.Email)
	}
	if !created.LastLoginAt.Valid {
		t.Error("last_login_at was not stamped")
	}

	// The session that came back carries the token and nothing else.
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies were set, want 1", len(cookies))
	}
	payload, _, _ := strings.Cut(cookies[0].Value, ".")
	for _, forbidden := range []string{"email", "name", "role"} {
		if strings.Contains(strings.ToLower(payload), forbidden) {
			t.Errorf("the session payload mentions %q", forbidden)
		}
	}

	// The request logger only ever records r.URL.Path, so the token never reaches
	// the log — a check every phase since 3 has made by hand.
	if strings.Contains(env.Logs(), token) {
		t.Error("the raw token appears in the log")
	}
	if strings.Contains(env.Logs(), "access_token") {
		t.Error("the string access_token appears in the log")
	}
}

// TestCallbackSanitisesThePassParameter. `pass` is attacker-supplied, and a bare
// leading slash is not enough: "//host" is protocol-relative.
func TestCallbackSanitisesThePassParameter(t *testing.T) {
	env := testsupport.New(t)

	tests := []struct {
		pass string
		want string
	}{
		{pass: "/riwayat", want: "/riwayat"},
		{pass: "/admin/jadwal", want: "/admin/jadwal"},
		{pass: "", want: "/"},
		{pass: "//evil.example", want: "/"},
		{pass: "/\\evil.example", want: "/"},
		{pass: "https://evil.example", want: "/"},
		{pass: "evil.example", want: "/"},
	}

	for i, tc := range tests {
		token := testsupport.MintToken(t, env.Cfg.Auth.Secret, uint64(8300+i))
		rec, _ := get(t, env.Deps.RequireAuth,
			"/?access_token="+url.QueryEscape(token)+"&pass="+url.QueryEscape(tc.pass))

		if got := rec.Header().Get("Location"); got != tc.want {
			t.Errorf("pass=%q redirected to %q, want %q", tc.pass, got, tc.want)
		}
	}
}

// TestCallbackWithABadTokenDoesNotLoop. A token that fails verification means the
// shared AUTH_SECRET is wrong, and bouncing back to SSO would produce another bad
// token and another bounce — the redirect loop authentication.md describes.
func TestCallbackWithABadTokenDoesNotLoop(t *testing.T) {
	env := testsupport.New(t)

	forged := testsupport.MintToken(t, "not-the-shared-secret", 8400)

	rec, h := get(t, env.Deps.RequireAuth, "/?access_token="+url.QueryEscape(forged))

	if h.ran {
		t.Error("a forged token was accepted")
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "dev-auth") {
		t.Errorf("Location = %q — bouncing back to SSO is the redirect loop", loc)
	}
	if loc != "/" {
		t.Errorf("Location = %q, want the public home page with a flash", loc)
	}
	// No local mirror was created for an unverified identity.
	if got := env.CountRows("users", "appskep_user_id = ?", 8400); got != 0 {
		t.Error("a users row was created from a forged token")
	}
}

// TestRefreshBranch drives the inline token refresh.
//
// auth.Refresher exists as an interface for exactly this, but auth.Service builds
// its own HTTPRefresher and offers no setter — so the fake goes at the other end
// of the wire, as an httptest.Server at AUTH_URL. That also exercises the real
// HTTPRefresher, which a hand-written fake would have skipped.
func TestRefreshBranch(t *testing.T) {
	t.Run("a successful refresh rotates the cookie", func(t *testing.T) {
		env := testsupport.New(t)

		fresh := testsupport.MintToken(t, env.Cfg.Auth.Secret, 8500)
		var gotToken string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotToken = r.URL.Query().Get("token")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fresh})
		}))
		defer srv.Close()

		deps := rewireAuth(t, env, srv.URL)

		// The refresh path re-verifies and then calls LoadUser, which reads the
		// local mirror — created by the callback in the real flow, so a test that
		// starts from a cookie has to create it.
		env.User(8500)

		stale := testsupport.MintToken(t, env.Cfg.Auth.Secret, 8500,
			testsupport.WithExpiredAt(time.Now().Add(-time.Hour)))
		cookie := testsupport.SessionCookie(t, env.Deps.Session, auth.Session{Token: stale})

		h := &ok{}
		r := httptest.NewRequest(http.MethodGet, "/riwayat", nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		deps.RequireAuth(h).ServeHTTP(rec, r)

		if !h.ran {
			t.Fatalf("the refresh path denied the request; status = %d", rec.Code)
		}
		if gotToken != stale {
			t.Errorf("the refresh call sent %q, want the stale token", gotToken)
		}
		// The rotated token is written back, so the next request does not refresh
		// again.
		cookies := rec.Result().Cookies()
		if len(cookies) == 0 {
			t.Fatal("no cookie was written after a successful refresh")
		}
		if cookies[0].Value == cookie.Value {
			t.Error("the session cookie was not rotated to the fresh token")
		}
	})

	t.Run("a failed refresh clears the session and redirects", func(t *testing.T) {
		env := testsupport.New(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()

		deps := rewireAuth(t, env, srv.URL)

		stale := testsupport.MintToken(t, env.Cfg.Auth.Secret, 8510,
			testsupport.WithExpiredAt(time.Now().Add(-time.Hour)))
		cookie := testsupport.SessionCookie(t, env.Deps.Session, auth.Session{Token: stale})

		h := &ok{}
		r := httptest.NewRequest(http.MethodGet, "/riwayat", nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		deps.RequireAuth(h).ServeHTTP(rec, r)

		if h.ran {
			t.Error("the handler ran after a failed refresh")
		}
		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want a redirect to SSO", rec.Code)
		}
		// The session is cleared, so the next request starts clean rather than
		// refreshing again on every hit.
		var cleared bool
		for _, c := range rec.Result().Cookies() {
			if c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Error("the session was not cleared after a failed refresh")
		}
	})

	t.Run("a token with no expired_at claim never refreshes", func(t *testing.T) {
		env := testsupport.New(t)

		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		deps := rewireAuth(t, env, srv.URL)
		env.User(8520)

		// Zero ExpiredAt disables the inline check rather than locking anyone out;
		// the signature is verified regardless.
		token := testsupport.MintToken(t, env.Cfg.Auth.Secret, 8520,
			testsupport.WithoutClaim("expired_at"))
		cookie := testsupport.SessionCookie(t, env.Deps.Session, auth.Session{Token: token})

		h := &ok{}
		r := httptest.NewRequest(http.MethodGet, "/riwayat", nil)
		r.AddCookie(cookie)
		deps.RequireAuth(h).ServeHTTP(httptest.NewRecorder(), r)

		if !h.ran {
			t.Error("a token with no expired_at was refused")
		}
		if called {
			t.Error("a token with no expired_at drove the refresh endpoint — that " +
				"would be one extra round trip on every single request")
		}
	})
}
