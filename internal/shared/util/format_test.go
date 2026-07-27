package util_test

import (
	"slices"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// jakarta is the application location. Every formatter here takes a time already
// in it — the renderer converts centrally — so the tests build their inputs the
// same way.
var jakarta = mustLoadJakarta()

func mustLoadJakarta() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		panic("util_test: loading Asia/Jakarta: " + err.Error())
	}
	return loc
}

func TestRupiah(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The three seeded prices, as sqlc hands them over.
		{name: "seeded urut", in: "75000.00", want: "Rp75.000"},
		{name: "seeded massage", in: "150000.00", want: "Rp150.000"},
		{name: "seeded bekam", in: "300000.00", want: "Rp300.000"},

		{name: "zero", in: "0.00", want: "Rp0"},
		{name: "empty is zero not a panic", in: "", want: "Rp0"},
		{name: "whitespace", in: "   ", want: "Rp0"},

		{name: "under a thousand", in: "500.00", want: "Rp500"},
		{name: "exactly a thousand", in: "1000.00", want: "Rp1.000"},
		{name: "four digits", in: "9999.00", want: "Rp9.999"},
		{name: "seven digits", in: "1234567.00", want: "Rp1.234.567"},
		{name: "ten digits", in: "1234567890.00", want: "Rp1.234.567.890"},

		{name: "no fraction at all", in: "150000", want: "Rp150.000"},
		{name: "leading zeros", in: "0000150000.00", want: "Rp150.000"},

		// A non-zero fraction is shown rather than rounded away, so a bad amount is
		// visible instead of silent.
		{name: "non-zero fraction shown", in: "150000.50", want: "Rp150.000,5"},
		{name: "one sen", in: "150000.01", want: "Rp150.000,01"},

		{name: "negative keeps its sign", in: "-150000.00", want: "-Rp150.000"},

		// A malformed value degrades to itself rather than to a panic or a number
		// that is quietly wrong.
		{name: "letters pass through", in: "abc.00", want: "Rpabc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := util.Rupiah(tc.in); got != tc.want {
				t.Errorf("Rupiah(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDateFormatters(t *testing.T) {
	// A Sunday, to exercise dayNamesID[0] — the index time.Weekday starts at.
	sunday := time.Date(2026, time.July, 26, 14, 30, 0, 0, jakarta)
	// A Monday in a month whose short name differs from its long one.
	monday := time.Date(2026, time.August, 3, 9, 5, 0, 0, jakarta)

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "DateID", got: util.DateID(sunday), want: "Minggu, 26 Juli 2026"},
		{name: "DateID Monday", got: util.DateID(monday), want: "Senin, 3 Agustus 2026"},
		{name: "DateShortID", got: util.DateShortID(sunday), want: "26 Juli 2026"},
		{name: "DateCompactID", got: util.DateCompactID(sunday), want: "26 Jul 2026"},
		{name: "DateCompactID abbreviates", got: util.DateCompactID(monday), want: "3 Agu 2026"},
		{name: "MonthYearID", got: util.MonthYearID(sunday), want: "Juli 2026"},
		{name: "DateTimeID", got: util.DateTimeID(sunday), want: "26 Juli 2026, 14:30"},
		{name: "DateTimeID pads", got: util.DateTimeID(monday), want: "3 Agustus 2026, 09:05"},
		{name: "DateISO", got: util.DateISO(sunday), want: "2026-07-26"},
	}

	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestDateFormattersHandleZero is load-bearing rather than defensive.
// monthNamesID is indexed directly by time.Month, and a zero time.Time has month
// 1 but is meaningless to show — while an out-of-range index would panic. Every
// formatter guards, and DateISO's guard returns "" rather than "-" because its
// output goes into a sitemap attribute, not onto a page.
func TestDateFormattersHandleZero(t *testing.T) {
	var zero time.Time

	dashes := map[string]string{
		"DateID":        util.DateID(zero),
		"DateShortID":   util.DateShortID(zero),
		"DateCompactID": util.DateCompactID(zero),
		"MonthYearID":   util.MonthYearID(zero),
		"DateTimeID":    util.DateTimeID(zero),
	}
	for name, got := range dashes {
		if got != "-" {
			t.Errorf("%s(zero) = %q, want %q", name, got, "-")
		}
	}

	if got := util.DateISO(zero); got != "" {
		t.Errorf("DateISO(zero) = %q, want an empty string", got)
	}
}

func TestHourMinuteAndTimeRange(t *testing.T) {
	// TIME columns arrive as "08:00:00": go-sql-driver always returns TIME as a
	// string, which is why sqlc.yaml overrides the mapping.
	tests := []struct{ in, want string }{
		{in: "08:00:00", want: "08:00"},
		{in: "15:30:00", want: "15:30"},
		{in: " 10:30:00 ", want: "10:30"},
		{in: "08:00", want: "08:00"},
		{in: "8:00", want: "8:00"}, // too short to trim, passed through
		{in: "", want: ""},
	}
	for _, tc := range tests {
		if got := util.HourMinute(tc.in); got != tc.want {
			t.Errorf("HourMinute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// An EN DASH (U+2013), not a hyphen, and written as an escape so a copy-paste
	// cannot silently downgrade it.
	if got, want := util.TimeRange("08:00:00", "10:30:00"), "08:00 – 10:30"; got != want {
		t.Errorf("TimeRange = %q, want %q", got, want)
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		in   int32
		want string
	}{
		// The seeded slot length, and the spec's own wording for it.
		{in: 150, want: "2,5 jam"},

		{in: 0, want: "-"},
		{in: -30, want: "-"},
		{in: 15, want: "15 menit"},
		{in: 45, want: "45 menit"},
		{in: 59, want: "59 menit"},
		{in: 60, want: "1 jam"},
		{in: 90, want: "1,5 jam"},
		{in: 120, want: "2 jam"},
		{in: 195, want: "3 jam 15 menit"},
		{in: 210, want: "3,5 jam"},
		{in: 480, want: "8 jam"},
	}

	for _, tc := range tests {
		if got := util.Duration(tc.in); got != tc.want {
			t.Errorf("Duration(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParagraphs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: []string{}},
		{name: "whitespace only", in: "   \n  ", want: []string{}},
		{name: "single block", in: "Satu paragraf.", want: []string{"Satu paragraf."}},
		{
			name: "blank line splits",
			in:   "Paragraf satu.\n\nParagraf dua.",
			want: []string{"Paragraf satu.", "Paragraf dua."},
		},
		{
			// A single newline is almost always wrapping in a textarea, not a
			// paragraph break, so it is left alone.
			name: "single newline is not a break",
			in:   "Baris satu.\nBaris dua.",
			want: []string{"Baris satu.\nBaris dua."},
		},
		{
			name: "CRLF normalised first",
			in:   "Paragraf satu.\r\n\r\nParagraf dua.",
			want: []string{"Paragraf satu.", "Paragraf dua."},
		},
		{
			name: "runs of blank lines do not produce empties",
			in:   "Satu.\n\n\n\nDua.",
			want: []string{"Satu.", "Dua."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := util.Paragraphs(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Paragraphs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "shorter than the limit", in: "Urut", max: 10, want: "Urut"},
		{name: "exactly the limit", in: "Urut", max: 4, want: "Urut"},
		{name: "zero limit", in: "Urut", max: 0, want: ""},
		{name: "negative limit", in: "Urut", max: -1, want: ""},
		{
			// Breaks on the last space past half the limit, so words stay whole.
			name: "breaks on a word",
			in:   "Terapi fisik guna mengurangi nyeri otot",
			max:  20,
			want: "Terapi fisik guna…",
		},
		{
			// No space past max/2, so it cuts mid-word rather than losing most of
			// the string.
			name: "cuts mid-word when there is no break",
			in:   "Sebuahkatayangsangatpanjangsekali",
			max:  10,
			want: "Sebuahkata…",
		},
		{
			name: "trailing punctuation trimmed before the ellipsis",
			in:   "Terapi fisik, untuk relaksasi",
			max:  14,
			want: "Terapi fisik…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := util.Truncate(tc.in, tc.max); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestTruncateCountsRunesNotBytes: the descriptions are Indonesian and a byte
// count would both cut a multi-byte character in half and shorten the string
// below what the limit promised.
func TestTruncateCountsRunesNotBytes(t *testing.T) {
	// Ten runes, thirty bytes.
	in := "日本語日本語日本語語"
	if got := util.Truncate(in, 10); got != in {
		t.Errorf("Truncate(%q, 10) = %q, want the input unchanged (10 runes)", in, got)
	}
	if got := util.Truncate(in, 5); got != "日本語日本…" {
		t.Errorf("Truncate(%q, 5) = %q, want the first five runes plus an ellipsis", in, got)
	}
}

func TestWhatsAppLink(t *testing.T) {
	tests := []struct {
		name   string
		number string
		text   string
		want   string
	}{
		{
			name:   "the seeded number",
			number: "6281234567890",
			want:   "https://wa.me/6281234567890",
		},
		{
			// The settings value is edited by hand, so it arrives however someone
			// typed it.
			name:   "punctuation stripped",
			number: "+62 812-3456-7890",
			want:   "https://wa.me/6281234567890",
		},
		{
			name:   "prefilled text is escaped",
			number: "6281234567890",
			text:   "Halo, saya mau tanya soal booking TPJ-20260726-A1B2",
			want:   "https://wa.me/6281234567890?text=Halo%2C+saya+mau+tanya+soal+booking+TPJ-20260726-A1B2",
		},
		{
			name:   "ampersand cannot inject a parameter",
			number: "6281234567890",
			text:   "Ari & Co",
			want:   "https://wa.me/6281234567890?text=Ari+%26+Co",
		},
		// Empty rather than a broken link, so a caller can test the result instead
		// of the input.
		{name: "unset number", number: "", want: ""},
		{name: "punctuation-only number", number: "+- ()", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := util.WhatsAppLink(tc.number, tc.text); got != tc.want {
				t.Errorf("WhatsAppLink(%q, %q) = %q, want %q", tc.number, tc.text, got, tc.want)
			}
		})
	}
}
