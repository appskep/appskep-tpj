package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
)

// CSRF lives in this package, beside the session, because the secret is carried
// *inside* the session cookie rather than in a cookie of its own.
//
// That choice is what makes this stronger than a plain double-submit cookie. In
// double-submit the server only checks that a cookie and a form field agree, so
// anyone who can write a cookie on the site's domain — a compromised sibling
// subdomain, of which a shared Appskep deployment has several — can set both
// halves to a value they know and the check passes. Here the secret rides in the
// HMAC-signed, HttpOnly session cookie: it cannot be read by script, cannot be
// forged without SESSION_KEY, and a cookie the attacker minted for themselves
// carries their secret, not the victim's.
//
// It also gets rotation for free: consumeCallback writes a brand new session on
// every sign-in, and logout clears it.

// csrfSecretLen is the number of random bytes behind a token. 32 matches the
// session key and the HMAC-SHA256 block size.
const csrfSecretLen = 32

// NewCSRFSecret mints the per-session secret. It is stored, never rendered —
// what reaches the page is always a fresh mask of it.
func NewCSRFSecret() (string, error) {
	b := make([]byte, csrfSecretLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// MaskToken renders the secret for one response as base64(mask ‖ mask XOR secret).
//
// The mask is fresh on every render, so the same secret never appears twice as
// the same string. That is what keeps the token out of reach of a BREACH-style
// compression oracle, and it is not theoretical here: the booking form reflects
// the customer's submitted input back into the same 422 response that carries
// the token, which is exactly the shape such an attack needs.
//
// An unusable secret yields "", and csrfField renders nothing for an empty
// token — so a bug here produces a form the middleware then rejects, never a
// form that silently skips the check.
func MaskToken(secret string) string {
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != csrfSecretLen {
		return ""
	}

	out := make([]byte, csrfSecretLen*2)
	mask := out[:csrfSecretLen]
	if _, err := rand.Read(mask); err != nil {
		return ""
	}
	for i, b := range raw {
		out[csrfSecretLen+i] = mask[i] ^ b
	}
	return base64.RawURLEncoding.EncodeToString(out)
}

// VerifyToken reports whether a submitted field matches the session's secret.
//
// An empty secret never verifies: a request whose session carries no secret has
// nothing to compare against, and treating that as a pass would exempt exactly
// the sessions that predate this code.
func VerifyToken(secret, field string) bool {
	if secret == "" || field == "" {
		return false
	}

	want, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(want) != csrfSecretLen {
		return false
	}

	masked, err := base64.RawURLEncoding.DecodeString(field)
	if err != nil || len(masked) != csrfSecretLen*2 {
		return false
	}

	got := make([]byte, csrfSecretLen)
	for i := range got {
		got[i] = masked[i] ^ masked[csrfSecretLen+i]
	}

	// ConstantTimeCompare, not bytes.Equal, for the same reason the session codec
	// uses hmac.Equal: the comparison must not leak how much of a guess was right.
	return subtle.ConstantTimeCompare(got, want) == 1
}

// WithCSRF attaches the session's CSRF secret. Only the auth middleware calls
// this, and only for a signed-in user.
func WithCSRF(ctx context.Context, secret string) context.Context {
	return context.WithValue(ctx, ctxKeyCSRF, secret)
}

// CSRFFrom returns the raw secret for this request, or "" when there is none —
// an anonymous visitor. The CSRF middleware compares against this.
func CSRFFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyCSRF).(string)
	return s
}

// MaskedTokenFrom returns a freshly masked token for rendering into a form. The
// renderer calls it once per response; a handler building a partial's payload
// calls it directly, because a partial is invoked with its own data and cannot
// reach the page envelope.
func MaskedTokenFrom(ctx context.Context) string {
	secret := CSRFFrom(ctx)
	if secret == "" {
		return ""
	}
	return MaskToken(secret)
}
