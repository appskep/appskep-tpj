package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The terapis module. Display-only, so none of this touches a slot — but
// AssertInvariant still ends every test, because the rule is unconditional and a
// module that quietly started writing to bookings should fail here first.

// TestTherapistPublishLifecycle is the base case: created, visible, hidden.
func TestTherapistPublishLifecycle(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	id, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:            "Rina Kartika",
		Specialization:  "Terapi kehamilan",
		Bio:             "Paragraf satu.\n\nParagraf dua.",
		YearsExperience: "7",
		IsActive:        true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, err := env.Deps.Therapists.GetActiveBySlug(ctx, "rina-kartika")
	if err != nil {
		t.Fatalf("GetActiveBySlug: %v", err)
	}
	if row.ID != id {
		t.Errorf("GetActiveBySlug returned id %d, want %d", row.ID, id)
	}
	if !row.YearsExperience.Valid || row.YearsExperience.Int32 != 7 {
		t.Errorf("years_experience = %+v, want a valid 7", row.YearsExperience)
	}

	if _, err := env.Deps.Therapists.SetActive(ctx, id, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	// Deactivating must take the public URL down, not merely unlist it.
	if _, err := env.Deps.Therapists.GetActiveBySlug(ctx, "rina-kartika"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("GetActiveBySlug after deactivation = %v, want ErrNotFound", err)
	}
	// ...while the admin can still reach it.
	if _, err := env.Deps.Therapists.Get(ctx, id); err != nil {
		t.Errorf("Get after deactivation: %v", err)
	}

	env.AssertInvariant()
}

// TestTherapistSlugIsAllocatedOnceAndStaysPut covers both halves of the slug
// rule: a collision gets a suffix, and a rename never moves the URL.
func TestTherapistSlugIsAllocatedOnceAndStaysPut(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	first, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{Name: "Budi Hartono", IsActive: true})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{Name: "Budi Hartono", IsActive: true})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	a, _ := env.Deps.Therapists.Get(ctx, first)
	b, _ := env.Deps.Therapists.Get(ctx, second)
	if a.Slug != "budi-hartono" {
		t.Errorf("first slug = %q, want %q", a.Slug, "budi-hartono")
	}
	if b.Slug != "budi-hartono-2" {
		t.Errorf("second slug = %q, want %q", b.Slug, "budi-hartono-2")
	}

	// A rename must not follow: the slug is the public URL and nothing redirects
	// the old one.
	if err := env.Deps.Therapists.Update(ctx, first, service.TherapistInput{
		Name:     "Budi Hartono Wijaya",
		IsActive: true,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	renamed, _ := env.Deps.Therapists.Get(ctx, first)
	if renamed.Slug != "budi-hartono" {
		t.Errorf("slug after rename = %q, want it unchanged at %q", renamed.Slug, "budi-hartono")
	}
	if renamed.Name != "Budi Hartono Wijaya" {
		t.Errorf("name after rename = %q, want it updated", renamed.Name)
	}

	env.AssertInvariant()
}

// TestTherapistTagsRoundTripAndAreIdempotent is the delete-all-then-insert
// rewrite: the stored set is always exactly the last submitted set, and
// resubmitting the same form changes nothing.
func TestTherapistTagsRoundTripAndAreIdempotent(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	urut := env.Service("urut-therapeutic")
	massage := env.Service("massage-therapeutic")

	id, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:       "Dewi Lestari",
		IsActive:   true,
		ServiceIDs: []int64{urut.ID, massage.ID},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got := sortedIDs(t, env, id); len(got) != 2 {
		t.Fatalf("tags after create = %v, want 2", got)
	}

	// Narrow to one.
	if err := env.Deps.Therapists.Update(ctx, id, service.TherapistInput{
		Name:       "Dewi Lestari",
		IsActive:   true,
		ServiceIDs: []int64{massage.ID},
	}); err != nil {
		t.Fatalf("Update to one tag: %v", err)
	}
	got := sortedIDs(t, env, id)
	if len(got) != 1 || got[0] != massage.ID {
		t.Fatalf("tags after narrowing = %v, want [%d]", got, massage.ID)
	}

	// Resubmitting the identical set must not duplicate the row — the whole
	// reason the rewrite is delete-all-then-insert rather than a blind INSERT.
	if err := env.Deps.Therapists.Update(ctx, id, service.TherapistInput{
		Name:       "Dewi Lestari",
		IsActive:   true,
		ServiceIDs: []int64{massage.ID, massage.ID},
	}); err != nil {
		t.Fatalf("Update with a repeated set: %v", err)
	}
	if got := sortedIDs(t, env, id); len(got) != 1 {
		t.Errorf("tags after resubmitting = %v, want exactly 1", got)
	}

	// Clearing every box must clear every tag.
	if err := env.Deps.Therapists.Update(ctx, id, service.TherapistInput{
		Name:     "Dewi Lestari",
		IsActive: true,
	}); err != nil {
		t.Fatalf("Update with no tags: %v", err)
	}
	if got := sortedIDs(t, env, id); len(got) != 0 {
		t.Errorf("tags after clearing = %v, want none", got)
	}

	env.AssertInvariant()
}

// TestUntaggableServiceIDsAreDropped is the "a disabled input is an affordance,
// not a guarantee" rule applied to a checkbox group: an id the form would never
// have offered is dropped, not written and not a 500.
func TestUntaggableServiceIDsAreDropped(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	urut := env.Service("urut-therapeutic")
	bekam := env.Service("bekam-therapeutic")

	// Deactivate one, so it is a real id that the form no longer offers — the
	// exact race a slow admin hits, not a synthetic missing row.
	if _, err := env.Deps.Catalog.SetActive(ctx, bekam.ID, false); err != nil {
		t.Fatalf("deactivating a service: %v", err)
	}

	id, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:     "Anwar Saputra",
		IsActive: true,
		// One good, one deactivated, one that never existed.
		ServiceIDs: []int64{urut.ID, bekam.ID, 999_999},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := sortedIDs(t, env, id)
	if len(got) != 1 || got[0] != urut.ID {
		t.Errorf("tags = %v, want only the active service %d", got, urut.ID)
	}

	env.AssertInvariant()
}

// TestDeletingATaggedServiceIsNotBlocked is the regression test for the CASCADE
// decision in 0001_schema.sql.
//
// Under ON DELETE RESTRICT the DELETE raises a foreign key violation that
// Catalog.Delete already translates to ErrHasBookings — telling the admin the
// layanan has bookings when it has none. The therapist must survive; only the
// join row goes.
func TestDeletingATaggedServiceIsNotBlocked(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	urut := env.Service("urut-therapeutic")

	id, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:       "Fajar Nugroho",
		IsActive:   true,
		ServiceIDs: []int64{urut.ID},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	switch err := env.Deps.Catalog.Delete(ctx, urut.ID); {
	case errors.Is(err, service.ErrHasBookings):
		t.Fatal("deleting a tagged layanan reported ErrHasBookings; " +
			"fk_ts_service must be ON DELETE CASCADE, not RESTRICT")
	case err != nil:
		t.Fatalf("Catalog.Delete: %v", err)
	}

	if _, err := env.Deps.Therapists.Get(ctx, id); err != nil {
		t.Errorf("therapist should survive its layanan being deleted, got %v", err)
	}
	if got := sortedIDs(t, env, id); len(got) != 0 {
		t.Errorf("tags after the layanan was deleted = %v, want none", got)
	}

	env.AssertInvariant()
}

// TestDeletingATherapistClearsTagsAndPhoto covers the two things that outlive a
// row if the delete only removes the row: the join rows and the uploaded file.
func TestDeletingATherapistClearsTagsAndPhoto(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	urut := env.Service("urut-therapeutic")

	id, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:       "Lina Marlina",
		IsActive:   true,
		ServiceIDs: []int64{urut.ID},
		Image:      testsupport.PNGUpload(t, "foto.png"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, err := env.Deps.Therapists.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !row.ImagePath.Valid {
		t.Fatal("image_path is NULL, so the upload never landed")
	}
	photo := filepath.Join(env.UploadRoot, filepath.FromSlash(row.ImagePath.String))
	if _, err := os.Stat(photo); err != nil {
		t.Fatalf("uploaded photo missing before delete: %v", err)
	}

	if err := env.Deps.Therapists.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := env.Deps.Therapists.Get(ctx, id); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if n := env.CountRows("therapist_services", "therapist_id = ?", id); n != 0 {
		t.Errorf("%d join rows survived the delete, want 0", n)
	}
	if _, err := os.Stat(photo); !os.IsNotExist(err) {
		t.Errorf("photo still on disk after delete (stat err = %v)", err)
	}

	env.AssertInvariant()
}

// TestTherapistPublicReadsAgree checks the three public reads describe the same
// world: the grid's tag map, the profile's service list, and the reverse lookup
// the layanan page uses.
func TestTherapistPublicReadsAgree(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	urut := env.Service("urut-therapeutic")

	id, err := env.Deps.Therapists.Create(ctx, service.TherapistInput{
		Name:       "Hendra Wijaya",
		IsActive:   true,
		ServiceIDs: []int64{urut.ID},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tags, err := env.Deps.Therapists.ServiceTagsForActive(ctx)
	if err != nil {
		t.Fatalf("ServiceTagsForActive: %v", err)
	}
	if got := tags[id]; len(got) != 1 || got[0].Slug != urut.Slug {
		t.Errorf("grid tags = %+v, want one %q", got, urut.Slug)
	}

	services, err := env.Deps.Therapists.ServicesFor(ctx, id)
	if err != nil {
		t.Fatalf("ServicesFor: %v", err)
	}
	if len(services) != 1 || services[0].ID != urut.ID {
		t.Errorf("profile services = %+v, want one %d", services, urut.ID)
	}

	forService, err := env.Deps.Therapists.ForService(ctx, urut.ID)
	if err != nil {
		t.Fatalf("ForService: %v", err)
	}
	if !containsTherapist(forService, id) {
		t.Errorf("ForService(%d) did not include therapist %d", urut.ID, id)
	}

	// Deactivating must drop them out of all three at once.
	if _, err := env.Deps.Therapists.SetActive(ctx, id, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	tags, err = env.Deps.Therapists.ServiceTagsForActive(ctx)
	if err != nil {
		t.Fatalf("ServiceTagsForActive after deactivation: %v", err)
	}
	if got := tags[id]; len(got) != 0 {
		t.Errorf("grid tags after deactivation = %+v, want none", got)
	}
	forService, err = env.Deps.Therapists.ForService(ctx, urut.ID)
	if err != nil {
		t.Fatalf("ForService after deactivation: %v", err)
	}
	if containsTherapist(forService, id) {
		t.Errorf("ForService still lists deactivated therapist %d", id)
	}

	env.AssertInvariant()
}

func sortedIDs(t *testing.T, env *testsupport.Env, therapistID int64) []int64 {
	t.Helper()

	ids, err := env.Deps.Therapists.ServiceIDs(context.Background(), therapistID)
	if err != nil {
		t.Fatalf("ServiceIDs: %v", err)
	}
	return ids
}

func containsTherapist(rows []sqlc.Therapist, id int64) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
