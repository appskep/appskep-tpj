package auth

import (
	"context"

	"github.com/remorac/appskep-tpj/internal/shared/model"
)

// ctxKey is unexported so nothing outside this package can collide with these
// keys or overwrite the authenticated user.
type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeyFlash
	ctxKeyCSRF
	ctxKeyToken
)

// WithUser attaches the signed-in user. Only the auth middleware calls this.
func WithUser(ctx context.Context, u *model.User) context.Context {
	return context.WithValue(ctx, ctxKeyUser, u)
}

// WithToken attaches the raw Appskep JWT so a handler can present it as the
// bearer token when editing the user's Appskep account. Only the auth
// middleware calls this, and only for a signed-in user.
//
// The token is never rendered into HTML — it stays server-side, exactly as the
// session cookie does, and reaches only the auth service it came from.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, ctxKeyToken, token)
}

// TokenFrom returns the raw JWT for this request, or "" when anonymous.
func TokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(ctxKeyToken).(string)
	return t
}

// UserFrom returns the signed-in user, or nil on an anonymous request.
//
// It returns a nil pointer rather than panicking on a missing value: public
// pages run under OptionalAuth and legitimately have no user, and the renderer
// calls this on every single render.
func UserFrom(ctx context.Context) *model.User {
	u, _ := ctx.Value(ctxKeyUser).(*model.User)
	return u
}

// WithFlash attaches the flash messages popped from the session.
func WithFlash(ctx context.Context, f []model.Flash) context.Context {
	return context.WithValue(ctx, ctxKeyFlash, f)
}

// FlashFrom returns the flash messages for this request, if any.
func FlashFrom(ctx context.Context) []model.Flash {
	f, _ := ctx.Value(ctxKeyFlash).([]model.Flash)
	return f
}
