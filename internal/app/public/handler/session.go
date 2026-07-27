package handler

import (
	"net/http"
	"strings"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/model"
)

// Session handles entering and leaving the site. There is no login form and no
// password check here: TPJ owns no credentials, so both routes are redirects.
type Session struct {
	deps *app.Deps
}

func NewSession(deps *app.Deps) *Session {
	return &Session{deps: deps}
}

// Login bounces to Appskep SSO. It exists so templates have a stable local URL
// to link to — the loginURL helper emits /login?next=<path> — rather than every
// layout knowing the auth host, which authentication.md lists as a defect of the
// reference app.
//
// The auth middleware redirects here implicitly too, for anyone who lands on a
// protected page without a session; this route is the explicit "Masuk" button.
func (h *Session) Login(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, h.deps.Cfg.Auth.LoginURL(h.deps.Cfg.App.URL+next), http.StatusFound)
}

// Logout clears the local session.
//
// It lands on the public home page rather than on the Appskep login page as
// PLAN.md sketched. Redirecting to SSO after logout re-enters the login flow
// immediately, and while the upstream Appskep session is still alive that flow
// completes without prompting — so "Keluar" would appear to do nothing. The
// upstream session is untouched either way: this app cannot invalidate a token
// it did not issue, which is a known limit of the SSO arrangement.
func (h *Session) Logout(w http.ResponseWriter, r *http.Request) {
	h.deps.SaveFlashAndRedirect(w, r, "/", model.FlashSuccess("Kamu sudah keluar."))
}
