package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Users is the admin view of the local users mirror.
//
// Identity is Appskep's and read-only here — there is no name field to edit and
// no password to reset. The two flags that are local, role and is_active, both
// go through service.Users, where the self-demotion and last-admin guards live.
type Users struct {
	deps *app.Deps
}

func NewUsers(deps *app.Deps) *Users {
	return &Users{deps: deps}
}

const usersPath = "/admin/users"

// userToggle pairs a user with the CSRF token its inline toggle forms need. See
// serviceToggle in layanan.go for why the token has to travel in the payload.
type userToggle struct {
	sqlc.User
	CSRF string
}

// userRow is one listed user with the decisions already made.
type userRow struct {
	User     userToggle
	Bookings int64
	// Self marks the signed-in operator's own row: the toggles on it would only
	// ever be refused, so the template renders a label instead.
	Self bool
}

type usersListData struct {
	Rows   []userRow
	Search string
	Total  int64
	Pager  pager
	Empty  emptyState
}

// List renders the paginated, searchable mirror.
func (h *Users) List(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	result, err := h.deps.Users.List(r.Context(), service.UserListQuery{
		Search:   search,
		Page:     page,
		PageSize: h.deps.Cfg.App.PageSize,
	})
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin users: listing", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	var selfID int64
	if u := auth.UserFrom(r.Context()); u != nil {
		selfID = u.ID
	}

	token := auth.MaskedTokenFrom(r.Context())

	rows := make([]userRow, 0, len(result.Items))
	for _, item := range result.Items {
		rows = append(rows, userRow{
			User:     userToggle{User: item.User, CSRF: token},
			Bookings: item.Bookings,
			Self:     item.User.ID == selfID,
		})
	}

	q := url.Values{}
	if search != "" {
		q.Set("q", search)
	}

	h.deps.View.Render(w, r, http.StatusOK, "admin/users", &view.View{
		Page: view.Page{Title: "Pengguna"},
		Data: usersListData{
			Rows:   rows,
			Search: search,
			Total:  result.Total,
			Pager: pager{
				Page:       result.Page,
				TotalPages: result.TotalPages,
				BaseURL:    baseURL(usersPath, q),
			},
			Empty: usersEmptyState(search),
		},
	})
}

// ToggleActive and ToggleRole flip a flag from the list.
//
// Both post the value they want rather than asking for a flip, so a
// double-clicked button or a resent request settles on the state of the last
// click instead of inverting whatever it finds — the Phase 4 rule.
func (h *Users) ToggleActive(w http.ResponseWriter, r *http.Request) {
	h.toggle(w, r, service.ActionUserActive, func(ctx context.Context, actor, id int64, v bool) (sqlc.User, error) {
		return h.deps.Users.SetActive(ctx, actor, id, v)
	}, "Status pengguna diperbarui.")
}

func (h *Users) ToggleRole(w http.ResponseWriter, r *http.Request) {
	h.toggle(w, r, service.ActionUserRole, func(ctx context.Context, actor, id int64, v bool) (sqlc.User, error) {
		return h.deps.Users.SetRole(ctx, actor, id, v)
	}, "Peran pengguna diperbarui.")
}

func (h *Users) toggle(
	w http.ResponseWriter,
	r *http.Request,
	action string,
	apply func(ctx context.Context, actor, id int64, v bool) (sqlc.User, error),
	msg string,
) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	var actor int64
	if u := auth.UserFrom(r.Context()); u != nil {
		actor = u.ID
	}

	want := r.PostFormValue("value") == "1"

	user, err := apply(r.Context(), actor, id, want)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNotFound):
			h.deps.ErrorPage(w, r, http.StatusNotFound)
		case errors.Is(err, service.ErrSelfDemotion):
			// A refusal the operator caused and can understand, not a failure.
			h.deps.FlashRedirect(w, r, usersPath, model.FlashWarning(
				"Anda tidak bisa mencabut akses akun Anda sendiri. Minta admin lain melakukannya."))
		case errors.Is(err, service.ErrLastAdmin):
			h.deps.FlashRedirect(w, r, usersPath, model.FlashWarning(
				"Ini satu-satunya admin aktif. Angkat admin lain dulu sebelum mencabut yang ini."))
		default:
			h.deps.Log.ErrorContext(r.Context(), "admin users: "+action, slog.Any("error", err))
			h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		}
		return
	}

	h.audit(r, action, id, map[string]any{"value": want})

	// Turbo sends this Accept header for a form it is handling. Without it — JS
	// off, or curl — the request gets an ordinary redirect, so the toggles keep
	// working rather than dumping a stream fragment into the window.
	if strings.Contains(r.Header.Get("Accept"), "text/vnd.turbo-stream.html") {
		h.deps.View.RenderStream(w, r, "admin/users", "user_toggles_stream", &view.View{
			Data: userToggle{User: user, CSRF: auth.MaskedTokenFrom(r.Context())},
		})
		return
	}
	h.deps.FlashRedirect(w, r, usersPath, model.FlashSuccess(msg))
}

func (h *Users) audit(r *http.Request, action string, id int64, meta map[string]any) {
	var actor int64
	if u := auth.UserFrom(r.Context()); u != nil {
		actor = u.ID
	}
	h.deps.Audit.Record(r.Context(), service.Entry{
		ActorID:  actor,
		Action:   action,
		Entity:   service.EntityUser,
		EntityID: id,
		Meta:     meta,
		IP:       clientIP(r),
	})
}

func (h *Users) idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return 0, false
	}
	return id, true
}

func usersEmptyState(search string) emptyState {
	if search != "" {
		return emptyState{
			Icon:        "search",
			Title:       "Tidak ada pengguna yang cocok",
			Body:        "Coba kata kunci lain, atau tampilkan semua.",
			ActionLabel: "Tampilkan semua",
			ActionHref:  usersPath,
		}
	}
	return emptyState{
		Icon:  "users",
		Title: "Belum ada pengguna",
		Body:  "Akun muncul di sini setelah seseorang login lewat Appskep untuk pertama kali.",
	}
}
