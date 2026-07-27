// Package public mounts the customer-facing website routes.
package public

import (
	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/app/public/handler"
)

// Routes builds the router mounted at /.
//
// OptionalAuth wraps everything: these pages stay reachable anonymously but
// render differently when signed in, and the SSO callback can land on any of
// them because client_base_url is whatever URL the visitor was refused at.
// Routes that genuinely require a user (booking, riwayat, profil in Phases 7
// and 9) take d.RequireAuth on their own group.
func Routes(d *app.Deps) chi.Router {
	r := chi.NewRouter()

	r.Use(d.OptionalAuth)
	// After OptionalAuth, never before: the token it checks is the session secret
	// that middleware resolves into the request context.
	r.Use(d.CSRF)

	// This router is mounted at /, so it catches every path no other subsystem
	// claimed — which makes its NotFound the site-wide 404.
	r.NotFound(d.NotFound)
	r.MethodNotAllowed(d.MethodNotAllowed)

	page := handler.NewPage(d)
	r.Get("/", page.Landing)

	layanan := handler.NewLayanan(d)
	r.Get("/layanan", layanan.List)
	r.Get("/layanan/{slug}", layanan.Detail)

	session := handler.NewSession(d)
	r.Get("/login", session.Login)
	// POST, not GET: a GET that ends a session is triggerable by any cross-site
	// <img src="/logout">. It carries a CSRF token like every other action.
	r.Post("/logout", session.Logout)

	// The signed-in half of the public site. RequireAuth bounces an anonymous
	// visitor to Appskep with client_base_url set to the URL they were refused
	// at, so the callback lands them back on the same booking step — no next=
	// parameter to sanitise and nothing to lose on the round trip.
	r.Group(func(r chi.Router) {
		r.Use(d.RequireAuth)

		booking := handler.NewBooking(d)
		r.Get("/booking", booking.Form)
		// Rate limited on the POST only: the GET renders the picker and costs
		// nothing, while the POST takes a slot lock inside a transaction.
		r.With(d.RateLimit(d.Limits.Booking)).Post("/booking", booking.Submit)

		// The payment page and its actions. POST shares the GET's URL, as the
		// booking flow does: it answers with a redirect to Midtrans, so there is
		// nothing for Turbo to discard.
		r.Get("/booking/{code}/pembayaran", booking.Payment)
		r.Get("/booking/{code}/status", booking.Status)
		// Both of these call Midtrans, so they share one budget: the point is to
		// bound calls to somebody else's API, not to ration one button.
		r.Group(func(r chi.Router) {
			r.Use(d.RateLimit(d.Limits.Payment))
			r.Post("/booking/{code}/pembayaran", booking.Pay)
			r.Post("/booking/{code}/periksa", booking.Check)
		})

		// Konfirmasi is the canonical page for a booking in ANY status, so it is
		// what riwayat links to and where Midtrans returns the customer. The
		// pembayaran page above is the pending-only action page and redirects
		// here once there is nothing left to pay.
		r.Get("/booking/{code}/konfirmasi", booking.Konfirmasi)
		r.Post("/booking/{code}/batal", booking.Cancel)

		riwayat := handler.NewRiwayat(d)
		r.Get("/riwayat", riwayat.List)

		profil := handler.NewProfil(d)
		r.Get("/profil", profil.Show)
		r.Post("/profil", profil.Save)
	})

	return r
}
