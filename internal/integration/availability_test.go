package integration

import (
	"context"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The public booking calendar, end to end.
//
// buildAvailableMonth's own tests are pure and live beside it; what they cannot
// reach is the half of the definition that lives in SQL. AvailableMonth is
// AvailableDates re-shaped precisely so there is only one definition of
// "bookable" — these tests are what hold the two halves together, because a grid
// that highlights a date the slot panel then refuses is exactly the drift the
// design exists to prevent.
//
// Two facts about the seed shape every assertion here. It fills days -2..+14 with
// four capacity-1 slots each, so nothing may assume an empty schedule: days 15
// and beyond are the only ones a test owns outright, and everything nearer is
// asserted as a delta. And it seeds no bookings at all, which is why a fixture
// booking is the only thing that can move a count.

// cleanDay is far enough ahead to be past the seeded range (-2..+14) and near
// enough to stay inside the 30-day booking window, so a test can own every slot
// on it and assert absolute counts.
const cleanDay = 18

// findCell locates one date in the month grid.
func findCell(t *testing.T, m service.AvailableMonth, want time.Time) service.AvailableDay {
	t.Helper()
	for _, week := range m.Weeks {
		for _, cell := range week {
			if cell.Date.Equal(want) {
				return cell
			}
		}
	}
	t.Fatalf("%s is not in the %s grid", want.Format(time.DateOnly), m.Anchor.Format("2006-01"))
	return service.AvailableDay{}
}

// TestAvailableMonthOffersOnlyBookableDays. A date whose last free slot has just
// been taken must stop being a link, because the calendar is what a visitor
// clicks and a full day one click away is a dead end with no explanation.
func TestAvailableMonthOffersOnlyBookableDays(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	user := env.User(9310)
	taken := env.FutureSlot(cleanDay, 1)
	free := env.FutureSlot(cleanDay+2, 1)

	before, err := env.Deps.Booking.AvailableMonth(ctx, taken.SlotDate)
	if err != nil {
		t.Fatalf("AvailableMonth: %v", err)
	}
	if cell := findCell(t, before, taken.SlotDate); !cell.Bookable() || cell.Free != 1 {
		t.Fatalf("%s before anyone books it: Free = %d, Bookable = %v; want 1 and true — "+
			"the seed is not supposed to reach day %d",
			taken.SlotDate.Format(time.DateOnly), cell.Free, cell.Bookable(), cleanDay)
	}

	env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: taken.ID})

	after, err := env.Deps.Booking.AvailableMonth(ctx, taken.SlotDate)
	if err != nil {
		t.Fatalf("AvailableMonth: %v", err)
	}

	full := findCell(t, after, taken.SlotDate)
	if full.Bookable() {
		t.Errorf("%s is still bookable with its only slot taken (Free = %d)",
			taken.SlotDate.Format(time.DateOnly), full.Free)
	}
	if !full.InWindow {
		t.Errorf("%s fell out of the window — it is %d days away",
			taken.SlotDate.Format(time.DateOnly), cleanDay)
	}

	if cell := findCell(t, after, free.SlotDate); !cell.Bookable() || cell.Free != 1 {
		t.Errorf("the untouched day: Free = %d, Bookable = %v; want 1 and true",
			cell.Free, cell.Bookable())
	}

	// A delta, because the seed's own slots are in the window too.
	if got := before.WindowFree - after.WindowFree; got != 1 {
		t.Errorf("WindowFree fell by %d, want exactly 1 — one booking took one slot", got)
	}

	env.AssertInvariant()
}

// TestAvailableMonthRespectsTheLeadTime is the one the pure tests cannot reach.
//
// The lead time is applied by ListAvailableDates' `starts_at >= ?` and nowhere
// else: buildAvailableMonth is handed dates, and a date has no clock. So a slot
// starting inside booking_lead_time_minutes has to be missing from the counts
// before the grid ever sees it — which is the whole reason the calendar is built
// from AvailableDates rather than from the month's own slots, the way the admin
// one is.
//
// It moves the SETTING rather than sampling the clock, per CLAUDE.md: a version
// that placed a slot "30 minutes from now" would skip itself near midnight and
// collide with a seeded 08:00/10:30/13:00/15:30 start on the hour. Widening the
// lead time to five days sweeps the whole seeded range out of the window at once,
// with no wall-clock arithmetic anywhere.
func TestAvailableMonthRespectsTheLeadTime(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	today := env.Deps.Schedule.Today()
	// near sits inside a five-day lead time and far outside it. far is on a clean
	// day so its count can be asserted exactly; near cannot be, because every day
	// inside five days is one the seed already filled — which is fine, since what
	// is asserted there is that it stops being bookable at all.
	near := env.FutureSlot(3, 1)
	far := env.FutureSlot(cleanDay, 1)

	before, err := env.Deps.Booking.AvailableMonth(ctx, today)
	if err != nil {
		t.Fatalf("AvailableMonth: %v", err)
	}
	if cell := findCell(t, before, near.SlotDate); !cell.Bookable() {
		t.Fatalf("%s is not bookable under the seeded two-hour lead time",
			near.SlotDate.Format(time.DateOnly))
	}

	// 5 days, in minutes. The setting is validated 0..10080, so this is inside the
	// range an operator could really type.
	if err := env.Deps.Settings.Update(ctx, map[string]string{
		service.KeyBookingLeadMinutes: "7200",
	}); err != nil {
		t.Fatalf("widening the lead time: %v", err)
	}

	after, err := env.Deps.Booking.AvailableMonth(ctx, today)
	if err != nil {
		t.Fatalf("AvailableMonth: %v", err)
	}

	if cell := findCell(t, after, near.SlotDate); cell.Bookable() {
		t.Errorf("%s is bookable three days out under a five-day lead time (Free = %d) — "+
			"the lead time did not reach the calendar",
			near.SlotDate.Format(time.DateOnly), cell.Free)
	}
	if cell := findCell(t, after, far.SlotDate); !cell.Bookable() || cell.Free != 1 {
		t.Errorf("%s is %d days out and should be unaffected: Free = %d, Bookable = %v",
			far.SlotDate.Format(time.DateOnly), cleanDay, cell.Free, cell.Bookable())
	}

	// Everything the seed put inside five days is gone, so the window shrank by
	// much more than the one slot this test placed there.
	if after.WindowFree >= before.WindowFree {
		t.Errorf("WindowFree = %d, was %d — widening the lead time freed nothing",
			after.WindowFree, before.WindowFree)
	}

	env.AssertInvariant()
}

// TestAvailableMonthAgreesWithAvailableSlots is the invariant that makes a
// highlighted day a promise.
//
// Every count the calendar prints has to be a list the panel can produce when the
// day is clicked. The two go through different queries — ListAvailableDates
// groups, ListAvailableSlotsByDate does not — so nothing but this stops them
// drifting apart the next time one of the predicates is edited. It runs over the
// whole grid, so the seeded days are checked as well as the ones this test makes.
func TestAvailableMonthAgreesWithAvailableSlots(t *testing.T) {
	env := testsupport.New(t)
	ctx := context.Background()

	user := env.User(9311)

	// A spread across the clean days: one date with two slots of which one gets
	// booked, one date whose only slot gets booked, and one left alone with
	// capacity to spare.
	partial := env.SlotAt(cleanDay, 9*60, 1)
	env.SlotAt(cleanDay, 14*60, 1)
	full := env.FutureSlot(cleanDay+2, 1)
	env.FutureSlot(cleanDay+4, 3)
	env.SlotAt(cleanDay+4, 16*60, 2)

	env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: partial.ID})
	env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: full.ID})

	m, err := env.Deps.Booking.AvailableMonth(ctx, env.Deps.Schedule.Today())
	if err != nil {
		t.Fatalf("AvailableMonth: %v", err)
	}

	checked := 0
	for _, week := range m.Weeks {
		for _, cell := range week {
			slots, serr := env.Deps.Booking.AvailableSlots(ctx, cell.Date)
			if serr != nil {
				t.Fatalf("AvailableSlots(%s): %v", cell.Date.Format(time.DateOnly), serr)
			}

			// Both directions. A count with no slots behind it is a dead end the
			// visitor reaches by clicking; slots with no count are a date the
			// calendar hides.
			if got := int64(len(slots)); got != cell.Free {
				t.Errorf("%s: the calendar says %d slot, the panel lists %d",
					cell.Date.Format(time.DateOnly), cell.Free, got)
			}
			if cell.Bookable() != (len(slots) > 0) {
				t.Errorf("%s: Bookable = %v but the panel lists %d slots",
					cell.Date.Format(time.DateOnly), cell.Bookable(), len(slots))
			}
			checked++
		}
	}
	if checked < 28 {
		t.Fatalf("only %d cells checked — the grid did not build", checked)
	}

	// And the emptied day is specifically not offered, which is the case a
	// count-only loop could pass by never having looked at a zero.
	if cell := findCell(t, m, full.SlotDate); cell.Bookable() {
		t.Errorf("%s is bookable with its only slot taken", full.SlotDate.Format(time.DateOnly))
	}
	// The half-emptied one still is, at exactly one.
	if cell := findCell(t, m, partial.SlotDate); !cell.Bookable() || cell.Free != 1 {
		t.Errorf("%s: Free = %d, Bookable = %v; want 1 and true — one of its two slots is left",
			partial.SlotDate.Format(time.DateOnly), cell.Free, cell.Bookable())
	}

	env.AssertInvariant()
}
