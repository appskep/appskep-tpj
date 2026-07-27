package integration

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The crawler endpoints mount on the root router rather than inside public.Routes
// so they skip OptionalAuth: it costs a session round-trip and would consume an
// inbound ?access_token=. Neither can go through view.Renderer, which forces
// text/html and globs only pages/*.html.

func serve(t *testing.T, h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// withEnv returns a Deps whose config carries a different ENV, for the one branch
// that only exists in production.
func withEnv(env *testsupport.Env, name string) *app.Deps {
	cfg := *env.Cfg
	cfg.Env = name

	deps := *env.Deps
	deps.Cfg = &cfg
	return &deps
}

// TestRobotsDisallowsEverythingOutsideProduction. A staging deployment sharing
// content with production is the classic way to get the wrong host into the
// index, and it cannot be fixed after the fact from the app.
func TestRobotsDisallowsEverythingOutsideProduction(t *testing.T) {
	env := testsupport.New(t)

	for _, name := range []string{"test", config.EnvDevelopment, "staging", ""} {
		t.Run("ENV="+name, func(t *testing.T) {
			rec := serve(t, withEnv(env, name).Robots, "/robots.txt")

			body := rec.Body.String()
			if !strings.Contains(body, "Disallow: /\n") {
				t.Errorf("robots.txt does not disallow everything:\n%s", body)
			}
			if strings.Contains(body, "Allow:") {
				t.Errorf("robots.txt allows crawling outside production:\n%s", body)
			}
			if strings.Contains(body, "Sitemap:") {
				t.Errorf("robots.txt advertises a sitemap outside production:\n%s", body)
			}
		})
	}
}

// TestRobotsInProduction is the branch Phase 6 could only reach through a
// throwaway, because ENV=production will not boot against a passwordless
// development database (PLAN.md:735). A config literal reaches it directly.
func TestRobotsInProduction(t *testing.T) {
	env := testsupport.New(t)

	rec := serve(t, withEnv(env, config.EnvProduction).Robots, "/robots.txt")

	body := rec.Body.String()
	if !strings.Contains(body, "Allow: /") {
		t.Errorf("production robots.txt does not allow crawling:\n%s", body)
	}
	if strings.Contains(body, "Disallow: /\n") {
		t.Errorf("production robots.txt still disallows everything:\n%s", body)
	}
	if !strings.Contains(body, "Sitemap: http://localhost:8080/sitemap.xml") {
		t.Errorf("production robots.txt does not point at the sitemap:\n%s", body)
	}

	// The authenticated areas stay out of the index even when crawling is on.
	for _, path := range []string{"/admin", "/api", "/booking", "/riwayat", "/profil", "/login", "/logout"} {
		if !strings.Contains(body, "Disallow: "+path+"\n") {
			t.Errorf("production robots.txt does not disallow %s:\n%s", path, body)
		}
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// TestSitemapIsBuiltFromTheActiveServices, so a new authenticated route cannot
// appear in it by accident.
func TestSitemap(t *testing.T) {
	env := testsupport.New(t)

	rec := serve(t, env.Deps.Sitemap, "/sitemap.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var doc struct {
		XMLName xml.Name `xml:"urlset"`
		URLs    []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the sitemap is not well-formed XML: %v\n%s", err, rec.Body)
	}

	locs := make([]string, len(doc.URLs))
	for i, u := range doc.URLs {
		locs[i] = u.Loc
	}
	joined := strings.Join(locs, "\n")

	// Every seeded layanan, plus the pages that list them.
	for _, want := range []string{
		"http://localhost:8080/",
		"http://localhost:8080/layanan",
		"http://localhost:8080/layanan/urut-therapeutic",
		"http://localhost:8080/layanan/massage-therapeutic",
		"http://localhost:8080/layanan/bekam-therapeutic",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the sitemap is missing %s:\n%s", want, joined)
		}
	}

	// And nothing that needs a session.
	for _, forbidden := range []string{"/admin", "/api", "/booking", "/riwayat", "/profil", "/login"} {
		if strings.Contains(joined, "localhost:8080"+forbidden) {
			t.Errorf("the sitemap lists the authenticated route %s:\n%s", forbidden, joined)
		}
	}

	// Absolute, from APP_URL — never built from r.Host.
	for _, loc := range locs {
		if !strings.HasPrefix(loc, "http://localhost:8080") {
			t.Errorf("loc %q is not absolute against APP_URL", loc)
		}
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("Content-Type = %q, want XML", ct)
	}
	// It carries no session: the endpoint skips OptionalAuth precisely so it
	// cannot consume an inbound access_token.
	if len(rec.Result().Cookies()) != 0 {
		t.Error("the sitemap set a cookie")
	}
}

// TestSitemapDropsADeactivatedService: Catalog.ListActive carries the is_active
// predicate in SQL, so the sitemap cannot advertise a page that 404s.
func TestSitemapDropsADeactivatedService(t *testing.T) {
	env := testsupport.New(t)

	env.Exec("UPDATE services SET is_active = 0 WHERE slug = ?", testsupport.SlugBekam)

	body := serve(t, env.Deps.Sitemap, "/sitemap.xml").Body.String()
	if strings.Contains(body, testsupport.SlugBekam) {
		t.Error("the sitemap still lists a deactivated layanan")
	}
	if !strings.Contains(body, testsupport.SlugUrut) {
		t.Error("the sitemap lost an active layanan")
	}
}

// TestSitemapEscapesASlug. It is built with encoding/xml rather than a template
// precisely so escaping is the encoder's problem: an ampersand in a slug would
// otherwise produce a document no parser accepts.
func TestSitemapEscapesASlug(t *testing.T) {
	env := testsupport.New(t)

	// Slugify would never produce this, but the column takes it and the sitemap
	// must survive whatever is in the column.
	env.Exec("UPDATE services SET slug = ? WHERE slug = ?", "urut-&-bekam", testsupport.SlugUrut)

	rec := serve(t, env.Deps.Sitemap, "/sitemap.xml")

	var doc struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("an ampersand in a slug broke the document: %v\n%s", err, rec.Body)
	}

	var found bool
	for _, u := range doc.URLs {
		if strings.HasSuffix(u.Loc, "urut-&-bekam") {
			found = true
		}
	}
	if !found {
		t.Error("the escaped slug did not round-trip through the parser")
	}
	// Raw in the bytes, escaped in the document.
	if strings.Contains(rec.Body.String(), "/urut-&-bekam<") {
		t.Error("the ampersand was written unescaped")
	}
}

func TestFavicon(t *testing.T) {
	env := testsupport.New(t)

	rec := serve(t, env.Deps.Favicon, "/favicon.ico")
	if rec.Code != http.StatusFound && rec.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want a redirect to the SVG", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, ".svg") {
		t.Errorf("Location = %q, want the SVG", loc)
	}
}
