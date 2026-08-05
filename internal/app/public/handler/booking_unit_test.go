package handler

import (
	"net/url"
	"testing"
)

// In-package: bookingHref, bookingMonthHref and bookingQuery are unexported, and
// all three are pure. Between them they build every link on the booking page, and
// the split between the first two is a behavioural rule rather than a tidy-up —
// which is what these tests pin.

// parseBookingHref re-parses a built link, so the assertions are on the values a
// browser would actually send back rather than on the encoded string.
func parseBookingHref(t *testing.T, href string) url.Values {
	t.Helper()

	u, err := url.Parse(href)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", href, err)
	}
	if u.Path != "/booking" {
		t.Fatalf("path = %q, want /booking", u.Path)
	}
	return u.Query()
}

// TestBookingHrefCarriesNoMonth is the rule that keeps the calendar unpinned.
//
// A date or slot link that dragged ?bulan= along would hold the grid on a month
// the visitor navigated away from several clicks ago — so the anchor lives on the
// month arrows alone, and bookingHref must never be able to emit one whatever it
// is handed.
func TestBookingHrefCarriesNoMonth(t *testing.T) {
	tests := []struct {
		name   string
		date   string
		slotID int64
	}{
		{name: "layanan only", date: "", slotID: 0},
		{name: "with a date", date: "2026-08-13", slotID: 0},
		{name: "with a date and a slot", date: "2026-08-13", slotID: 42},
		{name: "an unparseable slot is dropped", date: "2026-08-13", slotID: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := parseBookingHref(t, bookingHref("pijat-tradisional", tc.date, tc.slotID))

			if q.Has("bulan") {
				t.Errorf("bookingHref emitted bulan=%q — a selection link must not "+
					"pin the calendar to a month", q.Get("bulan"))
			}
			if got := q.Get("layanan"); got != "pijat-tradisional" {
				t.Errorf("layanan = %q", got)
			}
			if got := q.Get("tanggal"); got != tc.date {
				t.Errorf("tanggal = %q, want %q", got, tc.date)
			}
			if tc.slotID == 0 && q.Has("slot") {
				t.Errorf("slot = %q, want it omitted", q.Get("slot"))
			}
		})
	}
}

// TestBookingMonthHrefPreservesTheSelection. The arrows change only what is
// shown: whatever was picked has to come back untouched, or pressing › would
// quietly discard the visitor's date.
func TestBookingMonthHrefPreservesTheSelection(t *testing.T) {
	q := parseBookingHref(t, bookingMonthHref("pijat-tradisional", "2026-08-13", 42, "2026-09"))

	want := map[string]string{
		"layanan": "pijat-tradisional",
		"tanggal": "2026-08-13",
		"slot":    "42",
		"bulan":   "2026-09",
	}
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}

	// An empty anchor is omitted rather than sent blank: ParseMonth would fall back
	// to today anyway, but ?bulan= in the address bar reads as a state that is set.
	bare := parseBookingHref(t, bookingMonthHref("pijat-tradisional", "", 0, ""))
	if bare.Has("bulan") {
		t.Errorf("an empty month produced bulan=%q, want it omitted", bare.Get("bulan"))
	}
}

// TestBookingHrefEscapesTheSlug. A slug is admin-supplied text. Slugify keeps
// today's tame, but these links are assembled once and read for years, and a URL
// that stops being safe does so silently.
func TestBookingHrefEscapesTheSlug(t *testing.T) {
	const nasty = "pijat & refleksi /kaki?x=1#top"

	for name, href := range map[string]string{
		"bookingHref":      bookingHref(nasty, "2026-08-13", 42),
		"bookingMonthHref": bookingMonthHref(nasty, "2026-08-13", 42, "2026-09"),
	} {
		t.Run(name, func(t *testing.T) {
			q := parseBookingHref(t, href)
			if got := q.Get("layanan"); got != nasty {
				t.Errorf("layanan = %q, want it to survive the round trip as %q", got, nasty)
			}
			if got := q.Get("tanggal"); got != "2026-08-13" {
				t.Errorf("tanggal = %q — the slug leaked into the next parameter", got)
			}
		})
	}
}
