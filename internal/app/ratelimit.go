package app

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"

	appmw "github.com/remorac/appskep-tpj/internal/shared/middleware"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// RateLimit refuses a caller who is asking too often.
//
// It is applied per route rather than globally: the routes worth limiting are the
// ones that cost something on the way through — the booking commit takes a slot
// lock, and both payment actions call Midtrans — and a limit on ordinary page
// views would only get in the way of a family sharing an office NAT.
//
// Keyed on the resolved client IP rather than the user, because the point is to
// bound what an unknown caller can spend before their identity means anything,
// and because the webhook has no user at all.
//
// Lives here rather than in shared/middleware for the usual reason: refusing
// means rendering the styled page, and that needs Deps.
func (d *Deps) RateLimit(l *appmw.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := appmw.ClientIP(r, d.Cfg.Server.TrustedProxies)

			ok, retry := l.Allow(key)
			if ok {
				next.ServeHTTP(w, r)
				return
			}

			d.Log.WarnContext(r.Context(), "rate limit exceeded",
				slog.String("request_id", chimw.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client", key),
			)

			seconds := int(retry.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))

			if strings.HasPrefix(r.URL.Path, "/api") {
				util.JSON(w, http.StatusTooManyRequests,
					map[string]string{"error": "too many requests"})
				return
			}
			d.ErrorPage(w, r, http.StatusTooManyRequests)
		})
	}
}
