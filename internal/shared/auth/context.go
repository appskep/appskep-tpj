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
)

// WithUser attaches the signed-in user. Only the auth middleware calls this.
func WithUser(ctx context.Context, u *model.User) context.Context {
	return context.WithValue(ctx, ctxKeyUser, u)
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
