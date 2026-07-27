package view

import (
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
)

// View is the envelope handed to every template. Handlers fill in Page and Data;
// the renderer fills in everything else, so a handler can never forget the site
// name or the current user and produce a half-rendered layout.
type View struct {
	Site  Site
	Page  Page
	User  *User
	Flash []Flash
	// CSRF is the masked token for csrfField, filled by the renderer from the
	// session secret the auth middleware put in the request context. It is empty
	// for an anonymous visitor, and csrfField renders nothing while it is.
	CSRF string
	// Now is the current time in the application location, for countdowns and
	// "is this in the past" checks inside templates.
	Now time.Time
	// Data is the page's own payload.
	Data any
}

// Page is the per-page head and navigation metadata.
type Page struct {
	// Title goes in <title>, suffixed with the site name by the layout.
	Title string
	// Description is the meta description and the OG description.
	Description string
	// Path is the current request path, used to mark the active nav item.
	Path string
	// OGImage is the social preview image. A handler may set a site-relative path
	// ("/static/uploads/services/x.jpg"); fill resolves it against Site.URL,
	// because og:image must be absolute to be fetchable by a crawler. Empty falls
	// back to Site.OGImage.
	OGImage string
	// OGType is the og:type value. fill defaults it to "website"; a content page
	// that is about one thing sets "article".
	OGType string
	// Canonical is the absolute canonical URL. fill defaults it to
	// Site.URL + Path, which is right for every page that has no query-string
	// variants worth collapsing.
	Canonical string
	// NoIndex emits a robots noindex meta tag — for error pages and anything
	// behind auth.
	NoIndex bool
}

// Site is the settings-backed site identity, refreshed from the settings table.
type Site struct {
	Name        string
	Tagline     string
	Description string
	URL         string
	Email       string
	Phone       string
	// Address is the area served, not a venue: the therapist travels to the
	// customer, so nothing renders it as a place to come to.
	Address   string
	WhatsApp  string
	Instagram string
	// OGImage is the fallback social preview for pages with no image of their
	// own. Settings-driven and seeded empty: no default asset ships with the
	// repo, so this stays inert until someone sets it from the settings form.
	OGImage string
	// AssetVersion is appended to static asset URLs by the asset helper, so a
	// deploy invalidates the cached CSS without a fingerprinting build step.
	AssetVersion string
	// Year is the current year, for the footer copyright.
	Year int
}

// User is the template-facing view of the signed-in user.
//
// Deliberately neither sqlc's generated User (which carries sql.NullString and
// sql.NullTime fields that html/template renders as "{value true}") nor
// model.User: this is the subset a template may see, so a field added to the
// domain type does not silently become renderable.
type User struct {
	ID         int64
	Name       string
	Email      string
	Phone      string
	AvatarPath string
	IsAdmin    bool
}

// userFrom narrows the domain user to what templates are allowed to render.
func userFrom(u *model.User) *User {
	if u == nil {
		return nil
	}
	return &User{
		ID:         u.ID,
		Name:       u.Name,
		Email:      u.Email,
		Phone:      u.Phone,
		AvatarPath: u.AvatarPath,
		IsAdmin:    u.IsAdmin(),
	}
}

// Initial is the first letter of the user's name, for the avatar fallback.
func (u *User) Initial() string {
	for _, r := range u.Name {
		return string(r)
	}
	return "?"
}

// Flash is a one-shot message rendered above the page content: {Level, Message},
// where Level is one of "success", "error", "warning", "info" and selects the
// icon and colour in the alert partial.
//
// An alias rather than its own struct because the session codec has to serialise
// it, and the session cannot import view.
type Flash = model.Flash

// siteFrom builds the Site block from the settings cache. Called on every render
// so an admin settings edit shows up immediately, which is cheap: Settings is an
// in-memory map behind an RWMutex.
func (r *Renderer) siteFrom(s *service.Settings) Site {
	return Site{
		Name:         s.String(service.KeySiteName, r.cfg.App.Name),
		Tagline:      s.String(service.KeySiteTagline, ""),
		Description:  s.String(service.KeySiteDescription, ""),
		URL:          r.cfg.App.URL,
		Email:        s.String(service.KeyContactEmail, ""),
		Phone:        s.String(service.KeyContactPhone, ""),
		Address:      s.String(service.KeyContactAddress, ""),
		WhatsApp:     s.String(service.KeyWhatsAppNumber, ""),
		Instagram:    s.String(service.KeyInstagramURL, ""),
		OGImage:      s.String(service.KeySiteOGImage, ""),
		AssetVersion: r.assetVersion,
		Year:         time.Now().In(r.cfg.App.Location).Year(),
	}
}
