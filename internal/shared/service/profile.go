package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"strings"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/upload"
)

// Profile is the customer's own account page.
//
// The boundary moved in Phase 14. It used to touch only phone/address/avatar,
// because name, email and password belonged to Appskep and TPJ had no way to
// write them. It now can: the signed-in user's own JWT authorises the Appskep
// account API, so this type edits name/email/phone/birthdate/sex there (through
// the auth.Account seam) and the password through the same, then mirrors the
// account fields locally so the form prefills and the booking form reads them.
//
// TPJ still owns no credentials: it never verifies a password, stores one, or
// authenticates anyone — it forwards the signed-in user's intent to the service
// that does, exactly as it forwards a token refresh. Appskep remains the source
// of truth; the local row is a mirror updated only after Appskep has accepted
// the same values.
type Profile struct {
	store   *repository.Store
	avatars *upload.ImageStore
	account auth.Account
}

func NewProfile(store *repository.Store, avatars *upload.ImageStore, account auth.Account) *Profile {
	return &Profile{store: store, avatars: avatars, account: account}
}

// ValidationError field keys that are not HTML inputs but page-level notices.
// A rejection from Appskep does not say which field it concerns — an "email
// already used" and a "name too long" arrive in the same shape — so it renders
// as a notice on the account section rather than guessing a field. Same
// reasoning as promoting layanan/slot errors to the booking page notice.
const (
	accountErrorKey  = "akun"
	passwordErrorKey = "kata_sandi_baru"

	// minPasswordLen catches an obviously-too-short password before a network
	// round trip. Appskep's own policy (length, criteria, blacklist) is the
	// authoritative check and rejects the rest with its own message.
	minPasswordLen = 8
)

// ProfileInput is one submission of the profile form, as raw strings plus the
// optional file — the same shape ServiceInput has, and for the same reason: the
// service owns the parsing so a handler cannot validate differently.
type ProfileInput struct {
	// Name, Email, Phone, Birthdate and Sex are Appskep-owned and pushed to the
	// account API. Birthdate is yyyy-mm-dd; Sex is "1", "2" or empty. All but
	// name/email are optional here.
	Name      string
	Email     string
	Phone     string
	Birthdate string
	Sex       string
	// Address is local. Latitude and Longitude are the saved map pin, written by
	// the same widget the booking form uses. Optional, and both or neither —
	// clearing the pin submits two empty strings and stores two NULLs.
	Address   string
	Latitude  string
	Longitude string
	// Avatar is nil when no file was chosen. An empty file input still produces a
	// multipart part, so the handler checks Size before setting this.
	Avatar *multipart.FileHeader
	// RemoveAvatar clears the stored image. It loses to Avatar when both are set:
	// choosing a new file is a clearer intent than leaving a checkbox ticked.
	RemoveAvatar bool
}

// profileFields is one submission after validation: the parsed forms the DB
// wants beside the string forms the account API wants.
type profileFields struct {
	name, email, phone string
	birthdate          sql.NullTime
	sex                sql.NullInt16
	birthdateStr       string // yyyy-mm-dd for the account API, "" when unset
	sexStr             string // "1"/"2" for the account API, "" when unset
	lat, lng           sql.NullString
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
// The order is Phase 4's upload rule extended for the external call, and every
// step is load-bearing:
//
//  1. Validate the text fields FIRST, so a rejected value never strands a file.
//  2. Push the account fields to Appskep. A rejection here also strands no file,
//     which is why it comes before the avatar is written.
//  3. Write the new avatar file.
//  4. Update the row — all columns in one transaction, mirroring what Appskep
//     now holds.
//  5. Only then delete the old file.
//
// Interrupted anywhere, that leaves at most an orphaned file: invisible, costs
// disk. A local write failing after Appskep accepted is logged by the caller;
// Appskep is the source of truth, so the mirror simply catches up on next login.
func (p *Profile) Update(ctx context.Context, userID int64, token string, in ProfileInput) (model.User, error) {
	f, ve := validateProfile(in)
	if ve != nil {
		return model.User{}, ve
	}

	current, err := p.store.Queries.GetUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, fmt.Errorf("getting user %d: %w", userID, err)
	}

	// Push to Appskep before writing anything local. A rejection becomes a form
	// error keyed to the account notice; a transport failure is our own 500.
	if err := p.account.UpdateProfile(ctx, token, auth.AccountProfile{
		Name:      f.name,
		Email:     f.email,
		Phone:     f.phone,
		Birthdate: f.birthdateStr,
		Sex:       f.sexStr,
	}); err != nil {
		if rejected := accountRejection(err, accountErrorKey,
			"Perubahan ditolak oleh Appskep. Periksa kembali data akun kamu."); rejected != nil {
			return model.User{}, rejected
		}
		return model.User{}, fmt.Errorf("updating appskep account for user %d: %w", userID, err)
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
			Name:      f.name,
			Email:     f.email,
			Phone:     nullString(f.phone),
			Birthdate: f.birthdate,
			Sex:       f.sex,
			Address:   nullString(strings.TrimSpace(in.Address)),
			Latitude:  f.lat,
			Longitude: f.lng,
			ID:        userID,
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

// UpdatePassword changes the Appskep password. It writes nothing local — TPJ
// stores no password — so there is no transaction and no mirror.
//
// The bearer token is the whole authorisation: the account is the one it
// identifies, which is why there is no current-password field, matching the
// auth service's own set-password contract. Appskep enforces the strength
// policy; this only catches the empty box and the mismatched confirm before a
// round trip.
func (p *Profile) UpdatePassword(ctx context.Context, token, newPassword, confirm string) error {
	ve := NewValidationError()
	if !required(ve, passwordErrorKey, "Kata sandi baru", newPassword) {
		return ve
	}
	if len([]rune(newPassword)) < minPasswordLen {
		ve.Add(passwordErrorKey, fmt.Sprintf("Kata sandi minimal %d karakter.", minPasswordLen))
		return ve
	}
	if newPassword != confirm {
		ve.Add("konfirmasi_kata_sandi", "Konfirmasi kata sandi tidak cocok.")
		return ve
	}

	if err := p.account.SetPassword(ctx, token, newPassword, confirm); err != nil {
		if rejected := accountRejection(err, passwordErrorKey,
			"Kata sandi ditolak oleh Appskep. Coba kata sandi yang lebih kuat."); rejected != nil {
			return rejected
		}
		return fmt.Errorf("setting appskep password: %w", err)
	}
	return nil
}

// accountRejection turns an auth.ErrAccountRejected into a ValidationError on
// the given key, so the handler's existing errors.As(&ve) path renders it at
// 422 beside the form. It returns nil for any other error, which the caller
// then treats as a 500. Appskep's own message is preferred when present — it is
// the specific, actionable reason ("email already used") — with the Indonesian
// fallback for an empty one.
func accountRejection(err error, field, fallback string) *ValidationError {
	var rej *auth.ErrAccountRejected
	if !errors.As(err, &rej) {
		return nil
	}
	msg := strings.TrimSpace(rej.Message)
	if msg == "" {
		msg = fallback
	}
	ve := NewValidationError()
	ve.Add(field, msg)
	return ve
}

// validateProfile applies the rules for one submission and returns the parsed
// fields, or a ValidationError reporting every problem.
//
// Name and email are required — they are the account's identity and NOT NULL in
// the mirror. Phone, birthdate, sex and the pin are optional: a profile is
// prefill convenience, and refusing to save an address because the phone box is
// empty would be a worse page than one that stores what it was given. The phone
// and coordinate helpers are the booking form's, so a value accepted on one
// page cannot be rejected on the other.
func validateProfile(in ProfileInput) (profileFields, *ValidationError) {
	ve := NewValidationError()

	name := strings.TrimSpace(in.Name)
	emailValue := strings.TrimSpace(in.Email)
	phoneValue := strings.TrimSpace(in.Phone)
	address := strings.TrimSpace(in.Address)
	birthdateValue := strings.TrimSpace(in.Birthdate)
	sexValue := strings.TrimSpace(in.Sex)

	requiredMaxLen(ve, "nama", "Nama", name, maxCustomerNameLen)

	if required(ve, "email", "Email", emailValue) {
		email(ve, "email", emailValue)
	}

	if phoneValue != "" && maxLen(ve, "telepon", "Nomor telepon", phoneValue, maxCustomerPhoneLen) {
		phone(ve, "telepon", phoneValue)
	}

	maxLen(ve, "alamat", "Alamat", address, maxCustomerAddressLen)

	bd, _ := birthdate(ve, "tanggal_lahir", birthdateValue)
	sx, _ := sex(ve, "jenis_kelamin", sexValue)

	// Optional here as on the booking form, and the same helper, so a pin dropped
	// while booking is never refused when saved as the profile default.
	latitude, longitude := coordinatePair(ve, in.Latitude, in.Longitude)

	if ve.Any() {
		return profileFields{}, ve
	}
	return profileFields{
		name:         name,
		email:        emailValue,
		phone:        phoneValue,
		birthdate:    bd,
		sex:          sx,
		birthdateStr: birthdateValue,
		sexStr:       sexValue,
		lat:          latitude,
		lng:          longitude,
	}, nil
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
