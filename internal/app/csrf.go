package app

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// csrfFieldName is the form field csrfField emits.
const csrfFieldName = "_csrf"

// csrfMaxMemory bounds what a multipart body holds in memory while the token is
// read out of it. The rest of the body is still bounded — middleware.MaxBody
// wraps r.Body at the root, ahead of this — so this only decides where the bytes
// sit, not how many there can be.
const csrfMaxMemory = 1 << 20

// CSRF rejects a state-changing request that did not come from one of our own
// pages.
//
// It lives here rather than in shared/middleware for the same reason the auth
// gates do: refusing means rendering the styled 403, and that needs Deps.
//
// Applied per subsystem, immediately after the auth middleware, because the
// token is the session secret that middleware put in the request context. The
// /api router is deliberately not wrapped: the Midtrans webhook *is* a cross-site
// POST, and its SHA512 signature is its authentication.
func (d *Deps) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		status, reason := d.csrfReject(r)
		if status != 0 {
			d.Log.InfoContext(r.Context(), "csrf: request rejected",
				slog.String("request_id", chimw.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("reason", reason),
			)
			if strings.HasPrefix(r.URL.Path, "/api") {
				util.JSON(w, status, map[string]string{"error": reason})
				return
			}
			d.ErrorPage(w, r, status)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// safeMethod reports whether the method is defined as having no side effects,
// and so needs no token. Every route in this app that changes state is a POST.
func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// csrfReject returns the status to refuse a request with and why, or (0, "") to
// admit it. The reason is for the log; the user sees only the error page.
func (d *Deps) csrfReject(r *http.Request) (int, string) {
	// The origin check comes first because it costs nothing and needs no body. A
	// browser sends Origin on every POST, so a forged cross-site submission is
	// usually refused here without the request ever being parsed.
	if !d.originAllowed(r) {
		return http.StatusForbidden, "origin mismatch"
	}

	secret := auth.CSRFFrom(r.Context())
	if secret == "" {
		// No session secret means no signed-in user. Every state-changing route is
		// behind RequireAuth or RequireAdmin, so this is only reachable if one is
		// ever mounted without a gate — refusing is the safe answer to that.
		return http.StatusForbidden, "no session secret"
	}

	// Parsed explicitly rather than through PostFormValue, which discards the
	// error. Without this a body that blew the MaxBody cap reaches the token check
	// as an empty form and is reported as a CSRF failure — telling someone who
	// uploaded a large file that they lack permission.
	//
	// Reading the body here means a multipart upload is parsed before the
	// handler's own http.MaxBytesReader can narrow it. The root-level MaxBody
	// keeps that bounded; the per-store caps (2 MB service images, 1 MB avatars)
	// still hold because upload.ImageStore.Save enforces them itself on fh.Size
	// and through its own io.LimitReader. An oversized image is still a 422 with
	// its own message — only the point at which it is caught moves.
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		err = r.ParseMultipartForm(csrfMaxMemory)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge, "request body over the limit"
		}
		return http.StatusBadRequest, "unreadable request body"
	}

	if !auth.VerifyToken(secret, r.PostFormValue(csrfFieldName)) {
		return http.StatusForbidden, "token missing or invalid"
	}
	return 0, ""
}

// originAllowed checks the request's origin against this site.
//
// Origin is preferred; Referer is the fallback for the browsers and privacy
// settings that omit it. A request carrying neither is admitted on the token
// alone — that is the no-JS, no-header case, and the token is the real control
// here. This check is defence in depth on top of it.
//
// Both APP_URL's host and the request's own Host count as ours. Accepting Host
// too is what stops a machine browsing 127.0.0.1 while APP_URL says localhost
// from refusing every form it renders; an attacker's origin matches neither.
func (d *Deps) originAllowed(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" || raw == "null" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return true
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}

	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if app, err := url.Parse(d.Cfg.App.URL); err == nil && app.Host != "" {
		return strings.EqualFold(u.Host, app.Host)
	}
	return false
}
