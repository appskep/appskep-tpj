// Package admin mounts the admin panel routes.
package admin

import (
	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/app/admin/handler"
)

// Routes builds the router mounted at /admin.
//
// RequireAdmin wraps the whole subtree, so every route added here is admin-only
// by construction rather than by each handler remembering to check.
func Routes(d *app.Deps) chi.Router {
	r := chi.NewRouter()

	r.Use(d.RequireAdmin)
	// After RequireAdmin, never before: the token it checks is the session secret
	// that middleware resolves into the request context.
	r.Use(d.CSRF)

	// Set on this router rather than inherited from the root, so a bad URL under
	// /admin renders with admin chrome instead of dropping the operator onto the
	// public site.
	r.NotFound(d.NotFound)
	r.MethodNotAllowed(d.MethodNotAllowed)

	dashboard := handler.NewDashboard(d)
	r.Get("/", dashboard.Get)

	// Layanan (services). Paths are Indonesian to match the rest of the UI; the
	// toggles post the value they want rather than asking for a flip, so a
	// resent request is idempotent.
	layanan := handler.NewLayanan(d)
	r.Route("/layanan", func(r chi.Router) {
		r.Get("/", layanan.List)
		r.Post("/", layanan.Create)
		r.Get("/baru", layanan.New)
		r.Get("/{id}/edit", layanan.Edit)
		r.Post("/{id}", layanan.Update)
		r.Post("/{id}/hapus", layanan.Delete)
		r.Post("/{id}/aktif", layanan.ToggleActive)
		r.Post("/{id}/segera", layanan.ToggleComingSoon)
	})

	// Penjadwalan (schedule slots). The generator posts to the same path twice —
	// once for the preview, once with `konfirmasi=1` to commit — so a preview can
	// never be mistaken for a write.
	jadwal := handler.NewJadwal(d)
	r.Route("/jadwal", func(r chi.Router) {
		r.Get("/", jadwal.Index)
		r.Post("/", jadwal.Create)
		r.Get("/baru", jadwal.New)
		r.Get("/generate", jadwal.GenerateForm)
		r.Post("/generate", jadwal.Generate)
		r.Post("/massal", jadwal.Bulk)
		r.Get("/{id}/edit", jadwal.Edit)
		r.Post("/{id}", jadwal.Update)
		r.Post("/{id}/hapus", jadwal.Delete)
		r.Post("/{id}/aktif", jadwal.ToggleActive)
	})

	// Booking. Every action posts to its own path and answers with a redirect;
	// "ekspor" is a GET because a CSV download is a navigation, not a change.
	// "/ekspor" is registered before "/{id}" so chi cannot read it as an id.
	booking := handler.NewBooking(d)
	r.Route("/booking", func(r chi.Router) {
		r.Get("/", booking.List)
		r.Get("/ekspor", booking.Export)
		r.Get("/{id}", booking.Detail)
		r.Post("/{id}/catatan", booking.UpdateNotes)
		r.Post("/{id}/konfirmasi", booking.Confirm)
		r.Post("/{id}/selesai", booking.Complete)
		r.Post("/{id}/batal", booking.Cancel)
		r.Post("/{id}/jadwalkan-ulang", booking.Reschedule)
	})

	// Pembayaran. Read-only apart from the re-sync, which asks Midtrans what
	// happened and feeds the same state machine the webhook does.
	pembayaran := handler.NewPembayaran(d)
	r.Route("/pembayaran", func(r chi.Router) {
		r.Get("/", pembayaran.List)
		r.Get("/ekspor", pembayaran.Export)
		r.Get("/{id}", pembayaran.Detail)
		r.Post("/{id}/sync", pembayaran.Sync)
	})

	// Pengguna. Identity is Appskep's; only role and is_active are writable here.
	users := handler.NewUsers(d)
	r.Route("/users", func(r chi.Router) {
		r.Get("/", users.List)
		r.Post("/{id}/aktif", users.ToggleActive)
		r.Post("/{id}/peran", users.ToggleRole)
	})

	pengaturan := handler.NewPengaturan(d)
	r.Get("/pengaturan", pengaturan.Edit)
	r.Post("/pengaturan", pengaturan.Update)

	return r
}
