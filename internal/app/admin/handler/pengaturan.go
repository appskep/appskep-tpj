package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Pengaturan is the settings form.
//
// The form is generated from service.EditableSettings, not from the request:
// settings is a key/value table, and a handler that wrote whatever was posted
// would let anyone reaching this page invent keys or overwrite one the code
// reads with a value it cannot parse. The whitelist is the form.
type Pengaturan struct {
	deps *app.Deps
}

func NewPengaturan(deps *app.Deps) *Pengaturan {
	return &Pengaturan{deps: deps}
}

const pengaturanPath = "/admin/pengaturan"

// settingRow is one field of the form: its definition, its current value, and
// its message if the last submit rejected it.
type settingRow struct {
	Field service.SettingField
	Value string
	Error string
}

type pengaturanData struct {
	Rows []settingRow
	// HasErrors drives the summary notice at the top. A form this long can put
	// a rejected field below the fold.
	HasErrors bool
}

// Edit renders the form with the stored values.
func (h *Pengaturan) Edit(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, h.deps.Settings.All(), nil)
}

// Update validates and saves.
//
// A rejected submit re-renders at 422 with the submitted strings — never a
// redirect, which would lose everything typed.
func (h *Pengaturan) Update(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.deps.Log.InfoContext(r.Context(), "pengaturan: rejected form body", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusBadRequest)
		return
	}

	// Only the whitelisted keys are read out of the request, so a hand-posted
	// extra field reaches nothing.
	values := make(map[string]string, len(service.EditableSettings))
	for _, f := range service.EditableSettings {
		if _, ok := r.PostForm[f.Key]; ok {
			values[f.Key] = r.PostFormValue(f.Key)
		}
	}

	err := h.deps.Settings.Update(r.Context(), values)
	if err != nil {
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			h.render(w, r, http.StatusUnprocessableEntity, values, ve.Fields)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "pengaturan: saving", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	var actor int64
	if u := auth.UserFrom(r.Context()); u != nil {
		actor = u.ID
	}
	h.deps.Audit.Record(r.Context(), service.Entry{
		ActorID: actor,
		Action:  service.ActionSettingsUpdate,
		Entity:  service.EntitySetting,
		// The values themselves are not logged: several are contact details, and
		// the count is what an audit line is actually for.
		Meta: map[string]any{"fields": len(values)},
		IP:   clientIP(r),
	})

	// Settings.Update reloads the cache before returning, so the next request —
	// including the public site — already sees the new values.
	h.deps.FlashRedirect(w, r, pengaturanPath, model.FlashSuccess("Pengaturan disimpan."))
}

func (h *Pengaturan) render(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	values map[string]string,
	fieldErrors map[string]string,
) {
	rows := make([]settingRow, 0, len(service.EditableSettings))
	for _, f := range service.EditableSettings {
		rows = append(rows, settingRow{
			Field: f,
			Value: values[f.Key],
			Error: fieldErrors[f.Key],
		})
	}

	h.deps.View.Render(w, r, status, "admin/pengaturan", &view.View{
		Page: view.Page{Title: "Pengaturan"},
		Data: pengaturanData{Rows: rows, HasErrors: len(fieldErrors) > 0},
	})
}
