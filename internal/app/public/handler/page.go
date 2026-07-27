// Package handler holds the public website handlers.
package handler

import (
	"net/http"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Page serves the public content pages.
type Page struct {
	deps *app.Deps
}

func NewPage(deps *app.Deps) *Page {
	return &Page{deps: deps}
}

// landingData is the landing page payload.
type landingData struct {
	Services []sqlc.Service
	// Aches is the hero's list of complaints. Each row links to the layanan that
	// treats it, so the diagnosis doubles as navigation.
	Aches []achePoint
	Steps []bookingStep
	FAQs  []faq
	Terms string
	// EmptyServices is shown when every layanan has been deactivated from the
	// admin panel. Reachable, so it gets real copy rather than a blank section.
	EmptyServices emptyState
}

// emptyState is the payload for the empty partial.
type emptyState struct {
	Icon        string
	Title       string
	Body        string
	ActionLabel string
	ActionHref  string
}

// achePoint is one entry in the hero's ache list: a body part, the complaint, and
// the layanan that treats it.
//
// These are laid out in normal flow rather than pinned to coordinates on the
// illustration. Absolute positioning was tried first and was the wrong call: the
// labels collided with each other and with the figure, and the entrance animation
// briefly hid what is the hero's primary navigation.
type achePoint struct {
	Label string
	Note  string
	Href  string
}

type bookingStep struct {
	Icon  string
	Title string
	Body  string
}

type faq struct {
	Q string
	A string
}

// Landing renders the public home page.
func (h *Page) Landing(w http.ResponseWriter, r *http.Request) {
	services, err := h.deps.Catalog.ListActive(r.Context())
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "landing: listing services", "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// Each ache points at whichever layanan exists, in sort order, so the list
	// keeps working when the admin adds, reorders or hides a service.
	aches := []achePoint{
		{Label: "Leher & pundak", Note: "Kaku setelah seharian menunduk ke layar."},
		{Label: "Punggung bawah", Note: "Ngilu tiap bangun dari kursi."},
		{Label: "Kaki & lutut", Note: "Pegal padahal cuma jalan ke parkiran."},
	}
	for i := range aches {
		if i < len(services) {
			aches[i].Href = "/layanan/" + services[i].Slug
		} else {
			aches[i].Href = "/layanan"
		}
	}

	data := landingData{
		Services: services,
		Aches:    aches,
		Steps: []bookingStep{
			{Icon: "sparkles", Title: "Pilih layanan", Body: "Urut, massage, atau bekam therapeutic. Harga dan durasinya tertulis jelas."},
			{Icon: "calendar-days", Title: "Pilih jadwal", Body: "Lihat slot yang masih kosong dan ambil yang paling pas."},
			{Icon: "credit-card", Title: "Bayar online", Body: "Transfer, QRIS, atau kartu. Slot kamu ditahan sampai pembayaran selesai."},
			{Icon: "check-circle", Title: "Datang terapi", Body: "Bawa kode booking. Hadir 10 menit sebelum jadwal."},
		},
		FAQs: []faq{
			{
				Q: "Berapa lama satu sesi terapi?",
				A: "Berbeda-beda per layanan. Durasinya tertulis di tiap kartu layanan dan di halaman layanannya, termasuk konsultasi singkat di awal.",
			},
			{
				Q: "Apakah harus bayar di muka?",
				A: "Ya. Pembayaran online mengunci slot kamu — tanpa itu slot kembali tersedia untuk orang lain setelah satu jam.",
			},
			{
				Q: "Bagaimana kalau saya perlu mengubah jadwal?",
				A: "Hubungi kami lewat WhatsApp sebelum jadwal berjalan. Perubahan jadwal dibantu oleh pengelola.",
			},
			{
				Q: "Bekam basah itu bagaimana?",
				A: "Bekam basah dilakukan oleh terapis terlatih dengan alat sekali pakai, dan hanya di layanan Bekam Therapeutic.",
			},
		},
		Terms: h.deps.Settings.String(service.KeyBookingTerms, ""),
		EmptyServices: emptyState{
			Icon:  "sparkles",
			Title: "Belum ada layanan aktif",
			Body:  "Daftar layanan sedang diperbarui. Tanya jadwal langsung lewat WhatsApp sementara ini.",
		},
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/landing", &view.View{
		Page: view.Page{
			Title:       "Terapi buat badan yang ngaku masih muda",
			Description: h.deps.Settings.String(service.KeySiteDescription, ""),
		},
		Data: data,
	})
}
