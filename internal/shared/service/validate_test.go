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

// ---------------------------------------------------------------------------
// The map pin
// ---------------------------------------------------------------------------

// TestCoordinate covers the parser alone. The values here are what a hidden
// input can actually contain — a browser's toFixed output, a truncated value, a
// tampered one — because no human ever types into this field.
func TestCoordinate(t *testing.T) {
	tests := []struct {
		name  string
		value string
		limit float64
		want  string // "" means: expect a rejection
	}{
		// Normalisation to the DECIMAL(_,7) the column holds is the point: the
		// value validated has to be the value stored, or a re-rendered form and
		// the database disagree about where the customer is.
		{name: "trailing zeros added", value: "-7.797068", limit: 90, want: "-7.7970680"},
		{name: "excess precision truncated", value: "110.37052912345678", limit: 180, want: "110.3705291"},
		{name: "integer degrees", value: "110", limit: 180, want: "110.0000000"},
		{name: "zero is a real place", value: "0", limit: 90, want: "0.0000000"},

		{name: "latitude at the pole", value: "90", limit: 90, want: "90.0000000"},
		{name: "latitude past the pole", value: "90.1", limit: 90},
		{name: "longitude at the antimeridian", value: "-180", limit: 180, want: "-180.0000000"},
		{name: "longitude past it", value: "180.0001", limit: 180},

		{name: "empty", value: "", limit: 90},
		{name: "not a number", value: "dekat masjid", limit: 90},
		{name: "NaN", value: "NaN", limit: 90},
		{name: "infinity", value: "Inf", limit: 90},
		{name: "a pair in one field", value: "-7.79,110.37", limit: 90},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := coordinate(tc.value, tc.limit)

			if tc.want == "" {
				if ok {
					t.Fatalf("coordinate(%q) accepted it as %q", tc.value, got)
				}
				return
			}
			if !ok {
				t.Fatalf("coordinate(%q) was rejected", tc.value)
			}
			if got != tc.want {
				t.Errorf("coordinate(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestCoordinatePair states the both-or-neither rule, which is what keeps half a
// pin — a row no page can render and no therapist can drive to — out of the
// database. It is enforced here rather than by a CHECK constraint, so this test
// is the only thing that guards it.
func TestCoordinatePair(t *testing.T) {
	tests := []struct {
		name     string
		lat, lng string
		wantSet  bool
		wantErr  bool
	}{
		{name: "both empty is a valid absent pin", lat: "", lng: ""},
		{name: "whitespace counts as empty", lat: "  ", lng: "\t"},
		{name: "both set", lat: "-7.797068", lng: "110.370529", wantSet: true},

		{name: "latitude only", lat: "-7.797068", lng: "", wantErr: true},
		{name: "longitude only", lat: "", lng: "110.370529", wantErr: true},
		{name: "latitude out of range", lat: "-91", lng: "110.370529", wantErr: true},
		// 110 is a legal latitude-shaped number but not a legal latitude, and
		// -7 is a legal longitude — so a swapped pair is only caught because the
		// two limits differ. Worth stating: it is the mistake a caller passing
		// the arguments the wrong way round would make.
		{name: "swapped pair", lat: "110.370529", lng: "-7.797068", wantErr: true},
		{name: "both nonsense", lat: "x", lng: "y", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ve := NewValidationError()
			lat, lng := coordinatePair(ve, tc.lat, tc.lng)

			if tc.wantErr {
				if !ve.Any() {
					t.Fatal("coordinatePair accepted it")
				}
				if _, ok := ve.Fields[coordFieldKey]; !ok {
					t.Errorf("messages = %v, want one on %q", ve.Fields, coordFieldKey)
				}
				// A rejected pair must write nothing: returning one half plus an
				// error is exactly the state the rule exists to prevent.
				if lat.Valid || lng.Valid {
					t.Errorf("a rejected pair still returned values: %+v %+v", lat, lng)
				}
				return
			}

			if ve.Any() {
				t.Fatalf("coordinatePair rejected a valid pair: %v", ve.Fields)
			}
			if lat.Valid != tc.wantSet || lng.Valid != tc.wantSet {
				t.Errorf("Valid = %v/%v, want %v for both", lat.Valid, lng.Valid, tc.wantSet)
			}
		})
	}
}

// TestBookingAndProfileAgreeAboutCoordinates is the same property
// TestProfileAndBookingAgreeAboutPhoneNumbers states for phone numbers: a pin
// dropped while booking must not be refused when saved as the profile default.
// One helper is what guarantees it; this is what would notice if a second one
// appeared.
func TestBookingAndProfileAgreeAboutCoordinates(t *testing.T) {
	pairs := []struct{ lat, lng string }{
		{"-7.797068", "110.370529"},
		{"0", "0"},
		{"-90", "180"},
		{"", ""},
		{"-7.797068", ""},
		{"200", "110.370529"},
	}

	for _, p := range pairs {
		bookingVE := NewValidationError()
		coordinatePair(bookingVE, p.lat, p.lng)

		_, _, profileVE := validateProfile("", "", p.lat, p.lng)

		if bookingVE.Any() != (profileVE != nil) {
			t.Errorf("(%q,%q): the booking form and the profile form disagree "+
				"(booking rejected: %v, profile rejected: %v)",
				p.lat, p.lng, bookingVE.Any(), profileVE != nil)
		}
	}
}
