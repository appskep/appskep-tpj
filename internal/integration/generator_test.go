package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The generator's arithmetic is covered without a database in
// service/schedule_unit_test.go. What needs one is idempotency: skipping is
// enforced by uq_slots_date_start plus INSERT IGNORE, not by a check in Go.

// generatorInput builds a form over a date range starting well past the seeded
// slots, so nothing collides with them.
func generatorInput(env *testsupport.Env, days int) service.GenerateInput {
	today := time.Now().In(env.Cfg.App.Location)
	from := today.AddDate(0, 0, 40)
	to := from.AddDate(0, 0, days-1)

	return service.GenerateInput{
		From:        from.Format("2006-01-02"),
		To:          to.Format("2006-01-02"),
		WindowStart: "08:00",
		WindowEnd:   "20:00",
		Duration:    "150",
		Break:       "0",
		Capacity:    "1",
		Weekdays:    []string{"0", "1", "2", "3", "4", "5", "6"},
		IsActive:    true,
	}
}

// TestGeneratorIsIdempotent is Phase 5's measured result made permanent: two
// weeks of 08:00-20:00 x 150 minutes is 56 slots, and running it again creates
// none.
func TestGeneratorIsIdempotent(t *testing.T) {
	env := testsupport.New(t)

	before := env.CountRows("schedule_slots", "")
	in := generatorInput(env, 14)

	first, err := env.Deps.Schedule.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if first.Created != 56 || first.Skipped != 0 {
		t.Errorf("first run created %d and skipped %d, want 56 and 0 "+
			"(14 days x 4 slots — the 18:00 slot would end at 20:30 and is dropped)",
			first.Created, first.Skipped)
	}
	if got := env.CountRows("schedule_slots", ""); got != before+56 {
		t.Errorf("%d slots exist, want %d", got, before+56)
	}

	second, err := env.Deps.Schedule.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("the second Apply: %v", err)
	}
	if second.Created != 0 || second.Skipped != 56 {
		t.Errorf("the re-run created %d and skipped %d, want 0 and 56 — skipping is "+
			"the only safe behaviour, because the alternative is overwriting a slot "+
			"that may already hold bookings", second.Created, second.Skipped)
	}
	if got := env.CountRows("schedule_slots", ""); got != before+56 {
		t.Errorf("the re-run changed the row count to %d, want %d", got, before+56)
	}
}

// TestPlanWritesNothing. A preview that wrote rows would be worse than no
// preview, and Phase 5 verified this by counting rows across it.
func TestPlanWritesNothing(t *testing.T) {
	env := testsupport.New(t)

	before := env.CountRows("schedule_slots", "")
	in := generatorInput(env, 14)

	plan, err := env.Deps.Schedule.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.NewCount != 56 {
		t.Errorf("Plan reports %d new slots, want 56", plan.NewCount)
	}
	if plan.ExistingCount != 0 {
		t.Errorf("Plan reports %d existing, want 0", plan.ExistingCount)
	}
	if got := env.CountRows("schedule_slots", ""); got != before {
		t.Fatalf("Plan wrote %d rows", got-before)
	}

	// After a real run, the same preview must describe the same set as already
	// existing — Plan and Apply share one expansion so they cannot diverge.
	if _, err := env.Deps.Schedule.Apply(context.Background(), in); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	plan, err = env.Deps.Schedule.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("the second Plan: %v", err)
	}
	if plan.NewCount != 0 || plan.ExistingCount != 56 {
		t.Errorf("after Apply, Plan reports %d new / %d existing, want 0 / 56",
			plan.NewCount, plan.ExistingCount)
	}
	for _, c := range plan.Candidates {
		if !c.Exists {
			t.Fatalf("candidate %s %s is not marked as existing", c.Date.Format("2006-01-02"), c.Start)
		}
	}
}

// TestGeneratorRejectsTooManySlots. Without the cap, a mistyped year in the end
// date turns one submit into a hundred thousand inserts inside a single
// transaction.
func TestGeneratorRejectsTooManySlots(t *testing.T) {
	env := testsupport.New(t)

	before := env.CountRows("schedule_slots", "")
	in := generatorInput(env, 152) // 152 days x 4 = 608, past the 500 cap

	_, err := env.Deps.Schedule.Apply(context.Background(), in)

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Apply got %v, want a ValidationError", err)
	}
	if got := env.CountRows("schedule_slots", ""); got != before {
		t.Errorf("a rejected run wrote %d rows", got-before)
	}
}

// TestBookedSlotCannotBeMoved is Phase 5's rule, enforced by the service rather
// than by the form: a disabled input is an affordance, not a guarantee, and a
// disabled input submits nothing at all.
//
// Moving a booked slot would reschedule a paying customer silently — no
// notification, and the booking's own record would show the new time as if it had
// always been so.
func TestBookedSlotCannotBeMoved(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 2)
	env.Booking(testsupport.BookingInput{UserID: env.User(9800).ID, SlotID: slot.ID})

	// A hand-crafted POST trying to move it to another date and time, and to raise
	// the capacity at the same time.
	elsewhere := slot.SlotDate.AddDate(0, 0, 5).Format("2006-01-02")
	err := env.Deps.Schedule.Update(context.Background(), slot.ID, service.SlotInput{
		Date:      elsewhere,
		StartTime: "09:00",
		EndTime:   "11:30",
		Capacity:  "4",
		Note:      "dipindah",
		IsActive:  true,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := env.Deps.Schedule.Get(context.Background(), slot.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if !after.SlotDate.Equal(slot.SlotDate) {
		t.Errorf("slot_date moved from %v to %v", slot.SlotDate, after.SlotDate)
	}
	if after.StartTime != slot.StartTime || after.EndTime != slot.EndTime {
		t.Errorf("times moved from %s-%s to %s-%s",
			slot.StartTime, slot.EndTime, after.StartTime, after.EndTime)
	}
	// Capacity and note are the two fields a booked slot still accepts.
	if after.Capacity != 4 {
		t.Errorf("capacity = %d, want the submitted 4", after.Capacity)
	}
	if after.Note.String != "dipindah" {
		t.Errorf("note = %q, want the submitted value", after.Note.String)
	}

	env.AssertInvariant()
}

// TestDeleteRefusesABookedSlot: fk_bookings_slot is RESTRICT, and the service
// check is the friendly path in front of it.
func TestDeleteRefusesABookedSlot(t *testing.T) {
	env := testsupport.New(t)

	booked := env.SlotAt(3, 20*60, 1)
	free := env.SlotAt(4, 20*60, 1)
	env.Booking(testsupport.BookingInput{UserID: env.User(9810).ID, SlotID: booked.ID})

	if err := env.Deps.Schedule.Delete(context.Background(), booked.ID); err == nil {
		t.Error("Delete removed a slot that still holds a booking")
	}
	if got := env.CountRows("schedule_slots", "id = ?", booked.ID); got != 1 {
		t.Error("the booked slot was deleted")
	}

	if err := env.Deps.Schedule.Delete(context.Background(), free.ID); err != nil {
		t.Errorf("Delete refused an unbooked slot: %v", err)
	}
	if got := env.CountRows("schedule_slots", "id = ?", free.ID); got != 0 {
		t.Error("the unbooked slot survived")
	}

	env.AssertInvariant()
}

// TestBulkDeleteSkipsBookedSlots, and reports real numbers rather than a generic
// confirmation — which is what the two-step confirm flow shows the operator.
func TestBulkDeleteSkipsBookedSlots(t *testing.T) {
	env := testsupport.New(t)

	// Twenty days out: clear of the seeded slots (which stop at +14) and inside
	// the 30-day booking window, so the slot can actually be booked.
	const daysAhead = 20
	day := time.Now().In(env.Cfg.App.Location).AddDate(0, 0, daysAhead)
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, env.Cfg.App.Location)

	// Three slots on one day, one of them booked.
	a := env.SlotAt(daysAhead, 8*60, 1)
	env.SlotAt(daysAhead, 11*60, 1)
	env.SlotAt(daysAhead, 14*60, 1)
	env.Booking(testsupport.BookingInput{UserID: env.User(9820).ID, SlotID: a.ID})

	preview, err := env.Deps.Schedule.PreviewDeleteInRange(context.Background(), from, from)
	if err != nil {
		t.Fatalf("PreviewDeleteInRange: %v", err)
	}
	if preview.Affected != 2 || preview.Skipped != 1 {
		t.Errorf("preview reports %d to delete and %d skipped, want 2 and 1",
			preview.Affected, preview.Skipped)
	}
	// A preview writes nothing.
	if got := env.CountRows("schedule_slots", "slot_date = ?", from); got != 3 {
		t.Errorf("the preview deleted rows: %d remain, want 3", got)
	}

	result, err := env.Deps.Schedule.DeleteInRange(context.Background(), from, from)
	if err != nil {
		t.Fatalf("DeleteInRange: %v", err)
	}
	if result.Affected != 2 || result.Skipped != 1 {
		t.Errorf("delete reports %d deleted and %d skipped, want 2 and 1",
			result.Affected, result.Skipped)
	}
	if got := env.CountRows("schedule_slots", "slot_date = ?", from); got != 1 {
		t.Errorf("%d slots remain on the day, want the 1 booked one", got)
	}

	env.AssertInvariant()
}

// TestSlotUniquenessIsTheDatabases: CreateSlot on a date and time that already
// exists is refused by uq_slots_date_start, not by a check in Go.
func TestSlotUniquenessIsTheDatabases(t *testing.T) {
	env := testsupport.New(t)

	day := time.Now().In(env.Cfg.App.Location).AddDate(0, 0, 50).Format("2006-01-02")
	in := service.SlotInput{
		Date:      day,
		StartTime: "08:00",
		EndTime:   "10:30",
		Capacity:  "1",
		IsActive:  true,
	}

	if _, err := env.Deps.Schedule.Create(context.Background(), in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := env.Deps.Schedule.Create(context.Background(), in)

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("the duplicate got %v, want a ValidationError rather than a raw 1062", err)
	}
	if len(ve.Fields) == 0 {
		t.Error("the duplicate produced no message")
	}
}
