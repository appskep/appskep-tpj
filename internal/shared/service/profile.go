package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"strings"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/upload"
)

// Profile owns the part of a user record this system may change.
//
// That part is small and the boundary is the point: name and email belong to
// Appskep, are refreshed from the JWT on every login, and would be silently
// overwritten at the next sign-in if this type ever wrote them. Phone, address
// and avatar are local, so they are all this type touches — enforced by
// UpdateUserProfile and UpdateUserAvatar naming their columns rather than by any
// check here.
//
// There is no password, no email change and no account deletion, for the same
// reason there is no login form: TPJ owns no credentials (PLAN.md R1, R2). The
// profile page links to Appskep for all three.
type Profile struct {
	store   *repository.Store
	avatars *upload.ImageStore
}

func NewProfile(store *repository.Store, avatars *upload.ImageStore) *Profile {
	return &Profile{store: store, avatars: avatars}
}

// ProfileInput is one submission of the profile form, as raw strings plus the
// optional file — the same shape ServiceInput has, and for the same reason: the
// service owns the parsing so a handler cannot validate differently.
type ProfileInput struct {
	Phone   string
	Address string
	// Avatar is nil when no file was chosen. An empty file input still produces a
	// multipart part, so the handler checks Size before setting this.
	Avatar *multipart.FileHeader
	// RemoveAvatar clears the stored image. It loses to Avatar when both are set:
	// choosing a new file is a clearer intent than leaving a checkbox ticked.
	RemoveAvatar bool
}

// Get returns the user's current profile.
func (p *Profile) Get(ctx context.Context, userID int64) (model.User, error) {
	row, err := p.store.Queries.GetUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, fmt.Errorf("getting user %d: %w", userID, err)
	}
	return model.UserFromSQLC(row), nil
}

// Update applies one submission and returns the stored result.
//
// The ordering is Phase 4's upload rule, and every step of it is load-bearing:
//
//  1. Validate the text fields FIRST, so a rejected phone number never strands
//     an uploaded file on disk.
//  2. Write the new file.
//  3. Update the row — both columns in one transaction, because they are one
//     form and a half-applied save is not a state worth having.
//  4. Only then delete the old file.
//
// Interrupted anywhere, that leaves at most an orphaned file: invisible, costs
// disk. The other order leaves a row pointing at a file that is not there, which
// is a broken image on a page the user is looking at.
func (p *Profile) Update(ctx context.Context, userID int64, in ProfileInput) (model.User, error) {
	phone := strings.TrimSpace(in.Phone)
	address := strings.TrimSpace(in.Address)

	if ve := validateProfile(phone, address); ve != nil {
		return model.User{}, ve
	}

	current, err := p.store.Queries.GetUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, fmt.Errorf("getting user %d: %w", userID, err)
	}

	// avatar is what the row will carry; old is what to delete afterwards, and it
	// stays zero unless the stored file is genuinely being replaced or removed.
	avatar := current.AvatarPath
	var old sql.NullString

	switch {
	case in.Avatar != nil:
		saved, err := p.saveAvatar(in.Avatar)
		if err != nil {
			return model.User{}, err
		}
		avatar, old = saved, current.AvatarPath
	case in.RemoveAvatar:
		avatar, old = sql.NullString{}, current.AvatarPath
	}

	err = p.store.WithTx(ctx, func(q *sqlc.Queries) error {
		if err := q.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
			Phone:   nullString(phone),
			Address: nullString(address),
			ID:      userID,
		}); err != nil {
			return fmt.Errorf("updating profile for user %d: %w", userID, err)
		}
		if err := q.UpdateUserAvatar(ctx, sqlc.UpdateUserAvatarParams{
			AvatarPath: avatar,
			ID:         userID,
		}); err != nil {
			return fmt.Errorf("updating avatar for user %d: %w", userID, err)
		}
		return nil
	})
	if err != nil {
		// The row was not changed, so the file just written belongs to nobody.
		if in.Avatar != nil {
			p.discardAvatar(avatar)
		}
		return model.User{}, err
	}

	// The row is committed and no longer refers to it.
	p.discardAvatar(old)

	row, err := p.store.Queries.GetUser(ctx, userID)
	if err != nil {
		return model.User{}, fmt.Errorf("re-reading user %d: %w", userID, err)
	}
	return model.UserFromSQLC(row), nil
}

// validateProfile applies the rules for the two text fields.
//
// Both are optional here, unlike on the booking form: a profile is prefill
// convenience, and refusing to save an address because the phone box is empty
// would be a worse page than one that stores what it was given. The bounds and
// the phone-shape helpers are the booking form's, so a number accepted on one
// page cannot be rejected on the other — users.phone and users.address have the
// same widths as their bookings counterparts.
func validateProfile(phoneValue, address string) *ValidationError {
	ve := NewValidationError()

	if phoneValue != "" && maxLen(ve, "telepon", "Nomor telepon", phoneValue, maxCustomerPhoneLen) {
		phone(ve, "telepon", phoneValue)
	}

	maxLen(ve, "alamat", "Alamat", address, maxCustomerAddressLen)

	if ve.Any() {
		return ve
	}
	return nil
}

// saveAvatar stores an uploaded file, translating the uploader's errors into
// messages for the file input rather than into a 500.
func (p *Profile) saveAvatar(fh *multipart.FileHeader) (sql.NullString, error) {
	rel, err := p.avatars.Save(fh)
	if err != nil {
		ve := NewValidationError()
		switch {
		case errors.Is(err, upload.ErrTooLarge):
			ve.Add("avatar", fmt.Sprintf("Ukuran foto maksimal %d MB.", p.avatars.MaxBytes()/(1<<20)))
		case errors.Is(err, upload.ErrUnsupportedType):
			ve.Add("avatar", "Format foto harus JPG, PNG, atau WEBP.")
		default:
			return sql.NullString{}, fmt.Errorf("saving avatar: %w", err)
		}
		return sql.NullString{}, ve
	}
	return sql.NullString{String: rel, Valid: true}, nil
}

// discardAvatar removes a stored file, ignoring failure. Every caller is cleaning
// up after a decision already made, so there is nothing useful to do with an
// error and failing here would undo a successful write.
func (p *Profile) discardAvatar(path sql.NullString) {
	if path.Valid && path.String != "" {
		_ = p.avatars.Remove(path.String)
	}
}
