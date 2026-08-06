package handler

import (
	"database/sql"
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

// Terapis is the admin therapists module.
//
// A near-copy of Layanan, which is the reference implementation for every admin
// form: the same validation contract, the same 422 re-render, the same Turbo
// Stream toggle. What it adds is the "layanan yang dikuasai" checkbox group.
type Terapis struct {
	deps *app.Deps
}

func NewTerapis(deps *app.Deps) *Terapis {
	return &Terapis{deps: deps}
}

// terapisListPath is where every completed action returns to.
const terapisListPath = "/admin/terapis"

// therapistToggle pairs a therapist with the CSRF token its inline toggle form
// needs — see serviceToggle for why the token has to travel in the payload.
type therapistToggle struct {
	sqlc.Therapist
	CSRF string
}

// terapisListData is the terapis list page payload.
type terapisListData struct {
	Therapists []therapistToggle
	// Dialogs parallels Therapists: one delete confirmation each.
	Dialogs []confirmDialog
	Search  string
	Total   int64
	Pager   pager
	Empty   emptyState
}

// terapisFormData drives both the create and the edit page.
type terapisFormData struct {
	IsNew     bool
	Action    string
	Therapist sqlc.Therapist
	Form      terapisFormValues
	// ServiceChoices is the "layanan yang dikuasai" checkbox group. The template
	// cannot compute Checked — there is no `contains` helper and a partial takes
	// exactly one value — so the handler resolves it here.
	ServiceChoices []serviceChoice
	Errors         map[string]string
}

// serviceChoice is one checkbox. sqlc.Service is embedded so .Name and .ID
// resolve by promotion, exactly as they do everywhere else a row is wrapped.
type serviceChoice struct {
	sqlc.Service
	Checked bool
}

// terapisFormValues holds the submitted strings rather than parsed values, so a
// rejected submit re-renders exactly what was typed instead of silently
// normalising it or, worse, blanking it.
type terapisFormValues struct {
	Name            string
	Specialization  string
	Bio             string
	Certifications  string
	YearsExperience string
	SortOrder       string
	IsActive        bool
	ServiceIDs      []int64
}

// List renders the paginated, searchable table.
func (h *Terapis) List(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	result, err := h.deps.Therapists.List(r.Context(), service.ListQuery{
		Search:   q,
		Page:     page,
		PageSize: h.deps.Cfg.App.PageSize,
	})
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: listing", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	token := auth.MaskedTokenFrom(r.Context())

	h.deps.View.Render(w, r, http.StatusOK, "admin/terapis", &view.View{
		Page: view.Page{Title: "Terapis"},
		Data: terapisListData{
			Therapists: therapistToggles(result.Items, token),
			Dialogs:    terapisDeleteDialogs(result.Items, token),
			Search:     q,
			Total:      result.Total,
			Pager: pager{
				Page:       result.Page,
				TotalPages: result.TotalPages,
				BaseURL:    terapisListBaseURL(q),
			},
			Empty: terapisEmptyState(q),
		},
	})
}

// New renders the blank create form.
func (h *Terapis) New(w http.ResponseWriter, r *http.Request) {
	h.renderForm(w, r, http.StatusOK, terapisFormData{
		IsNew:  true,
		Action: terapisListPath,
		// A new therapist is published unless the admin says otherwise.
		Form: terapisFormValues{IsActive: true},
	})
}

// Create validates and inserts a therapist.
func (h *Terapis) Create(w http.ResponseWriter, r *http.Request) {
	in, values, ok := h.parseForm(w, r)
	if !ok {
		return
	}

	if _, err := h.deps.Therapists.Create(r.Context(), in); err != nil {
		h.formError(w, r, err, terapisFormData{
			IsNew:  true,
			Action: terapisListPath,
			Form:   values,
		}, "terapis: creating")
		return
	}

	h.deps.FlashRedirect(w, r, terapisListPath, model.FlashSuccess("Terapis berhasil ditambahkan."))
}

// Edit renders the form for an existing therapist.
func (h *Terapis) Edit(w http.ResponseWriter, r *http.Request) {
	row, ok := h.load(w, r)
	if !ok {
		return
	}

	ids, err := h.deps.Therapists.ServiceIDs(r.Context(), row.ID)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: loading tags", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	h.renderForm(w, r, http.StatusOK, terapisFormData{
		Action:    terapisListPath + "/" + strconv.FormatInt(row.ID, 10),
		Therapist: row,
		Form: terapisFormValues{
			Name:            row.Name,
			Specialization:  row.Specialization.String,
			Bio:             row.Bio.String,
			Certifications:  row.Certifications.String,
			YearsExperience: nullInt32String(row.YearsExperience),
			SortOrder:       strconv.Itoa(int(row.SortOrder)),
			IsActive:        row.IsActive,
			ServiceIDs:      ids,
		},
	})
}

// Update validates and saves an existing therapist.
func (h *Terapis) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	in, values, ok := h.parseForm(w, r)
	if !ok {
		return
	}

	if err := h.deps.Therapists.Update(r.Context(), id, in); err != nil {
		// Reload the row so the form can still show the current photo and slug
		// alongside the rejected input.
		row, _ := h.deps.Therapists.Get(r.Context(), id)
		h.formError(w, r, err, terapisFormData{
			Action:    terapisListPath + "/" + strconv.FormatInt(id, 10),
			Therapist: row,
			Form:      values,
		}, "terapis: updating")
		return
	}

	h.deps.FlashRedirect(w, r, terapisListPath, model.FlashSuccess("Terapis berhasil disimpan."))
}

// Delete removes a therapist.
//
// Unlike a layanan there is nothing to block it: no booking references a
// therapist, and the layanan tags go with the row via ON DELETE CASCADE.
func (h *Terapis) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	switch err := h.deps.Therapists.Delete(r.Context(), id); {
	case err == nil:
		h.deps.FlashRedirect(w, r, terapisListPath, model.FlashSuccess("Terapis berhasil dihapus."))
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
	default:
		h.deps.Log.ErrorContext(r.Context(), "terapis: deleting", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

// ToggleActive publishes or unpublishes a therapist from the list page.
//
// The form posts the value it wants rather than asking for a flip, so a
// double-clicked button lands on the state the last click chose instead of
// inverting whatever it finds.
func (h *Terapis) ToggleActive(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	row, err := h.deps.Therapists.SetActive(r.Context(), id, r.PostFormValue("value") == "1")
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "terapis: toggling", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// Turbo sends this Accept header for a form it is handling. Without it — JS
	// disabled, or curl — the request gets an ordinary redirect, so the toggle
	// keeps working rather than dumping a stream fragment into the window.
	if strings.Contains(r.Header.Get("Accept"), "text/vnd.turbo-stream.html") {
		h.deps.View.RenderStream(w, r, "admin/terapis", "trp_toggles_stream", &view.View{
			Data: therapistToggle{Therapist: row, CSRF: auth.MaskedTokenFrom(r.Context())},
		})
		return
	}
	h.deps.FlashRedirect(w, r, terapisListPath, model.FlashSuccess("Status terapis diperbarui."))
}

// parseForm reads a multipart submission into a TherapistInput plus the raw
// values needed to re-render the form.
//
// It bounds the body before parsing: ParseMultipartForm's argument only caps what
// is held in memory, and everything past it goes to a temp file with no limit at
// all.
func (h *Terapis) parseForm(w http.ResponseWriter, r *http.Request) (service.TherapistInput, terapisFormValues, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	if err := r.ParseMultipartForm(formMemoryBytes); err != nil {
		// Almost always the size cap. Re-rendering the form with a message beats
		// a 500 for something the admin can fix by picking a smaller file.
		h.deps.Log.InfoContext(r.Context(), "terapis: rejected form body", slog.Any("error", err))
		h.renderForm(w, r, http.StatusRequestEntityTooLarge, terapisFormData{
			IsNew:  chi.URLParam(r, "id") == "",
			Action: r.URL.Path,
			Errors: map[string]string{
				"image": "Ukuran unggahan terlalu besar. Maksimal 2 MB.",
			},
		})
		return service.TherapistInput{}, terapisFormValues{}, false
	}

	values := terapisFormValues{
		Name:            r.PostFormValue("name"),
		Specialization:  r.PostFormValue("specialization"),
		Bio:             r.PostFormValue("bio"),
		Certifications:  r.PostFormValue("certifications"),
		YearsExperience: r.PostFormValue("years_experience"),
		SortOrder:       r.PostFormValue("sort_order"),
		IsActive:        r.PostFormValue("is_active") == "1",
		ServiceIDs:      parseIDs(r.PostForm["service_ids"]),
	}

	in := service.TherapistInput{
		Name:            values.Name,
		Specialization:  values.Specialization,
		Bio:             values.Bio,
		Certifications:  values.Certifications,
		YearsExperience: values.YearsExperience,
		SortOrder:       values.SortOrder,
		IsActive:        values.IsActive,
		ServiceIDs:      values.ServiceIDs,
		RemoveImage:     r.PostFormValue("remove_image") == "1",
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
func (h *Terapis) formError(w http.ResponseWriter, r *http.Request, err error, data terapisFormData, logMsg string) {
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

// renderForm fills in the checkbox group and renders.
//
// The selection comes from data.Form.ServiceIDs, which is the stored set on a
// GET and the *submitted* set on a 422 — what was ticked wins on a re-render,
// including the boxes the admin deliberately cleared.
func (h *Terapis) renderForm(w http.ResponseWriter, r *http.Request, status int, data terapisFormData) {
	title := "Ubah Terapis"
	if data.IsNew {
		title = "Tambah Terapis"
	}
	data.Errors = orEmpty(data.Errors)

	// Only active layanan may be tagged. A read that fails costs the admin the
	// checkbox group, not the whole form — they can still fix a rejected name.
	services, err := h.deps.Catalog.ListActive(r.Context())
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "terapis: listing services for form", slog.Any("error", err))
	} else {
		selected := make(map[int64]bool, len(data.Form.ServiceIDs))
		for _, id := range data.Form.ServiceIDs {
			selected[id] = true
		}
		data.ServiceChoices = make([]serviceChoice, 0, len(services))
		for _, s := range services {
			data.ServiceChoices = append(data.ServiceChoices, serviceChoice{
				Service: s,
				Checked: selected[s.ID],
			})
		}
	}

	h.deps.View.Render(w, r, status, "admin/terapis-form", &view.View{
		Page: view.Page{Title: title},
		Data: data,
	})
}

// load fetches the therapist named by the URL, writing the error page itself
// when there is not one.
func (h *Terapis) load(w http.ResponseWriter, r *http.Request) (sqlc.Therapist, bool) {
	id, ok := h.idParam(w, r)
	if !ok {
		return sqlc.Therapist{}, false
	}

	row, err := h.deps.Therapists.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return sqlc.Therapist{}, false
		}
		h.deps.Log.ErrorContext(r.Context(), "terapis: loading", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return sqlc.Therapist{}, false
	}
	return row, true
}

func (h *Terapis) idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return 0, false
	}
	return id, true
}

// parseIDs turns the checkbox group's raw values into ids, skipping anything
// unparseable. The service decides which of them may actually be written.
func parseIDs(raw []string) []int64 {
	if len(raw) == 0 {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// nullInt32String renders an optional integer for a text input: a NULL becomes
// an empty box, not a "0" the admin then has to clear.
func nullInt32String(n sql.NullInt32) string {
	if !n.Valid {
		return ""
	}
	return strconv.Itoa(int(n.Int32))
}

// terapisListBaseURL builds the prefix the pagination partial appends "page=" to.
// It must carry the active search and end in ? or &, or paging would drop the
// filter.
func terapisListBaseURL(search string) string {
	if search == "" {
		return terapisListPath + "?"
	}
	return terapisListPath + "?q=" + url.QueryEscape(search) + "&"
}

// therapistToggles attaches the request's CSRF token to each row.
func therapistToggles(rows []sqlc.Therapist, token string) []therapistToggle {
	out := make([]therapistToggle, 0, len(rows))
	for _, t := range rows {
		out = append(out, therapistToggle{Therapist: t, CSRF: token})
	}
	return out
}

// terapisDeleteDialogs builds one confirmation per row. The id matches the
// trigger button's data-dialog in the template, so the two are generated from
// the same therapist id rather than hand-kept in sync.
func terapisDeleteDialogs(rows []sqlc.Therapist, token string) []confirmDialog {
	dialogs := make([]confirmDialog, 0, len(rows))
	for _, t := range rows {
		dialogs = append(dialogs, confirmDialog{
			CSRF:  token,
			ID:    "hapus-terapis-" + strconv.FormatInt(t.ID, 10),
			Title: "Hapus terapis ini?",
			Body: "\"" + t.Name + "\" akan dihapus permanen beserta daftar layanan " +
				"yang dikuasainya. Kalau hanya ingin menyembunyikan dari situs, " +
				"nonaktifkan saja.",
			ConfirmLabel: "Hapus",
			Action:       terapisListPath + "/" + strconv.FormatInt(t.ID, 10) + "/hapus",
		})
	}
	return dialogs
}

// terapisEmptyState distinguishes "no therapists yet" from "nothing matched",
// which need different offers: one to create, one to clear the filter.
func terapisEmptyState(search string) emptyState {
	if search != "" {
		return emptyState{
			Icon:        "search",
			Title:       "Tidak ada terapis yang cocok",
			Body:        "Coba kata kunci lain, atau tampilkan semua terapis.",
			ActionLabel: "Tampilkan semua",
			ActionHref:  terapisListPath,
		}
	}
	return emptyState{
		Icon:        "user",
		Title:       "Belum ada terapis",
		Body:        "Tambahkan profil terapis agar pengunjung tahu siapa yang akan datang.",
		ActionLabel: "Tambah terapis",
		ActionHref:  terapisListPath + "/baru",
	}
}
