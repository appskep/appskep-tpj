package service

import (
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
)

// In-package: expand, mondayOffset, weekdaySet, matchesStatus and dayOf are all
// unexported, and neither Schedule nor Settings can be built from outside without
// a database. expand itself touches no store — only s.loc — so a struct literal
// is enough, which is what makes the generator's arithmetic testable without
// MariaDB at all.

func testLoc(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("loading Asia/Jakarta: %v", err)
	}
	return loc
}

// testSchedule builds a Schedule with no store. Every function under test here
// reads s.loc and, for Window, s.settings — never s.store.
func testSchedule(t *testing.T, settings map[string]string) *Schedule {
	t.Helper()
	return &Schedule{loc: testLoc(t), settings: &Settings{values: settings}}
}

// allWeekdays is time.Weekday numbers as the form submits them.
var allWeekdays = []string{"0", "1", "2", "3", "4", "5", "6"}

func generateInput() GenerateInput {
	return GenerateInput{
		From:        "2026-08-03", // a Monday
		To:          "2026-08-03",
		WindowStart: "08:00",
		WindowEnd:   "20:00",
		Duration:    "150",
		Break:       "0",
		Capacity:    "1",
		Weekdays:    allWeekdays,
		IsActive:    true,
	}
}

// TestExpandLastSlotMustFitWhole is the rule that decides whether customers get
// booked after closing time.
//
// `t + duration <= windowEnd` — with 08:00-20:00 and 150 minutes that is four
// slots, not five: the 18:00 one would end at 20:30. Phase 5 verified this by
// generating two weeks and counting 56 rows (14 x 4); this is the same arithmetic
// without the database.
func TestExpandLastSlotMustFitWhole(t *testing.T) {
	s := testSchedule(t, nil)

	cands, from, to, ve := s.expand(generateInput())
	if ve != nil {
		t.Fatalf("expand rejected a valid form: %v", ve.Fields)
	}

	want := []Candidate{
		{Start: "08:00:00", End: "10:30:00"},
		{Start: "10:30:00", End: "13:00:00"},
		{Start: "13:00:00", End: "15:30:00"},
		{Start: "15:30:00", End: "18:00:00"},
	}
	if len(cands) != len(want) {
		t.Fatalf("08:00-20:00 x 150min produced %d slots, want %d — the last slot "+
			"must fit whole, so the 18:00 one (ending 20:30) is dropped", len(cands), len(want))
	}
	for i, c := range cands {
		if c.Start != want[i].Start || c.End != want[i].End {
			t.Errorf("slot %d = %s-%s, want %s-%s", i, c.Start, c.End, want[i].Start, want[i].End)
		}
	}

	if !from.Equal(to) {
		t.Errorf("from/to = %v/%v, want the same single day", from, to)
	}
}

func TestExpandBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		windowStart string
		windowEnd   string
		duration    string
		brk         string
		want        int
		why         string
	}{
		{
			name: "exactly one slot fits", windowStart: "08:00", windowEnd: "10:30",
			duration: "150", brk: "0", want: 1,
		},
		{
			name: "one minute short of a slot", windowStart: "08:00", windowEnd: "10:29",
			duration: "150", brk: "0", want: 0,
			why: "a slot that would spill past closing time is not created",
		},
		{
			name: "two exact slots", windowStart: "08:00", windowEnd: "13:00",
			duration: "150", brk: "0", want: 2,
		},
		{
			// The break pushes each start later, but the bound is still on the END of
			// a slot: 08:00, 11:00, 14:00, 17:00 — the last ends at 19:30, inside the
			// window. A fifth would start at 20:00 and is dropped.
			name: "a break moves the starts", windowStart: "08:00", windowEnd: "20:00",
			duration: "150", brk: "30", want: 4,
			why: "the break shifts the starts; the window bound is on the slot's end",
		},
		{
			name: "a break does not extend past the window", windowStart: "08:00", windowEnd: "16:00",
			duration: "150", brk: "30", want: 2,
		},
		{
			name: "short slots", windowStart: "08:00", windowEnd: "09:00",
			duration: "15", brk: "0", want: 4,
		},
		{
			name: "the whole day", windowStart: "00:00", windowEnd: "23:59",
			duration: "480", brk: "0", want: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testSchedule(t, nil)
			in := generateInput()
			in.WindowStart, in.WindowEnd = tc.windowStart, tc.windowEnd
			in.Duration, in.Break = tc.duration, tc.brk

			cands, _, _, ve := s.expand(in)
			if tc.want == 0 {
				if ve == nil {
					t.Fatalf("expand accepted a window nothing fits in, producing %d slots", len(cands))
				}
				if _, ok := ve.Fields["window_end"]; !ok {
					t.Errorf("messages = %v, want one on window_end", ve.Fields)
				}
				return
			}
			if ve != nil {
				t.Fatalf("expand rejected a valid form: %v", ve.Fields)
			}
			if len(cands) != tc.want {
				starts := make([]string, len(cands))
				for i, c := range cands {
					starts[i] = c.Start
				}
				t.Errorf("got %d slots %v, want %d — %s", len(cands), starts, tc.want, tc.why)
			}
		})
	}
}

// TestExpandNeverProducesMidnight. util.FormatClock(1440) renders "24:00:00",
// which MySQL rejects on a TIME column. The window walk is what guarantees the
// generator cannot reach it, and this is the check that keeps that true.
func TestExpandNeverProducesMidnight(t *testing.T) {
	s := testSchedule(t, nil)

	in := generateInput()
	in.WindowStart, in.WindowEnd = "00:00", "23:59"
	in.Duration, in.Break = "15", "0"

	cands, _, _, ve := s.expand(in)
	if ve != nil {
		t.Fatalf("expand: %v", ve.Fields)
	}
	for _, c := range cands {
		if c.Start >= "24:00:00" || c.End >= "24:00:00" {
			t.Fatalf("generated %s-%s, which MySQL will reject", c.Start, c.End)
		}
	}
}

func TestExpandWeekdayFilter(t *testing.T) {
	s := testSchedule(t, nil)

	in := generateInput()
	in.From, in.To = "2026-08-03", "2026-08-09" // Monday to Sunday
	in.WindowStart, in.WindowEnd = "08:00", "10:30"
	in.Duration = "150"

	t.Run("weekdays only", func(t *testing.T) {
		in := in
		in.Weekdays = []string{"1", "2", "3", "4", "5"}

		cands, _, _, ve := s.expand(in)
		if ve != nil {
			t.Fatalf("expand: %v", ve.Fields)
		}
		if len(cands) != 5 {
			t.Errorf("Mon-Fri over a full week produced %d slots, want 5", len(cands))
		}
		for _, c := range cands {
			if d := c.Date.Weekday(); d == time.Saturday || d == time.Sunday {
				t.Errorf("a %v slot was generated with weekends deselected", d)
			}
		}
	})

	t.Run("a single day of the week", func(t *testing.T) {
		in := in
		in.Weekdays = []string{"0"} // Sunday

		cands, _, _, ve := s.expand(in)
		if ve != nil {
			t.Fatalf("expand: %v", ve.Fields)
		}
		if len(cands) != 1 {
			t.Fatalf("one Sunday in the range produced %d slots, want 1", len(cands))
		}
		if got := cands[0].Date.Weekday(); got != time.Sunday {
			t.Errorf("generated a %v, want Sunday", got)
		}
	})

	t.Run("no day selected", func(t *testing.T) {
		in := in
		in.Weekdays = nil

		_, _, _, ve := s.expand(in)
		if ve == nil {
			t.Fatal("expand accepted a form with no weekday selected")
		}
		if got, want := ve.Fields["weekdays"], "Pilih minimal satu hari."; got != want {
			t.Errorf("message = %q, want %q", got, want)
		}
	})
}

// TestExpandGuards covers the bounds that stop one submit becoming a hundred
// thousand inserts inside a single transaction.
func TestExpandGuards(t *testing.T) {
	t.Run("the 500-slot ceiling", func(t *testing.T) {
		s := testSchedule(t, nil)

		in := generateInput()
		// 180 days x 4 slots = 720, past the cap and inside the day range.
		in.From, in.To = "2026-08-03", "2027-01-30"
		in.Duration = "150"

		_, _, _, ve := s.expand(in)
		if ve == nil {
			t.Fatal("expand accepted a run over the 500-slot cap")
		}
		if _, ok := ve.Fields["to"]; !ok {
			t.Errorf("messages = %v, want one on the date range", ve.Fields)
		}
	})

	t.Run("the 180-day ceiling", func(t *testing.T) {
		s := testSchedule(t, nil)

		in := generateInput()
		// A mistyped year is the case this exists for.
		in.From, in.To = "2026-08-03", "2027-08-03"

		_, _, _, ve := s.expand(in)
		if ve == nil {
			t.Fatal("expand accepted a range over 180 days")
		}
		if got := ve.Fields["to"]; got != "Rentang maksimal 180 hari sekali generate." {
			t.Errorf("message = %q", got)
		}
	})

	t.Run("exactly 180 days is accepted", func(t *testing.T) {
		s := testSchedule(t, nil)

		in := generateInput()
		in.From = "2026-08-03"
		in.To = "2027-01-30" // from + 180
		// One slot a day keeps it under the 500-slot cap.
		in.WindowStart, in.WindowEnd, in.Duration = "08:00", "10:30", "150"

		cands, _, _, ve := s.expand(in)
		if ve != nil {
			t.Fatalf("expand rejected exactly 180 days: %v", ve.Fields)
		}
		if len(cands) != 181 {
			t.Errorf("got %d slots, want 181 (inclusive of both ends)", len(cands))
		}
	})
}

func TestExpandValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GenerateInput)
		field  string
		msg    string
	}{
		{
			name:   "no start date",
			mutate: func(in *GenerateInput) { in.From = "" },
			field:  "from", msg: "Tanggal awal wajib diisi.",
		},
		{
			name:   "no end date",
			mutate: func(in *GenerateInput) { in.To = "" },
			field:  "to", msg: "Tanggal akhir wajib diisi.",
		},
		{
			name:   "unparseable start date",
			mutate: func(in *GenerateInput) { in.From = "03/08/2026" },
			field:  "from", msg: "Format tanggal awal tidak valid.",
		},
		{
			name:   "end before start",
			mutate: func(in *GenerateInput) { in.From, in.To = "2026-08-10", "2026-08-03" },
			field:  "to", msg: "Tanggal akhir tidak boleh sebelum tanggal awal.",
		},
		{
			name:   "unparseable opening time",
			mutate: func(in *GenerateInput) { in.WindowStart = "8am" },
			field:  "window_start", msg: "Jam buka tidak valid. Contoh: 08:00",
		},
		{
			name:   "closing before opening",
			mutate: func(in *GenerateInput) { in.WindowStart, in.WindowEnd = "20:00", "08:00" },
			field:  "window_end", msg: "Jam tutup harus setelah jam buka.",
		},
		{
			name:   "closing equal to opening",
			mutate: func(in *GenerateInput) { in.WindowStart, in.WindowEnd = "08:00", "08:00" },
			field:  "window_end", msg: "Jam tutup harus setelah jam buka.",
		},
		{
			name:   "duration under the floor",
			mutate: func(in *GenerateInput) { in.Duration = "14" },
			field:  "duration",
		},
		{
			name:   "duration over the ceiling",
			mutate: func(in *GenerateInput) { in.Duration = "481" },
			field:  "duration",
		},
		{
			name:   "duration is not a number",
			mutate: func(in *GenerateInput) { in.Duration = "dua jam" },
			field:  "duration",
		},
		{
			name:   "negative break",
			mutate: func(in *GenerateInput) { in.Break = "-1" },
			field:  "break",
		},
		{
			name:   "break over the ceiling",
			mutate: func(in *GenerateInput) { in.Break = "241" },
			field:  "break",
		},
		{
			name:   "capacity below one",
			mutate: func(in *GenerateInput) { in.Capacity = "0" },
			field:  "capacity",
		},
		{
			name:   "capacity over the ceiling",
			mutate: func(in *GenerateInput) { in.Capacity = "21" },
			field:  "capacity",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testSchedule(t, nil)
			in := generateInput()
			tc.mutate(&in)

			cands, _, _, ve := s.expand(in)
			if ve == nil {
				t.Fatalf("expand accepted %s, producing %d slots", tc.name, len(cands))
			}
			if _, ok := ve.Fields[tc.field]; !ok {
				t.Fatalf("messages = %v, want one on %q", ve.Fields, tc.field)
			}
			if tc.msg != "" && ve.Fields[tc.field] != tc.msg {
				t.Errorf("message = %q, want %q", ve.Fields[tc.field], tc.msg)
			}
			if cands != nil {
				t.Error("expand returned candidates alongside a validation error")
			}
		})
	}
}

// TestExpandReportsEveryProblemAtOnce is Phase 4's convention: a rejected form
// reports every problem on one submit, so a user does not fix them one at a time.
func TestExpandReportsEveryProblemAtOnce(t *testing.T) {
	s := testSchedule(t, nil)

	in := generateInput()
	in.From = ""
	in.WindowStart = "nope"
	in.Duration = "5"
	in.Capacity = "0"
	in.Weekdays = nil

	_, _, _, ve := s.expand(in)
	if ve == nil {
		t.Fatal("expand accepted a form with five problems")
	}
	for _, field := range []string{"from", "window_start", "duration", "capacity", "weekdays"} {
		if _, ok := ve.Fields[field]; !ok {
			t.Errorf("no message for %q; got %v", field, ve.Fields)
		}
	}
}

// TestExpandIsDeterministic: Plan and Apply both call it, so a preview that
// described a different set from the one written would make the preview
// worthless.
func TestExpandIsDeterministic(t *testing.T) {
	s := testSchedule(t, nil)
	in := generateInput()
	in.From, in.To = "2026-08-03", "2026-08-16"

	first, _, _, ve := s.expand(in)
	if ve != nil {
		t.Fatalf("expand: %v", ve.Fields)
	}
	second, _, _, ve := s.expand(in)
	if ve != nil {
		t.Fatalf("expand: %v", ve.Fields)
	}

	if len(first) != len(second) {
		t.Fatalf("two expansions of one form gave %d and %d slots", len(first), len(second))
	}
	// Phase 5's measured number: 14 days x 4 slots.
	if len(first) != 56 {
		t.Errorf("two weeks of 08:00-20:00 x 150min = %d slots, want 56", len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("slot %d differs between expansions: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestWindow(t *testing.T) {
	t.Run("the seeded settings", func(t *testing.T) {
		s := testSchedule(t, map[string]string{
			KeyBookingLeadMinutes:  "120",
			KeyBookingMaxDaysAhead: "30",
		})

		startsAt, until := s.Window()

		lead := time.Until(startsAt)
		if lead < 119*time.Minute || lead > 121*time.Minute {
			t.Errorf("lead time = %v, want about 120 minutes", lead)
		}

		// until is a DATE compared against slot_date — the last bookable day,
		// inclusive — not an instant. Its clock must therefore be midnight.
		if h, m, sec := until.Clock(); h != 0 || m != 0 || sec != 0 {
			t.Errorf("until = %v, want a midnight date rather than an instant", until)
		}
		if got := until.Sub(s.Today()).Hours() / 24; got != 30 {
			t.Errorf("until is %v days ahead, want 30", got)
		}
	})

	t.Run("falls back when the settings row is missing", func(t *testing.T) {
		// A missing key behaves like the shipped default rather than like "no lead
		// time at all", which would let someone book a slot starting in a minute.
		s := testSchedule(t, nil)

		startsAt, until := s.Window()
		if lead := time.Until(startsAt); lead < 119*time.Minute {
			t.Errorf("lead time with no settings row = %v, want the 120-minute default", lead)
		}
		if got := until.Sub(s.Today()).Hours() / 24; got != 30 {
			t.Errorf("days ahead with no settings row = %v, want the default 30", got)
		}
	})

	t.Run("an unparseable row falls back too", func(t *testing.T) {
		s := testSchedule(t, map[string]string{
			KeyBookingLeadMinutes:  "dua jam",
			KeyBookingMaxDaysAhead: "",
		})

		startsAt, _ := s.Window()
		if lead := time.Until(startsAt); lead < 119*time.Minute {
			t.Errorf("lead time = %v, want the default when the row does not parse", lead)
		}
	})
}

func TestGenerateDefaults(t *testing.T) {
	s := testSchedule(t, map[string]string{
		KeySlotDefaultDuration: "150",
		KeySlotDefaultCapacity: "2",
	})

	in := s.GenerateDefaults()

	if in.Duration != "150" {
		t.Errorf("Duration = %q, want the settings value", in.Duration)
	}
	if in.Capacity != "2" {
		t.Errorf("Capacity = %q, want the settings value", in.Capacity)
	}
	if in.From != s.Today().Format(dateLayout) {
		t.Errorf("From = %q, want today", in.From)
	}

	// The defaults must themselves be a valid form — an admin who opens the page
	// and presses generate should not get a validation error.
	cands, _, _, ve := s.expand(in)
	if ve != nil {
		t.Fatalf("the prefilled form does not validate: %v", ve.Fields)
	}
	if len(cands) != 56 {
		t.Errorf("the prefilled form generates %d slots, want 56 (14 days x 4)", len(cands))
	}
}

func TestWeekdaySet(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []time.Weekday
	}{
		{name: "sunday is zero", in: []string{"0"}, want: []time.Weekday{time.Sunday}},
		{name: "saturday is six", in: []string{"6"}, want: []time.Weekday{time.Saturday}},
		{
			name: "the seeded slot_weekdays",
			in:   []string{"1", "2", "3", "4", "5", "6", "0"},
			want: []time.Weekday{
				time.Sunday, time.Monday, time.Tuesday, time.Wednesday,
				time.Thursday, time.Friday, time.Saturday,
			},
		},
		{name: "empty", in: nil},
		{name: "out of range is dropped", in: []string{"7", "-1"}},
		{name: "not a number is dropped", in: []string{"senin"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := weekdaySet(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("weekdaySet(%v) has %d entries, want %d", tc.in, len(got), len(tc.want))
			}
			for _, d := range tc.want {
				if !got[d] {
					t.Errorf("weekdaySet(%v) is missing %v", tc.in, d)
				}
			}
		})
	}
}

// TestMondayOffset: time.Weekday starts on Sunday and the calendar grid is
// Monday-first, so this offset is where that mismatch is handled — once, in one
// function, rather than in the template.
func TestMondayOffset(t *testing.T) {
	want := map[time.Weekday]int{
		time.Monday:    0,
		time.Tuesday:   1,
		time.Wednesday: 2,
		time.Thursday:  3,
		time.Friday:    4,
		time.Saturday:  5,
		time.Sunday:    6,
	}
	for day, offset := range want {
		if got := mondayOffset(day); got != offset {
			t.Errorf("mondayOffset(%v) = %d, want %d", day, got, offset)
		}
	}
}

func TestLocked(t *testing.T) {
	// A booked slot cannot be moved: rescheduling a paying customer silently, with
	// no notification and a record that shows the new time as if it had always been
	// so, is the defect this guards. Schedule.validate enforces the same rule
	// server-side, because a disabled input is an affordance, not a guarantee.
	if !Locked(sqlc.ScheduleSlot{BookedCount: 1}) {
		t.Error("a slot with one booking is not locked")
	}
	if Locked(sqlc.ScheduleSlot{BookedCount: 0}) {
		t.Error("an unbooked slot is locked")
	}
}
