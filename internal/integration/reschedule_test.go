package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// Reschedule is the only transaction in the system that locks TWO slots, and the
// only one where the lock ORDER — ascending id, as the transaction's first two
// statements — is what keeps it safe.

// TestRescheduleMovesTheHold is the ordinary case, and it checks the two fields
// that must survive untouched: a customer owes what they agreed to, whatever the
// layanan costs now and whenever they are now coming.
func TestRescheduleMovesTheHold(t *testing.T) {
	env := testsupport.New(t)

	from := env.SlotAt(3, 20*60, 1)
	to := env.SlotAt(4, 20*60, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9700).ID, SlotID: from.ID})

	if err := env.Deps.Booking.Reschedule(context.Background(), booking.ID, to.ID); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}

	if got := env.BookedCount(from.ID); got != 0 {
		t.Errorf("old slot booked_count = %d, want 0", got)
	}
	if got := env.BookedCount(to.ID); got != 1 {
		t.Errorf("new slot booked_count = %d, want 1", got)
	}

	moved, err := env.Deps.Booking.AdminDetail(context.Background(), booking.ID)
	if err != nil {
		t.Fatalf("AdminDetail: %v", err)
	}
	if moved.SlotID != to.ID {
		t.Errorf("booking is on slot %d, want %d", moved.SlotID, to.ID)
	}
	if moved.PriceAmount != booking.PriceAmount {
		t.Errorf("price_amount changed from %q to %q — the snapshot is the whole point",
			booking.PriceAmount, moved.PriceAmount)
	}
	if moved.BookingCode != booking.BookingCode {
		t.Errorf("booking_code changed from %q to %q — it has been read over the phone",
			booking.BookingCode, moved.BookingCode)
	}
	// The status is untouched: a paid booking that moves is still paid.
	if moved.Status != sqlc.BookingsStatusPendingPayment {
		t.Errorf("status = %q, want it unchanged", moved.Status)
	}

	env.AssertInvariant()
}

// TestRescheduleCrossDirectionDoesNotDeadlock is Phase 10's ask (PLAN.md:1334).
//
// Two operators moving two bookings between the same PAIR of slots in opposite
// directions is the exact shape that deadlocks without an ordering rule: each
// transaction holds the slot the other needs next. Ascending id is what makes
// both take them in the same order and so makes one simply wait.
//
// Run repeatedly, because a deadlock is a race and one round proves little.
func TestRescheduleCrossDirectionDoesNotDeadlock(t *testing.T) {
	env := testsupport.New(t)

	// Capacity 2 on both, so neither move can fail merely for being full — the
	// only thing under test here is the locking.
	a := env.SlotAt(3, 20*60, 2)
	b := env.SlotAt(4, 20*60, 2)

	first := env.Booking(testsupport.BookingInput{UserID: env.User(9710).ID, SlotID: a.ID})
	second := env.Booking(testsupport.BookingInput{UserID: env.User(9711).ID, SlotID: b.ID})

	const rounds = 20
	for round := range rounds {
		// Every round swaps the pair: first goes a->b while second goes b->a, then
		// back again on the next round.
		target := map[int64]int64{first.ID: b.ID, second.ID: a.ID}
		if round%2 == 1 {
			target = map[int64]int64{first.ID: a.ID, second.ID: b.ID}
		}

		var errs [2]error
		start := make(chan struct{})

		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			errs[0] = env.Deps.Booking.Reschedule(context.Background(), first.ID, target[first.ID])
		})
		wg.Go(func() {
			<-start
			errs[1] = env.Deps.Booking.Reschedule(context.Background(), second.ID, target[second.ID])
		})
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err == nil {
				continue
			}
			// The service retries once on a deadlock or lock-wait timeout, so nothing
			// should surface. If one does, the ordering rule has been broken.
			if strings.Contains(err.Error(), "Deadlock") || strings.Contains(err.Error(), "1213") {
				t.Fatalf("round %d, mover %d: %v — the two-slot lock order has regressed. "+
					"Both slots must be locked with GetSlotForUpdate in ascending id "+
					"order, as the transaction's first two statements.", round, i, err)
			}
			if strings.Contains(err.Error(), "Lock wait timeout") {
				t.Fatalf("round %d, mover %d: %v", round, i, err)
			}
			t.Errorf("round %d, mover %d: %v", round, i, err)
		}

		env.AssertInvariant()
	}

	// Both bookings still hold exactly one slot each between them.
	total := env.BookedCount(a.ID) + env.BookedCount(b.ID)
	if total != 2 {
		t.Errorf("the two slots hold %d bookings between them after %d rounds, want 2",
			total, rounds)
	}
}

// TestRescheduleRefusesAFullTarget: HoldSlot on the target has to report one
// affected row before the booking is allowed to move.
func TestRescheduleRefusesAFullTarget(t *testing.T) {
	env := testsupport.New(t)

	from := env.SlotAt(3, 20*60, 1)
	full := env.SlotAt(4, 20*60, 1)

	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9720).ID, SlotID: from.ID})
	env.Booking(testsupport.BookingInput{UserID: env.User(9721).ID, SlotID: full.ID})

	err := env.Deps.Booking.Reschedule(context.Background(), booking.ID, full.ID)
	if err == nil {
		t.Fatal("Reschedule moved a booking into a full slot")
	}

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("error = %v, want a ValidationError an operator can read", err)
	}

	if got := env.BookedCount(from.ID); got != 1 {
		t.Errorf("the original slot lost its hold on a failed move: booked_count = %d", got)
	}
	if got := env.BookedCount(full.ID); got != 1 {
		t.Errorf("the full slot went to %d, want 1", got)
	}
	env.AssertInvariant()
}

// TestRescheduleRefusesAnInactiveTarget.
func TestRescheduleRefusesAnInactiveTarget(t *testing.T) {
	env := testsupport.New(t)

	from := env.SlotAt(3, 20*60, 1)
	off := env.SlotAt(4, 20*60, 1)
	env.Exec("UPDATE schedule_slots SET is_active = 0 WHERE id = ?", off.ID)

	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9730).ID, SlotID: from.ID})

	if err := env.Deps.Booking.Reschedule(context.Background(), booking.ID, off.ID); err == nil {
		t.Fatal("Reschedule moved a booking into a deactivated slot")
	}
	if got := env.BookedCount(from.ID); got != 1 {
		t.Errorf("booked_count on the original slot = %d, want 1", got)
	}
	env.AssertInvariant()
}

// TestRescheduleRefusesTheSameSlot: a no-op move is a validation error rather
// than a transaction that releases and re-takes the same slot.
func TestRescheduleRefusesTheSameSlot(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9740).ID, SlotID: slot.ID})

	err := env.Deps.Booking.Reschedule(context.Background(), booking.ID, slot.ID)

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
	if got := ve.Fields["slot"]; got != "Jadwal baru sama dengan jadwal saat ini." {
		t.Errorf("message = %q", got)
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1", got)
	}
	env.AssertInvariant()
}

// TestRescheduleRefusesAFinalBooking. ErrBookingFinal is the single sentinel for
// "this booking no longer holds its slot", shared by cancel and reschedule
// because both are guarded on the identical status set.
func TestRescheduleRefusesAFinalBooking(t *testing.T) {
	env := testsupport.New(t)

	from := env.SlotAt(3, 20*60, 1)
	to := env.SlotAt(4, 20*60, 1)
	user := env.User(9750)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: from.ID})

	if _, _, err := env.Deps.Booking.CancelForUser(context.Background(),
		booking.BookingCode, user.ID); err != nil {
		t.Fatalf("CancelForUser: %v", err)
	}

	if err := env.Deps.Booking.Reschedule(context.Background(), booking.ID, to.ID); !errors.Is(err, service.ErrBookingFinal) {
		t.Fatalf("rescheduling a cancelled booking got %v, want ErrBookingFinal", err)
	}
	if got := env.BookedCount(to.ID); got != 0 {
		t.Errorf("the target slot was taken by a cancelled booking: booked_count = %d", got)
	}
	env.AssertInvariant()
}

// TestAdminCancelReachesAPaidBooking. An operator must never need the database to
// free a slot, so cancelling a paid booking is allowed — the UI names the amount
// and says the refund is manual (PLAN.md Q6).
func TestAdminCancel(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9760).ID, SlotID: slot.ID})

	// Move it to paid the way the webhook would, so the cancellation is of a
	// booking with money behind it.
	env.Exec("UPDATE bookings SET status = 'paid' WHERE id = ?", booking.ID)

	was, moved, err := env.Deps.Booking.AdminCancel(context.Background(), booking.ID,
		"Pelanggan menelepon dan membatalkan.")
	if err != nil {
		t.Fatalf("AdminCancel: %v", err)
	}
	if !moved {
		t.Error("moved = false on the call that cancelled")
	}
	if was != sqlc.BookingsStatusPaid {
		t.Errorf("was = %q, want paid — the caller needs it to decide whether to "+
			"mention a refund", was)
	}
	if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusCancelled {
		t.Errorf("status = %q, want cancelled", got)
	}
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d, want 0", got)
	}

	// A second cancellation is a no-op, not a second release.
	_, moved, err = env.Deps.Booking.AdminCancel(context.Background(), booking.ID, "Lagi.")
	if !errors.Is(err, service.ErrBookingFinal) {
		t.Errorf("the second cancellation got %v, want ErrBookingFinal", err)
	}
	if moved {
		t.Error("moved = true on an already-cancelled booking")
	}
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after a refused second cancellation, want 0", got)
	}

	env.AssertInvariant()
}

// TestAdminCancelRequiresAReason: it goes into activity_logs and onto the
// booking, and an empty one makes the audit trail useless.
func TestAdminCancelRequiresAReason(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9770).ID, SlotID: slot.ID})

	_, _, err := env.Deps.Booking.AdminCancel(context.Background(), booking.ID, "   ")

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
	if _, ok := ve.Fields["alasan"]; !ok {
		t.Errorf("messages = %v, want one on alasan", ve.Fields)
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d after a refused cancellation, want 1", got)
	}
	env.AssertInvariant()
}

// TestConfirmAndCompleteHoldTheSlot. `completed` is the status the invariant
// insists still holds its slot — the moment an admin completes a booking is where
// getting that wrong would show.
func TestConfirmAndCompleteHoldTheSlot(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9780).ID, SlotID: slot.ID})
	env.Exec("UPDATE bookings SET status = 'paid' WHERE id = ?", booking.ID)

	moved, err := env.Deps.Booking.Confirm(context.Background(), booking.ID)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !moved {
		t.Error("Confirm reported no transition")
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d after confirming, want 1", got)
	}

	moved, err = env.Deps.Booking.Complete(context.Background(), booking.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !moved {
		t.Error("Complete reported no transition")
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d after completing, want 1 — a completed booking "+
			"still holds its slot (bookings.active_slot_id)", got)
	}

	// And a repeat is a no-op rather than an error page: someone already did it.
	moved, err = env.Deps.Booking.Complete(context.Background(), booking.ID)
	if err != nil {
		t.Fatalf("the second Complete errored: %v", err)
	}
	if moved {
		t.Error("the second Complete reported a transition")
	}

	env.AssertInvariant()
}
