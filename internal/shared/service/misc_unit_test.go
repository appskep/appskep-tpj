package service

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
)

// scheduleSlot builds a slot for the tests that need one. Shared across this
// package's test files.
func scheduleSlot(t *testing.T, year int, month time.Month, day int, start, end string) sqlc.ScheduleSlot {
	t.Helper()

	loc := testLoc(t)
	date := time.Date(year, month, day, 0, 0, 0, 0, loc)
	return sqlc.ScheduleSlot{
		SlotDate:  date,
		StartTime: start,
		EndTime:   end,
		StartsAt:  date,
		Capacity:  1,
		IsActive:  true,
	}
}

// ---------------------------------------------------------------------------
// Catalog form rules (Phase 4)
// ---------------------------------------------------------------------------

func serviceInput() ServiceInput {
	return ServiceInput{
		Name:        "Urut Therapeutic",
		Description: "Terapi fisik guna mengurangi nyeri otot.",
		Price:       "75000",
		Duration:    "150",
		SortOrder:   "1",
	}
}

func TestCatalogValidate(t *testing.T) {
	t.Run("a valid form", func(t *testing.T) {
		p, ve := validate(serviceInput())
		if ve != nil {
			t.Fatalf("validate rejected a valid form: %v", ve.Fields)
		}
		// Normalised to what the DECIMAL(12,2) column stores.
		if p.price != "75000.00" {
			t.Errorf("price = %q, want %q", p.price, "75000.00")
		}
		if p.duration != 150 || p.sortOrder != 1 {
			t.Errorf("duration/sortOrder = %d/%d", p.duration, p.sortOrder)
		}
		if !p.description.Valid {
			t.Error("description was dropped")
		}
	})

	t.Run("a blank description is NULL, not an empty string", func(t *testing.T) {
		in := serviceInput()
		in.Description = "   "

		p, ve := validate(in)
		if ve != nil {
			t.Fatalf("validate: %v", ve.Fields)
		}
		if p.description.Valid {
			t.Error("a whitespace description was stored rather than nulled")
		}
	})

	t.Run("a blank sort order means first", func(t *testing.T) {
		in := serviceInput()
		in.SortOrder = ""

		p, ve := validate(in)
		if ve != nil {
			t.Fatalf("validate: %v", ve.Fields)
		}
		if p.sortOrder != 0 {
			t.Errorf("sortOrder = %d, want 0", p.sortOrder)
		}
	})

	tests := []struct {
		name   string
		mutate func(*ServiceInput)
		field  string
		msg    string
	}{
		{
			name:   "no name",
			mutate: func(in *ServiceInput) { in.Name = "  " },
			field:  "name", msg: "Nama layanan wajib diisi.",
		},
		{
			name:   "name over the column width",
			mutate: func(in *ServiceInput) { in.Name = strings.Repeat("a", 151) },
			field:  "name", msg: "Nama layanan maksimal 150 karakter.",
		},
		{
			name:   "price is not a number",
			mutate: func(in *ServiceInput) { in.Price = "abc" },
			field:  "price", msg: "Harga tidak valid. Contoh: 150000 atau 150.000",
		},
		{
			// The thousandfold-error case, refused before it reaches the column.
			name:   "price with a mistyped separator",
			mutate: func(in *ServiceInput) { in.Price = "150000.555" },
			field:  "price",
		},
		{
			name:   "negative price",
			mutate: func(in *ServiceInput) { in.Price = "-5000" },
			field:  "price",
		},
		{
			name:   "no duration",
			mutate: func(in *ServiceInput) { in.Duration = "" },
			field:  "duration", msg: "Durasi wajib diisi.",
		},
		{
			name:   "duration is not a number",
			mutate: func(in *ServiceInput) { in.Duration = "dua jam" },
			field:  "duration", msg: "Durasi harus berupa angka dalam menit.",
		},
		{
			name:   "duration under the floor",
			mutate: func(in *ServiceInput) { in.Duration = "14" },
			field:  "duration", msg: "Durasi harus antara 15 dan 480 menit.",
		},
		{
			name:   "duration over the ceiling",
			mutate: func(in *ServiceInput) { in.Duration = "481" },
			field:  "duration",
		},
		{
			name:   "negative sort order",
			mutate: func(in *ServiceInput) { in.SortOrder = "-3" },
			field:  "sort_order", msg: "Urutan harus berupa angka 0 atau lebih.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := serviceInput()
			tc.mutate(&in)

			_, ve := validate(in)
			if ve == nil {
				t.Fatalf("validate accepted %s", tc.name)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("messages = %v, want one on %q", ve.Fields, tc.field)
			}
			if tc.msg != "" && ve.Fields[tc.field] != tc.msg {
				t.Errorf("message = %q, want %q", ve.Fields[tc.field], tc.msg)
			}
		})
	}
}

// TestCatalogValidateReportsEveryProblem is Phase 4's founding convention, and
// the exact case its verification pass used: an empty name, price=abc, duration=5
// and sort_order=-3 must produce all four messages on one submit.
func TestCatalogValidateReportsEveryProblem(t *testing.T) {
	_, ve := validate(ServiceInput{Name: "", Price: "abc", Duration: "5", SortOrder: "-3"})
	if ve == nil {
		t.Fatal("validate accepted a form with four problems")
	}
	for _, field := range []string{"name", "price", "duration", "sort_order"} {
		if _, ok := ve.Fields[field]; !ok {
			t.Errorf("no message for %q; got %v", field, ve.Fields)
		}
	}
}

// ---------------------------------------------------------------------------
// Profile form rules (Phase 9)
// ---------------------------------------------------------------------------

// TestValidateProfileSharesThePhoneRule is why `phone` moved into validate.go: a
// number accepted while booking must not be rejected when saved to the profile,
// and the only way to guarantee that is one implementation.
func TestValidateProfile(t *testing.T) {
	tests := []struct {
		name    string
		phoneIn string
		address string
		field   string
	}{
		{name: "both valid", phoneIn: "081234567890", address: "Jl. Contoh No. 1"},
		// Optional on the profile, required on the booking form — the caller's
		// decision, which is why `phone` does not check required-ness itself.
		{name: "phone may be empty here", phoneIn: "", address: "Jl. Contoh No. 1"},
		{name: "address may be empty", phoneIn: "081234567890", address: ""},
		{name: "both empty", phoneIn: "", address: ""},

		{name: "phone with letters", phoneIn: "0812ABCD", address: "", field: "telepon"},
		{name: "phone too short", phoneIn: "1234567", address: "", field: "telepon"},
		{
			name:    "phone over the column width",
			phoneIn: strings.Repeat("1", 31),
			field:   "telepon",
		},
		{
			name:    "address over the column width",
			phoneIn: "081234567890",
			address: strings.Repeat("a", 501),
			field:   "alamat",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Valid name and email so only phone and address can drive a rejection.
			_, ve := validateProfile(ProfileInput{
				Name: "Budi", Email: "budi@example.com",
				Phone: tc.phoneIn, Address: tc.address,
			})

			if tc.field == "" {
				if ve != nil {
					t.Fatalf("validateProfile rejected a valid form: %v", ve.Fields)
				}
				return
			}
			if ve == nil {
				t.Fatalf("validateProfile accepted %s", tc.name)
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Errorf("messages = %v, want one on %q", ve.Fields, tc.field)
			}
		})
	}
}

// TestProfileAndBookingAgreeAboutPhoneNumbers states the property directly.
func TestProfileAndBookingAgreeAboutPhoneNumbers(t *testing.T) {
	numbers := []string{
		"081234567890", "+6281234567890", "0812 3456 7890",
		"0812-3456-7890", "(0274) 123456", "12345678",
	}

	for _, n := range numbers {
		bookingVE := NewValidationError()
		phone(bookingVE, "telepon", n)

		// Valid name and email so only the phone number can drive a rejection.
		_, profileVE := validateProfile(ProfileInput{
			Name: "Budi", Email: "budi@example.com", Phone: n,
		})

		if bookingVE.Any() != (profileVE != nil) {
			t.Errorf("%q: the booking form and the profile form disagree "+
				"(booking rejected: %v, profile rejected: %v)",
				n, bookingVE.Any(), profileVE != nil)
		}
	}
}

// ---------------------------------------------------------------------------
// Payment helpers (Phase 8/10)
// ---------------------------------------------------------------------------

// TestSameAmount compares two DECIMAL strings by value, textually — never through
// a float64, which CLAUDE.md forbids anywhere on a payment path. It guards the
// webhook's "nominal tidak cocok" branch.
func TestSameAmount(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		// The same amount written two ways: Midtrans sends "150000.00", the column
		// may read back either way.
		{a: "150000.00", b: "150000", want: true},
		{a: "150000", b: "150000.00", want: true},
		{a: "150000.00", b: "150000.00", want: true},
		{a: "0.00", b: "0", want: true},

		{a: "150000.00", b: "1500000.00"},
		{a: "150000.00", b: "15000.00"},
		// A fraction cannot be charged at all, so neither side is comparable.
		{a: "150000.50", b: "150000.50"},
		{a: "", b: "150000.00"},
		{a: "abc", b: "150000.00"},
		{a: "150000.00", b: ""},
	}

	for _, tc := range tests {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if got := sameAmount(tc.a, tc.b); got != tc.want {
				t.Errorf("sameAmount(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestValidJSON: MariaDB implements JSON as LONGTEXT with an implicit
// json_valid() check, so a body that is not JSON has to be wrapped rather than
// stored raw — and it MUST be stored, because an unparseable notification is
// exactly the one worth reading later.
func TestValidJSON(t *testing.T) {
	t.Run("valid JSON is stored verbatim", func(t *testing.T) {
		in := []byte(`{"order_id":"tpj-a","gross_amount":"150000.00"}`)
		if got := string(validJSON(in)); got != string(in) {
			t.Errorf("validJSON rewrote a valid document: %q", got)
		}
	})

	t.Run("garbage is wrapped, not dropped", func(t *testing.T) {
		got := validJSON([]byte("this is not json"))
		if !json.Valid(got) {
			t.Fatalf("validJSON produced something MariaDB will reject: %q", got)
		}

		var wrapped map[string]string
		if err := json.Unmarshal(got, &wrapped); err != nil {
			t.Fatalf("unmarshalling the wrapper: %v", err)
		}
		if wrapped["raw"] != "this is not json" {
			t.Errorf("the original body was not preserved: %+v", wrapped)
		}
	})

	// An empty body is wrapped too, and this is load-bearing rather than tidy.
	// payment_notifications.payload is NOT NULL, so a nil here made a zero-length
	// POST to the public webhook fail its own audit insert and answer 500 — the
	// status that tells Midtrans to retry. Anything on the internet can send a
	// zero-length body to an unauthenticated endpoint.
	t.Run("empty is wrapped, so the NOT NULL audit column can take it", func(t *testing.T) {
		for _, in := range [][]byte{nil, {}} {
			got := validJSON(in)
			if len(got) == 0 {
				t.Fatalf("validJSON(%q) = nil, which cannot be written to payload NOT NULL", in)
			}
			if !json.Valid(got) {
				t.Errorf("validJSON(%q) = %q, which json_valid() will reject", in, got)
			}
		}
	})
}

// TestNullJSON: raw_response is nullable, and an empty result must write NULL
// rather than an empty string, which json_valid() rejects. Every payment row is
// NULL there until Midtrans answers, which is the ordinary case, not the edge one.
func TestNullJSON(t *testing.T) {
	if got := nullJSON(nil); got.Valid {
		t.Errorf("nullJSON(nil) = %+v, want NULL", got)
	}
	if got := nullJSON([]byte(`{"a":1}`)); !got.Valid || got.String != `{"a":1}` {
		t.Errorf("nullJSON = %+v", got)
	}
	if got := nullJSON([]byte("garbage")); !got.Valid || !json.Valid([]byte(got.String)) {
		t.Errorf("nullJSON of garbage = %+v, want valid wrapped JSON", got)
	}
}

// TestClip bounds a value to its column width. Midtrans is free to send a longer
// string than the schema anticipated, and strict mode turns that into a failed
// INSERT on a webhook we are obliged to accept.
func TestClip(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{in: "bank_transfer", n: 50, want: "bank_transfer"},
		{in: "abcdef", n: 6, want: "abcdef"},
		{in: "abcdef", n: 3, want: "abc"},
		{in: "", n: 5, want: ""},
		{in: "abc", n: 0, want: ""},
	}
	for _, tc := range tests {
		if got := clip(tc.in, tc.n); got != tc.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestNullString(t *testing.T) {
	if got := nullString(""); got.Valid {
		t.Errorf("nullString(\"\") = %+v, want NULL", got)
	}
	if got := nullString("x"); !got.Valid || got.String != "x" {
		t.Errorf("nullString(\"x\") = %+v", got)
	}
}

// TestPaymentState derives the lifecycle state the same way the list query's CASE
// does, for the one caller that holds a sqlc.Payment rather than a list row. The
// order matters: a paid payment that later expires is still paid.
func TestPaymentState(t *testing.T) {
	at := sql.NullTime{Time: time.Now(), Valid: true}

	tests := []struct {
		name string
		row  sqlc.Payment
		want string
	}{
		{name: "nothing set", row: sqlc.Payment{}, want: PaymentStatePending},
		{name: "paid", row: sqlc.Payment{PaidAt: at}, want: PaymentStatePaid},
		{name: "cancelled", row: sqlc.Payment{CancelledAt: at}, want: PaymentStateCancelled},
		{name: "expired", row: sqlc.Payment{ExpiredAt: at}, want: PaymentStateExpired},
		{
			// Money arriving after the slot was released: paid_at is set because the
			// money is real, and it wins over both other stamps.
			name: "paid wins over expired",
			row:  sqlc.Payment{PaidAt: at, ExpiredAt: at},
			want: PaymentStatePaid,
		},
		{
			name: "paid wins over cancelled",
			row:  sqlc.Payment{PaidAt: at, CancelledAt: at},
			want: PaymentStatePaid,
		},
		{
			name: "cancelled wins over expired",
			row:  sqlc.Payment{CancelledAt: at, ExpiredAt: at},
			want: PaymentStateCancelled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PaymentState(tc.row); got != tc.want {
				t.Errorf("PaymentState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPaymentStateFilter(t *testing.T) {
	for _, state := range []string{
		PaymentStatePending, PaymentStatePaid, PaymentStateExpired, PaymentStateCancelled,
	} {
		list, filtered := PaymentStateFilter(state)
		if !filtered || list != state {
			t.Errorf("PaymentStateFilter(%q) = %q/%v", state, list, filtered)
		}
	}

	for _, state := range []string{"", "lunas", "PAID", "settlement", "unknown"} {
		list, filtered := PaymentStateFilter(state)
		if filtered {
			t.Errorf("PaymentStateFilter(%q) claimed to be a real state", state)
		}
		if list != allPaymentStates {
			t.Errorf("PaymentStateFilter(%q) = %q, want every state", state, list)
		}
	}
}

// ---------------------------------------------------------------------------
// Settings (Phase 10)
// ---------------------------------------------------------------------------

func TestSettingsFallbacks(t *testing.T) {
	// A nil map is the shape a Settings built outside NewSettings has, and every
	// reader must survive it — the layouts read site_name on every render.
	var empty Settings

	if got := empty.String(KeySiteName, "Terapi Pemuda Jompo"); got != "Terapi Pemuda Jompo" {
		t.Errorf("String on a nil map = %q, want the fallback", got)
	}
	if got := empty.Int(KeyBookingLeadMinutes, 120); got != 120 {
		t.Errorf("Int on a nil map = %d, want the fallback", got)
	}
	if got := empty.All(); len(got) != 0 {
		t.Errorf("All on a nil map = %v", got)
	}

	s := &Settings{values: map[string]string{
		KeySiteName:            "TPJ",
		KeyBookingLeadMinutes:  "90",
		KeyBookingMaxDaysAhead: "not a number",
		KeySiteTagline:         "   ",
	}}

	if got := s.String(KeySiteName, "fallback"); got != "TPJ" {
		t.Errorf("String = %q, want the row", got)
	}
	// A whitespace-only row is treated as absent, so the layout does not render a
	// blank tagline where the default would read.
	if got := s.String(KeySiteTagline, "fallback"); got != "fallback" {
		t.Errorf("String on a blank row = %q, want the fallback", got)
	}
	if got := s.Int(KeyBookingLeadMinutes, 120); got != 90 {
		t.Errorf("Int = %d, want the row", got)
	}
	// An unparseable row is not an error: the env value is the documented
	// fallback, and one broken row must not take the site down.
	if got := s.Int(KeyBookingMaxDaysAhead, 30); got != 30 {
		t.Errorf("Int on an unparseable row = %d, want the fallback", got)
	}

	// All returns a copy, so a caller cannot mutate the cache from under a render.
	all := s.All()
	all[KeySiteName] = "mutated"
	if s.String(KeySiteName, "") != "TPJ" {
		t.Error("All returned the live map rather than a copy")
	}
}

// TestEditableSettingsIsTheWriteWhitelist. The settings form is generated from
// this table and the table is what Update iterates — never the request — so
// nobody who can reach the page can invent a key or overwrite one the code reads
// with a value it cannot parse.
func TestEditableSettings(t *testing.T) {
	if len(EditableSettings) == 0 {
		t.Fatal("EditableSettings is empty — the settings form would render nothing")
	}

	seen := make(map[string]bool, len(EditableSettings))
	for _, f := range EditableSettings {
		if f.Key == "" {
			t.Error("a field has no key")
		}
		if f.Label == "" {
			t.Errorf("%q has no label", f.Key)
		}
		if seen[f.Key] {
			t.Errorf("%q appears twice — the second would silently win", f.Key)
		}
		seen[f.Key] = true

		if f.Numeric {
			if f.Min > f.Max {
				t.Errorf("%q has Min %d above Max %d", f.Key, f.Min, f.Max)
			}
			if f.Multiline {
				t.Errorf("%q is numeric and multiline", f.Key)
			}
		}
	}

	// The keys the code reads by constant must be editable, or an operator cannot
	// change the values the booking rules depend on without a deploy.
	for _, key := range []string{
		KeySiteName, KeyWhatsAppNumber, KeyBookingLeadMinutes,
		KeyBookingMaxDaysAhead, KeyPaymentExpiryMinutes,
	} {
		if !seen[key] {
			t.Errorf("%q is read by the code but is not in EditableSettings", key)
		}
	}
}
