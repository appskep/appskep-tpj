package app

import (
	"net/http"
	"strings"

	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// errorCopy is the user-facing text for a status code. In Bahasa Indonesia, and
// it says what to do next rather than only what went wrong — an error page with
// no way forward is a dead end.
type errorCopy struct {
	Status  int
	Title   string
	Message string
	// Action is an optional way out, rendered as a button.
	ActionLabel string
	ActionHref  string
}

var errorCopies = map[int]errorCopy{
	http.StatusNotFound: {
		Status:      http.StatusNotFound,
		Title:       "Halaman tidak ditemukan",
		Message:     "Alamat yang kamu buka tidak ada, atau sudah dipindahkan.",
		ActionLabel: "Ke halaman utama",
		ActionHref:  "/",
	},
	http.StatusForbidden: {
		Status:      http.StatusForbidden,
		Title:       "Kamu tidak punya akses",
		Message:     "Halaman ini hanya untuk admin. Kalau kamu merasa seharusnya bisa membukanya, hubungi pengelola.",
		ActionLabel: "Ke halaman utama",
		ActionHref:  "/",
	},
	http.StatusMethodNotAllowed: {
		Status:      http.StatusMethodNotAllowed,
		Title:       "Permintaan tidak dikenali",
		Message:     "Halaman ini tidak menerima jenis permintaan tersebut.",
		ActionLabel: "Ke halaman utama",
		ActionHref:  "/",
	},
	http.StatusBadRequest: {
		Status:      http.StatusBadRequest,
		Title:       "Permintaan tidak bisa dibaca",
		Message:     "Data yang dikirim tidak lengkap atau rusak di tengah jalan. Coba kirim ulang formulirnya.",
		ActionLabel: "Coba lagi",
		ActionHref:  "",
	},
	http.StatusTooManyRequests: {
		Status:      http.StatusTooManyRequests,
		Title:       "Terlalu banyak permintaan",
		Message:     "Kamu mengirim terlalu banyak permintaan dalam waktu singkat. Tunggu sebentar, lalu coba lagi.",
		ActionLabel: "Coba lagi",
		ActionHref:  "",
	},
	http.StatusRequestEntityTooLarge: {
		Status:      http.StatusRequestEntityTooLarge,
		Title:       "Data yang dikirim terlalu besar",
		Message:     "Berkas atau isian yang kamu kirim melebihi batas. Kecilkan ukuran berkasnya, lalu coba lagi.",
		ActionLabel: "Coba lagi",
		ActionHref:  "",
	},
	http.StatusInternalServerError: {
		Status:      http.StatusInternalServerError,
		Title:       "Terjadi kesalahan di server",
		Message:     "Kesalahan ini sudah dicatat. Coba muat ulang halaman sebentar lagi.",
		ActionLabel: "Muat ulang",
		ActionHref:  "",
	},
}

// ErrorPage renders the styled error page for a status code.
//
// The layout follows the request path so a bad URL under /admin keeps the admin
// chrome instead of dropping the operator onto the public site, and /api gets
// nothing from here at all — that router answers with JSON.
func (d *Deps) ErrorPage(w http.ResponseWriter, r *http.Request, status int) {
	ec, ok := errorCopies[status]
	if !ok {
		ec = errorCopies[http.StatusInternalServerError]
		ec.Status = status
	}

	layout := view.LayoutPublic
	if strings.HasPrefix(r.URL.Path, "/admin") {
		layout = view.LayoutAdmin
	}

	// The reload action needs the current path, which the copy table cannot know.
	if ec.ActionHref == "" {
		ec.ActionHref = r.URL.Path
	}

	d.View.Render(w, r, status, layout+"/error", &view.View{
		Page: view.Page{Title: ec.Title, NoIndex: true},
		Data: ec,
	})
}

// NotFound is the chi NotFound handler for an HTML subsystem.
func (d *Deps) NotFound(w http.ResponseWriter, r *http.Request) {
	d.ErrorPage(w, r, http.StatusNotFound)
}

// MethodNotAllowed is the chi MethodNotAllowed handler for an HTML subsystem.
func (d *Deps) MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	d.ErrorPage(w, r, http.StatusMethodNotAllowed)
}
