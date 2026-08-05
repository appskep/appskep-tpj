package service

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
)

// In-package: newBookingCode, randomCode, slotMessage, expiry and holdsSlot are
// all unexported, and Booking cannot be built from outside without a store. None
// of the five touches one.

// testBooking builds a Booking with no store, sharing one Settings map with the
// Schedule it delegates Window() to — which is how the real graph is wired
// (main.go passes one Schedule instance into NewBooking so the window has exactly
// one definition).
func testBooking(t *testing.T, settings map[string]string, expiryFallback int) *Booking {
	t.Helper()

	s := &Settings{values: settings}
	loc := testLoc(t)
	return &Booking{
		settings:              s,
		schedule:              &Schedule{settings: s, loc: loc},
		loc:                   loc,
		expiryFallbackMinutes: expiryFallback,
	}
}

var bookingCodePattern = regexp.MustCompile(`^TPJ-\d{8}-[` + codeAlphabet + `]{4}$`)

// TestNewBookingCodeShape: the code is read aloud over the phone and typed back
// in, and the date is the SLOT's date rather than today's so it says when the
// customer is coming.
func TestNewBookingCodeShape(t *testing.T) {
	day := time.Date(2026, time.July, 27, 0, 0, 0, 0, testLoc(t))

	code, err := newBookingCode(day)
	if err != nil {
		t.Fatalf("newBookingCode: %v", err)
	}

	if !bookingCodePattern.MatchString(code) {
		t.Fatalf("code = %q, want TPJ-<yyyymmdd>-XXXX over the safe alphabet", code)
	}
	if !strings.HasPrefix(code, "TPJ-20260727-") {
		t.Errorf("code = %q, want the slot's own date", code)
	}
}

// TestBookingCodeAlphabetOmitsConfusablePairs. 0/O, 1/I and 1/L are the pairs
// that get confused reading a code over the phone, so none of the five appears —
// and this is checked over enough draws that a single unlucky sample cannot pass.
func TestBookingCodeAlphabetOmitsConfusablePairs(t *testing.T) {
	for _, c := range "01OIL" {
		if strings.ContainsRune(codeAlphabet, c) {
			t.Errorf("codeAlphabet contains %q, which is confusable when read aloud", c)
		}
	}

	day := time.Date(2026, time.July, 27, 0, 0, 0, 0, testLoc(t))
	for range 2000 {
		code, err := newBookingCode(day)
		if err != nil {
			t.Fatalf("newBookingCode: %v", err)
		}
		suffix := code[len("TPJ-20260727-"):]
		if strings.ContainsAny(suffix, "01OIL") {
			t.Fatalf("code %q carries a confusable character", code)
		}
		if len(suffix) != codeRandomLen {
			t.Fatalf("code %q has a %d-character suffix, want %d", code, len(suffix), codeRandomLen)
		}
	}
}

// TestRandomCodeIsUniform: bytes at or above 248 are discarded rather than
// folded, so no character is more likely than another. A biased draw shrinks the
// effective keyspace, and a guessable booking code is a way to read someone
// else's booking.
func TestRandomCodeIsUniform(t *testing.T) {
	const draws = 40000

	counts := make(map[rune]int, len(codeAlphabet))
	for range draws {
		s, err := randomCode(1)
		if err != nil {
			t.Fatalf("randomCode: %v", err)
		}
		counts[rune(s[0])]++
	}

	// Every character must appear, and none may be wildly over-represented. With
	// 40000 draws over 31 characters the mean is ~1290; folding rather than
	// discarding would make the first nine characters about 12% more likely, which
	// a +/-25% band catches while staying far outside ordinary sampling noise.
	expected := float64(draws) / float64(len(codeAlphabet))
	for _, c := range codeAlphabet {
		got := float64(counts[c])
		if got < expected*0.75 || got > expected*1.25 {
			t.Errorf("character %q drawn %.0f times, want about %.0f — the draw is biased",
				c, got, expected)
		}
	}
	if len(counts) != len(codeAlphabet) {
		t.Errorf("only %d of %d characters were ever drawn", len(counts), len(codeAlphabet))
	}
}

func TestRandomCodeLength(t *testing.T) {
	for _, n := range []int{1, 4, 8, 32} {
		got, err := randomCode(n)
		if err != nil {
			t.Fatalf("randomCode(%d): %v", n, err)
		}
		if len(got) != n {
			t.Errorf("randomCode(%d) returned %d characters", n, len(got))
		}
	}
}

// TestBookingCodesHaveRealEntropy.
//
// This is deliberately NOT a "no collisions" test. Four characters over a
// 31-character alphabet is 31^4 = 923,521 combinations, so by the birthday
// bound 2000 draws expect about n²/2N ≈ 2.2 collisions — a first draft asserting
// "at most 3" failed roughly one run in three, which is the test being wrong
// rather than the code.
//
// Uniqueness is not this function's job anyway: uq_bookings_code is what
// guarantees it, with the collision retried inside the same transaction so the
// slot hold survives. What this function must provide is enough entropy that a
// booking code cannot be guessed — a guessable code is a way to read someone
// else's booking. So the assertion is on the scale of the keyspace, with a
// margin far outside any plausible sampling noise: a generator that collapsed to
// a handful of values, or that stopped varying at all, fails loudly, while an
// unlucky run cannot.
func TestBookingCodesHaveRealEntropy(t *testing.T) {
	const draws = 2000

	day := time.Date(2026, time.July, 27, 0, 0, 0, 0, testLoc(t))

	seen := make(map[string]bool, draws)
	for range draws {
		code, err := newBookingCode(day)
		if err != nil {
			t.Fatalf("newBookingCode: %v", err)
		}
		seen[code] = true
	}

	// Expected collisions are ~2; 100 would mean the keyspace had shrunk by
	// orders of magnitude.
	const floor = draws - 100
	if len(seen) < floor {
		t.Errorf("%d distinct codes in %d draws (want at least %d) — the suffix has "+
			"lost most of its entropy, and a guessable booking code is a way to read "+
			"someone else's booking", len(seen), draws, floor)
	}
}

// buildAvailableMonth: the public calendar's arithmetic.
//
// Every input is a parameter, which is what keeps these nine tests free of
// MariaDB — and is also the reason the function exists separately from
// AvailableMonth, which is nothing but a query and this call.

// monthDay builds a date in the test location. The grid compares dates with
// Equal and Before, so every value one of these tests hands it has to be built
// the same way the production code builds today and until.
func monthDay(t *testing.T, y int, m time.Month, d int) time.Time {
	t.Helper()
	return time.Date(y, m, d, 0, 0, 0, 0, testLoc(t))
}

// freeOn turns a list of days in one month into the map buildAvailableMonth
// takes, keyed the way AvailableMonth keys it.
func freeOn(t *testing.T, y int, m time.Month, days map[int]int64) map[string]int64 {
	t.Helper()
	free := make(map[string]int64, len(days))
	for day, n := range days {
		free[monthDay(t, y, m, day).Format(dateLayout)] = n
	}
	return free
}

// findDay locates one date in the grid. It returns the cell by value; every
// assertion below is on a copy, which is fine because nothing mutates it.
func findDay(t *testing.T, v AvailableMonth, want time.Time) AvailableDay {
	t.Helper()
	for _, week := range v.Weeks {
		for _, cell := range week {
			if cell.Date.Equal(want) {
				return cell
			}
		}
	}
	t.Fatalf("%s is not in the grid", want.Format(dateLayout))
	return AvailableDay{}
}

// TestBuildAvailableMonthIsWholeWeeksMondayFirst. A week that reflows is not a
// week; the grid always holds seven-day rows starting on Monday, with the
// leading and trailing days of the neighbouring months drawn in to complete
// them. August 2026 starts on a Saturday, which is the case that exercises both
// ends.
func TestBuildAvailableMonthIsWholeWeeksMondayFirst(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 5)
	until := monthDay(t, 2026, time.September, 4)

	v := buildAvailableMonth(monthDay(t, 2026, time.August, 1), today, until, nil, loc)

	if len(v.Weeks) == 0 {
		t.Fatal("no weeks")
	}
	for i, week := range v.Weeks {
		if len(week) != 7 {
			t.Fatalf("week %d has %d days, want 7", i, len(week))
		}
	}
	if got := v.Weeks[0][0].Date.Weekday(); got != time.Monday {
		t.Errorf("the grid starts on %v, want Monday", got)
	}
	last := v.Weeks[len(v.Weeks)-1]
	if got := last[6].Date.Weekday(); got != time.Sunday {
		t.Errorf("the grid ends on %v, want Sunday", got)
	}

	// 1 August 2026 is a Saturday, so the first row holds five days of July.
	for i := range 5 {
		if !v.Weeks[0][i].Outside {
			t.Errorf("Weeks[0][%d] (%s) is not marked Outside", i,
				v.Weeks[0][i].Date.Format(dateLayout))
		}
	}
	if v.Weeks[0][5].Outside {
		t.Error("1 August is marked Outside its own month")
	}
	if !v.Anchor.Equal(monthDay(t, 2026, time.August, 1)) {
		t.Errorf("Anchor = %s, want 2026-08-01", v.Anchor.Format(dateLayout))
	}
}

// TestBuildAvailableMonthPrevIsEmptyAtTheWindowStart. This is the clamp the
// visitor sees: at the near edge there is no back arrow at all, because a month
// entirely before the window is forty inert cells.
func TestBuildAvailableMonthPrevIsEmptyAtTheWindowStart(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 5)
	until := monthDay(t, 2026, time.September, 4)

	at := buildAvailableMonth(monthDay(t, 2026, time.August, 1), today, until, nil, loc)
	if at.Prev != "" {
		t.Errorf("Prev = %q at the window's own month, want empty", at.Prev)
	}

	next := buildAvailableMonth(monthDay(t, 2026, time.September, 1), today, until, nil, loc)
	if next.Prev != "2026-08" {
		t.Errorf("Prev = %q one month on, want 2026-08", next.Prev)
	}
}

// TestBuildAvailableMonthNextIsEmptyAtTheWindowEnd. The far edge, and the
// boundary is inclusive: a window ending on 30 September still offers September,
// so Next must be empty there rather than one month too generous.
func TestBuildAvailableMonthNextIsEmptyAtTheWindowEnd(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.September, 1)

	within := buildAvailableMonth(
		monthDay(t, 2026, time.September, 1), today, monthDay(t, 2026, time.September, 30), nil, loc)
	if within.Next != "" {
		t.Errorf("Next = %q with until inside the anchor month, want empty", within.Next)
	}

	beyond := buildAvailableMonth(
		monthDay(t, 2026, time.September, 1), today, monthDay(t, 2026, time.October, 1), nil, loc)
	if beyond.Next != "2026-10" {
		t.Errorf("Next = %q with until in the next month, want 2026-10", beyond.Next)
	}
}

// TestBuildAvailableMonthNextCrossesTheYear. AddDate normalises, so this needs no
// special case in the code — but it is exactly what a hand-rolled month+1 gets
// wrong, and a booking window spanning New Year is an ordinary December.
func TestBuildAvailableMonthNextCrossesTheYear(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.December, 20)
	until := monthDay(t, 2027, time.January, 19)

	v := buildAvailableMonth(monthDay(t, 2026, time.December, 1), today, until, nil, loc)

	if v.Next != "2027-01" {
		t.Errorf("Next = %q, want 2027-01", v.Next)
	}
	if v.Prev != "" {
		t.Errorf("Prev = %q, want empty — the window starts in December", v.Prev)
	}

	jan := buildAvailableMonth(monthDay(t, 2027, time.January, 1), today, until, nil, loc)
	if jan.Prev != "2026-12" {
		t.Errorf("Prev = %q, want 2026-12", jan.Prev)
	}
}

// TestBuildAvailableMonthClampsAnAnchorOutsideTheWindow. ?bulan= is
// attacker-supplied like every other query parameter, and a 2030 anchor rendered
// literally is a page with nothing on it and a Prev arrow forty presses from the
// way back.
func TestBuildAvailableMonthClampsAnAnchorOutsideTheWindow(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 5)
	until := monthDay(t, 2026, time.September, 4)

	far := buildAvailableMonth(monthDay(t, 2030, time.January, 1), today, until, nil, loc)
	if !far.Anchor.Equal(monthDay(t, 2026, time.September, 1)) {
		t.Errorf("Anchor = %s, want it clamped to 2026-09-01", far.Anchor.Format(dateLayout))
	}
	if far.Next != "" {
		t.Errorf("Next = %q at the far edge, want empty", far.Next)
	}
	if far.Prev == "" {
		t.Error("Prev is empty at the far edge — there would be no way back")
	}

	past := buildAvailableMonth(monthDay(t, 1999, time.February, 1), today, until, nil, loc)
	if !past.Anchor.Equal(monthDay(t, 2026, time.August, 1)) {
		t.Errorf("Anchor = %s, want it clamped to 2026-08-01", past.Anchor.Format(dateLayout))
	}
	if past.Prev != "" {
		t.Errorf("Prev = %q at the near edge, want empty", past.Prev)
	}
}

// TestBuildAvailableMonthIgnoresCountsOutsideTheWindow is the load-bearing one.
//
// AvailableDates is bounded by the same window, so today the two agree and this
// can never fire. That is precisely why it is here: the count is what renders in
// the cell and what makes it a link, and a widened query — or a caller that
// passed a different map — must not be able to turn a past or too-distant day
// into something a visitor can click.
func TestBuildAvailableMonthIgnoresCountsOutsideTheWindow(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 10)
	until := monthDay(t, 2026, time.August, 20)

	free := freeOn(t, 2026, time.August, map[int]int64{
		3:  5, // before today
		15: 2, // inside
		28: 7, // past until
	})

	v := buildAvailableMonth(monthDay(t, 2026, time.August, 1), today, until, free, loc)

	for _, day := range []int{3, 28} {
		cell := findDay(t, v, monthDay(t, 2026, time.August, day))
		if cell.Free != 0 {
			t.Errorf("%d August is outside the window but carries Free = %d", day, cell.Free)
		}
		if cell.InWindow {
			t.Errorf("%d August is marked InWindow", day)
		}
		if cell.Bookable() {
			t.Errorf("%d August is bookable — a visitor could click a day that cannot be booked", day)
		}
	}

	inside := findDay(t, v, monthDay(t, 2026, time.August, 15))
	if !inside.Bookable() || inside.Free != 2 {
		t.Errorf("15 August: Free = %d, Bookable = %v; want 2 and true", inside.Free, inside.Bookable())
	}

	// And the discarded counts must not reach the month total either.
	if v.Free != 2 {
		t.Errorf("Free = %d, want 2 — only the in-window day counts", v.Free)
	}
}

// TestBuildAvailableMonthOutsideDaysDoNotCountTowardTheMonth. A trailing cell
// belonging to the next month is still a real bookable date — clicking it is how
// the calendar carries forward — but the line under the grid names this month, so
// it must not be counted there.
func TestBuildAvailableMonthOutsideDaysDoNotCountTowardTheMonth(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 25)
	until := monthDay(t, 2026, time.September, 24)

	free := map[string]int64{
		monthDay(t, 2026, time.August, 26).Format(dateLayout):    3,
		monthDay(t, 2026, time.September, 1).Format(dateLayout):  4,
		monthDay(t, 2026, time.September, 20).Format(dateLayout): 9,
	}

	v := buildAvailableMonth(monthDay(t, 2026, time.August, 1), today, until, free, loc)

	// 31 August 2026 is a Monday, so the last row runs into September and holds
	// the 1st.
	sept1 := findDay(t, v, monthDay(t, 2026, time.September, 1))
	if !sept1.Outside {
		t.Fatal("1 September is not marked Outside in the August grid")
	}
	if !sept1.Bookable() {
		t.Error("1 September is not bookable — a trailing cell must still carry the calendar forward")
	}
	if v.Free != 3 {
		t.Errorf("Free = %d, want 3 — only August's own days", v.Free)
	}
	if v.WindowFree != 16 {
		t.Errorf("WindowFree = %d, want 16 — the whole window regardless of the anchor", v.WindowFree)
	}
}

// TestBuildAvailableMonthWindowFreeIsAnchorIndependent. WindowFree is what tells
// "this month is quiet, try the next" apart from "there is nothing to book at
// all", so it must describe the window and never the page.
func TestBuildAvailableMonthWindowFreeIsAnchorIndependent(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 20)
	until := monthDay(t, 2026, time.September, 19)

	free := map[string]int64{
		monthDay(t, 2026, time.August, 22).Format(dateLayout):    2,
		monthDay(t, 2026, time.September, 10).Format(dateLayout): 5,
	}

	for _, anchor := range []time.Time{
		monthDay(t, 2026, time.August, 1),
		monthDay(t, 2026, time.September, 1),
		monthDay(t, 2030, time.January, 1), // clamped, and still the same total
	} {
		v := buildAvailableMonth(anchor, today, until, free, loc)
		if v.WindowFree != 7 {
			t.Errorf("anchor %s: WindowFree = %d, want 7",
				anchor.Format(monthLayout), v.WindowFree)
		}
	}
}

// TestBuildAvailableMonthMarksExactlyOneToday. Today is drawn differently from
// every other cell, and it appears in two grids — its own month's, and the
// leading week of the next month's. Marking it twice in one grid, or not at all,
// are both visible mistakes.
func TestBuildAvailableMonthMarksExactlyOneToday(t *testing.T) {
	loc := testLoc(t)
	today := monthDay(t, 2026, time.August, 31) // a Monday, so it leads September's grid
	until := monthDay(t, 2026, time.September, 30)

	for _, tc := range []struct {
		name        string
		anchor      time.Time
		wantOutside bool
	}{
		{"its own month", monthDay(t, 2026, time.August, 1), false},
		{"the next month's leading week", monthDay(t, 2026, time.September, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := buildAvailableMonth(tc.anchor, today, until, nil, loc)

			var marked []AvailableDay
			for _, week := range v.Weeks {
				for _, cell := range week {
					if cell.IsToday {
						marked = append(marked, cell)
					}
				}
			}
			if len(marked) != 1 {
				t.Fatalf("%d cells marked IsToday, want exactly 1", len(marked))
			}
			if !marked[0].Date.Equal(today) {
				t.Errorf("IsToday is on %s, want %s",
					marked[0].Date.Format(dateLayout), today.Format(dateLayout))
			}
			if marked[0].Outside != tc.wantOutside {
				t.Errorf("Outside = %v, want %v", marked[0].Outside, tc.wantOutside)
			}
			if !marked[0].InWindow {
				t.Error("today is not InWindow — the window starts today")
			}
		})
	}
}

func TestSlotMessage(t *testing.T) {
	loc := testLoc(t)
	b := testBooking(t, map[string]string{
		KeyBookingLeadMinutes:  "120",
		KeyBookingMaxDaysAhead: "30",
	}, 60)

	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// A slot comfortably inside the window: three days out.
	bookable := func() sqlc.ScheduleSlot {
		day := today.AddDate(0, 0, 3)
		return sqlc.ScheduleSlot{
			SlotDate:    day,
			StartsAt:    day.Add(10 * time.Hour),
			Capacity:    1,
			BookedCount: 0,
			IsActive:    true,
		}
	}

	tests := []struct {
		name   string
		mutate func(*sqlc.ScheduleSlot)
		want   string
	}{
		{name: "bookable", mutate: func(*sqlc.ScheduleSlot) {}, want: ""},
		{
			name:   "deactivated",
			mutate: func(s *sqlc.ScheduleSlot) { s.IsActive = false },
			want:   "Jadwal tersebut sudah tidak tersedia. Pilih jadwal lain.",
		},
		{
			name:   "full",
			mutate: func(s *sqlc.ScheduleSlot) { s.BookedCount = 1 },
			want:   "Jadwal tersebut sudah penuh. Pilih jadwal lain.",
		},
		{
			name:   "over capacity",
			mutate: func(s *sqlc.ScheduleSlot) { s.Capacity, s.BookedCount = 2, 3 },
			want:   "Jadwal tersebut sudah penuh. Pilih jadwal lain.",
		},
		{
			// Inside the 120-minute lead time.
			name: "starting in an hour",
			mutate: func(s *sqlc.ScheduleSlot) {
				s.SlotDate = today
				s.StartsAt = now.Add(time.Hour)
			},
			want: "Jadwal tersebut terlalu dekat atau sudah lewat. Pilih jadwal lain.",
		},
		{
			name: "already started",
			mutate: func(s *sqlc.ScheduleSlot) {
				s.SlotDate = today
				s.StartsAt = now.Add(-time.Hour)
			},
			want: "Jadwal tersebut terlalu dekat atau sudah lewat. Pilih jadwal lain.",
		},
		{
			// Past the 30-day ceiling.
			name: "sixty days out",
			mutate: func(s *sqlc.ScheduleSlot) {
				day := today.AddDate(0, 0, 60)
				s.SlotDate, s.StartsAt = day, day.Add(10*time.Hour)
			},
			want: "Jadwal tersebut terlalu jauh ke depan. Pilih jadwal lain.",
		},
		{
			name: "exactly at the ceiling is bookable",
			mutate: func(s *sqlc.ScheduleSlot) {
				day := today.AddDate(0, 0, 30)
				s.SlotDate, s.StartsAt = day, day.Add(10*time.Hour)
			},
			want: "",
		},
		{
			// The order matters: an inactive full slot reports inactive, which is the
			// more useful of the two.
			name: "inactive wins over full",
			mutate: func(s *sqlc.ScheduleSlot) {
				s.IsActive = false
				s.BookedCount = 1
			},
			want: "Jadwal tersebut sudah tidak tersedia. Pilih jadwal lain.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			slot := bookable()
			tc.mutate(&slot)

			if got := b.slotMessage(slot); got != tc.want {
				t.Errorf("slotMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExpiry covers the settings-over-environment ladder CLAUDE.md specifies: the
// DB row wins when present and parseable, the env value is the fallback, and the
// compiled default backs up both.
func TestExpiry(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]string
		fallback int
		want     time.Duration
	}{
		{
			name:     "the settings row wins",
			settings: map[string]string{KeyPaymentExpiryMinutes: "45"},
			fallback: 60,
			want:     45 * time.Minute,
		},
		{
			name:     "the env value is the fallback",
			settings: nil,
			fallback: 90,
			want:     90 * time.Minute,
		},
		{
			name:     "an unparseable row falls back to env",
			settings: map[string]string{KeyPaymentExpiryMinutes: "satu jam"},
			fallback: 90,
			want:     90 * time.Minute,
		},
		{
			name:     "an empty row falls back to env",
			settings: map[string]string{KeyPaymentExpiryMinutes: ""},
			fallback: 90,
			want:     90 * time.Minute,
		},
		{
			// config.validate refuses a non-positive PAYMENT_EXPIRY_MINUTES at boot,
			// so this only fires if something got past it — and a zero hold would
			// expire every booking the moment it was made.
			name:     "a zero env value falls back to the compiled default",
			settings: nil,
			fallback: 0,
			want:     defaultExpiryMinutes * time.Minute,
		},
		{
			name:     "a zero row falls back too",
			settings: map[string]string{KeyPaymentExpiryMinutes: "0"},
			fallback: 0,
			want:     defaultExpiryMinutes * time.Minute,
		},
		{
			name:     "a negative row falls back",
			settings: map[string]string{KeyPaymentExpiryMinutes: "-30"},
			fallback: 60,
			want:     60 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := testBooking(t, tc.settings, tc.fallback)
			if got := b.expiry(); got != tc.want {
				t.Errorf("expiry() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBookingStatusFilter is the whitelist a request's ?status= resolves through.
//
// The value reaches SQL as a bind parameter either way, so this is not about
// injection. It is about what an unrecognised value should mean: mapping through a
// whitelist makes a hand-typed or stale query string fall back to "everything",
// where passing it through would return an empty list that reads as "you have no
// bookings".
func TestBookingStatusFilter(t *testing.T) {
	real := []string{
		"pending_payment", "paid", "confirmed", "completed", "cancelled", "expired",
	}

	for _, status := range real {
		t.Run(status, func(t *testing.T) {
			list, filtered := BookingStatusFilter(status)
			if !filtered {
				t.Errorf("BookingStatusFilter(%q) reported no filter", status)
			}
			if list != status {
				t.Errorf("list = %q, want %q", list, status)
			}
		})
	}

	unknown := []string{"", "lunas", "PAID", "pending", "'; DROP TABLE bookings; --", "all"}
	for _, status := range unknown {
		t.Run("unknown/"+status, func(t *testing.T) {
			list, filtered := BookingStatusFilter(status)
			if filtered {
				t.Errorf("BookingStatusFilter(%q) claimed to be a real status", status)
			}
			// Every status, so the page says "you have none yet" rather than "none
			// with this status" — the handler needs the second return to choose.
			if list != allBookingStatuses {
				t.Errorf("list = %q, want every status", list)
			}
		})
	}
}

// TestAllBookingStatusesCoversTheEnum. The "no filter" value is spelled out as a
// comma-joined string because sqlc's MySQL engine has no sqlc.slice(), so it can
// drift from the schema silently — a status missing from it would vanish from
// every unfiltered list.
func TestAllBookingStatusesCoversTheEnum(t *testing.T) {
	every := []sqlc.BookingsStatus{
		sqlc.BookingsStatusPendingPayment,
		sqlc.BookingsStatusPaid,
		sqlc.BookingsStatusConfirmed,
		sqlc.BookingsStatusCompleted,
		sqlc.BookingsStatusCancelled,
		sqlc.BookingsStatusExpired,
	}

	parts := strings.Split(allBookingStatuses, ",")
	if len(parts) != len(every) {
		t.Fatalf("allBookingStatuses lists %d statuses, the enum has %d", len(parts), len(every))
	}
	for _, s := range every {
		if !strings.Contains(allBookingStatuses, string(s)) {
			t.Errorf("allBookingStatuses is missing %q — it would vanish from every unfiltered list", s)
		}
	}
}

// TestCanCancelOrReschedule pins the guard set: the statuses an operator may
// still act on. `completed` is deliberately NOT among them — the visit happened,
// and cancelling or moving it afterwards is a correction to make in the record,
// not a slot to give back.
//
// This test is why the predicate is no longer called holdsSlot. Written against
// the old name and its old doc comment ("the rule the booked_count invariant is
// defined by"), it expected `completed` to be true and failed. The behaviour at
// all four call sites was correct; the name promised something else. See
// TestSlotReleasingStatuses below for the set the name used to claim.
func TestCanCancelOrReschedule(t *testing.T) {
	want := map[sqlc.BookingsStatus]bool{
		sqlc.BookingsStatusPendingPayment: true,
		sqlc.BookingsStatusPaid:           true, // allowed; the refund is manual (Q6)
		sqlc.BookingsStatusConfirmed:      true,
		sqlc.BookingsStatusCompleted:      false,
		sqlc.BookingsStatusCancelled:      false,
		sqlc.BookingsStatusExpired:        false,
	}

	for status, want := range want {
		if got := canCancelOrReschedule(status); got != want {
			t.Errorf("canCancelOrReschedule(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestSlotReleasingStatuses pins the OTHER set — the one the booked_count
// invariant is actually defined by, and the one that must never be confused with
// the guard above.
//
// Only 'cancelled' and 'expired' release a slot. The rule is encoded once, in the
// bookings.active_slot_id generated column, so no status transition can forget to
// clear it; this test is the Go-side mirror of that column, and it exists so a
// future reader who needs "does this status hold its slot" finds an answer here
// rather than reaching for canCancelOrReschedule and releasing every completed
// booking's slot.
func TestSlotReleasingStatuses(t *testing.T) {
	releases := map[sqlc.BookingsStatus]bool{
		sqlc.BookingsStatusPendingPayment: false,
		sqlc.BookingsStatusPaid:           false,
		sqlc.BookingsStatusConfirmed:      false,
		sqlc.BookingsStatusCompleted:      false, // holds — an admin completing a booking must not free the slot
		sqlc.BookingsStatusCancelled:      true,
		sqlc.BookingsStatusExpired:        true,
	}

	for status, releasesSlot := range releases {
		// The generated column's expression, restated: NULL (released) when the
		// status is cancelled or expired, the slot id otherwise.
		inGeneratedColumn := status == sqlc.BookingsStatusCancelled || status == sqlc.BookingsStatusExpired
		if inGeneratedColumn != releasesSlot {
			t.Errorf("%q: this test and 0001_schema.sql disagree about whether it releases the slot", status)
		}

		// And the guard set is a strict subset of the holding set — never the
		// reverse. A status that releases its slot can never be cancellable.
		if releasesSlot && canCancelOrReschedule(status) {
			t.Errorf("%q releases its slot but is still cancellable — cancelling it "+
				"would release the same slot twice", status)
		}
	}
}
