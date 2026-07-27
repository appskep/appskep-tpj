package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/model"
)

// minKeyLen is the shortest SESSION_KEY that boots. 32 bytes is the block size
// HMAC-SHA256 keys to; anything shorter buys nothing and is usually a typo.
const minKeyLen = 32

// Session is what the cookie carries.
//
// It holds the Appskep token and nothing else about the user. Name and email are
// re-derived from the verified claims on every request, so a forged cookie
// cannot change the displayed identity — the reference app stores them in the
// session and prefers them over the claims, which is why its hardcoded cookie
// key is an identity-spoofing bug rather than a cosmetic one.
type Session struct {
	Token string        `json:"t,omitempty"`
	Flash []model.Flash `json:"f,omitempty"`
	// CSRF is the per-session secret behind every form token. It lives here, in
	// the signed HttpOnly cookie, rather than in a cookie of its own — see
	// csrf.go for why that is what makes the check unforgeable.
	CSRF string `json:"c,omitempty"`
}

// Empty reports whether there is nothing worth writing a cookie for.
func (s Session) Empty() bool { return s.Token == "" && len(s.Flash) == 0 && s.CSRF == "" }

// SessionManager reads and writes the signed session cookie.
//
// The cookie value is base64url(payload) + "." + base64url(HMAC-SHA256(payload)).
// The payload is not encrypted: it holds a JWT the bearer already has, and
// encryption would hide a tampering bug rather than prevent one. What matters is
// that the signature makes the payload unforgeable.
type SessionManager struct {
	key     []byte
	name    string
	maxAge  time.Duration
	secure  bool
	maxSize int
}

// NewSessionManager fails the boot on a weak key rather than silently signing
// with one.
func NewSessionManager(cfg config.SessionConfig) (*SessionManager, error) {
	if len(cfg.Key) < minKeyLen {
		return nil, errors.New("auth: SESSION_KEY must be at least 32 bytes")
	}
	if cfg.Name == "" {
		return nil, errors.New("auth: SESSION_NAME is empty")
	}
	return &SessionManager{
		key:    []byte(cfg.Key),
		name:   cfg.Name,
		maxAge: cfg.MaxAge,
		secure: cfg.Secure,
		// Browsers cap a cookie at ~4KB. A JWT plus a flash message is far under
		// that; the guard exists so a future addition fails loudly here instead of
		// producing a cookie the browser silently drops.
		maxSize: 3800,
	}, nil
}

// Load returns the session carried by the request. A missing, malformed, or
// badly signed cookie yields the zero Session — an anonymous visitor, never an
// error page. Tampering is indistinguishable from having no cookie at all, which
// is the correct response to it.
func (m *SessionManager) Load(r *http.Request) Session {
	c, err := r.Cookie(m.name)
	if err != nil {
		return Session{}
	}

	payload, ok := m.verify(c.Value)
	if !ok {
		return Session{}
	}

	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}
	}
	return s
}

// Save writes the session cookie. An empty session clears it instead, so
// dropping the last flash does not leave a signed empty cookie behind.
func (m *SessionManager) Save(w http.ResponseWriter, s Session) error {
	if s.Empty() {
		m.Clear(w)
		return nil
	}

	payload, err := json.Marshal(s)
	if err != nil {
		return err
	}

	value := m.sign(payload)
	if len(value) > m.maxSize {
		return errors.New("auth: session cookie too large")
	}

	http.SetCookie(w, &http.Cookie{
		Name:     m.name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(m.maxAge.Seconds()),
		HttpOnly: true,
		Secure:   m.secure,
		// Lax rather than Strict: the SSO callback is a cross-site top-level
		// navigation from dev-auth.appskep.id, and Strict would withhold the
		// cookie on the very first request after login.
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Clear expires the cookie.
func (m *SessionManager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *SessionManager) sign(payload []byte) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil))
}

func (m *SessionManager) verify(value string) ([]byte, bool) {
	body, sig, found := strings.Cut(value, ".")
	if !found {
		return nil, false
	}

	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(body)
	if err != nil {
		return nil, false
	}
	got, err := enc.DecodeString(sig)
	if err != nil {
		return nil, false
	}

	mac := hmac.New(sha256.New, m.key)
	mac.Write(payload)
	// hmac.Equal, not bytes.Equal: the comparison must not leak how many leading
	// bytes of a guessed signature were right.
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, false
	}
	return payload, true
}
