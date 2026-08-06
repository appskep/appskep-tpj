package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/upload"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Therapists is the business logic behind the terapis module: the profile
// content shown on /terapis and the rules the admin form is validated against.
//
// It is display-only. Nothing here is read by the booking transaction — parallel
// therapists are modelled as schedule_slots.capacity, and this is the marketing
// side of the same fact. Adding a therapist can never affect availability.
//
// The shape is catalog.go's, deliberately: validation in the service so create
// and edit cannot drift, slug allocated once at creation, and the image ordering
// that keeps the table and the uploads directory consistent.
type Therapists struct {
	store  *repository.Store
	images *upload.ImageStore
	log    *slog.Logger

	// hasActive answers "does the public terapis section have anything in it",
	// which decides whether the site advertises it in the nav and the footer at
	// all. Cached because the renderer asks on every page render and the answer
	// changes a handful of times ever — the same trade Settings makes, and for
	// the same reason.
	//
	// atomic.Bool rather than a mutex: one word, written by four admin actions
	// and read by every response.
	hasActive atomic.Bool
}

// NewTherapists builds the service and performs the initial presence load.
//
// It takes a context and returns an error for the reason NewSettings does: a
// wrong flag is a defect no request reports. The menu is simply there or not
// there, nothing looks broken, and the site would serve that way until someone
// happened to notice.
func NewTherapists(ctx context.Context, store *repository.Store, images *upload.ImageStore, log *slog.Logger) (*Therapists, error) {
	t := &Therapists{store: store, images: images, log: log}

	present, err := store.Queries.HasActiveTherapists(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading therapist presence: %w", err)
	}
	t.hasActive.Store(present)

	return t, nil
}

// HasActive reports whether any therapist is published. No context and no query:
// it is read on the render path, which is why the value is cached.
func (t *Therapists) HasActive() bool { return t.hasActive.Load() }

// refresh re-reads the presence flag after a write.
//
// Best-effort, and deliberately so — Audit.Record's contract. The write it
// follows has already committed, so returning an error here would report a 500
// for a save that worked and send the admin back to submit it again. A failed
// refresh costs a stale menu until the next write or the next restart, which is
// strictly better than that.
//
// Called as the last statement of every method that can change the answer:
// Create, Update, SetActive and Delete. Update counts because TherapistInput
// carries IsActive, so it is a publish path even though its name does not say so.
func (t *Therapists) refresh(ctx context.Context) {
	present, err := t.store.Queries.HasActiveTherapists(ctx)
	if err != nil {
		t.log.WarnContext(ctx, "therapists: refreshing presence flag", slog.Any("error", err))
		return
	}
	t.hasActive.Store(present)
}

// Experience bounds, mirroring chk_therapists_experience in 0001_schema.sql.
// Checked here as well as by the database so the admin gets a message next to
// the field instead of a 500 from a constraint violation.
const (
	MinYearsExperience = 0
	MaxYearsExperience = 70
)

// therapistSlugFallback is the stem used when a name contains nothing a slug can
// be built from. The uniqueness loop then makes it terapis-2, terapis-3.
const therapistSlugFallback = "terapis"

// TherapistInput is one submitted form, still as raw strings — the service
// parses them so the rules and the messages that describe them live together.
//
// ServiceIDs is the "layanan yang dikuasai" checkbox set. Image is nil unless a
// new file was uploaded; RemoveImage clears an existing one.
type TherapistInput struct {
	Name            string
	Specialization  string
	Bio             string
	Certifications  string
	YearsExperience string
	SortOrder       string
	IsActive        bool
	ServiceIDs      []int64
	Image           *multipart.FileHeader
	RemoveImage     bool
}

// TherapistListResult is one page of the admin list plus what the pagination
// partial needs. Page and TotalPages are int, not sqlc's int32, because the
// partial reaches them through `add`, which is func(int, int) int.
type TherapistListResult struct {
	Items      []sqlc.Therapist
	Page       int
	TotalPages int
	Total      int64
}

// ServiceTag is the minimum a therapist card needs to name a layanan and link to
// it. The grid query selects these four columns rather than whole service rows,
// so a card cannot accidentally render a price it was never given.
type ServiceTag struct {
	ID   int64
	Slug string
	Name string
}

// parsedTherapist is one validated form, ready for the database.
type parsedTherapist struct {
	name           string
	specialization sql.NullString
	bio            sql.NullString
	certifications sql.NullString
	years          sql.NullInt32
	sortOrder      int32
}

// validateTherapist applies every field rule at once, so the admin sees all the
// problems on one submit rather than discovering them one at a time.
//
// The keys are the HTML input names, which is what lets the form render each
// message beside the box that caused it.
func validateTherapist(in TherapistInput) (parsedTherapist, *ValidationError) {
	ve := NewValidationError()
	var p parsedTherapist

	p.name = strings.TrimSpace(in.Name)
	requiredMaxLen(ve, "name", "Nama terapis", p.name, maxNameLen)

	spec := strings.TrimSpace(in.Specialization)
	if maxLen(ve, "specialization", "Spesialisasi", spec, maxNameLen) && spec != "" {
		p.specialization = sql.NullString{String: spec, Valid: true}
	}

	// bio and certifications are TEXT: long enough that any cap would be
	// arbitrary, and both are prose the admin writes for the public page.
	if bio := strings.TrimSpace(in.Bio); bio != "" {
		p.bio = sql.NullString{String: bio, Valid: true}
	}
	if certs := strings.TrimSpace(in.Certifications); certs != "" {
		p.certifications = sql.NullString{String: certs, Valid: true}
	}

	// Optional, and blank is not zero: "belum diisi" and "baru mulai" are
	// different things to show on a profile, which is why the column is nullable.
	if years := strings.TrimSpace(in.YearsExperience); years != "" {
		switch n, err := strconv.Atoi(years); {
		case err != nil:
			ve.Add("years_experience", "Pengalaman harus berupa angka tahun.")
		case n < MinYearsExperience || n > MaxYearsExperience:
			ve.Add("years_experience", fmt.Sprintf("Pengalaman harus antara %d dan %d tahun.",
				MinYearsExperience, MaxYearsExperience))
		default:
			p.years = sql.NullInt32{Int32: int32(n), Valid: true}
		}
	}

	// Sort order is optional; a blank field means "put it first".
	if so := strings.TrimSpace(in.SortOrder); so != "" {
		n, err := strconv.Atoi(so)
		if err != nil || n < 0 {
			ve.Add("sort_order", "Urutan harus berupa angka 0 atau lebih.")
		} else {
			p.sortOrder = int32(n)
		}
	}

	if ve.Any() {
		return parsedTherapist{}, ve
	}
	return p, nil
}

// List returns one page of therapists matching the search term.
func (t *Therapists) List(ctx context.Context, q ListQuery) (TherapistListResult, error) {
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}

	// EscapeLike keeps a typed "%" or "_" a literal character rather than a
	// wildcard that would quietly ignore the rest of the search.
	search := "%"
	if term := strings.TrimSpace(q.Search); term != "" {
		search = "%" + util.EscapeLike(term) + "%"
	}

	total, err := t.store.Queries.CountTherapistsAdmin(ctx, search)
	if err != nil {
		return TherapistListResult{}, fmt.Errorf("counting therapists: %w", err)
	}

	totalPages := int((total + int64(q.PageSize) - 1) / int64(q.PageSize))
	// Clamp after counting: a deletion or a filter can leave ?page= past the end,
	// and showing the last page beats showing an empty one with a working "next".
	if totalPages > 0 && q.Page > totalPages {
		q.Page = totalPages
	}

	items, err := t.store.Queries.ListTherapistsAdmin(ctx, sqlc.ListTherapistsAdminParams{
		Search: search,
		Limit:  int32(q.PageSize),
		Offset: int32((q.Page - 1) * q.PageSize),
	})
	if err != nil {
		return TherapistListResult{}, fmt.Errorf("listing therapists: %w", err)
	}

	return TherapistListResult{Items: items, Page: q.Page, TotalPages: totalPages, Total: total}, nil
}

// Get returns one therapist, or ErrNotFound.
func (t *Therapists) Get(ctx context.Context, id int64) (sqlc.Therapist, error) {
	row, err := t.store.Queries.GetTherapist(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Therapist{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Therapist{}, fmt.Errorf("getting therapist %d: %w", id, err)
	}
	return row, nil
}

// ListActive returns every therapist the public site may show, in sort order.
func (t *Therapists) ListActive(ctx context.Context) ([]sqlc.Therapist, error) {
	items, err := t.store.Queries.ListActiveTherapists(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing active therapists: %w", err)
	}
	return items, nil
}

// GetActiveBySlug returns one publicly visible therapist, or ErrNotFound.
//
// The is_active predicate is in the query, so deactivating a therapist in the
// admin panel takes their public URL down on the next request.
func (t *Therapists) GetActiveBySlug(ctx context.Context, slug string) (sqlc.Therapist, error) {
	row, err := t.store.Queries.GetActiveTherapistBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Therapist{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Therapist{}, fmt.Errorf("getting therapist by slug %q: %w", slug, err)
	}
	return row, nil
}

// ServiceIDs returns the layanan a therapist is tagged with, including any that
// have since been deactivated — the admin form needs the stored truth.
func (t *Therapists) ServiceIDs(ctx context.Context, id int64) ([]int64, error) {
	ids, err := t.store.Queries.ListServiceIDsForTherapist(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("listing service ids for therapist %d: %w", id, err)
	}
	return ids, nil
}

// ServicesFor returns the active layanan a therapist handles, for their profile
// page.
func (t *Therapists) ServicesFor(ctx context.Context, id int64) ([]sqlc.Service, error) {
	items, err := t.store.Queries.ListServicesForTherapist(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("listing services for therapist %d: %w", id, err)
	}
	return items, nil
}

// ServiceTagsForActive returns every active therapist's layanan tags in one
// query, keyed by therapist id.
//
// One round trip for the whole grid: the obvious alternative is ServicesFor per
// card, which is a query per therapist on the busiest page of the section.
func (t *Therapists) ServiceTagsForActive(ctx context.Context) (map[int64][]ServiceTag, error) {
	rows, err := t.store.Queries.ListServiceTagsForActiveTherapists(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing service tags: %w", err)
	}

	tags := make(map[int64][]ServiceTag, len(rows))
	for _, row := range rows {
		tags[row.TherapistID] = append(tags[row.TherapistID], ServiceTag{
			ID:   row.ID,
			Slug: row.Slug,
			Name: row.Name,
		})
	}
	return tags, nil
}

// ForService returns the active therapists who handle one layanan, for the strip
// on its detail page.
func (t *Therapists) ForService(ctx context.Context, serviceID int64) ([]sqlc.Therapist, error) {
	items, err := t.store.Queries.ListActiveTherapistsForService(ctx, serviceID)
	if err != nil {
		return nil, fmt.Errorf("listing therapists for service %d: %w", serviceID, err)
	}
	return items, nil
}

// Create validates the form, stores any uploaded photo, and inserts the row with
// its service tags.
//
// Text fields are validated before the image is touched, so a missing name never
// leaves an orphan file in the uploads directory.
func (t *Therapists) Create(ctx context.Context, in TherapistInput) (int64, error) {
	p, ve := validateTherapist(in)
	if ve != nil {
		return 0, ve
	}

	tagIDs, err := t.taggableIDs(ctx, in.ServiceIDs)
	if err != nil {
		return 0, err
	}

	imagePath, err := t.saveImage(in)
	if err != nil {
		return 0, err
	}

	id, err := t.insert(ctx, p, imagePath, in.IsActive, tagIDs)
	if err != nil {
		// The row never landed, so the file it would have pointed at is garbage.
		t.discard(imagePath)
		return 0, err
	}

	// The first published therapist is what puts the section into the public nav.
	t.refresh(ctx)
	return id, nil
}

// insert runs the slug allocation, the INSERT and the tag writes in one
// transaction, retrying once when another admin took the slug in between.
func (t *Therapists) insert(
	ctx context.Context,
	p parsedTherapist,
	imagePath sql.NullString,
	isActive bool,
	tagIDs []int64,
) (int64, error) {
	for range 2 {
		var id int64

		err := t.store.WithTx(ctx, func(q *sqlc.Queries) error {
			slug, serr := allocateTherapistSlug(ctx, q, p.name, 0)
			if serr != nil {
				return serr
			}

			res, ierr := q.CreateTherapist(ctx, sqlc.CreateTherapistParams{
				Slug:            slug,
				Name:            p.name,
				Specialization:  p.specialization,
				Bio:             p.bio,
				Certifications:  p.certifications,
				YearsExperience: p.years,
				ImagePath:       imagePath,
				IsActive:        isActive,
				SortOrder:       p.sortOrder,
			})
			if ierr != nil {
				return ierr
			}

			if id, ierr = res.LastInsertId(); ierr != nil {
				return ierr
			}
			return writeTags(ctx, q, id, tagIDs)
		})

		if err == nil {
			return id, nil
		}
		// allocateTherapistSlug's check takes no lock — uq_therapists_slug is what
		// actually enforces uniqueness. Two admins creating the same name at the
		// same moment is the one case that reaches here, and a retry resolves it
		// because the winner's row is now visible to the loser's check.
		if repository.IsDuplicateKeyOn(err, "uq_therapists_slug") {
			continue
		}
		return 0, fmt.Errorf("creating therapist: %w", err)
	}

	ve := NewValidationError()
	ve.Add("name", "Nama terapis bentrok dengan terapis lain. Coba ubah sedikit namanya.")
	return 0, ve
}

// Update validates the form and writes it, handling a photo replacement or
// removal separately from the rest of the fields.
//
// The row and its tags are written in one transaction — unlike Catalog.Update,
// which has no second table to keep in step. A half-applied save would leave the
// profile describing one thing and its layanan chips another.
//
// No SELECT ... FOR UPDATE: two admins editing the same therapist is benign, and
// the lock-ordering rule exists for rows the booking path contends on. Nothing
// here is one.
//
// The slug is deliberately not recomputed. It is the public URL of the profile,
// and regenerating it on a rename would break links, bookmarks and search
// results with no redirect behind them.
func (t *Therapists) Update(ctx context.Context, id int64, in TherapistInput) error {
	current, err := t.Get(ctx, id)
	if err != nil {
		return err
	}

	p, ve := validateTherapist(in)
	if ve != nil {
		return ve
	}

	tagIDs, err := t.taggableIDs(ctx, in.ServiceIDs)
	if err != nil {
		return err
	}

	imagePath, err := t.saveImage(in)
	if err != nil {
		return err
	}

	err = t.store.WithTx(ctx, func(q *sqlc.Queries) error {
		if uerr := q.UpdateTherapist(ctx, sqlc.UpdateTherapistParams{
			Name:            p.name,
			Specialization:  p.specialization,
			Bio:             p.bio,
			Certifications:  p.certifications,
			YearsExperience: p.years,
			IsActive:        in.IsActive,
			SortOrder:       p.sortOrder,
			ID:              id,
		}); uerr != nil {
			return uerr
		}
		return writeTags(ctx, q, id, tagIDs)
	})
	if err != nil {
		t.discard(imagePath)
		return fmt.Errorf("updating therapist %d: %w", id, err)
	}

	// Update is a publish path too: TherapistInput carries IsActive, so this can
	// take the last published therapist offline without SetActive ever running.
	// Easy to miss, which is why it has a test of its own.
	t.refresh(ctx)

	return t.applyImageChange(ctx, id, current.ImagePath, imagePath, in.RemoveImage)
}

// writeTags replaces a therapist's layanan set.
//
// Delete-all-then-insert rather than a diff: the set is a handful of rows, and
// rewriting is idempotent — resubmitting the same form produces the same rows,
// which is the same property the admin toggles get from posting the value they
// want rather than a flip.
func writeTags(ctx context.Context, q *sqlc.Queries, therapistID int64, serviceIDs []int64) error {
	if err := q.DeleteTherapistServices(ctx, therapistID); err != nil {
		return fmt.Errorf("clearing tags for therapist %d: %w", therapistID, err)
	}
	for _, sid := range serviceIDs {
		if err := q.AddTherapistService(ctx, sqlc.AddTherapistServiceParams{
			TherapistID: therapistID,
			ServiceID:   sid,
		}); err != nil {
			return fmt.Errorf("tagging therapist %d with service %d: %w", therapistID, sid, err)
		}
	}
	return nil
}

// taggableIDs narrows a submitted checkbox set to the layanan that may actually
// be tagged, dropping the rest silently and de-duplicating what remains.
//
// The checkboxes are rendered from this same list, so an id outside it means
// either a hand-crafted POST or a layanan deactivated between the form being
// rendered and submitted. Neither is the admin's mistake and neither should be a
// 500 from a foreign key violation — the "a disabled input is an affordance, not
// a guarantee" rule, applied to a checkbox group.
//
// The consequence, which the form says out loud: editing a therapist who is
// tagged to a since-deactivated layanan drops that tag on save.
func (t *Therapists) taggableIDs(ctx context.Context, submitted []int64) ([]int64, error) {
	if len(submitted) == 0 {
		return nil, nil
	}

	active, err := t.store.Queries.ListActiveServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing active services: %w", err)
	}

	allowed := make(map[int64]bool, len(active))
	for _, s := range active {
		allowed[s.ID] = true
	}

	seen := make(map[int64]bool, len(submitted))
	out := make([]int64, 0, len(submitted))
	for _, id := range submitted {
		if allowed[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// applyImageChange writes the new image_path and then deletes the file the row
// used to point at.
//
// The ordering is the point: write the new file, update the row, delete the old
// file. Interrupted anywhere, that leaves at worst an orphaned file — invisible,
// costing only disk. The other order leaves a row pointing at a file that is
// gone, which is a broken image on the public site.
func (t *Therapists) applyImageChange(ctx context.Context, id int64, old, uploaded sql.NullString, remove bool) error {
	switch {
	case uploaded.Valid:
		if err := t.setImage(ctx, id, uploaded); err != nil {
			t.discard(uploaded)
			return err
		}
		t.discard(old) // best effort; an orphan is harmless
	case remove && old.Valid:
		if err := t.setImage(ctx, id, sql.NullString{}); err != nil {
			return err
		}
		t.discard(old)
	}
	return nil
}

func (t *Therapists) setImage(ctx context.Context, id int64, path sql.NullString) error {
	if err := t.store.Queries.UpdateTherapistImage(ctx, sqlc.UpdateTherapistImageParams{
		ImagePath: path,
		ID:        id,
	}); err != nil {
		return fmt.Errorf("updating photo for therapist %d: %w", id, err)
	}
	return nil
}

// SetActive flips is_active and returns the refreshed row, which the turbo
// stream re-renders the toggle cell from.
func (t *Therapists) SetActive(ctx context.Context, id int64, v bool) (sqlc.Therapist, error) {
	if _, err := t.Get(ctx, id); err != nil {
		return sqlc.Therapist{}, err
	}
	if err := t.store.Queries.SetTherapistActive(ctx, sqlc.SetTherapistActiveParams{
		IsActive: v,
		ID:       id,
	}); err != nil {
		return sqlc.Therapist{}, fmt.Errorf("setting therapist %d active: %w", id, err)
	}

	// Publishing the first, or unpublishing the last, moves the section in and
	// out of the public nav.
	t.refresh(ctx)

	return t.Get(ctx, id)
}

// Delete removes a therapist and their photo.
//
// Simpler than Catalog.Delete because nothing references a therapist except
// therapist_services, which CASCADEs — there is no booking to strand and so no
// ErrHasBookings path.
func (t *Therapists) Delete(ctx context.Context, id int64) error {
	row, err := t.Get(ctx, id)
	if err != nil {
		return err
	}

	res, err := t.store.Queries.DeleteTherapist(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting therapist %d: %w", id, err)
	}
	if rows, rerr := res.RowsAffected(); rerr == nil && rows == 0 {
		return ErrNotFound
	}

	// Only once the row is gone, so a failed delete never strands the photo.
	t.discard(row.ImagePath)

	// Deleting the last one takes the section out of the public nav.
	t.refresh(ctx)
	return nil
}

// saveImage stores an uploaded file, translating the uploader's errors into
// messages for the file input.
func (t *Therapists) saveImage(in TherapistInput) (sql.NullString, error) {
	if in.Image == nil {
		return sql.NullString{}, nil
	}

	rel, err := t.images.Save(in.Image)
	if err != nil {
		ve := NewValidationError()
		switch {
		case errors.Is(err, upload.ErrTooLarge):
			ve.Add("image", fmt.Sprintf("Ukuran foto maksimal %d MB.", t.images.MaxBytes()/(1<<20)))
		case errors.Is(err, upload.ErrUnsupportedType):
			ve.Add("image", "Format foto harus JPG, PNG, atau WEBP.")
		default:
			return sql.NullString{}, fmt.Errorf("saving photo: %w", err)
		}
		return sql.NullString{}, ve
	}

	return sql.NullString{String: rel, Valid: true}, nil
}

// discard removes a stored photo, ignoring failure. Every caller is cleaning up
// after a decision that has already been made, so there is nothing useful to do
// with an error and failing here would undo a successful write.
func (t *Therapists) discard(p sql.NullString) {
	if p.Valid && p.String != "" {
		_ = t.images.Remove(p.String)
	}
}

// allocateTherapistSlug picks the first free slug derived from name, appending
// -2, -3 and so on.
//
// exclude is the id whose own slug does not count as a collision; pass 0 when
// creating, which the query's comment requires. It is a separate function from
// allocateSlug rather than a parameterised one because the only thing that
// differs is the sqlc method, and sqlc generates no interface over the two.
func allocateTherapistSlug(ctx context.Context, q *sqlc.Queries, name string, exclude int64) (string, error) {
	stem := util.Slugify(name)
	if stem == "" {
		stem = therapistSlugFallback
	}

	for n := 1; n <= 50; n++ {
		candidate := stem
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			// Keep the result inside VARCHAR(150) once the suffix is added.
			if len(stem)+len(suffix) > maxSlugLen {
				stem = strings.TrimRight(stem[:maxSlugLen-len(suffix)], "-")
			}
			candidate = stem + suffix
		}

		taken, err := q.TherapistSlugTaken(ctx, sqlc.TherapistSlugTakenParams{
			Slug: candidate,
			ID:   exclude,
		})
		if err != nil {
			return "", fmt.Errorf("checking slug %q: %w", candidate, err)
		}
		if !taken {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no free slug for %q after 50 attempts", name)
}
