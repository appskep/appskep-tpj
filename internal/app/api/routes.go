// Package api mounts the internal JSON routes: the health check under /api and
// the Midtrans webhook under /midtrans. These routes are not part of the public
// HTML site — they answer JSON to a program, never a rendered page.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/app/api/handler"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Routes builds the router mounted at /api.
func Routes(d *app.Deps) chi.Router {
	r := chi.NewRouter()

	health := handler.NewHealth(d.Store)
	r.Get("/health", health.Get)

	jsonErrors(r)
	return r
}

// WebhookRoutes builds the router mounted at /midtrans, carrying the single
// route POST /midtrans/notification.
//
// It is a second router rather than part of Routes only because it hangs off the
// root instead of /api. What matters is identical and load-bearing for both:
// public and ungated by design — Midtrans holds no credential of ours, so the
// SHA512 signature is the whole authentication. Neither router takes the auth
// middleware nor d.CSRF, and neither may: a cross-site POST with no session is
// exactly what a webhook is. That is also why this cannot live in public.Routes,
// which wraps everything in OptionalAuth and d.CSRF.
func WebhookRoutes(d *app.Deps) chi.Router {
	r := chi.NewRouter()

	midtrans := handler.NewMidtrans(d)
	// The one bound on an ungated public endpoint. Deliberately generous: Midtrans
	// retries a notification it did not see acknowledged, and refusing one means a
	// customer who paid stays unpaid on the site.
	r.With(d.RateLimit(d.Limits.Webhook)).Post("/notification", midtrans.Notify)

	jsonErrors(r)
	return r
}

// jsonErrors installs the not-found and method-not-allowed responders shared by
// both routers: JSON, not the styled HTML error pages the site uses, because the
// caller is a program that wants a parseable body.
func jsonErrors(r chi.Router) {
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		util.JSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		util.JSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	})
}
