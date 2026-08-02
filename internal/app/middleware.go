package app

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Recoverer turns a panic into the styled 500 page.
//
// It replaces chi's Recoverer, which can only produce a bare "Internal Server
// Error" string — a user who hits a bug should still land on a page that looks
// like the site and tells them what to do next. It also lives here rather than in
// shared/middleware because rendering that page needs Deps.
func (d *Deps) Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww, ok := w.(chimw.WrapResponseWriter)
		if !ok {
			ww = chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		}

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			// http.ErrAbortHandler is the documented way for a handler to abort
			// without being reported. Passing it on lets the server suppress it.
			if err, isErr := rec.(error); isErr && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}

			d.Log.ErrorContext(r.Context(), "panic recovered",
				slog.String("request_id", chimw.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)

			// The handler may already have written a status and part of a body.
			// Rendering an error page on top would append a second document to a
			// response the client is already reading as successful.
			if ww.Status() != 0 || ww.BytesWritten() > 0 {
				return
			}

			// Both JSON mounts: /api and the Midtrans webhook at /midtrans. The
			// same pair is listed in middleware.SecureHeaders and the two must agree
			// — shared/middleware cannot import this package to share a constant.
			if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/midtrans") {
				util.JSON(ww, http.StatusInternalServerError,
					map[string]string{"error": "internal server error"})
				return
			}

			d.ErrorPage(ww, r, http.StatusInternalServerError)
		}()

		next.ServeHTTP(ww, r)
	})
}

// StaticHandler serves static/ at /static/, with cache headers.
//
// There is no fingerprinting build step, so cacheability comes from the asset
// helper appending ?v=<stamp> to every URL a template emits. That makes a long
// immutable max-age safe for the build outputs: a changed app.css arrives under a
// new query string and is fetched fresh.
//
// Uploads are excluded from that: their filenames are random but their content is
// replaceable from the admin panel, and they are never referenced through the
// asset helper, so they get a short max-age instead.
func (d *Deps) StaticHandler() http.Handler {
	fileServer := http.FileServer(http.Dir("static"))

	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case d.Cfg.IsDevelopment():
			// Caching in development means editing input.css and seeing nothing
			// change, which reads as a broken build.
			w.Header().Set("Cache-Control", "no-store")
		case strings.HasPrefix(r.URL.Path, "uploads/"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
			// User-supplied bytes, served from our own origin. The upload store
			// only ever writes .jpg/.png/.webp — the extension comes from the
			// sniffed magic bytes, never the filename — so this cannot execute
			// today. It is here for the day someone adds image/svg+xml to the
			// whitelist, which would otherwise be a stored-XSS on our own origin.
			w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		default:
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		// Uploads are user-supplied. Sniffing is what turns a file that claims to
		// be a jpg into executable script in the browser.
		w.Header().Set("X-Content-Type-Options", "nosniff")

		fileServer.ServeHTTP(w, r)
	}))
}
