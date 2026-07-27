// Package api mounts the internal JSON routes: health checks and the Midtrans
// webhook. These routes are not part of the public HTML site.
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

	// The Midtrans payment webhook. Public and ungated by design — Midtrans holds
	// no credential of ours, so the SHA512 signature is the whole authentication.
	// This router takes neither the auth middleware nor d.CSRF, and must not: a
	// cross-site POST with no session is exactly what a webhook is.
	midtrans := handler.NewMidtrans(d)
	// The one bound on an ungated public endpoint. Deliberately generous: Midtrans
	// retries a notification it did not see acknowledged, and refusing one means a
	// customer who paid stays unpaid on the site.
	r.With(d.RateLimit(d.Limits.Webhook)).Post("/webhook/midtrans", midtrans.Notify)

	// JSON, not the styled HTML error pages the site uses: a client calling /api
	// wants a parseable body.
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		util.JSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		util.JSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	})

	return r
}
