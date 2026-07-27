package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// In-package: claimUserID and claimTime are unexported, and expired_at's wire
// format is still unconfirmed against a real Appskep token — so the shapes
// claimTime accepts are exactly what this file pins down.

const secret = "the-shared-appskep-hmac-secret"

func mint(t *testing.T, key string, claims jwt.MapClaims) string {
	t.Helper()

	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

func goodClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"user_id":    float64(19),
		"email":      "budi@example.test",
		"name":       "Budi Santoso",
		"expired_at": time.Now().Add(time.Hour).Unix(),
	}
}

func TestVerifyAcceptsARealToken(t *testing.T) {
	c, err := Verify(secret, mint(t, secret, goodClaims()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if c.UserID != 19 {
		t.Errorf("UserID = %d, want 19", c.UserID)
	}
	if c.Email != "budi@example.test" || c.Name != "Budi Santoso" {
		t.Errorf("identity = %q / %q", c.Name, c.Email)
	}
	if c.Raw == "" {
		t.Error("Raw is empty — the refresh call and the session both need it")
	}
	if c.Expired(time.Now()) {
		t.Error("a token expiring in an hour reported as expired")
	}
}

// TestVerifyPinsTheSigningMethod is described in the source as "the single most
// important line in the package", and it is: without the pin, a token with
// alg=none parses successfully and forges any identity it likes.
func TestVerifyPinsTheSigningMethod(t *testing.T) {
	t.Run("alg=none is refused", func(t *testing.T) {
		unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, goodClaims()).
			SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatalf("minting an unsigned token: %v", err)
		}

		if _, err := Verify(secret, unsigned); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Verify accepted an alg=none token (err = %v)", err)
		}
	})

	t.Run("RS256 is refused", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generating a key: %v", err)
		}
		token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, goodClaims()).SignedString(key)
		if err != nil {
			t.Fatalf("signing RS256: %v", err)
		}

		// The classic confusion attack: an asymmetric token verified against the
		// HMAC secret as if the secret were the public key.
		if _, err := Verify(secret, token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Verify accepted an RS256 token (err = %v)", err)
		}
	})
}

func TestVerifyRejects(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		token  func(t *testing.T) string
		want   error
	}{
		{
			name: "empty token", secret: secret,
			token: func(*testing.T) string { return "" }, want: ErrNoToken,
		},
		{
			name: "whitespace token", secret: secret,
			token: func(*testing.T) string { return "   " }, want: ErrNoToken,
		},
		{
			name: "wrong secret", secret: secret,
			token: func(t *testing.T) string { return mint(t, "some-other-secret", goodClaims()) },
			want:  ErrInvalidToken,
		},
		{
			name: "garbage", secret: secret,
			token: func(*testing.T) string { return "not.a.jwt" }, want: ErrInvalidToken,
		},
		{
			name: "tampered payload", secret: secret,
			token: func(t *testing.T) string {
				parts := strings.Split(mint(t, secret, goodClaims()), ".")
				return parts[0] + ".eyJ1c2VyX2lkIjo5OTk5fQ." + parts[2]
			},
			want: ErrInvalidToken,
		},
		{
			// users.name and users.email are NOT NULL, and a token missing either
			// would leave a nameless row no admin screen can identify.
			name: "no email", secret: secret,
			token: func(t *testing.T) string {
				c := goodClaims()
				delete(c, "email")
				return mint(t, secret, c)
			},
			want: ErrInvalidToken,
		},
		{
			name: "no name", secret: secret,
			token: func(t *testing.T) string {
				c := goodClaims()
				delete(c, "name")
				return mint(t, secret, c)
			},
			want: ErrInvalidToken,
		},
		{
			name: "blank name", secret: secret,
			token: func(t *testing.T) string {
				c := goodClaims()
				c["name"] = "   "
				return mint(t, secret, c)
			},
			want: ErrInvalidToken,
		},
		{
			name: "no user_id", secret: secret,
			token: func(t *testing.T) string {
				c := goodClaims()
				delete(c, "user_id")
				return mint(t, secret, c)
			},
			want: ErrInvalidToken,
		},
		{
			// A standard exp is enforced by the library regardless of expired_at.
			name: "standard exp in the past", secret: secret,
			token: func(t *testing.T) string {
				c := goodClaims()
				c["exp"] = time.Now().Add(-time.Hour).Unix()
				return mint(t, secret, c)
			},
			want: ErrInvalidToken,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(tc.secret, tc.token(t)); !errors.Is(err, tc.want) {
				t.Fatalf("Verify error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestVerifyRefusesAnEmptySecret: a misconfigured AUTH_SECRET must never mean
// "verify nothing". config.validate refuses to boot without one; this is the
// second guard.
func TestVerifyRefusesAnEmptySecret(t *testing.T) {
	if _, err := Verify("", mint(t, secret, goodClaims())); err == nil {
		t.Fatal("Verify accepted a token against an empty secret")
	}
}

func TestClaimUserID(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want uint64
		err  bool
	}{
		// JSON numbers decode to float64, so the id arrives as a float even though
		// it is an integer key.
		{name: "json number", in: float64(19), want: 19},
		{name: "the seeded admin", in: float64(1), want: 1},
		{name: "int64", in: int64(42), want: 42},
		{name: "large but exact", in: float64(1 << 52), want: 1 << 52},

		{name: "zero", in: float64(0), err: true},
		{name: "negative", in: float64(-1), err: true},
		{name: "fractional is not truncated", in: float64(19.5), err: true},
		{name: "missing", in: nil, err: true},
		// Rejected rather than parsed: an id that arrives as a string means the
		// upstream contract changed, and guessing would mirror the wrong identity.
		{name: "string", in: "19", err: true},
		{name: "bool", in: true, err: true},
		{name: "object", in: map[string]any{}, err: true},
		{name: "too large for int64", in: float64(math.MaxInt64) * 2, err: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := claimUserID(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("claimUserID(%v) accepted the value, got %d", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("claimUserID(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("claimUserID(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestClaimTime pins every shape expired_at is accepted in.
//
// The wire format is still unconfirmed against a real Appskep token —
// authentication.md never states it, and Phase 3 could not reach dev-auth with a
// live session. Both plausible numeric shapes and three string layouts are
// accepted; anything else yields the zero time, which DISABLES the inline check
// rather than locking every user out or spinning the refresh endpoint on every
// request. The signature is verified regardless, so a zero here is safe.
func TestClaimTime(t *testing.T) {
	want := time.Date(2026, time.July, 27, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		in   any
		want time.Time
	}{
		{name: "unix seconds", in: float64(want.Unix()), want: want},
		{name: "unix milliseconds", in: float64(want.UnixMilli()), want: want},
		{name: "RFC3339", in: want.Format(time.RFC3339), want: want},
		{name: "MySQL datetime", in: "2026-07-27 10:30:00", want: want},
		{name: "ISO without a zone", in: "2026-07-27T10:30:00", want: want},

		// Every one of these disables the inline check.
		{name: "missing", in: nil},
		{name: "zero", in: float64(0)},
		{name: "negative", in: float64(-1)},
		{name: "unparseable string", in: "sometime next week"},
		{name: "empty string", in: ""},
		{name: "wrong type", in: true},
		{name: "date only", in: "2026-07-27"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claimTime(tc.in)
			if !got.Equal(tc.want) {
				t.Errorf("claimTime(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestClaimTimeMillisecondBoundary: the millisecond branch triggers above 1e12,
// which as seconds would be the year 33658 — far past any plausible token.
func TestClaimTimeMillisecondBoundary(t *testing.T) {
	// Just under the threshold: read as seconds.
	if got := claimTime(float64(1e12 - 1)); got.Year() < 30000 {
		t.Errorf("1e12-1 read as %v, want a second-based epoch far in the future", got)
	}
	// Just over: read as milliseconds, landing in 2001.
	if got := claimTime(float64(1e12 + 1)); got.Year() != 2001 {
		t.Errorf("1e12+1 read as %v, want it interpreted as milliseconds", got)
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{name: "an hour ago", at: now.Add(-time.Hour), want: true},
		{name: "a second ago", at: now.Add(-time.Second), want: true},
		{name: "an hour ahead", at: now.Add(time.Hour)},
		// Zero means "do not check inline" — never "expired", which would send
		// every request into the refresh endpoint.
		{name: "zero disables the check", at: time.Time{}},
		{name: "exactly now is not yet expired", at: now},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Claims{ExpiredAt: tc.at}
			if got := c.Expired(now); got != tc.want {
				t.Errorf("Expired() = %v, want %v", got, tc.want)
			}
		})
	}
}
