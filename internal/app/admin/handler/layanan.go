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

// Layanan is the admin services module.
type Layanan struct {
	deps *app.Deps
}

func NewLayanan(deps *app.Deps) *Layanan {
	return &Layanan{deps: deps}
}

// listPath is where every completed action returns to.
const listPath = "/admin/layanan"

// formMemoryBytes is how much of a multipart form is held in memory before the
// rest spills to a temp file. It is not a size limit — see maxRequestBytes.
const formMemoryBytes = 1 << 20

// maxRequestBytes bounds the whole request body: the image cap plus room for the
// text fields around it.
//
// This is the limit that actually protects the server. ParseMultipartForm's
// argument only decides how much is buffered in memory; everything beyond it is
// written to disk, unbounded, so without http.MaxBytesReader a single request
// can fill the volume.
const maxRequestBytes = 2<<20 + 1<<20

// serviceToggle pairs a service with the CSRF token its inline toggle forms
// need.
//
// The token has to travel in the payload because a {{define}} invoked as
// {{template "svc_toggles" .}} rebinds $ to its own argument, putting the page
// envelope — and the .CSRF on it — out of reach from inside. The struct is
// embedded so every field the row already renders still resolves by promotion.
// Same rule as every other partial here: it receives exactly one value, built in
// the handler package.
type serviceToggle struct {
	sqlc.Service
	CSRF string
}

// listData is the layanan list page payload.
type listData struct {
	Services []serviceToggle
	// Dialogs parallels Services: one delete confirmation each.
	//
	// A separate slice rather than a field on a row wrapper because the confirm
	// partial takes exactly one value and there is no dict helper in the FuncMap
	// — every partial in this codebase is fed a handler-side struct.
	Dialogs []confirmDialog
	Search  string
	Total   int64
	Pager   pager
	Empty   emptyState
}

// confirmDialog is the confirm partial's payload.
type confirmDialog struct {
	ID           string
	Title        string
	Body         string
	ConfirmLabel string
	Action       string
	CSRF         string
}

// pager is the pagination partial's payload. Page and TotalPages are int because
// the partial reaches them through `add`, which is func(int, int) int.
type pager struct {
	Page       int
	TotalPages int
	BaseURL    string
}

// emptyState is the empty partial's payload.
type emptyState struct {
	Icon        string
	Title       string
	Body        string
	ActionLabel string
	ActionHref  string
}

// formData drives both the create and the edit page — the same fields, differing
// only in where they post and whether there is a slug to show.
type formData struct {
	IsNew   bool
	Action  string
	Service sqlc.Service
	Form    formValues
	Errors  map[string]string
}

// formValues holds the submitted strings rather than parsed values, so a
// rejected submit re-renders exactly what was typed instead of silently
// normalising it or, worse, blanking it.
type formValues struct {
	Name         string
	Description  string
	Price        string
	Duration     string
	SortOrder    string
	IsActive     bool
	IsComingSoon bool
}

// List renders the paginated, searchable table.
func (h *Layanan) List(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	result, err := h.deps.Catalog.List(r.Context(), service.ListQuery{
		Search:   q,
		Page:     page,
		PageSize: h.deps.Cfg.App.PageSize,
	})
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "layanan: listing", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	token := auth.MaskedTokenFrom(r.Context())

	h.deps.View.Render(w, r, http.StatusOK, "admin/layanan", &view.View{
		Page: view.Page{Title: "Layanan"},
		Data: listData{
			Services: serviceToggles(result.Items, token),
			Dialogs:  deleteDialogs(result.Items, token),
			Search:   q,
			Total:    result.Total,
			Pager: pager{
				Page:       result.Page,
				TotalPages: result.TotalPages,
				BaseURL:    listBaseURL(q),
			},
			Empty: emptyStateFor(q),
		},
	})
}

// New renders the blank create form.
func (h *Layanan) New(w http.ResponseWriter, r *http.Request) {
	h.renderForm(w, r, http.StatusOK, formData{
		IsNew:  true,
		Action: listPath,
		// A new service is active and not coming soon unless the admin says
		// otherwise, which is what they want the overwhelming majority of the time.
		Form: formValues{IsActive: true},
	})
}

// Create validates and inserts a service.
func (h *Layanan) Create(w http.ResponseWriter, r *http.Request) {
	in, values, ok := h.parseForm(w, r)
	if !ok {
		return
	}

	if _, err := h.deps.Catalog.Create(r.Context(), in); err != nil {
		h.formError(w, r, err, formData{
			IsNew:  true,
			Action: listPath,
			Form:   values,
		}, "layanan: creating")
		return
	}

	h.deps.FlashRedirect(w, r, listPath, model.FlashSuccess("Layanan berhasil ditambahkan."))
}

// Edit renders the form for an existing service.
func (h *Layanan) Edit(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.load(w, r)
	if !ok {
		return
	}

	h.renderForm(w, r, http.StatusOK, formData{
		Action:  listPath + "/" + strconv.FormatInt(svc.ID, 10),
		Service: svc,
		Form: formValues{
			Name:         svc.Name,
			Description:  svc.Description.String,
			Price:        svc.Price,
			Duration:     strconv.Itoa(int(svc.DurationMinutes)),
			SortOrder:    strconv.Itoa(int(svc.SortOrder)),
			IsActive:     svc.IsActive,
			IsComingSoon: svc.IsComingSoon,
		},
	})
}

// Update validates and saves an existing service.
func (h *Layanan) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	in, values, ok := h.parseForm(w, r)
	if !ok {
		return
	}

	if err := h.deps.Catalog.Update(r.Context(), id, in); err != nil {
		// Reload the row so the form can still show the current image and slug
		// alongside the rejected input.
		svc, _ := h.deps.Catalog.Get(r.Context(), id)
		h.formError(w, r, err, formData{
			Action:  listPath + "/" + strconv.FormatInt(id, 10),
			Service: svc,
			Form:    values,
		}, "layanan: updating")
		return
	}

	h.deps.FlashRedirect(w, r, listPath, model.FlashSuccess("Layanan berhasil disimpan."))
}

// Delete removes a service, or explains why it cannot.
func (h *Layanan) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	switch err := h.deps.Catalog.Delete(r.Context(), id); {
	case err == nil:
		h.deps.FlashRedirect(w, r, listPath, model.FlashSuccess("Layanan berhasil dihapus."))
	case errors.Is(err, service.ErrHasBookings):
		// Not an error the admin did anything wrong to cause, so it points at
		// the action they actually want instead of just refusing.
		h.deps.FlashRedirect(w, r, listPath, model.FlashWarning(
			"Layanan tidak bisa dihapus karena sudah punya booking. Nonaktifkan saja agar tidak muncul di situs."))
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
	default:
		h.deps.Log.ErrorContext(r.Context(), "layanan: deleting", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

// ToggleActive and ToggleComingSoon flip a flag from the list page.
func (h *Layanan) ToggleActive(w http.ResponseWriter, r *http.Request) {
	h.toggle(w, r, h.deps.Catalog.SetActive, "Status layanan diperbarui.")
}

func (h *Layanan) ToggleComingSoon(w http.ResponseWriter, r *http.Request) {
	h.toggle(w, r, h.deps.Catalog.SetComingSoon, "Status layanan diperbarui.")
}

// toggle applies a flag change and answers with a turbo stream when the browser
// asked for one.
//
// The form posts the value it wants rather than asking for a flip, so a
// double-clicked button or a resent request lands on the state the last click
// chose instead of inverting whatever it finds.
func (h *Layanan) toggle(
	w http.ResponseWriter,
	r *http.Request,
	apply func(ctx context.Context, id int64, v bool) (sqlc.Service, error),
	msg string,
) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	svc, err := apply(r.Context(), id, r.PostFormValue("value") == "1")
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "layanan: toggling", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// Turbo sends this Accept header for a form it is handling. Without it — JS
	// disabled, or curl — the request gets an ordinary redirect, so the toggles
	// keep working rather than dumping a stream fragment into the window.
	if strings.Contains(r.Header.Get("Accept"), "text/vnd.turbo-stream.html") {
		h.deps.View.RenderStream(w, r, "admin/layanan", "svc_toggles_stream", &view.View{
			Data: serviceToggle{Service: svc, CSRF: auth.MaskedTokenFrom(r.Context())},
		})
		return
	}
	h.deps.FlashRedirect(w, r, listPath, model.FlashSuccess(msg))
}

// parseForm reads a multipart submission into a ServiceInput plus the raw values
// needed to re-render the form.
//
// It bounds the body before parsing: ParseMultipartForm's argument only caps what
// is held in memory, and everything past it goes to a temp file with no limit at
// all.
func (h *Layanan) parseForm(w http.ResponseWriter, r *http.Request) (service.ServiceInput, formValues, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	if err := r.ParseMultipartForm(formMemoryBytes); err != nil {
		// Almost always the size cap. Re-rendering the form with a message beats
		// a 500 for something the admin can fix by picking a smaller file.
		h.deps.Log.InfoContext(r.Context(), "layanan: rejected form body", slog.Any("error", err))
		h.renderForm(w, r, http.StatusRequestEntityTooLarge, formData{
			IsNew:  chi.URLParam(r, "id") == "",
			Action: r.URL.Path,
			Errors: map[string]string{
				"image": "Ukuran unggahan terlalu besar. Maksimal 2 MB.",
			},
		})
		return service.ServiceInput{}, formValues{}, false
	}

	values := formValues{
		Name:         r.PostFormValue("name"),
		Description:  r.PostFormValue("description"),
		Price:        r.PostFormValue("price"),
		Duration:     r.PostFormValue("duration"),
		SortOrder:    r.PostFormValue("sort_order"),
		IsActive:     r.PostFormValue("is_active") == "1",
		IsComingSoon: r.PostFormValue("is_coming_soon") == "1",
	}

	in := service.ServiceInput{
		Name:         values.Name,
		Description:  values.Description,
		Price:        values.Price,
		Duration:     values.Duration,
		SortOrder:    values.SortOrder,
		IsActive:     values.IsActive,
		IsComingSoon: values.IsComingSoon,
		RemoveImage:  r.PostFormValue("remove_image") == "1",
	}

	// An empty file input still produces a part, so the header is only taken when
	// a file was actually chosen.
	if fhs := r.MultipartForm.File["image"]; len(fhs) > 0 && fhs[0].Size > 0 {
		in.Image = fhs[0]
	}

	return in, values, true
}

// formError renders a failed save: a validation problem re-renders the form with
// the messages, anything else is a 500.
func (h *Layanan) formError(w http.ResponseWriter, r *http.Request, err error, data formData, logMsg string) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		data.Errors = ve.Fields
		// 422 rather than 200: the submission was understood and rejected, and a
		// 200 would tell Turbo the navigation succeeded.
		h.renderForm(w, r, http.StatusUnprocessableEntity, data)
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
	default:
		h.deps.Log.ErrorContext(r.Context(), logMsg, slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

func (h *Layanan) renderForm(w http.ResponseWriter, r *http.Request, status int, data formData) {
	title := "Ubah Layanan"
	if data.IsNew {
		title = "Tambah Layanan"
	}
	if data.Errors == nil {
		data.Errors = map[string]string{}
	}

	h.deps.View.Render(w, r, status, "admin/layanan-form", &view.View{
		Page: view.Page{Title: title},
		Data: data,
	})
}

// load fetches the service named by the URL, writing the error page itself when
// there is not one.
func (h *Layanan) load(w http.ResponseWriter, r *http.Request) (sqlc.Service, bool) {
	id, ok := h.idParam(w, r)
	if !ok {
		return sqlc.Service{}, false
	}

	svc, err := h.deps.Catalog.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return sqlc.Service{}, false
		}
		h.deps.Log.ErrorContext(r.Context(), "layanan: loading", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return sqlc.Service{}, false
	}
	return svc, true
}

func (h *Layanan) idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return 0, false
	}
	return id, true
}

// listBaseURL builds the prefix the pagination partial appends "page=" to. It
// must carry the active search and end in ? or &, or paging would drop the
// filter.
func listBaseURL(search string) string {
	if search == "" {
		return listPath + "?"
	}
	return listPath + "?q=" + url.QueryEscape(search) + "&"
}

// deleteDialogs builds one confirmation per row. The id matches the trigger
// button's onclick in the template, so the two are generated from the same
// service id rather than hand-kept in sync.
// serviceToggles attaches the request's CSRF token to each row.
func serviceToggles(services []sqlc.Service, token string) []serviceToggle {
	rows := make([]serviceToggle, 0, len(services))
	for _, s := range services {
		rows = append(rows, serviceToggle{Service: s, CSRF: token})
	}
	return rows
}

func deleteDialogs(services []sqlc.Service, token string) []confirmDialog {
	dialogs := make([]confirmDialog, 0, len(services))
	for _, s := range services {
		dialogs = append(dialogs, confirmDialog{
			CSRF:  token,
			ID:    "hapus-" + strconv.FormatInt(s.ID, 10),
			Title: "Hapus layanan ini?",
			Body: "\"" + s.Name + "\" akan dihapus permanen. Layanan yang sudah punya " +
				"booking tidak bisa dihapus — nonaktifkan saja agar tidak muncul di situs.",
			ConfirmLabel: "Hapus",
			Action:       listPath + "/" + strconv.FormatInt(s.ID, 10) + "/hapus",
		})
	}
	return dialogs
}

// emptyStateFor distinguishes "no services yet" from "nothing matched", which
// need different offers: one to create, one to clear the filter.
func emptyStateFor(search string) emptyState {
	if search != "" {
		return emptyState{
			Icon:        "search",
			Title:       "Tidak ada layanan yang cocok",
			Body:        "Coba kata kunci lain, atau tampilkan semua layanan.",
			ActionLabel: "Tampilkan semua",
			ActionHref:  listPath,
		}
	}
	return emptyState{
		Icon:        "sparkles",
		Title:       "Belum ada layanan",
		Body:        "Tambahkan layanan pertama agar pengunjung bisa mulai memesan.",
		ActionLabel: "Tambah layanan",
		ActionHref:  listPath + "/baru",
	}
}
