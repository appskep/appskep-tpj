package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/app/public"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The public nav's Terapis entry is data-conditional: the section is content an
// operator fills in, and until they have, a menu entry leading to "Belum ada
// profil terapis" advertises a room with nothing in it.
//
// The answer is a cached flag on service.Therapists, refreshed by its four write
// methods. These tests exist because a cache is only as good as its
// invalidation, and every one of them drives a different writer.
//
// NOTE for anyone extending this file: testsupport.Reset and Env.Exec write SQL
// directly and bypass the service, so they do NOT refresh the flag. Change
// therapist rows through service.Therapists or the assertions below will be
// measuring a stale cache rather than the code.

// landing renders GET / and returns the HTML. The landing page carries the
// desktop nav, the mobile drawer and the footer, which is all three surfaces
// that advertise the section.
func landing(t *testing.T, env *testsupport.Env) string {
	t.Helper()

	rec := httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// terapisLinks counts the links to the section on a page. Three when the section
// is advertised: the desktop nav, the mobile drawer, and the footer. The count
// matters rather than mere presence — the drawer is a second {{range publicNav}}
// and a fix applied to one and not the other is invisible at desktop width.
func terapisLinks(body string) int {
	return strings.Count(body, `href="/terapis"`)
}

func sitemapMentionsTerapis(t *testing.T, env *testsupport.Env) bool {
	t.Helper()
	return strings.Contains(serve(t, env.Deps.Sitemap, "/sitemap.xml").Body.String(), "/terapis")
}

// The seeded state: three published therapists, so the section is advertised
// everywhere. This is the control the rest of the file measures against.
func TestTherapistNavAppearsWhenThereArePublishedTherapists(t *testing.T) {
	env := testsupport.New(t)

	if n := terapisLinks(landing(t, env)); n != 3 {
		t.Errorf("landing page has %d links to /terapis, want 3 (nav, drawer, footer)", n)
	}
	if !sitemapMentionsTerapis(t, env) {
		t.Error("sitemap omits /terapis while therapists are published")
	}

	env.AssertInvariant()
}

// SetActive is the obvious unpublish path — the toggle on /admin/terapis.
func TestTherapistNavHidesWhenAllAreDeactivated(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	seeded, err := env.Deps.Therapists.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, row := range seeded {
		if _, err := env.Deps.Therapists.SetActive(ctx, row.ID, false); err != nil {
			t.Fatalf("SetActive(%d, false): %v", row.ID, err)
		}
	}

	if n := terapisLinks(landing(t, env)); n != 0 {
		t.Errorf("landing page still has %d links to /terapis with none published, want 0", n)
	}
	if sitemapMentionsTerapis(t, env) {
		t.Error("sitemap still lists /terapis with no therapists published")
	}

	env.AssertInvariant()
}

// Update is the hook that is easy to forget: its name says nothing about
// publishing, but TherapistInput carries IsActive, so saving the edit form with
// the "Aktif" box cleared takes the last therapist offline without SetActive
// ever running.
//
// This is the test to break the code against — the deactivate, delete and create
// tests all still pass with Update's refresh call missing.
func TestTherapistNavHidesWhenUpdateUnpublishesTheLastOne(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	seeded, err := env.Deps.Therapists.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(seeded) == 0 {
		t.Fatal("the seed published no therapists; this test has nothing to unpublish")
	}

	// All but one through SetActive, then the last one through Update — so the
	// transition to zero is Update's alone.
	for _, row := range seeded[1:] {
		if _, err := env.Deps.Therapists.SetActive(ctx, row.ID, false); err != nil {
			t.Fatalf("SetActive(%d, false): %v", row.ID, err)
		}
	}
	if n := terapisLinks(landing(t, env)); n != 3 {
		t.Fatalf("the section should still be advertised with one therapist left, got %d links", n)
	}

	last := seeded[0]
	if err := env.Deps.Therapists.Update(ctx, last.ID, service.TherapistInput{
		Name:     last.Name,
		IsActive: false,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if n := terapisLinks(landing(t, env)); n != 0 {
		t.Errorf("Update unpublished the last therapist but the nav still has %d links; "+
			"the refresh hook in Therapists.Update is missing", n)
	}

	env.AssertInvariant()
}

func TestTherapistNavHidesWhenTheLastOneIsDeleted(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	seeded, err := env.Deps.Therapists.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, row := range seeded {
		if err := env.Deps.Therapists.Delete(ctx, row.ID); err != nil {
			t.Fatalf("Delete(%d): %v", row.ID, err)
		}
	}

	if n := terapisLinks(landing(t, env)); n != 0 {
		t.Errorf("landing page still has %d links to /terapis after deleting them all, want 0", n)
	}

	env.AssertInvariant()
}

// The flag must not be one-way: publishing the first therapist has to bring the
// section back without a restart, which is the whole point of refreshing rather
// than reading once at boot.
func TestTherapistNavReturnsWhenOneIsPublishedAgain(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	seeded, err := env.Deps.Therapists.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, row := range seeded {
		if _, err := env.Deps.Therapists.SetActive(ctx, row.ID, false); err != nil {
			t.Fatalf("SetActive(%d, false): %v", row.ID, err)
		}
	}
	if n := terapisLinks(landing(t, env)); n != 0 {
		t.Fatalf("precondition failed: %d links with none published", n)
	}

	// Through Create, so this covers the fresh-install path — an operator adding
	// their very first therapist.
	if _, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:     "Rina Kartika",
		IsActive: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if n := terapisLinks(landing(t, env)); n != 3 {
		t.Errorf("publishing a therapist left %d links to /terapis, want 3", n)
	}
	if !sitemapMentionsTerapis(t, env) {
		t.Error("sitemap omits /terapis after one was published")
	}

	env.AssertInvariant()
}

// Hiding the menu must not take the route down with it. The page still serves
// its empty state — the empty partial always carries a way forward, and an
// operator following "Lihat di situs publik" deserves an explanation rather than
// a 404. Pinned here so a later tidy-up cannot quietly change it.
func TestTerapisPageStillServesWhileTheNavIsHidden(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	seeded, err := env.Deps.Therapists.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, row := range seeded {
		if _, err := env.Deps.Therapists.SetActive(ctx, row.ID, false); err != nil {
			t.Fatalf("SetActive(%d, false): %v", row.ID, err)
		}
	}

	rec := httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terapis", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /terapis = %d with none published, want 200 and the empty state", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Belum ada profil terapis") {
		t.Error("GET /terapis rendered 200 but not the empty state")
	}

	// A deactivated profile is still a 404 — that half is unchanged.
	rec = httptest.NewRecorder()
	public.Routes(env.Deps).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/terapis/"+seeded[0].Slug, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET a deactivated profile = %d, want 404", rec.Code)
	}

	env.AssertInvariant()
}
