// Package view renders HTML templates.
//
// It lives in its own package rather than in util (as PLAN.md line 231 sketched)
// because the render envelope carries a user view-model while the formatters it
// calls live in util. Keeping the renderer in util would make util depend on the
// envelope and the envelope depend on util, so util stays a leaf package of pure
// helpers and this package composes them.
package view

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/service"
)

// Layout names, matching the directories under template/layouts/.
const (
	LayoutPublic = "public"
	LayoutAdmin  = "admin"
)

// Renderer holds the parsed template sets and renders pages into responses.
type Renderer struct {
	fsys     fs.FS
	cfg      *config.Config
	log      *slog.Logger
	settings *service.Settings
	// therapists answers one question on the render path — whether the public
	// terapis section has anything in it — so the nav does not advertise a
	// section that is empty. It is a cached flag, not a query; see
	// service.Therapists.HasActive.
	therapists *service.Therapists

	assetVersion string

	// mu guards pages, which is replaced wholesale on a development reparse.
	mu    sync.RWMutex
	pages map[string]*template.Template
}

// New parses every page at startup and returns an error if any template is
// malformed, so a broken template is a failed boot rather than a 500 in
// production.
//
// fsys is rooted at the template directory. It is an fs.FS rather than a path so
// Phase 14 can swap in an embed.FS to ship a standalone binary with no other
// change here.
func New(
	fsys fs.FS,
	cfg *config.Config,
	log *slog.Logger,
	settings *service.Settings,
	therapists *service.Therapists,
) (*Renderer, error) {
	r := &Renderer{
		fsys:         fsys,
		cfg:          cfg,
		log:          log,
		settings:     settings,
		therapists:   therapists,
		assetVersion: assetVersion(),
	}

	pages, err := r.parse()
	if err != nil {
		return nil, err
	}
	r.pages = pages
	return r, nil
}

// parse builds one template set per page.
//
// html/template allows only one definition of a given name per set, so a single
// set cannot hold two pages that both define "content". The standard answer, used
// here, is a set per page: a base set carrying the FuncMap and every partial is
// cloned for each page, then the page's layout and the page itself are parsed on
// top. Keys are "<layout>/<page>", e.g. "public/landing".
func (r *Renderer) parse() (map[string]*template.Template, error) {
	base := template.New("base").Funcs(r.funcMap())

	partials, err := fs.Glob(r.fsys, "partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("view: globbing partials: %w", err)
	}
	if len(partials) > 0 {
		if base, err = base.ParseFS(r.fsys, partials...); err != nil {
			return nil, fmt.Errorf("view: parsing partials: %w", err)
		}
	}

	pages := make(map[string]*template.Template)

	for _, layout := range []string{LayoutPublic, LayoutAdmin} {
		layoutFile := path.Join("layouts", layout, "base.html")
		if _, err := fs.Stat(r.fsys, layoutFile); err != nil {
			return nil, fmt.Errorf("view: layout %s: %w", layoutFile, err)
		}

		pageFiles, err := fs.Glob(r.fsys, path.Join("pages", layout, "*.html"))
		if err != nil {
			return nil, fmt.Errorf("view: globbing pages for %s: %w", layout, err)
		}

		for _, pageFile := range pageFiles {
			set, err := base.Clone()
			if err != nil {
				return nil, fmt.Errorf("view: cloning base set: %w", err)
			}
			if set, err = set.ParseFS(r.fsys, layoutFile, pageFile); err != nil {
				return nil, fmt.Errorf("view: parsing %s: %w", pageFile, err)
			}

			name := strings.TrimSuffix(path.Base(pageFile), ".html")
			pages[layout+"/"+name] = set
		}
	}

	if len(pages) == 0 {
		return nil, fmt.Errorf("view: no pages found under pages/{public,admin}")
	}
	return pages, nil
}

// lookup returns the set for a page key, reparsing everything first in
// development so a template edit is visible on the next request with no restart.
// config.IsDevelopment's doc comment promises exactly this.
func (r *Renderer) lookup(page string) (*template.Template, error) {
	if r.cfg.IsDevelopment() {
		pages, err := r.parse()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.pages = pages
		r.mu.Unlock()
	}

	r.mu.RLock()
	set, ok := r.pages[page]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("view: no such page %q", page)
	}
	return set, nil
}

// Render writes a full page.
//
// page is "<layout>/<name>", e.g. "public/landing" — the layout is part of the
// key rather than a separate argument, so a page can never be rendered into the
// wrong chrome.
//
// The template is executed into a buffer before anything is written to the
// response. A template error midway through would otherwise have already emitted
// a 200 and half a page, leaving the browser with a truncated document and no
// indication that anything failed.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, status int, page string, v *View) {
	set, err := r.lookup(page)
	if err != nil {
		r.fail(w, req, page, err)
		return
	}

	r.fill(req, v)

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "base", v); err != nil {
		r.fail(w, req, page, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A page rendered for a signed-in visitor names them, lists their bookings or
	// carries a CSRF token, and none of that may be held by a shared cache or
	// re-shown by the back button after a logout. Anonymous pages are left
	// cacheable — they are the same document for everyone.
	if v.User != nil {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		// The client went away mid-write. Nothing to recover, and the header is
		// already sent, so this is logged and dropped.
		r.log.DebugContext(req.Context(), "view: write interrupted",
			slog.String("page", page), slog.Any("error", err))
	}
}

// RenderFragment writes a single named template with no layout — the response
// body for a Turbo Frame request. The frame's own markup lives in a partial, so
// the fragment can be re-rendered on its own without re-sending the page.
func (r *Renderer) RenderFragment(w http.ResponseWriter, req *http.Request, status int, page, fragment string, v *View) {
	set, err := r.lookup(page)
	if err != nil {
		r.fail(w, req, page, err)
		return
	}

	r.fill(req, v)

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, fragment, v); err != nil {
		r.fail(w, req, page+"#"+fragment, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A fragment is live state fetched from the same URL over and over — that is
	// what a frame poller is. Without this the browser answers the second and
	// every later fetch out of its own cache and the frame never changes, which
	// looks exactly like a poller that stopped: the requests simply do not reach
	// the server. Measured on the payment page before this line existed.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// RenderStream writes a Turbo Stream response: one or more <turbo-stream>
// elements that update named targets in place. Used by the inline toggles in
// Phases 4 and 5 and by the slot picker in Phase 7.
//
// The content type is what makes Turbo treat the body as a stream rather than
// replacing the page.
func (r *Renderer) RenderStream(w http.ResponseWriter, req *http.Request, page, fragment string, v *View) {
	set, err := r.lookup(page)
	if err != nil {
		r.fail(w, req, page, err)
		return
	}

	r.fill(req, v)

	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, fragment, v); err != nil {
		r.fail(w, req, page+"#"+fragment, err)
		return
	}

	w.Header().Set("Content-Type", "text/vnd.turbo-stream.html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}

// fill populates the parts of the envelope the renderer owns, leaving anything
// the handler already set alone.
func (r *Renderer) fill(req *http.Request, v *View) {
	v.Site = r.siteFrom(r.settings)
	v.Now = time.Now().In(r.cfg.App.Location)

	if v.Page.Path == "" {
		v.Page.Path = req.URL.Path
	}
	if v.Page.Description == "" {
		v.Page.Description = v.Site.Description
	}
	if v.Page.OGType == "" {
		v.Page.OGType = "website"
	}
	if v.Page.Canonical == "" {
		v.Page.Canonical = v.Site.URL + v.Page.Path
	}

	// og:image must be an absolute URL — a crawler fetches it from its own host,
	// not from the page. Handlers set the site-relative path they already have and
	// the base is applied once, here, from APP_URL rather than from r.Host.
	if v.Page.OGImage == "" {
		v.Page.OGImage = v.Site.OGImage
	}
	if strings.HasPrefix(v.Page.OGImage, "/") {
		v.Page.OGImage = v.Site.URL + v.Page.OGImage
	}

	// The signed-in user, the CSRF token and any pending flash come from the auth
	// middleware via the request context, which is why req is threaded through
	// here rather than only into Render. A handler that set any of them
	// explicitly keeps its value.
	if v.User == nil {
		v.User = userFrom(auth.UserFrom(req.Context()))
	}
	if v.Flash == nil {
		v.Flash = auth.FlashFrom(req.Context())
	}
	// A fresh mask per response, and empty for an anonymous visitor — csrfField
	// renders nothing for an empty token, so a public page is unchanged.
	if v.CSRF == "" {
		v.CSRF = auth.MaskedTokenFrom(req.Context())
	}
}

// fail reports a render failure. The header has not been written at this point,
// so a plain 500 is still possible — and it must stay plain: rendering the styled
// error page here could fail for the same reason and recurse.
func (r *Renderer) fail(w http.ResponseWriter, req *http.Request, page string, err error) {
	r.log.ErrorContext(req.Context(), "view: render failed",
		slog.String("page", page), slog.Any("error", err))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)

	if r.cfg.IsDevelopment() {
		// The template error is the whole point of dev mode; escape it so a
		// malformed template cannot inject markup into its own error page.
		_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><title>Template error</title>"+
			"<pre style=\"white-space:pre-wrap;font:14px ui-monospace,monospace;padding:2rem\">%s\n\n%s</pre>",
			template.HTMLEscapeString(page), template.HTMLEscapeString(err.Error()))
		return
	}
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>Terjadi kesalahan</title>" +
		"<p>Terjadi kesalahan pada server. Silakan coba lagi.</p>"))
}

// Settings exposes the settings cache so handlers can read site configuration
// without taking a second dependency.
func (r *Renderer) Settings() *service.Settings { return r.settings }

// assetVersion produces the cache-busting stamp appended to static asset URLs by
// the asset helper.
//
// It is a hash of app.css's CONTENT, not its modification time. Production serves
// /static with `max-age=31536000, immutable`, so the stamp is the only thing that
// can ever make a browser or a CDN edge fetch the stylesheet again — a stamp that
// fails to change pins the old file for a year, with no revalidation. An mtime
// fails to change in more ways than it looks: a deploy that normalises timestamps
// (rsync without -t, a tar extract, a fresh checkout) can hand back an older
// value than the one already in circulation, and two rebuilds within the same
// second are indistinguishable. A content hash changes if and only if the CSS
// changed, which is also what keeps the cache warm across a redeploy that did not
// touch the styles.
//
// Read once at boot, so a CSS rebuild still needs a restart to reach the HTML —
// that is what `build: tailwind` in the Makefile and the staleness guard in
// main.go are for.
//
// When the file is missing — a checkout where `make tailwind` has not run — the
// process start time is used, which is correct in development and harmlessly
// conservative in production.
func assetVersion() string {
	f, err := os.Open("static/css/app.css")
	if err == nil {
		defer f.Close()

		sum := sha256.New()
		if _, err := io.Copy(sum, f); err == nil {
			return hex.EncodeToString(sum.Sum(nil))[:12]
		}
	}
	return strconv.FormatInt(time.Now().Unix(), 10)
}
