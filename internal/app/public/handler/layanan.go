package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/util"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Layanan serves the public catalogue: the grid and one service's detail page.
type Layanan struct {
	deps *app.Deps
}

func NewLayanan(deps *app.Deps) *Layanan {
	return &Layanan{deps: deps}
}

// upcomingDays is how many dates the detail page previews. Three fits one row on
// a phone and is enough to answer "can I come this week" without turning the page
// into the booking form — that is Phase 7's.
const upcomingDays = 3

// listData is the /layanan payload.
type listData struct {
	Services []sqlc.Service
	Empty    emptyState
}

// detailData is the /layanan/{slug} payload.
type detailData struct {
	Service sqlc.Service
	// Paragraphs is the description split on blank lines. Split in the handler
	// rather than the template only because the template also needs to know
	// whether there is any description at all.
	Paragraphs []string
	// Upcoming is empty for a coming-soon service, and also when the schedule read
	// failed — the section simply does not render. See Detail.
	Upcoming []service.UpcomingDay
	// Others is every other active service, for the strip at the foot of the page.
	Others []sqlc.Service
}

// List renders the layanan grid.
func (h *Layanan) List(w http.ResponseWriter, r *http.Request) {
	services, err := h.deps.Catalog.ListActive(r.Context())
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "layanan: listing services", "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/layanan", &view.View{
		Page: view.Page{
			Title:       "Layanan",
			Description: "Urut, massage, dan bekam therapeutic. Harga, durasi, dan jadwalnya tertulis jelas — pilih yang paling pas untuk keluhanmu.",
		},
		Data: listData{
			Services: services,
			Empty: emptyState{
				Icon:  "sparkles",
				Title: "Belum ada layanan aktif",
				Body:  "Daftar layanan sedang diperbarui. Tanya jadwal langsung lewat WhatsApp sementara ini.",
			},
		},
	})
}

// Detail renders one service.
//
// An inactive or unknown slug is a 404, not an empty page: GetActiveBySlug
// carries the is_active predicate, so deactivating a service in the admin panel
// takes its public URL down on the next request.
func (h *Layanan) Detail(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	svc, err := h.deps.Catalog.GetActiveBySlug(r.Context(), slug)
	if errors.Is(err, service.ErrNotFound) {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "layanan: getting service", "slug", slug, "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	data := detailData{Service: svc}
	if svc.Description.Valid {
		data.Paragraphs = util.Paragraphs(svc.Description.String)
	}

	// Only a bookable service advertises availability. A coming-soon one has
	// nothing to offer yet, and showing free slots beside a disabled CTA would
	// read as a bug.
	if !svc.IsComingSoon {
		days, err := h.deps.Schedule.Upcoming(r.Context(), upcomingDays)
		if err != nil {
			// The page's job is to describe the service. A schedule read that fails
			// costs the visitor the availability strip, not the page.
			h.deps.Log.ErrorContext(r.Context(), "layanan: reading upcoming slots", "slug", slug, "error", err)
		} else {
			data.Upcoming = days
		}
	}

	if others, err := h.deps.Catalog.ListActive(r.Context()); err != nil {
		h.deps.Log.ErrorContext(r.Context(), "layanan: listing other services", "error", err)
	} else {
		for _, o := range others {
			if o.ID != svc.ID {
				data.Others = append(data.Others, o)
			}
		}
	}

	// The description is the best summary this page has; the site description is
	// about the business, not this service. Truncated to the length a search
	// result and an OG card actually show.
	description := util.Truncate(svc.Description.String, 155)

	page := view.Page{
		Title:       svc.Name,
		Description: description,
		// article rather than website: this page is about one thing.
		OGType: "article",
	}
	if svc.ImagePath.Valid {
		// Site-relative; the renderer resolves it against APP_URL.
		page.OGImage = "/static/" + svc.ImagePath.String
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/layanan-detail", &view.View{
		Page: page,
		Data: data,
	})
}
