// Package auth implements the Appskep SSO half of the system: verifying the
// JWT the auth service issues, keeping it in a signed cookie, refreshing it, and
// mirroring the identity it carries into the local users table.
//
// TPJ owns no credentials. There is no login form, no password check, and no
// user lookup for authentication — see PLAN.md R1/R2.
package auth

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Errors callers distinguish. Everything else is wrapped and treated the same:
// clear the session, send the user back to Appskep.
var (
	ErrNoToken      = errors.New("auth: no token")
	ErrInvalidToken = errors.New("auth: invalid token")
)

// Claims is the subset of the Appskep JWT this app uses.
type Claims struct {
	// UserID is the `user_id` claim: the identity key users.appskep_user_id
	// matches on.
	UserID uint64
	Email  string
	Name   string
	// ExpiredAt is the `expired_at` claim, zero when it is absent or in a shape
	// we do not recognise. Zero means "do not check inline" — the signature has
	// still been verified, and a standard `exp` claim, if the token carries one,
	// has already been enforced by the JWT library.
	ExpiredAt time.Time
	// Raw is the original token string, kept for the refresh call and for
	// re-storing the session. It is never rendered into HTML.
	Raw string
}

// Expired reports whether the inline expiry check says the token is stale.
func (c *Claims) Expired(now time.Time) bool {
	return !c.ExpiredAt.IsZero() && c.ExpiredAt.Before(now)
}

// Verify parses and validates an Appskep JWT against the shared HMAC secret.
//
// The signing method is pinned to HMAC. Without that pin, a token with
// `"alg":"none"` — or one signed with the public half of an asymmetric key —
// parses successfully and forges any identity it likes. This is the single most
// important line in the package.
func Verify(secret, token string) (*Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrNoToken
	}
	if secret == "" {
		return nil, errors.New("auth: AUTH_SECRET is empty")
	}

	parsed, err := jwt.Parse(token,
		func(t *jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims shape", ErrInvalidToken)
	}

	userID, err := claimUserID(mc["user_id"])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	c := &Claims{
		UserID:    userID,
		Email:     strings.TrimSpace(claimString(mc["email"])),
		Name:      strings.TrimSpace(claimString(mc["name"])),
		ExpiredAt: claimTime(mc["expired_at"]),
		Raw:       token,
	}

	// users.name and users.email are NOT NULL. A token missing either would make
	// the login upsert write empty strings and leave a nameless row that no admin
	// screen can identify, so it is rejected here instead.
	if c.Email == "" || c.Name == "" {
		return nil, fmt.Errorf("%w: token carries no name or email", ErrInvalidToken)
	}
	return c, nil
}

// claimUserID narrows the `user_id` claim. JSON numbers decode to float64, so
// the value arrives as a float even though it is an integer key — reject
// anything that is not a positive whole number rather than truncating it.
func claimUserID(v any) (uint64, error) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int64:
		f = float64(n)
	case string:
		return 0, fmt.Errorf("user_id is a string (%q), expected a number", n)
	case nil:
		return 0, errors.New("user_id claim is missing")
	default:
		return 0, fmt.Errorf("user_id claim has unexpected type %T", v)
	}

	if f <= 0 || f != math.Trunc(f) || f > math.MaxInt64 {
		return 0, fmt.Errorf("user_id claim is not a positive integer (%v)", f)
	}
	return uint64(f), nil
}

func claimString(v any) string {
	s, _ := v.(string)
	return s
}

// claimTime reads `expired_at`, whose wire format is not documented in
// authentication.md — the reference app only ever compares it to now. Both
// plausible shapes are accepted; an unrecognised one yields the zero time, which
// disables the inline expiry check rather than locking every user out or
// spinning the refresh endpoint on every request.
func claimTime(v any) time.Time {
	switch t := v.(type) {
	case float64:
		if t <= 0 {
			return time.Time{}
		}
		// Milliseconds if the value is far past any plausible second-based epoch
		// (1e12 seconds is the year 33658).
		if t > 1e12 {
			return time.UnixMilli(int64(t))
		}
		return time.Unix(int64(t), 0)
	case string:
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}
