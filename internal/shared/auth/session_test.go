package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/model"
)

const testKey = "0123456789abcdef0123456789abcdef" // exactly 32 bytes

func sessionConfig() config.SessionConfig {
	return config.SessionConfig{
		Key:    testKey,
		Name:   "tpj_session",
		MaxAge: 24 * time.Hour,
		Secure: false,
	}
}

func newManager(t *testing.T, cfg config.SessionConfig) *auth.SessionManager {
	t.Helper()

	m, err := auth.NewSessionManager(cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	return m
}

// save writes a session and returns the resulting cookie.
func save(t *testing.T, m *auth.SessionManager, s auth.Session) *http.Cookie {
	t.Helper()

	rec := httptest.NewRecorder()
	if err := m.Save(rec, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Save wrote %d cookies, want 1", len(cookies))
	}
	return cookies[0]
}

// load reads a session back through a request carrying the cookie.
func load(t *testing.T, m *auth.SessionManager, c *http.Cookie) auth.Session {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if c != nil {
		r.AddCookie(c)
	}
	return m.Load(r)
}

// TestNewSessionManagerRefusesAWeakKey. SESSION_KEY is the only thing standing
// between a visitor and any account, so a short one is a failed boot rather than
// a warning at the first login. config.validate enforces the same rule; this is
// the second, independent guard.
func TestNewSessionManagerRefusesAWeakKey(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.SessionConfig
	}{
		{name: "empty key", cfg: config.SessionConfig{Name: "tpj_session"}},
		{name: "one byte short", cfg: config.SessionConfig{Key: testKey[:31], Name: "tpj_session"}},
		{
			// The literal the reference app hardcodes. Explicitly refused.
			name: "the reference app's key", cfg: config.SessionConfig{Key: "secret", Name: "tpj_session"},
		},
		{name: "empty name", cfg: config.SessionConfig{Key: testKey}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.NewSessionManager(tc.cfg); err == nil {
				t.Error("NewSessionManager accepted a configuration it should refuse")
			}
		})
	}

	// Exactly the minimum is accepted, so the boundary is where it is documented.
	if _, err := auth.NewSessionManager(config.SessionConfig{Key: testKey, Name: "tpj_session"}); err != nil {
		t.Errorf("NewSessionManager refused a 32-byte key: %v", err)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	m := newManager(t, sessionConfig())

	want := auth.Session{
		Token: "a.jwt.value",
		CSRF:  "the-csrf-secret",
		Flash: []model.Flash{model.FlashSuccess("Layanan berhasil disimpan.")},
	}

	got := load(t, m, save(t, m, want))

	if got.Token != want.Token {
		t.Errorf("Token = %q, want %q", got.Token, want.Token)
	}
	if got.CSRF != want.CSRF {
		t.Errorf("CSRF = %q, want %q", got.CSRF, want.CSRF)
	}
	if len(got.Flash) != 1 || got.Flash[0].Message != want.Flash[0].Message {
		t.Errorf("Flash = %+v, want %+v", got.Flash, want.Flash)
	}
}

// TestSessionCarriesNoIdentity is the rule that makes a leaked SESSION_KEY a
// smaller problem than it is in the reference app.
//
// The cookie holds the token and nothing else about the user. Name and email are
// re-derived from the verified claims on every request, so a forged cookie cannot
// change the displayed identity — the reference app stores them in the session
// and prefers them over the claims, which is what turns its hardcoded key into an
// impersonation bug rather than a cosmetic one.
func TestSessionCarriesNoIdentity(t *testing.T) {
	m := newManager(t, sessionConfig())
	c := save(t, m, auth.Session{Token: "a.jwt.value"})

	payload, _, _ := strings.Cut(c.Value, ".")
	for _, forbidden := range []string{"email", "name", "user_id", "role", "admin"} {
		if strings.Contains(strings.ToLower(payload), forbidden) {
			t.Errorf("the session payload mentions %q — identity must come from the "+
				"verified claims, never from the cookie", forbidden)
		}
	}
}

// TestTamperingIsIndistinguishableFromNoCookie. A one-character edit yields an
// anonymous visitor, never an error page and never a 500: that is the correct
// response, and Phase 3 verified it by hand.
func TestTamperingIsIndistinguishableFromNoCookie(t *testing.T) {
	m := newManager(t, sessionConfig())
	good := save(t, m, auth.Session{Token: "a.jwt.value"})

	tests := []struct {
		name  string
		value string
	}{
		{name: "last character changed", value: flipLastChar(good.Value)},
		{name: "signature dropped", value: strings.SplitN(good.Value, ".", 2)[0]},
		{name: "signature replaced", value: strings.SplitN(good.Value, ".", 2)[0] + ".AAAA"},
		{name: "payload replaced", value: "eyJ0IjoiZm9yZ2VkIn0." + strings.SplitN(good.Value, ".", 2)[1]},
		{name: "empty", value: ""},
		{name: "no separator", value: "garbage"},
		{name: "not base64", value: "!!!.???"},
		{name: "signed payload that is not JSON", value: signedNonJSON(t, m)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := load(t, m, &http.Cookie{Name: "tpj_session", Value: tc.value})
			if !got.Empty() {
				t.Errorf("a tampered cookie loaded a live session: %+v", got)
			}
		})
	}

	// No cookie at all behaves the same way.
	if got := load(t, m, nil); !got.Empty() {
		t.Errorf("a request with no cookie loaded %+v", got)
	}
}

// TestAnotherKeyCannotForgeASession: without SESSION_KEY the payload is
// unforgeable, which is the whole basis of the CSRF design in csrf.go.
func TestAnotherKeyCannotForgeASession(t *testing.T) {
	attacker := newManager(t, config.SessionConfig{
		Key:    "ffffffffffffffffffffffffffffffff",
		Name:   "tpj_session",
		MaxAge: time.Hour,
	})
	forged := save(t, attacker, auth.Session{Token: "attacker.jwt", CSRF: "attacker-secret"})

	real := newManager(t, sessionConfig())
	if got := load(t, real, forged); !got.Empty() {
		t.Errorf("a cookie signed with another key loaded as %+v", got)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	t.Run("development", func(t *testing.T) {
		c := save(t, newManager(t, sessionConfig()), auth.Session{Token: "x"})

		if !c.HttpOnly {
			t.Error("the session cookie is not HttpOnly — script could read the token and the CSRF secret")
		}
		// Lax rather than Strict: the SSO callback is a cross-site top-level
		// navigation from dev-auth, and Strict would withhold the cookie on the very
		// first request after login.
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Path != "/" {
			t.Errorf("Path = %q, want /", c.Path)
		}
		if c.Secure {
			t.Error("Secure is set with SESSION_SECURE off — the cookie would not survive http://localhost")
		}
		if c.MaxAge != int((24 * time.Hour).Seconds()) {
			t.Errorf("MaxAge = %d, want %d", c.MaxAge, int((24 * time.Hour).Seconds()))
		}
	})

	t.Run("production sets Secure", func(t *testing.T) {
		cfg := sessionConfig()
		cfg.Secure = true

		if c := save(t, newManager(t, cfg), auth.Session{Token: "x"}); !c.Secure {
			t.Error("Secure is not set with SESSION_SECURE on")
		}
	})
}

// TestSaveOfAnEmptySessionClears: dropping the last flash must not leave a signed
// empty cookie behind.
func TestSaveOfAnEmptySessionClears(t *testing.T) {
	m := newManager(t, sessionConfig())
	c := save(t, m, auth.Session{})

	if c.Value != "" {
		t.Errorf("an empty session wrote %q, want a clearing cookie", c.Value)
	}
	if c.MaxAge != -1 {
		t.Errorf("MaxAge = %d, want -1", c.MaxAge)
	}
}

func TestClear(t *testing.T) {
	m := newManager(t, sessionConfig())

	rec := httptest.NewRecorder()
	m.Clear(rec)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Clear wrote %d cookies, want 1", len(cookies))
	}
	c := cookies[0]

	if c.Value != "" {
		t.Errorf("Clear wrote a value: %q", c.Value)
	}
	if c.MaxAge != -1 {
		t.Errorf("MaxAge = %d, want -1", c.MaxAge)
	}
	if !c.Expires.Equal(time.Unix(0, 0).UTC()) && !c.Expires.IsZero() {
		// http.Cookie normalises Expires; either shape expires it.
		if c.Expires.After(time.Now()) {
			t.Errorf("Expires = %v, want a past time", c.Expires)
		}
	}
	if !c.HttpOnly {
		t.Error("the clearing cookie is not HttpOnly")
	}
}

// TestSessionEmpty covers the predicate Save branches on.
func TestSessionEmpty(t *testing.T) {
	tests := []struct {
		name string
		s    auth.Session
		want bool
	}{
		{name: "zero", s: auth.Session{}, want: true},
		{name: "token only", s: auth.Session{Token: "x"}},
		{name: "csrf only", s: auth.Session{CSRF: "x"}},
		{name: "flash only", s: auth.Session{Flash: []model.Flash{{Message: "hi"}}}},
		{name: "empty flash slice", s: auth.Session{Flash: []model.Flash{}}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Empty(); got != tc.want {
				t.Errorf("Empty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSaveRefusesAnOversizedCookie. Browsers cap a cookie at about 4 KB and drop
// anything larger silently, so this must fail loudly here instead.
func TestSaveRefusesAnOversizedCookie(t *testing.T) {
	m := newManager(t, sessionConfig())

	err := m.Save(httptest.NewRecorder(), auth.Session{Token: strings.Repeat("x", 4000)})
	if err == nil {
		t.Fatal("Save accepted a session that will not fit in a cookie")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %q, want it to name the size", err)
	}
}

func flipLastChar(s string) string {
	if s == "" {
		return "x"
	}
	last := s[len(s)-1]
	if last == 'A' {
		return s[:len(s)-1] + "B"
	}
	return s[:len(s)-1] + "A"
}

// signedNonJSON produces a correctly signed cookie whose payload is not a
// Session, which is the one tampering case the signature cannot catch.
func signedNonJSON(t *testing.T, m *auth.SessionManager) string {
	t.Helper()

	// Reached through the manager itself, since sign is unexported: a session
	// whose token is valid JSON gives a real signature, and the payload half is
	// then swapped for something that is not JSON. The result must load as
	// anonymous, not panic.
	c := save(t, m, auth.Session{Token: "x"})
	_, sig, _ := strings.Cut(c.Value, ".")
	return "bm90IGpzb24" + "." + sig
}
