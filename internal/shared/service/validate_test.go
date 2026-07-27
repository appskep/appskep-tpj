package service

import (
	"strings"
	"testing"
)

// In-package: required, maxLen, requiredMaxLen and phone are unexported. They are
// the four idioms six validators were each writing by hand, and every message
// here is byte-identical to what a customer reads — so a change to the copy is a
// deliberate one rather than a typo nobody notices.

func TestRequired(t *testing.T) {
	ve := NewValidationError()

	if required(ve, "nama", "Nama", "Budi") != true {
		t.Error("required rejected a non-empty value")
	}
	if ve.Any() {
		t.Errorf("required recorded a message for a valid value: %v", ve.Fields)
	}

	if required(ve, "nama", "Nama", "") != false {
		t.Error("required accepted an empty value")
	}
	if got, want := ve.Fields["nama"], "Nama wajib diisi."; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestMaxLen(t *testing.T) {
	ve := NewValidationError()

	if maxLen(ve, "nama", "Nama", strings.Repeat("a", 150), 150) != true {
		t.Error("maxLen rejected a value exactly at the limit")
	}
	if ve.Any() {
		t.Errorf("maxLen recorded a message at the boundary: %v", ve.Fields)
	}

	if maxLen(ve, "nama", "Nama", strings.Repeat("a", 151), 150) != false {
		t.Error("maxLen accepted a value one over the limit")
	}
	if got, want := ve.Fields["nama"], "Nama maksimal 150 karakter."; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestMaxLenCountsRunesNotBytes is the reason validate.go says "Rune length,
// never len()". These fields hold Indonesian names and addresses; a byte count
// rejects a shorter string than the column can hold and than the message
// promises.
func TestMaxLenCountsRunesNotBytes(t *testing.T) {
	// 150 runes, 450 bytes.
	value := strings.Repeat("日", 150)
	if len(value) <= 150 {
		t.Fatalf("the fixture is not multi-byte: %d bytes for %d runes", len(value), len([]rune(value)))
	}

	ve := NewValidationError()
	if !maxLen(ve, "alamat", "Alamat", value, 150) {
		t.Errorf("maxLen rejected %d runes against a limit of 150 — it is counting bytes",
			len([]rune(value)))
	}

	// And one rune over is still refused.
	ve = NewValidationError()
	if maxLen(ve, "alamat", "Alamat", value+"日", 150) {
		t.Error("maxLen accepted 151 runes against a limit of 150")
	}
}

// TestRequiredMaxLenStopsAtTheFirstProblem: reporting "wajib diisi" and
// "maksimal 150 karakter" about the same empty box helps nobody.
func TestRequiredMaxLenStopsAtTheFirstProblem(t *testing.T) {
	ve := NewValidationError()

	if requiredMaxLen(ve, "nama", "Nama", "", 150) {
		t.Error("requiredMaxLen accepted an empty value")
	}
	if len(ve.Fields) != 1 {
		t.Errorf("recorded %d messages for one empty field: %v", len(ve.Fields), ve.Fields)
	}
	if got := ve.Fields["nama"]; got != "Nama wajib diisi." {
		t.Errorf("message = %q, want the required message", got)
	}
}

func TestPhone(t *testing.T) {
	const (
		shapeMsg  = "Nomor telepon hanya boleh berisi angka, spasi, dan tanda + - ( )."
		digitsMsg = "Nomor telepon minimal 8 angka."
	)

	tests := []struct {
		name string
		in   string
		ok   bool
		msg  string
	}{
		// The punctuation people actually type into a phone field.
		{name: "plain mobile", in: "081234567890", ok: true},
		{name: "international", in: "+6281234567890", ok: true},
		{name: "spaced", in: "0812 3456 7890", ok: true},
		{name: "dashed", in: "0812-3456-7890", ok: true},
		{name: "area code in brackets", in: "(0274) 123456", ok: true},
		{name: "exactly eight digits", in: "12345678", ok: true},

		{name: "seven digits", in: "1234567", msg: digitsMsg},
		{name: "no digits at all", in: "+- ()", msg: digitsMsg},

		{name: "letters", in: "0812ABCD5678", msg: shapeMsg},
		{name: "email in the phone box", in: "budi@example.test", msg: shapeMsg},
		{name: "a dot", in: "0812.3456.7890", msg: shapeMsg},
		{name: "a slash", in: "0812/3456", msg: shapeMsg},
		{name: "a newline", in: "0812345678\n", msg: shapeMsg},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ve := NewValidationError()
			got := phone(ve, "telepon", tc.in)

			if got != tc.ok {
				t.Fatalf("phone(%q) = %v, want %v", tc.in, got, tc.ok)
			}
			if tc.ok {
				if ve.Any() {
					t.Errorf("phone(%q) recorded %v", tc.in, ve.Fields)
				}
				return
			}
			if ve.Fields["telepon"] != tc.msg {
				t.Errorf("message = %q, want %q", ve.Fields["telepon"], tc.msg)
			}
		})
	}
}

// TestPhoneIsOneImplementation. It lives in validate.go rather than in
// booking.go, where it grew, because the profile form borrows it verbatim: a
// number accepted while booking must not be rejected when saved to the profile.
// Whether the field is required at all is the caller's decision.
func TestPhoneAcceptsAnEmptyValue(t *testing.T) {
	ve := NewValidationError()

	// The shape check passes trivially, and the digit floor is what refuses it —
	// so an optional phone field must check required-ness itself, and the profile
	// validator does.
	if phone(ve, "telepon", "") {
		t.Error("phone accepted an empty value outright")
	}
	if got := ve.Fields["telepon"]; got != "Nomor telepon minimal 8 angka." {
		t.Errorf("message for an empty phone = %q", got)
	}
}

func TestValidationError(t *testing.T) {
	ve := NewValidationError()

	if ve.Any() {
		t.Error("a fresh ValidationError reports problems")
	}

	ve.Add("nama", "Nama wajib diisi.")
	ve.Add("harga", "Harga tidak valid.")

	if !ve.Any() {
		t.Error("Any() is false after two messages")
	}

	// First message per field wins, so a cheap check (required) can run before an
	// expensive one (uniqueness) without the second overwriting the more useful
	// first.
	ve.Add("nama", "Nama sudah dipakai.")
	if got := ve.Fields["nama"]; got != "Nama wajib diisi." {
		t.Errorf("the second message for a field overwrote the first: %q", got)
	}

	// Sorted, so two log lines for the same failure match. The messages themselves
	// are user-facing copy and stay out of it.
	if got, want := ve.Error(), "validation failed: harga, nama"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	for _, msg := range []string{"wajib diisi", "tidak valid"} {
		if strings.Contains(ve.Error(), msg) {
			t.Errorf("Error() leaks user-facing copy into the log: %q", ve.Error())
		}
	}
}
