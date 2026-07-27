// Package middleware holds cross-cutting HTTP middleware shared by the public,
// admin, and api routers.
package middleware

import (
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// Logger emits one structured line per request, carrying the chi request ID so
// a log line can be tied back to a response header.
//
// It logs r.URL.Path and never r.URL.RawQuery, and that is load-bearing rather
// than incidental: the Appskep SSO callback arrives as ?access_token=<JWT>, so
// logging the full URL would write a live credential into the access log of
// every request that completes a sign-in.
//
// The client address comes from ClientIP with the same trusted-proxy list the
// rate limiter uses, so a log line and a limit decision always name the same
// caller.
func Logger(log *slog.Logger, trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			defer func() {
				log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
					slog.String("request_id", middleware.GetReqID(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", ww.Status()),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", time.Since(start)),
					slog.String("remote", ClientIP(r, trusted)),
				)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
