package auth_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
)

func newSecret(t *testing.T) string {
	t.Helper()

	s, err := auth.NewCSRFSecret()
	if err != nil {
		t.Fatalf("NewCSRFSecret: %v", err)
	}
	return s
}

func TestCSRFRoundTrip(t *testing.T) {
	secret := newSecret(t)

	token := auth.MaskToken(secret)
	if token == "" {
		t.Fatal("MaskToken returned an empty token for a valid secret")
	}
	if !auth.VerifyToken(secret, token) {
		t.Error("a freshly masked token did not verify against its own secret")
	}
}

// TestMaskIsFreshEveryRender is the property the mask exists for, and it is not
// theoretical: the booking form reflects the customer's submitted input back into
// the same 422 response that carries the token, which is exactly the shape a
// BREACH-style compression oracle needs. A constant token in that response would
// be extractable.
func TestMaskIsFreshEveryRender(t *testing.T) {
	secret := newSecret(t)

	seen := make(map[string]bool, 100)
	for range 100 {
		token := auth.MaskToken(secret)
		if seen[token] {
			t.Fatalf("MaskToken produced the same token twice (%q) — the one-time pad is not fresh", token)
		}
		seen[token] = true

		// Every one of them must still verify.
		if !auth.VerifyToken(secret, token) {
			t.Fatalf("a freshly masked token failed to verify: %q", token)
		}
	}
}

// TestSecretsAreDistinct: a secret is minted per session, so two sessions must
// never share one.
func TestSecretsAreDistinct(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		s := newSecret(t)
		if seen[s] {
			t.Fatalf("NewCSRFSecret returned a duplicate: %q", s)
		}
		seen[s] = true
	}
}

func TestVerifyTokenRejects(t *testing.T) {
	secret := newSecret(t)
	other := newSecret(t)
	valid := auth.MaskToken(secret)

	tests := []struct {
		name   string
		secret string
		field  string
		why    string
	}{
		{
			name: "another session's token", secret: secret, field: auth.MaskToken(other),
			why: "this is the cross-session case the middleware exists to stop",
		},
		{
			// An empty secret must never pass. Treating it as one would exempt
			// exactly the sessions that predate this code.
			name: "no secret in the session", secret: "", field: valid,
			why: "a session with no secret has nothing to compare against",
		},
		{name: "no field submitted", secret: secret, field: "", why: "an absent form field"},
		{name: "both empty", secret: "", field: ""},
		{name: "not base64", secret: secret, field: "!!!not base64!!!"},
		{name: "truncated field", secret: secret, field: valid[:len(valid)-4]},
		{name: "field one byte short", secret: secret, field: shortenToken(t, valid, 1)},
		{name: "field one byte long", secret: secret, field: lengthenToken(t, valid, 1)},
		{name: "empty base64 field", secret: secret, field: ""},
		{name: "secret is not base64", secret: "!!!", field: valid},
		{
			name:   "secret is the wrong length",
			secret: base64.RawURLEncoding.EncodeToString([]byte("too short")),
			field:  valid,
		},
		{
			// The mask half flipped: decoding gives a different secret.
			name: "tampered token", secret: secret, field: flipLastBit(t, valid),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if auth.VerifyToken(tc.secret, tc.field) {
				t.Errorf("VerifyToken accepted %s — %s", tc.name, tc.why)
			}
		})
	}
}

// TestMaskTokenRejectsAnUnusableSecret: a bug here must produce a form the
// middleware then rejects, never one that silently skips the check. csrfField
// renders nothing for an empty token, so an empty return is the safe outcome.
func TestMaskTokenRejectsAnUnusableSecret(t *testing.T) {
	for _, bad := range []string{
		"",
		"!!! not base64 !!!",
		base64.RawURLEncoding.EncodeToString([]byte("31 bytes is one short.........")),
		base64.RawURLEncoding.EncodeToString(make([]byte, 64)),
	} {
		if got := auth.MaskToken(bad); got != "" {
			t.Errorf("MaskToken(%q) = %q, want an empty token", bad, got)
		}
	}
}

// TestMaskedTokenLength: the wire shape is base64(mask ‖ mask XOR secret), so it
// carries exactly twice the secret's bytes.
func TestMaskedTokenLength(t *testing.T) {
	raw, err := base64.RawURLEncoding.DecodeString(auth.MaskToken(newSecret(t)))
	if err != nil {
		t.Fatalf("the masked token is not base64url: %v", err)
	}
	if len(raw) != 64 {
		t.Errorf("masked token is %d bytes, want 64 (32 mask + 32 XOR)", len(raw))
	}
}

func TestCSRFContext(t *testing.T) {
	ctx := context.Background()

	// An anonymous request carries no secret, and MaskedTokenFrom must render
	// nothing rather than an unverifiable token.
	if got := auth.CSRFFrom(ctx); got != "" {
		t.Errorf("CSRFFrom(empty ctx) = %q, want empty", got)
	}
	if got := auth.MaskedTokenFrom(ctx); got != "" {
		t.Errorf("MaskedTokenFrom(empty ctx) = %q, want empty", got)
	}

	secret := newSecret(t)
	ctx = auth.WithCSRF(ctx, secret)

	if got := auth.CSRFFrom(ctx); got != secret {
		t.Errorf("CSRFFrom = %q, want the stored secret", got)
	}

	token := auth.MaskedTokenFrom(ctx)
	if !auth.VerifyToken(secret, token) {
		t.Error("MaskedTokenFrom produced a token that does not verify")
	}
	// Fresh per call, for the BREACH reason above.
	if token == auth.MaskedTokenFrom(ctx) {
		t.Error("MaskedTokenFrom returned the same token twice")
	}
}

// --- helpers that manipulate an encoded token ---

func decodeToken(t *testing.T, token string) []byte {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decoding %q: %v", token, err)
	}
	return raw
}

func flipLastBit(t *testing.T, token string) string {
	t.Helper()

	raw := decodeToken(t, token)
	raw[len(raw)-1] ^= 0x01
	return base64.RawURLEncoding.EncodeToString(raw)
}

func shortenToken(t *testing.T, token string, n int) string {
	t.Helper()

	raw := decodeToken(t, token)
	return base64.RawURLEncoding.EncodeToString(raw[:len(raw)-n])
}

func lengthenToken(t *testing.T, token string, n int) string {
	t.Helper()

	raw := decodeToken(t, token)
	return base64.RawURLEncoding.EncodeToString(append(raw, make([]byte, n)...))
}

// TestSecretIsBase64URL: the secret rides inside a JSON payload in a cookie, so
// it must carry no character that would need escaping there or in a header.
func TestSecretIsBase64URL(t *testing.T) {
	s := newSecret(t)

	if strings.ContainsAny(s, "+/=\"\\;, ") {
		t.Errorf("NewCSRFSecret produced %q, which carries a character unsafe in a cookie", s)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("the secret is not base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("secret is %d bytes, want 32", len(raw))
	}
}
