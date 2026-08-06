package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/upload"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Catalog is the business logic behind the admin layanan module: the rules that
// decide whether a submitted form is acceptable, and the ordering that keeps the
// services table and the uploads directory consistent with each other.
//
// Validation lives here rather than in the handler so create and edit cannot
// drift apart — they are the same rules applied to the same input, and two
// copies of "durasi 15–480" would eventually disagree.
type Catalog struct {
	store  *repository.Store
	images *upload.ImageStore
}

func NewCatalog(store *repository.Store, images *upload.ImageStore) *Catalog {
	return &Catalog{store: store, images: images}
}

// Duration bounds, mirroring chk_services_duration in 0001_schema.sql. They are
// checked here as well as by the database so the user gets a message next to the
// field instead of a 500 from a constraint violation.
const (
	MinDurationMinutes = 15
	MaxDurationMinutes = 480
)

// maxNameLen matches services.name VARCHAR(150), and therapists.name and
// therapists.specialization, which are the same width.
const maxNameLen = 150

// slugFallback is the stem used when a name contains nothing a slug can be built
// from — "🌿" or "!!!". The uniqueness loop then makes it layanan-2, layanan-3.
const slugFallback = "layanan"

// ServiceInput is one submitted form, still as raw strings.
//
// The service parses them rather than the handler so the parsing rules and the
// error messages that describe them live together. Image is nil unless a new
// file was uploaded; RemoveImage clears an existing one.
type ServiceInput struct {
	Name         string
	Description  string
	Price        string
	Duration     string
	SortOrder    string
	IsActive     bool
	IsComingSoon bool
	Image        *multipart.FileHeader
	RemoveImage  bool
}

// ListQuery is a page of the admin list.
type ListQuery struct {
	Search   string
	Page     int
	PageSize int
}

// ListResult is that page plus what the pagination partial needs. Page and
// TotalPages are int, not the int32 sqlc uses, because the partial's `add`
// helper is func(int, int) int.
type ListResult struct {
	Items      []sqlc.Service
	Page       int
	TotalPages int
	Total      int64
}

// List returns one page of services matching the search term.
func (c *Catalog) List(ctx context.Context, q ListQuery) (ListResult, error) {
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}

	// The queries match with LIKE and their comments require "%" for an empty
	// box. EscapeLike keeps a typed "%" or "_" a literal character rather than a
	// wildcard that would quietly ignore the rest of the search.
	search := "%"
	if term := strings.TrimSpace(q.Search); term != "" {
		search = "%" + util.EscapeLike(term) + "%"
	}

	total, err := c.store.Queries.CountServicesAdmin(ctx, search)
	if err != nil {
		return ListResult{}, fmt.Errorf("counting services: %w", err)
	}

	totalPages := int((total + int64(q.PageSize) - 1) / int64(q.PageSize))
	// Clamp after counting: a deletion or a filter can leave ?page= past the end,
	// and showing the last page beats showing an empty one with a working "next".
	if totalPages > 0 && q.Page > totalPages {
		q.Page = totalPages
	}

	items, err := c.store.Queries.ListServicesAdmin(ctx, sqlc.ListServicesAdminParams{
		Search: search,
		Limit:  int32(q.PageSize),
		Offset: int32((q.Page - 1) * q.PageSize),
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("listing services: %w", err)
	}

	return ListResult{Items: items, Page: q.Page, TotalPages: totalPages, Total: total}, nil
}

// Get returns one service, or ErrNotFound.
func (c *Catalog) Get(ctx context.Context, id int64) (sqlc.Service, error) {
	svc, err := c.store.Queries.GetService(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Service{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Service{}, fmt.Errorf("getting service %d: %w", id, err)
	}
	return svc, nil
}

// ListActive returns every service the public site may show, in sort order.
// Coming-soon services are included: they are displayed with a label, and only
// the booking path excludes them (ListBookableServices).
func (c *Catalog) ListActive(ctx context.Context) ([]sqlc.Service, error) {
	items, err := c.store.Queries.ListActiveServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing active services: %w", err)
	}
	return items, nil
}

// GetActiveBySlug returns one publicly visible service, or ErrNotFound.
//
// Deliberately GetActiveServiceBySlug and not GetServiceBySlug: a deactivated
// service must 404 rather than render, so the is_active predicate belongs in the
// query where it cannot be forgotten at a call site.
func (c *Catalog) GetActiveBySlug(ctx context.Context, slug string) (sqlc.Service, error) {
	svc, err := c.store.Queries.GetActiveServiceBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Service{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Service{}, fmt.Errorf("getting service by slug %q: %w", slug, err)
	}
	return svc, nil
}

// parsed is the validated form: the same values, in the types the database
// wants.
type parsed struct {
	name        string
	description sql.NullString
	price       string
	duration    int32
	sortOrder   int32
}

// validate applies every field rule at once, so the user sees all the problems
// on one submit rather than discovering them one at a time.
func validate(in ServiceInput) (parsed, *ValidationError) {
	ve := NewValidationError()
	var p parsed

	p.name = strings.TrimSpace(in.Name)
	requiredMaxLen(ve, "name", "Nama layanan", p.name, maxNameLen)

	if desc := strings.TrimSpace(in.Description); desc != "" {
		p.description = sql.NullString{String: desc, Valid: true}
	}

	price, err := util.ParsePrice(in.Price)
	if err != nil {
		ve.Add("price", "Harga tidak valid. Contoh: 150000 atau 150.000")
	}
	p.price = price

	switch d, derr := strconv.Atoi(strings.TrimSpace(in.Duration)); {
	case strings.TrimSpace(in.Duration) == "":
		ve.Add("duration", "Durasi wajib diisi.")
	case derr != nil:
		ve.Add("duration", "Durasi harus berupa angka dalam menit.")
	case d < MinDurationMinutes || d > MaxDurationMinutes:
		ve.Add("duration", fmt.Sprintf("Durasi harus antara %d dan %d menit.",
			MinDurationMinutes, MaxDurationMinutes))
	default:
		p.duration = int32(d)
	}

	// Sort order is optional; a blank field means "put it first".
	if so := strings.TrimSpace(in.SortOrder); so != "" {
		n, serr := strconv.Atoi(so)
		if serr != nil || n < 0 {
			ve.Add("sort_order", "Urutan harus berupa angka 0 atau lebih.")
		} else {
			p.sortOrder = int32(n)
		}
	}

	if ve.Any() {
		return parsed{}, ve
	}
	return p, nil
}

// Create validates the form, stores any uploaded image, and inserts the row.
//
// Text fields are validated before the image is touched, so a missing name never
// leaves an orphan file in the uploads directory.
func (c *Catalog) Create(ctx context.Context, in ServiceInput) (int64, error) {
	p, ve := validate(in)
	if ve != nil {
		return 0, ve
	}

	imagePath, err := c.saveImage(in)
	if err != nil {
		return 0, err
	}

	id, err := c.insert(ctx, p, imagePath, in)
	if err != nil {
		// The row never landed, so the file it would have pointed at is garbage.
		c.discard(imagePath)
		return 0, err
	}
	return id, nil
}

// insert runs the slug allocation and the INSERT, retrying once when another
// admin took the slug in between.
func (c *Catalog) insert(ctx context.Context, p parsed, imagePath sql.NullString, in ServiceInput) (int64, error) {
	for range 2 {
		var id int64

		err := c.store.WithTx(ctx, func(q *sqlc.Queries) error {
			slug, serr := allocateSlug(ctx, q, p.name, 0)
			if serr != nil {
				return serr
			}

			res, ierr := q.CreateService(ctx, sqlc.CreateServiceParams{
				Slug:            slug,
				Name:            p.name,
				Description:     p.description,
				Price:           p.price,
				DurationMinutes: p.duration,
				ImagePath:       imagePath,
				IsActive:        in.IsActive,
				IsComingSoon:    in.IsComingSoon,
				SortOrder:       p.sortOrder,
			})
			if ierr != nil {
				return ierr
			}

			id, ierr = res.LastInsertId()
			return ierr
		})

		if err == nil {
			return id, nil
		}
		// allocateSlug's check takes no lock — uq_services_slug is what actually
		// enforces uniqueness. Two admins creating the same name at the same
		// moment is the one case that reaches here, and a retry resolves it
		// because the winner's row is now visible to the loser's check.
		if repository.IsDuplicateKeyOn(err, "uq_services_slug") {
			continue
		}
		return 0, fmt.Errorf("creating service: %w", err)
	}

	ve := NewValidationError()
	ve.Add("name", "Nama layanan bentrok dengan layanan lain. Coba ubah sedikit namanya.")
	return 0, ve
}

// Update validates the form and writes it, handling an image replacement or
// removal separately from the rest of the fields.
//
// The slug is deliberately not recomputed: it is the public URL of the service,
// and regenerating it on every rename would break links, bookmarks and search
// results with no redirect behind them. It is generated once, at creation.
func (c *Catalog) Update(ctx context.Context, id int64, in ServiceInput) error {
	current, err := c.Get(ctx, id)
	if err != nil {
		return err
	}

	p, ve := validate(in)
	if ve != nil {
		return ve
	}

	imagePath, err := c.saveImage(in)
	if err != nil {
		return err
	}

	err = c.store.Queries.UpdateService(ctx, sqlc.UpdateServiceParams{
		Slug:            current.Slug,
		Name:            p.name,
		Description:     p.description,
		Price:           p.price,
		DurationMinutes: p.duration,
		IsActive:        in.IsActive,
		IsComingSoon:    in.IsComingSoon,
		SortOrder:       p.sortOrder,
		ID:              id,
	})
	if err != nil {
		c.discard(imagePath)
		return fmt.Errorf("updating service %d: %w", id, err)
	}

	return c.applyImageChange(ctx, id, current.ImagePath, imagePath, in.RemoveImage)
}

// applyImageChange writes the new image_path and then deletes the file the row
// used to point at.
//
// The ordering is the point: write the new file, update the row, delete the old
// file. Interrupted anywhere, that leaves at worst an orphaned file — invisible,
// costing only disk. The other order leaves a row pointing at a file that is
// gone, which is a broken image on the public site.
func (c *Catalog) applyImageChange(ctx context.Context, id int64, old, uploaded sql.NullString, remove bool) error {
	switch {
	case uploaded.Valid:
		if err := c.setImage(ctx, id, uploaded); err != nil {
			c.discard(uploaded)
			return err
		}
		c.discard(old) // best effort; an orphan is harmless
	case remove && old.Valid:
		if err := c.setImage(ctx, id, sql.NullString{}); err != nil {
			return err
		}
		c.discard(old)
	}
	return nil
}

func (c *Catalog) setImage(ctx context.Context, id int64, path sql.NullString) error {
	if err := c.store.Queries.UpdateServiceImage(ctx, sqlc.UpdateServiceImageParams{
		ImagePath: path,
		ID:        id,
	}); err != nil {
		return fmt.Errorf("updating image for service %d: %w", id, err)
	}
	return nil
}

// SetActive flips is_active and returns the refreshed row, which the turbo
// stream re-renders the toggle cell from.
func (c *Catalog) SetActive(ctx context.Context, id int64, v bool) (sqlc.Service, error) {
	if _, err := c.Get(ctx, id); err != nil {
		return sqlc.Service{}, err
	}
	if err := c.store.Queries.SetServiceActive(ctx, sqlc.SetServiceActiveParams{
		IsActive: v,
		ID:       id,
	}); err != nil {
		return sqlc.Service{}, fmt.Errorf("setting service %d active: %w", id, err)
	}
	return c.Get(ctx, id)
}

// SetComingSoon flips is_coming_soon.
//
// A coming-soon service stays visible on the public site but is not bookable;
// ListBookableServices already excludes it, so this flag is enforced by the
// query rather than by anything here.
func (c *Catalog) SetComingSoon(ctx context.Context, id int64, v bool) (sqlc.Service, error) {
	if _, err := c.Get(ctx, id); err != nil {
		return sqlc.Service{}, err
	}
	if err := c.store.Queries.SetServiceComingSoon(ctx, sqlc.SetServiceComingSoonParams{
		IsComingSoon: v,
		ID:           id,
	}); err != nil {
		return sqlc.Service{}, fmt.Errorf("setting service %d coming soon: %w", id, err)
	}
	return c.Get(ctx, id)
}

// Delete removes a service that no booking references, returning ErrHasBookings
// otherwise.
//
// The count and the delete are two round-trips, so a booking can be created in
// between. fk_bookings_service is RESTRICT, which catches exactly that and is
// translated to the same error — the check is the friendly path, the constraint
// is the correct one.
func (c *Catalog) Delete(ctx context.Context, id int64) error {
	svc, err := c.Get(ctx, id)
	if err != nil {
		return err
	}

	n, err := c.store.Queries.CountBookingsForService(ctx, id)
	if err != nil {
		return fmt.Errorf("counting bookings for service %d: %w", id, err)
	}
	if n > 0 {
		return ErrHasBookings
	}

	res, err := c.store.Queries.DeleteService(ctx, id)
	if err != nil {
		if repository.IsForeignKeyViolation(err) {
			return ErrHasBookings
		}
		return fmt.Errorf("deleting service %d: %w", id, err)
	}
	if rows, rerr := res.RowsAffected(); rerr == nil && rows == 0 {
		return ErrNotFound
	}

	// Only once the row is gone, so a failed delete never strands the image.
	c.discard(svc.ImagePath)
	return nil
}

// saveImage stores an uploaded file, translating the uploader's errors into
// messages for the file input.
func (c *Catalog) saveImage(in ServiceInput) (sql.NullString, error) {
	if in.Image == nil {
		return sql.NullString{}, nil
	}

	rel, err := c.images.Save(in.Image)
	if err != nil {
		ve := NewValidationError()
		switch {
		case errors.Is(err, upload.ErrTooLarge):
			ve.Add("image", fmt.Sprintf("Ukuran gambar maksimal %d MB.", c.images.MaxBytes()/(1<<20)))
		case errors.Is(err, upload.ErrUnsupportedType):
			ve.Add("image", "Format gambar harus JPG, PNG, atau WEBP.")
		default:
			return sql.NullString{}, fmt.Errorf("saving image: %w", err)
		}
		return sql.NullString{}, ve
	}

	return sql.NullString{String: rel, Valid: true}, nil
}

// discard removes a stored image, ignoring failure. Every caller is cleaning up
// after a decision that has already been made, so there is nothing useful to do
// with an error and failing here would undo a successful write.
func (c *Catalog) discard(p sql.NullString) {
	if p.Valid && p.String != "" {
		_ = c.images.Remove(p.String)
	}
}

// allocateSlug picks the first free slug derived from name, appending -2, -3 and
// so on.
//
// exclude is the id whose own slug does not count as a collision; pass 0 when
// creating, which the query's comment requires.
func allocateSlug(ctx context.Context, q *sqlc.Queries, name string, exclude int64) (string, error) {
	stem := util.Slugify(name)
	if stem == "" {
		stem = slugFallback
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

		taken, err := q.ServiceSlugTaken(ctx, sqlc.ServiceSlugTakenParams{
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

// maxSlugLen matches services.slug and therapists.slug, both VARCHAR(150), as
// util.Slugify enforces.
const maxSlugLen = 150
