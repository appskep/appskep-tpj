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

// Terapis serves the public therapist profiles: the grid and one profile.
type Terapis struct {
	deps *app.Deps
}

func NewTerapis(deps *app.Deps) *Terapis {
	return &Terapis{deps: deps}
}

// therapistCard is the therapist_card partial's payload.
//
// A partial receives exactly one value and there is no dict helper, so the row
// and its layanan tags are joined here rather than in the template. The row is
// embedded, so .Name, .Slug and .ImagePath resolve by promotion — the same trick
// the admin toggles use.
type therapistCard struct {
	sqlc.Therapist
	Services []service.ServiceTag
}

// terapisListData is the /terapis payload.
type terapisListData struct {
	Therapists []therapistCard
	Empty      emptyState
}

// terapisDetailData is the /terapis/{slug} payload.
type terapisDetailData struct {
	Therapist sqlc.Therapist
	// Bio and Certifications are split on blank lines by util.Paragraphs, so the
	// template writes its own <p> tags and every paragraph still goes through
	// html/template's escaping.
	Bio            []string
	Certifications []string
	// Services is the active layanan this therapist handles, linked to their
	// detail pages.
	Services []sqlc.Service
	// Others is every other active therapist, for the strip at the foot.
	Others []therapistCard
}

// List renders the therapist grid.
func (h *Terapis) List(w http.ResponseWriter, r *http.Request) {
	therapists, err := h.deps.Therapists.ListActive(r.Context())
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: listing", "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/terapis", &view.View{
		Page: view.Page{
			Title:       "Terapis",
			Description: "Kenali terapis yang akan datang ke rumah atau kos kamu — spesialisasi, pengalaman, dan sertifikasinya tertulis jelas.",
		},
		Data: terapisListData{
			Therapists: h.cards(r, therapists),
			Empty: emptyState{
				Icon:  "user",
				Title: "Belum ada profil terapis",
				Body:  "Profil terapis sedang disiapkan. Tanya langsung lewat WhatsApp sementara ini.",
			},
		},
	})
}

// Detail renders one therapist.
//
// An inactive or unknown slug is a 404, not an empty page: GetActiveBySlug
// carries the is_active predicate, so deactivating a therapist in the admin
// panel takes their public URL down on the next request.
func (h *Terapis) Detail(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	row, err := h.deps.Therapists.GetActiveBySlug(r.Context(), slug)
	if errors.Is(err, service.ErrNotFound) {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: getting therapist", "slug", slug, "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	data := terapisDetailData{Therapist: row}
	if row.Bio.Valid {
		data.Bio = util.Paragraphs(row.Bio.String)
	}
	if row.Certifications.Valid {
		data.Certifications = util.Paragraphs(row.Certifications.String)
	}

	// The page's job is to describe the therapist. A tag read that fails costs
	// the visitor the layanan chips, not the page.
	if services, serr := h.deps.Therapists.ServicesFor(r.Context(), row.ID); serr != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: listing services", "slug", slug, "error", serr)
	} else {
		data.Services = services
	}

	if others, oerr := h.deps.Therapists.ListActive(r.Context()); oerr != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: listing other therapists", "error", oerr)
	} else {
		rest := make([]sqlc.Therapist, 0, len(others))
		for _, o := range others {
			if o.ID != row.ID {
				rest = append(rest, o)
			}
		}
		data.Others = h.cards(r, rest)
	}

	// The bio is the best summary this page has, truncated to the length a search
	// result and an OG card actually show.
	page := view.Page{
		Title:       row.Name,
		Description: util.Truncate(row.Bio.String, 155),
		// article rather than website: this page is about one person.
		OGType: "article",
	}
	if row.ImagePath.Valid {
		// Site-relative; the renderer resolves it against APP_URL.
		page.OGImage = "/static/" + row.ImagePath.String
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/terapis-detail", &view.View{
		Page: page,
		Data: data,
	})
}

// cards attaches each therapist's layanan tags, in one query for the whole set
// rather than one per card.
//
// A failed tag read is logged and the cards render without chips: the grid's job
// is to name the people, and the tags are decoration on top of that.
func (h *Terapis) cards(r *http.Request, rows []sqlc.Therapist) []therapistCard {
	tags, err := h.deps.Therapists.ServiceTagsForActive(r.Context())
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: listing service tags", "error", err)
	}

	cards := make([]therapistCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, therapistCard{Therapist: row, Services: tags[row.ID]})
	}
	return cards
}
