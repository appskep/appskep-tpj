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

// Profil is the customer's own account page.
//
// It edits three fields and links out for everything else. Name, email and
// password belong to Appskep (PLAN.md R1, R2) — there is no local form for any
// of them, and the page says so rather than rendering a disabled input the user
// might keep trying.
type Profil struct {
	deps *app.Deps
}

func NewProfil(deps *app.Deps) *Profil {
	return &Profil{deps: deps}
}

const profilPath = "/profil"

// Body limits for the avatar submission. maxProfilBytes bounds the whole
// request; the extra megabyte over the file cap is headroom for the text fields
// and the multipart framing, so a legal 1 MB image is never refused for being
// wrapped in an envelope.
const (
	maxProfilBytes  = 1<<20 + 1<<20
	profilMemBytes  = 1 << 20
	avatarFieldName = "avatar"
)

// profilFormValues holds the submitted strings verbatim.
//
// Never the parsed values: a rejected form has to re-render exactly what the
// user typed. Field names match the HTML input names, which are also the
// ValidationError keys.
type profilFormValues struct {
	Telepon string
	Alamat  string
	// Written by the map widget, empty when no pin is saved. Strings the whole
	// way, for the same reason bookingFormValues carries them as strings.
	Latitude  string
	Longitude string
}

// profilFormFromUser is the stored profile as form values.
//
// A function rather than three struct literals: Show and both of Save's failure
// paths rebuild this, and the 413 branch in particular is written before any
// form parsing has happened, so a field added here and forgotten there would
// silently blank the user's saved pin on an oversized upload.
func profilFormFromUser(u model.User) profilFormValues {
	return profilFormValues{
		Telepon:   u.Phone,
		Alamat:    u.Address,
		Latitude:  u.Latitude,
		Longitude: u.Longitude,
	}
}

// HasLocation reports whether a pin is saved. Both halves, as everywhere else.
func (v profilFormValues) HasLocation() bool {
	return v.Latitude != "" && v.Longitude != ""
}

type profilData struct {
	User model.User
	Form profilFormValues
	// Errors is keyed by input name, so a template looks a message up with the
	// same string the input carries.
	Errors map[string]string
	// HasAvatar decides between the image and the initial fallback, and whether
	// the "hapus foto" control has anything to remove.
	HasAvatar bool
	// Location is the map_picker partial's payload, filled by render from Form
	// and Errors so none of the four call sites can forget it.
	Location locationPicker
}

// Show renders the profile form.
func (h *Profil) Show(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		// RequireAuth guarantees this cannot happen; refusing rather than
		// dereferencing keeps a routing mistake from becoming a panic.
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	h.render(w, r, http.StatusOK, profilData{
		User:      *user,
		Form:      profilFormFromUser(*user),
		HasAvatar: user.AvatarPath != "",
	})
}

// Save applies one submission.
//
// It redirects on success (303), so the form must NOT carry data-turbo="false" —
// that attribute is for a POST answering 200, which Turbo silently discards.
// A rejected submission re-renders at 422 with every typed value intact and
// never redirects, which would lose the input.
func (h *Profil) Save(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	// MaxBytesReader BEFORE ParseMultipartForm: the latter's argument only caps
	// what is held in memory, and everything past it spills to a temp file with no
	// limit at all.
	r.Body = http.MaxBytesReader(w, r.Body, maxProfilBytes)

	if err := r.ParseMultipartForm(profilMemBytes); err != nil {
		// Almost always the size cap. Re-rendering with a message beats a 500 for
		// something the user can fix by picking a smaller file.
		h.deps.Log.InfoContext(r.Context(), "profil: rejected form body", slog.Any("error", err))
		h.render(w, r, http.StatusRequestEntityTooLarge, profilData{
			User:      *user,
			Form:      profilFormFromUser(*user),
			HasAvatar: user.AvatarPath != "",
			Errors: map[string]string{
				avatarFieldName: "Ukuran unggahan terlalu besar. Maksimal 1 MB.",
			},
		})
		return
	}

	values := profilFormValues{
		Telepon:   r.PostFormValue("telepon"),
		Alamat:    r.PostFormValue("alamat"),
		Latitude:  r.PostFormValue("latitude"),
		Longitude: r.PostFormValue("longitude"),
	}

	in := service.ProfileInput{
		Phone:        values.Telepon,
		Address:      values.Alamat,
		Latitude:     values.Latitude,
		Longitude:    values.Longitude,
		RemoveAvatar: r.PostFormValue("remove_avatar") == "1",
	}

	// An empty file input still produces a part, so the header is only taken when
	// a file was actually chosen.
	if fhs := r.MultipartForm.File[avatarFieldName]; len(fhs) > 0 && fhs[0].Size > 0 {
		in.Avatar = fhs[0]
	}

	// The result is not kept: the redirect below re-enters RequireAuth, which
	// re-reads the users row on every request, so the page that renders next
	// shows the stored values rather than this function's idea of them.
	if _, err := h.deps.Profile.Update(r.Context(), user.ID, in); err != nil {
		h.formError(w, r, err, *user, values)
		return
	}

	// FlashRedirect, not SaveFlashAndRedirect: the latter replaces the whole
	// session with the flash and would sign the user out on saving their profile.
	h.deps.FlashRedirect(w, r, profilPath, model.FlashSuccess("Profil berhasil disimpan."))
}

// formError renders a failed save: a validation problem re-renders the form with
// the messages, anything else is logged and becomes the 500 page.
func (h *Profil) formError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	user model.User,
	values profilFormValues,
) {
	data := profilData{User: user, Form: values, HasAvatar: user.AvatarPath != ""}

	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		data.Errors = ve.Fields
		// 422 rather than 200: the submission was understood and rejected, and a
		// 200 would tell Turbo the navigation succeeded.
		h.render(w, r, http.StatusUnprocessableEntity, data)
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
	default:
		h.deps.Log.ErrorContext(r.Context(), "profil: saving profile",
			"user_id", user.ID, slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

func (h *Profil) render(w http.ResponseWriter, r *http.Request, status int, data profilData) {
	data.Location = locationPickerFor(h.deps, data.Form.Latitude, data.Form.Longitude,
		"Dipakai untuk mengisi formulir booking berikutnya. Bisa diubah saat booking.",
		data.Errors)

	h.deps.View.Render(w, r, status, "public/profil", &view.View{
		Page: view.Page{
			Title:       "Profil",
			Description: "Kelola data kontak dan foto profil kamu.",
			// Behind auth, and robots.txt already disallows /profil.
			NoIndex: true,
		},
		Data: data,
	})
}
