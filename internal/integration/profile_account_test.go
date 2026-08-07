package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The Appskep account fields (Phase 14). The profile page now edits name, email,
// phone, birthdate and sex on the Appskep account through the account API, using
// the signed-in user's own token, and mirrors the result locally. These prove
// the push happens, that a rejection is reported and changes nothing, and that a
// password change never touches the local row.

// TestProfileUpdatePushesAccountFieldsToAppskep is the feature: the five account
// fields reach Appskep with the caller's token, and the local mirror ends up
// holding the same values so the form and the booking form prefill from them.
func TestProfileUpdatePushesAccountFieldsToAppskep(t *testing.T) {
	env := testsupport.New(t)
	user := env.User(9410)

	saved, err := env.Deps.Profile.Update(context.Background(), user.ID, "jwt-abc",
		service.ProfileInput{
			Name:      "Sitti Rahmah",
			Email:     "sitti@example.test",
			Phone:     "081234567890",
			Birthdate: "1998-04-05",
			Sex:       "2",
			Address:   "Jl. Contoh No. 1",
		})
	if err != nil {
		t.Fatalf("Profile.Update: %v", err)
	}

	// Pushed to Appskep, exactly once, with the caller's token.
	if got := env.Account.ProfileCount(); got != 1 {
		t.Fatalf("UpdateProfile calls = %d, want 1", got)
	}
	pushed, _ := env.Account.LastProfile()
	want := auth.AccountProfile{
		Name: "Sitti Rahmah", Email: "sitti@example.test",
		Phone: "081234567890", Birthdate: "1998-04-05", Sex: "2",
	}
	if pushed != want {
		t.Errorf("pushed = %+v, want %+v", pushed, want)
	}

	// Mirrored locally: the returned row and a fresh read both carry the values.
	if saved.Name != "Sitti Rahmah" || saved.Email != "sitti@example.test" ||
		saved.Phone != "081234567890" || saved.Birthdate != "1998-04-05" || saved.Sex != "2" {
		t.Errorf("returned mirror = %+v, want the pushed values", saved)
	}
	reread, err := env.Deps.Profile.Get(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("Profile.Get: %v", err)
	}
	if reread.Birthdate != "1998-04-05" || reread.Sex != "2" {
		t.Errorf("stored birthdate/sex = (%q,%q), want (1998-04-05, 2)",
			reread.Birthdate, reread.Sex)
	}

	env.AssertInvariant()
}

// TestProfileUpdateRejectionLeavesLocalRowUnchanged. When Appskep refuses the
// change, the caller sees a ValidationError carrying Appskep's message on the
// account notice, and the local mirror is untouched — Appskep is the source of
// truth, so a value it rejected must never land locally.
func TestProfileUpdateRejectionLeavesLocalRowUnchanged(t *testing.T) {
	env := testsupport.New(t)
	user := env.User(9411)

	env.Account.UpdateErr = &auth.ErrAccountRejected{Message: "Email sudah dipakai."}

	_, err := env.Deps.Profile.Update(context.Background(), user.ID, "jwt-abc",
		service.ProfileInput{
			Name:  "Nama Baru",
			Email: "taken@example.test",
			Phone: "081234567890",
		})

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a *service.ValidationError", err)
	}
	if msg := ve.Fields["akun"]; msg != "Email sudah dipakai." {
		t.Errorf("account notice = %q, want Appskep's message", msg)
	}
	if env.Account.ProfileCount() != 0 {
		t.Errorf("a rejected update was still recorded as accepted")
	}

	// The local row still holds the seeded identity, not the rejected one.
	reread, err := env.Deps.Profile.Get(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("Profile.Get: %v", err)
	}
	if reread.Name == "Nama Baru" || reread.Email == "taken@example.test" {
		t.Errorf("the rejected values reached the local mirror: %+v", reread)
	}

	env.AssertInvariant()
}

// TestUpdatePasswordCallsSetPassword. A password change forwards to Appskep and
// writes nothing local — TPJ stores no password — and a mismatched confirm is
// caught before any call.
func TestUpdatePasswordCallsSetPassword(t *testing.T) {
	env := testsupport.New(t)
	user := env.User(9412)
	_ = user

	// A mismatched confirm never reaches Appskep.
	err := env.Deps.Profile.UpdatePassword(context.Background(), "jwt-abc",
		"rahasia-baru-123", "beda")
	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("mismatch error = %v, want a *service.ValidationError", err)
	}
	if _, ok := ve.Fields["konfirmasi_kata_sandi"]; !ok {
		t.Errorf("messages = %v, want one on konfirmasi_kata_sandi", ve.Fields)
	}
	if env.Account.PasswordCount() != 0 {
		t.Fatalf("a mismatched confirm was forwarded to Appskep")
	}

	// A matching pair is forwarded exactly once.
	if err := env.Deps.Profile.UpdatePassword(context.Background(), "jwt-abc",
		"rahasia-baru-123", "rahasia-baru-123"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if got := env.Account.PasswordCount(); got != 1 {
		t.Errorf("SetPassword calls = %d, want 1", got)
	}

	env.AssertInvariant()
}
