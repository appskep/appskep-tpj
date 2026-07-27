package util_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/util"
)

func TestParsePrice(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  error
	}{
		// The two shapes the form hint promises.
		{name: "plain digits", in: "150000", want: "150000.00"},
		{name: "dotted thousands", in: "150.000", want: "150000.00"},

		{name: "currency prefix", in: "Rp150.000", want: "150000.00"},
		{name: "lowercase prefix", in: "rp150000", want: "150000.00"},
		{name: "prefix with spaces", in: "Rp 150 000", want: "150000.00"},
		// A paste out of a spreadsheet carries U+00A0, which is invisible in the
		// form. Written as an escape rather than a literal, so an editor or a
		// copy-paste cannot silently normalise it back to an ordinary space and
		// leave this case testing nothing.
		{name: "non-breaking space", in: "Rp\u00a0150\u00a0000", want: "150000.00"},
		{name: "tab and newline", in: "\t150000\n", want: "150000.00"},

		// A comma is the decimal point, Indonesian convention.
		{name: "comma decimal", in: "150.000,50", want: "150000.50"},
		{name: "comma one digit", in: "150000,5", want: "150000.50"},
		{name: "comma zero fraction", in: "150000,00", want: "150000.00"},

		// With no comma, one or two trailing digits after a lone dot is a decimal
		// point; three is the unambiguous thousands case above.
		{name: "lone dot two digits", in: "150000.50", want: "150000.50"},
		{name: "lone dot one digit", in: "150000.5", want: "150000.50"},

		{name: "multiple thousands groups", in: "1.234.567", want: "1234567.00"},
		{name: "zero", in: "0", want: "0.00"},
		{name: "leading zeros stripped", in: "000150000", want: "150000.00"},
		{name: "all zeros", in: "0000", want: "0.00"},
		{name: "ten digits is the width of DECIMAL(12,2)", in: "9999999999", want: "9999999999.00"},

		// THE bug Phase 4 caught before it shipped (PLAN.md:495). Read as thousands
		// this is 150,000,555 — a thousandfold error from one mistyped separator,
		// silently charged to a customer.
		{name: "mistyped separator is refused", in: "150000.555", err: util.ErrPriceInvalid},
		{name: "bad grouping", in: "1.23.456", err: util.ErrPriceInvalid},
		{name: "leading group too wide", in: "1234.567", err: util.ErrPriceInvalid},

		{name: "empty", in: "", err: util.ErrPriceInvalid},
		{name: "spaces only", in: "   ", err: util.ErrPriceInvalid},
		{name: "prefix only", in: "Rp", err: util.ErrPriceInvalid},
		{name: "negative refused not clamped", in: "-5000", err: util.ErrPriceInvalid},
		{name: "letters", in: "abc", err: util.ErrPriceInvalid},
		{name: "mixed", in: "150000x", err: util.ErrPriceInvalid},
		{name: "three fraction digits", in: "150000,555", err: util.ErrPriceInvalid},
		{name: "two commas", in: "150,000,50", err: util.ErrPriceInvalid},
		{name: "eleven digits overflows the column", in: "12345678901", err: util.ErrPriceInvalid},
		{name: "no whole part", in: ",50", err: util.ErrPriceInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := util.ParsePrice(tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("ParsePrice(%q) error = %v, want %v", tc.in, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePrice(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParsePrice(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParsePriceNeverLosesPrecision round-trips ParsePrice through PriceToRupiah
// for the three seeded prices, which is the path a real payment takes:
// services.price -> bookings.price_amount -> payments.gross_amount.
func TestParsePriceRoundTripsToRupiah(t *testing.T) {
	for _, typed := range []string{"75000", "150.000", "Rp300.000"} {
		stored, err := util.ParsePrice(typed)
		if err != nil {
			t.Fatalf("ParsePrice(%q): %v", typed, err)
		}
		rupiah, err := util.PriceToRupiah(stored)
		if err != nil {
			t.Fatalf("PriceToRupiah(%q): %v", stored, err)
		}
		if !strings.HasSuffix(stored, ".00") {
			t.Errorf("ParsePrice(%q) = %q, want a .00 fraction", typed, stored)
		}
		if rupiah <= 0 {
			t.Errorf("PriceToRupiah(%q) = %d, want a positive amount", stored, rupiah)
		}
	}
}

func TestPriceToRupiah(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  error
	}{
		{in: "150000.00", want: 150000},
		{in: "0.00", want: 0},
		{in: "75000", want: 75000},
		{in: " 300000.00 ", want: 300000},
		{in: "150000.0", want: 150000},

		// Midtrans has no concept of sen for IDR, so a fraction cannot be charged
		// and rounding it would bill an amount nobody agreed to.
		{in: "150000.50", err: util.ErrPriceFraction},
		{in: "150000.01", err: util.ErrPriceFraction},

		{in: "", err: util.ErrPriceInvalid},
		{in: "abc", err: util.ErrPriceInvalid},
		{in: ".00", err: util.ErrPriceInvalid},
		{in: "150000.ab", err: util.ErrPriceInvalid},
		{in: "-150000.00", err: util.ErrPriceInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := util.PriceToRupiah(tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("PriceToRupiah(%q) error = %v, want %v", tc.in, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PriceToRupiah(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("PriceToRupiah(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseClock(t *testing.T) {
	tests := []struct {
		in   string
		want int
		err  bool
	}{
		{in: "00:00", want: 0},
		{in: "08:00", want: 480},
		{in: "08:30", want: 510},
		{in: "20:00", want: 1200},
		{in: "23:59", want: 1439},
		// Accepted so a value read back out of a TIME column parses.
		{in: "08:00:00", want: 480},
		{in: " 10:30:00 ", want: 630},

		// Seconds must be zero: the schedule is built on whole minutes, so a stray
		// value here is a typo rather than an intent.
		{in: "08:00:30", err: true},
		{in: "08:00:01", err: true},

		{in: "", err: true},
		{in: "8:00", err: true}, // groups must be two digits
		{in: "08:0", err: true},
		{in: "24:00", err: true}, // h > 23
		{in: "08:60", err: true}, // m > 59
		{in: "08", err: true},
		{in: "08:00:00:00", err: true},
		{in: "ab:cd", err: true},
		{in: "-1:00", err: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := util.ParseClock(tc.in)
			if tc.err {
				if !errors.Is(err, util.ErrClockInvalid) {
					t.Fatalf("ParseClock(%q) error = %v, want ErrClockInvalid", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseClock(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseClock(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatClock(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{in: 0, want: "00:00:00"},
		{in: 480, want: "08:00:00"},
		{in: 510, want: "08:30:00"},
		{in: 1200, want: "20:00:00"},
		{in: 1439, want: "23:59:00"},
		// Negative clamps rather than rendering "-1:-0:00".
		{in: -1, want: "00:00:00"},
		{in: -600, want: "00:00:00"},
	}

	for _, tc := range tests {
		if got := util.FormatClock(tc.in); got != tc.want {
			t.Errorf("FormatClock(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatClockAtMidnightIsTheCallersProblem pins the documented hazard: 1440
// renders as "24:00:00", which MySQL rejects. Nothing in the app may produce it —
// Schedule.expand's window walk stops before it can and ParseClock cannot return
// it — but the behaviour is recorded here so a future change to either is a
// deliberate one.
func TestFormatClockAtMidnightIsTheCallersProblem(t *testing.T) {
	if got := util.FormatClock(util.MinutesPerDay); got != "24:00:00" {
		t.Errorf("FormatClock(MinutesPerDay) = %q, want %q — the documented hazard changed",
			got, "24:00:00")
	}
}

// TestClockRoundTrip is the property the generator relies on: every minute of the
// day survives FormatClock -> ParseClock unchanged.
func TestClockRoundTrip(t *testing.T) {
	for m := range util.MinutesPerDay {
		s := util.FormatClock(m)
		back, err := util.ParseClock(s)
		if err != nil {
			t.Fatalf("ParseClock(FormatClock(%d)=%q): %v", m, s, err)
		}
		if back != m {
			t.Fatalf("round trip of %d gave %d (via %q)", m, back, s)
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "the seeded three", in: "Urut Therapeutic", want: "urut-therapeutic"},
		{name: "massage", in: "Massage Therapeutic", want: "massage-therapeutic"},
		{name: "digits kept", in: "Paket 2 Orang", want: "paket-2-orang"},
		{name: "runs collapse", in: "Urut   ---   Therapeutic", want: "urut-therapeutic"},
		{name: "leading separators dropped", in: "  ---Urut", want: "urut"},
		{name: "trailing separators dropped", in: "Urut---  ", want: "urut"},
		{name: "punctuation becomes a separator", in: "Bekam (Basah) & Kering", want: "bekam-basah-kering"},
		// Accented and non-Latin runes are dropped, not transliterated: adding a
		// table would mean golang.org/x/text for a cosmetic gain on ASCII copy.
		{name: "accents dropped", in: "Café Terapi", want: "caf-terapi"},
		{name: "non-latin drops out entirely", in: "терапия", want: ""},
		{name: "nothing usable", in: "!!! ???", want: ""},
		{name: "empty", in: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := util.Slugify(tc.in); got != tc.want {
				t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlugifyBoundsTheColumn: services.slug is VARCHAR(150), and a slug over that
// would be a write error on a name a user is allowed to type.
func TestSlugifyBoundsTheColumn(t *testing.T) {
	long := strings.Repeat("terapi ", 40) // 280 characters
	got := util.Slugify(long)

	if len(got) > 150 {
		t.Errorf("Slugify produced %d characters, over the 150-byte column", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("Slugify(%q...) = %q, want no trailing dash after truncation", long[:20], got)
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// Without these two, the admin search box quietly ignores what was typed:
		// "%" matches every row and "_" matches any character.
		{in: "%", want: `\%`},
		{in: "_", want: `\_`},
		{in: `\`, want: `\\`},
		{in: "urut", want: "urut"},
		{in: "50%_off", want: `50\%\_off`},
		// The backslash is escaped first, so an escape sequence cannot be forged
		// out of a literal backslash followed by a percent.
		{in: `\%`, want: `\\\%`},
		{in: "", want: ""},
	}

	for _, tc := range tests {
		if got := util.EscapeLike(tc.in); got != tc.want {
			t.Errorf("EscapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
