package app

import (
	"encoding/xml"
	"log/slog"
	"net/http"
	"strings"

	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Crawler-facing endpoints: robots.txt, sitemap.xml and the /favicon.ico every
// browser asks for whether or not the page links to one.
//
// They are methods on Deps and are mounted on the ROOT router beside
// StaticHandler rather than inside public.Routes, because that router wraps
// everything in OptionalAuth — a session round-trip, and a middleware that will
// consume an ?access_token= callback, on requests that carry neither. They are
// site infrastructure, like the static handler, not pages.
//
// Neither of the first two can go through view.Renderer: Render hardcodes
// text/html and only ever loads pages/{public,admin}/*.html.

// robotsDisallow lists the paths no crawler should index: the admin panel, the
// JSON API, the SSO entry points, and every page that only makes sense for a
// signed-in user (each of which redirects to Appskep anyway, so a crawler that
// followed one would index a login screen).
var robotsDisallow = []string{
	"/admin",
	"/api",
	"/booking",
	"/riwayat",
	"/profil",
	"/login",
	"/logout",
}

// Robots serves /robots.txt.
//
// Outside production the whole site is disallowed. A staging deployment sharing
// content with production is the classic way to get the wrong host into the
// index, and it is not something that can be fixed after the fact from the app.
func (d *Deps) Robots(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("User-agent: *\n")

	if !d.Cfg.IsProduction() {
		b.WriteString("Disallow: /\n")
	} else {
		for _, p := range robotsDisallow {
			b.WriteString("Disallow: " + p + "\n")
		}
		b.WriteString("Allow: /\n\n")
		b.WriteString("Sitemap: " + d.Cfg.App.URL + "/sitemap.xml\n")
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(b.String()))
}

// urlset and sitemapURL are the sitemaps.org 0.9 schema. Built with encoding/xml
// rather than a template so escaping is the encoder's problem: a slug or a site
// URL containing an ampersand would otherwise produce a document no parser
// accepts.
type urlset struct {
	XMLName xml.Name     `xml:"urlset"`
	NS      string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc        string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq,omitempty"`
	Priority   string `xml:"priority,omitempty"`
}

// Sitemap serves /sitemap.xml: the public pages only.
//
// Everything in robotsDisallow is absent by construction — the document is built
// from the active services, not from the route table, so a new authenticated
// route cannot accidentally appear here.
func (d *Deps) Sitemap(w http.ResponseWriter, r *http.Request) {
	services, err := d.Catalog.ListActive(r.Context())
	if err != nil {
		d.Log.ErrorContext(r.Context(), "sitemap: listing services", slog.Any("error", err))
		http.Error(w, "sitemap unavailable", http.StatusInternalServerError)
		return
	}

	// The landing page and the catalogue both change whenever any service does,
	// so the newest service timestamp is their lastmod too.
	var newest string
	for _, svc := range services {
		if iso := util.DateISO(svc.UpdatedAt); iso > newest {
			newest = iso
		}
	}

	base := strings.TrimSuffix(d.Cfg.App.URL, "/")
	doc := urlset{
		NS: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs: []sitemapURL{
			{Loc: base + "/", LastMod: newest, ChangeFreq: "weekly", Priority: "1.0"},
			{Loc: base + "/layanan", LastMod: newest, ChangeFreq: "weekly", Priority: "0.9"},
		},
	}
	for _, svc := range services {
		doc.URLs = append(doc.URLs, sitemapURL{
			Loc:        base + "/layanan/" + svc.Slug,
			LastMod:    util.DateISO(svc.UpdatedAt),
			ChangeFreq: "monthly",
			Priority:   "0.8",
		})
	}

	// Encoded into a buffer first, for the same reason view.Render buffers: an
	// encoding failure partway through would otherwise have already sent a 200
	// and half a document.
	var buf strings.Builder
	buf.WriteString(xml.Header)

	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		d.Log.ErrorContext(r.Context(), "sitemap: encoding", slog.Any("error", err))
		http.Error(w, "sitemap unavailable", http.StatusInternalServerError)
		return
	}
	buf.WriteString("\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(buf.String()))
}

// Favicon answers the /favicon.ico every browser requests regardless of what the
// document declares. Without this route the public router's catch-all renders the
// full styled 404 page for it, on every first page view.
//
// 302 rather than 301: a permanent redirect is cached by browsers indefinitely
// and is effectively impossible to walk back if the icon ever moves.
func (d *Deps) Favicon(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/static/img/favicon.svg", http.StatusFound)
}
