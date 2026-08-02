package app

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
)

// The auth middlewares live here rather than in shared/middleware, which is
// where PLAN.md sketched them, for the same reason Recoverer does: denying a
// request means rendering the styled 403, and that needs Deps. shared/middleware
// cannot import internal/app — internal/app already imports it.

// OptionalAuth resolves the session when there is one and lets the request
// through either way. For public pages that render differently when signed in.
func (d *Deps) OptionalAuth(next http.Handler) http.Handler {
	return d.authenticate(next, denyAnonymous)
}

// RequireAuth admits any authenticated Appskep user, per PLAN.md Q7.
func (d *Deps) RequireAuth(next http.Handler) http.Handler {
	return d.authenticate(next, denyRedirect)
}

// RequireAdmin admits only local role='admin' users, per PLAN.md Q1. It wraps
// the whole /admin subtree, so an admin route is admin-only by construction
// rather than by each handler remembering to check.
func (d *Deps) RequireAdmin(next http.Handler) http.Handler {
	return d.authenticate(next, denyForbidden)
}

// denyMode is what a middleware does when the request turns out not to be
// authenticated (or not authorised).
type denyMode int

const (
	// denyAnonymous continues without a user.
	denyAnonymous denyMode = iota
	// denyRedirect bounces to Appskep SSO.
	denyRedirect
	// denyForbidden bounces to SSO when anonymous, and renders 403 when the
	// user is signed in but is not an admin.
	denyForbidden
)

// authenticate is the single gate all three middlewares share.
//
// Every path that denies a request writes its response and returns immediately.
// The reference app's middleware calls Abort() and falls through to the rest of
// its body, which is how a denied request still triggered a token refresh and a
// log line — see authentication.md §Security notes.
func (d *Deps) authenticate(next http.Handler, mode denyMode) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. The SSO callback. Appskep sends the browser back to whatever URL we
		//    passed as client_base_url, with the token in the query string.
		if token := r.URL.Query().Get("access_token"); token != "" {
			d.consumeCallback(w, r, token)
			return
		}

		sess := d.Session.Load(r)
		if sess.Token == "" {
			d.deny(w, r, next, mode, nil, sess)
			return
		}

		claims, err := d.Auth.Verify(sess.Token)
		if err != nil {
			d.Log.DebugContext(r.Context(), "auth: session token rejected", slog.Any("error", err))
			d.Session.Clear(w)
			d.deny(w, r, next, mode, nil, auth.Session{})
			return
		}

		// 2. Inline refresh. The Appskep token carries its own expired_at and the
		//    auth service refreshes it on demand; there is no refresh token to
		//    keep, so this is a blocking call on the request path, bounded by a
		//    timeout in the refresher.
		if claims.Expired(time.Now().In(d.Cfg.App.Location)) {
			refreshed, rerr := d.Auth.Refresh(r.Context(), sess.Token)
			if rerr != nil {
				d.Log.InfoContext(r.Context(), "auth: token refresh failed", slog.Any("error", rerr))
				d.Session.Clear(w)
				d.deny(w, r, next, mode, nil, auth.Session{})
				return
			}
			claims = refreshed
			sess.Token = claims.Raw
			if serr := d.Session.Save(w, sess); serr != nil {
				d.Log.ErrorContext(r.Context(), "auth: saving refreshed session", slog.Any("error", serr))
			}
		}

		// 3. The local mirror. Read every request so a demotion or deactivation
		//    takes effect on the next click.
		//
		// Both failures here end the session, so both say why. Clearing and falling
		// through to deny renders an ordinary signed-out page under OptionalAuth,
		// which is indistinguishable from never having logged in — the visitor
		// re-runs the SSO round trip forever and the only evidence is a log line.
		// For a deactivated user that silence is permanent: UpsertUserFromSSO
		// deliberately leaves is_active alone on its ON DUPLICATE KEY branch, so
		// every future login re-lands on this same branch.
		//
		// SaveFlashAndRedirect rather than Clear-then-Save: two Set-Cookie headers
		// for one name is order-dependent, and this helper replaces the session in a
		// single write. It is documented for exactly this — a request that is ending
		// a session. The destination is "/" and never SSO, for the reason deny
		// already applies to a non-admin: logging in again changes nothing.
		user, err := d.Auth.LoadUser(r.Context(), claims.UserID)
		if err != nil {
			d.Log.WarnContext(r.Context(), "auth: loading local user",
				slog.Uint64("appskep_user_id", claims.UserID), slog.Any("error", err))
			d.SaveFlashAndRedirect(w, r, "/",
				model.FlashError("Sesi Anda berakhir. Silakan masuk lagi."))
			return
		}
		if !user.IsActive {
			d.Log.InfoContext(r.Context(), "auth: refusing deactivated user",
				slog.Int64("user_id", user.ID))
			d.SaveFlashAndRedirect(w, r, "/",
				model.FlashError("Akun Anda dinonaktifkan. Hubungi admin TPJ."))
			return
		}

		if mode == denyForbidden && !user.IsAdmin() {
			d.deny(w, r, next, mode, &user, sess)
			return
		}

		req := d.withSession(w, r, &user, sess)
		if req == nil {
			// withSession already wrote an error page.
			return
		}
		next.ServeHTTP(w, req)
	})
}

// consumeCallback handles the redirect back from Appskep.
//
// The token arrives as a query parameter, so it is in the browser's history, in
// any Referer the page emits, and in the access log of anything in front of this
// server. It cannot be removed from the inbound request, but it must not survive
// it: the response is always a redirect to a clean path.
func (d *Deps) consumeCallback(w http.ResponseWriter, r *http.Request, token string) {
	dest := safePath(r.URL.Query().Get("pass"))

	claims, err := d.Auth.Verify(token)
	if err != nil {
		// Deliberately not a redirect back to Appskep. A token that fails
		// verification means the shared AUTH_SECRET is wrong, and bouncing to SSO
		// would produce another bad token and another bounce — the redirect loop
		// authentication.md's troubleshooting section describes. Landing on the
		// public home page with a message ends the loop and is diagnosable.
		d.Log.WarnContext(r.Context(), "auth: SSO callback token rejected", slog.Any("error", err))
		d.SaveFlashAndRedirect(w, r, "/",
			model.FlashError("Proses masuk gagal. Silakan coba lagi."))
		return
	}

	user, err := d.Auth.SyncUser(r.Context(), claims)
	if err != nil {
		d.Log.ErrorContext(r.Context(), "auth: syncing user from SSO", slog.Any("error", err))
		d.Session.Clear(w)
		d.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// The session carries the token and nothing else. Name and email come back
	// from the claims on every request, so they are not forgeable by editing a
	// cookie even if the signing key ever leaked.
	if err := d.Session.Save(w, auth.Session{Token: claims.Raw}); err != nil {
		d.Log.ErrorContext(r.Context(), "auth: saving session", slog.Any("error", err))
		d.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	d.Log.InfoContext(r.Context(), "auth: signed in",
		slog.Int64("user_id", user.ID),
		slog.Uint64("appskep_user_id", user.AppskepUserID),
		slog.String("role", user.Role),
	)

	// no-store so the redirect carrying a freshly-minted session is never held by
	// a shared cache.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// deny ends the request for an unauthenticated or unauthorised visitor.
//
// Under OptionalAuth there is nothing to deny: the page is public and simply
// renders signed-out, so the request continues to next with no user in context.
func (d *Deps) deny(w http.ResponseWriter, r *http.Request, next http.Handler, mode denyMode, user *model.User, sess auth.Session) {
	switch mode {
	case denyAnonymous:
		// Never nil: withSession only fails while minting a CSRF secret, and it
		// mints one only for a signed-in user.
		next.ServeHTTP(w, d.withSession(w, r, nil, sess))
	case denyForbidden:
		if user != nil {
			// Signed in, just not an admin. Sending them to SSO would be a loop:
			// logging in again changes nothing.
			d.ErrorPage(w, r, http.StatusForbidden)
			return
		}
		fallthrough
	default:
		http.Redirect(w, r, d.Cfg.Auth.LoginURL(d.absURL(r)), http.StatusFound)
	}
}

// withSession attaches the user, the CSRF secret and any pending flash to the
// request context, consuming the flash so it is shown exactly once.
//
// The cookie is written at most once here, no matter how many of those reasons
// apply. Saving twice would be harmless; re-loading the session between them
// would not be, and that is the trap FlashRedirect documents below — Session.Load
// reads the *request* cookie, which still carries the flash this call just
// consumed, so a second load would resurrect it.
func (d *Deps) withSession(w http.ResponseWriter, r *http.Request, user *model.User, sess auth.Session) *http.Request {
	ctx := r.Context()
	dirty := false

	if user != nil {
		ctx = auth.WithUser(ctx, user)

		// The CSRF secret is minted only for a signed-in visitor. Every
		// state-changing route in the app sits behind RequireAuth or RequireAdmin,
		// so an anonymous token would protect nothing — and minting one would set a
		// cookie on every crawler hit and every anonymous landing-page view.
		if sess.CSRF == "" {
			secret, err := auth.NewCSRFSecret()
			if err != nil {
				// crypto/rand failing is not a condition to serve through: every
				// form rendered from here on would carry no token and every POST
				// would be rejected.
				d.Log.ErrorContext(ctx, "auth: minting CSRF secret", slog.Any("error", err))
				d.ErrorPage(w, r, http.StatusInternalServerError)
				return nil
			}
			sess.CSRF = secret
			dirty = true
		}
		ctx = auth.WithCSRF(ctx, sess.CSRF)
	}

	if len(sess.Flash) > 0 {
		ctx = auth.WithFlash(ctx, sess.Flash)
		sess.Flash = nil
		dirty = true
	}

	if dirty {
		if err := d.Session.Save(w, sess); err != nil {
			d.Log.ErrorContext(ctx, "auth: saving session", slog.Any("error", err))
		}
	}
	return r.WithContext(ctx)
}

// SaveFlashAndRedirect replaces the session with one carrying only the given
// messages and redirects. Used wherever a request ends in a redirect that needs
// to say something on arrival — a failed sign-in, a completed logout.
//
// It drops any existing session on purpose: both current callers are ending a
// session, and a flash helper that preserved a token would be the wrong tool for
// either of them.
func (d *Deps) SaveFlashAndRedirect(w http.ResponseWriter, r *http.Request, dest string, flash ...model.Flash) {
	if err := d.Session.Save(w, auth.Session{Flash: flash}); err != nil {
		d.Log.ErrorContext(r.Context(), "auth: saving flash", slog.Any("error", err))
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, safePath(dest), http.StatusSeeOther)
}

// FlashRedirect redirects with a message while keeping the caller signed in.
//
// The counterpart to SaveFlashAndRedirect, which discards the session on
// purpose: that one is for requests that are ending a session, this one is for a
// completed action inside an authenticated flow. Using the wrong one after
// "Layanan berhasil disimpan" would log the admin out on every save.
//
// The flash is replaced rather than appended. withSession has already consumed
// whatever was pending and re-saved the cookie, but Session.Load reads the
// *request* cookie, which still carries the consumed message — appending would
// show it a second time.
func (d *Deps) FlashRedirect(w http.ResponseWriter, r *http.Request, dest string, flash ...model.Flash) {
	sess := d.Session.Load(r)
	sess.Flash = flash

	if err := d.Session.Save(w, sess); err != nil {
		d.Log.ErrorContext(r.Context(), "auth: saving flash", slog.Any("error", err))
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, safePath(dest), http.StatusSeeOther)
}

// absURL is the absolute URL of the current request, which Appskep needs as
// client_base_url so it knows where to send the token back to.
//
// Built from APP_URL rather than from r.Host and X-Forwarded-Proto: those are
// attacker-controlled, and this value decides where a valid token gets
// delivered. main.go rejects chi's RealIP for the same reason.
func (d *Deps) absURL(r *http.Request) string {
	return d.Cfg.App.URL + r.URL.RequestURI()
}

// safePath sanitises a caller-supplied redirect target. Anything that is not a
// plain site-relative path becomes "/", so neither the `pass` parameter nor a
// `next` query can bounce a freshly-authenticated user off-site.
func safePath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "/"
	}
	// "//evil.example" and "/\evil.example" are both read as protocol-relative
	// URLs by browsers, so neither is site-relative despite the leading slash.
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}
