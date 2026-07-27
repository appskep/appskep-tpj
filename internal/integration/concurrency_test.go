package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// This is the test PLAN.md says matters most.
//
// Double-booking one slot "takes money from a customer for a service that cannot
// be delivered, and it does so silently" (PLAN.md:1656). The window is
// milliseconds wide, booking traffic concentrates exactly where it hurts — one
// popular slot, one promo broadcast, many simultaneous taps — and it is invisible
// in manual testing and in any test that runs requests sequentially. It will not
// show up until production.
//
// PLAN.md:1682 states what this file is for: "Without this test, a future
// refactor that moves the check outside the transaction reintroduces the bug
// undetected."

// raceResult is what one goroutine's booking attempt produced.
type raceResult struct {
	ok  bool
	err error
}

// race fires n concurrent bookings at one slot, one per user, and returns what
// each attempt got.
//
// Every goroutine is released from a single barrier so they arrive at the row
// lock together rather than in a queue — a staggered start would test almost
// nothing.
func race(t *testing.T, env *testsupport.Env, slotID int64, users []int64) []raceResult {
	t.Helper()

	results := make([]raceResult, len(users))
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i, userID := range users {
		wg.Go(func() {
			<-start
			_, err := env.Deps.Booking.Create(context.Background(), service.CreateInput{
				UserID:      userID,
				ServiceSlug: testsupport.SlugUrut,
				SlotID:      fmt.Sprint(slotID),
				Name:        fmt.Sprintf("Pembalap %d", i),
				Phone:       "081234567890",
				Address:     fmt.Sprintf("Jl. Balap No. %d, Sleman", i),
			})
			results[i] = raceResult{ok: err == nil, err: err}
		})
	}

	close(start)
	wg.Wait()
	return results
}

// assertFriendlyLoss is the assertion that matters most in this file: the error
// IDENTITY of a loser, not merely the count of winners.
//
// A loser may lose in one of two legitimate places, and which one is genuinely
// racy:
//
//   - inside the transaction, at the row lock, giving service.ErrSlotTaken; or
//   - before it, in validate(), which reads the slot without a lock — if the
//     winner has already committed by then, slotMessage sees a full slot and the
//     attempt is refused as a ValidationError on the picker.
//
// Both render the same friendly "pilih jadwal lain" and both are correct. What
// must NEVER appear is anything else, and that is what this catches: Phase 7
// measured a defect nothing else would have found. Reading `services` one
// statement too early established the REPEATABLE READ snapshot before the locking
// read, so a locking read that then met a newer row failed with MariaDB
// ER_CHECKREAD (1020) — 19 of 20 racers got a 500 instead of a friendly message,
// with booked_count still correct so nothing looked wrong in the data.
func assertFriendlyLoss(t *testing.T, racer int, err error) {
	t.Helper()

	if errors.Is(err, service.ErrSlotTaken) {
		return
	}

	var ve *service.ValidationError
	if errors.As(err, &ve) {
		if _, ok := ve.Fields["slot"]; ok {
			return
		}
		t.Errorf("racer %d was refused with %v, want a message on the slot field", racer, ve.Fields)
		return
	}

	t.Errorf("racer %d lost with %v, want service.ErrSlotTaken or a slot "+
		"ValidationError — a loser must get the friendly re-render, never a 500. "+
		"Check that GetSlotForUpdate is still the transaction's literal first "+
		"statement (see the ER_CHECKREAD note in Booking.create).", racer, err)
}

// TestOneCapacityOneWinner is the headline case: twenty goroutines, twenty
// distinct users, one capacity-1 slot. Exactly one may succeed.
func TestOneCapacityOneWinner(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)

	const racers = 20
	users := make([]int64, racers)
	for i := range users {
		users[i] = env.User(uint64(9000 + i)).ID
	}

	results := race(t, env, slot.ID, users)

	winners, atTheLock := 0, 0
	for i, r := range results {
		if r.ok {
			winners++
			continue
		}
		assertFriendlyLoss(t, i, r.err)
		if errors.Is(r.err, service.ErrSlotTaken) {
			atTheLock++
		}
	}

	if winners != 1 {
		t.Fatalf("%d of %d racers booked a capacity-1 slot, want exactly 1", winners, racers)
	}
	// Not asserted, because how many racers reach the lock before the winner
	// commits is genuinely timing-dependent. Reported so a run where every loser
	// was refused by the pre-lock check — and the lock therefore never exercised —
	// is visible with -v rather than silently passing.
	t.Logf("%d of %d losers were refused at the row lock, the rest before it",
		atTheLock, racers-winners)
	if atTheLock == 0 {
		t.Log("WARNING: no racer reached the row lock in this run; the locking path " +
			"was not exercised")
	}

	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1", got)
	}
	if got := env.CountRows("bookings", "slot_id = ?", slot.ID); got != 1 {
		t.Errorf("%d booking rows exist for the slot, want 1", got)
	}

	env.AssertInvariant()
}

// TestCapacityNAdmitsExactlyN: the lock works for any capacity, not only for 1.
// Capacity models parallel therapists, so this is the shape a real clinic runs.
func TestCapacityNAdmitsExactlyN(t *testing.T) {
	const capacity = 3
	env := testsupport.New(t)

	slot := env.FutureSlot(3, capacity)

	users := make([]int64, capacity*2)
	for i := range users {
		users[i] = env.User(uint64(9100 + i)).ID
	}

	results := race(t, env, slot.ID, users)

	winners := 0
	for i, r := range results {
		if r.ok {
			winners++
			continue
		}
		assertFriendlyLoss(t, i, r.err)
	}

	if winners != capacity {
		t.Fatalf("%d of %d racers booked a capacity-%d slot, want exactly %d",
			winners, len(users), capacity, capacity)
	}
	if got := env.BookedCount(slot.ID); got != capacity {
		t.Errorf("booked_count = %d, want %d", got, capacity)
	}

	env.AssertInvariant()
}

// TestOneUserCannotDoubleBookASlot exercises the second line of defence:
// uq_bookings_active_slot_user, via the active_slot_id generated column. It is
// enforced by the engine, so a double-submitted form is refused even if every
// check in Go were removed.
//
// The remedy differs from ErrSlotTaken, which is why the error does: this user
// does not need another slot, they need to be told they already have this one.
func TestOneUserCannotDoubleBookASlot(t *testing.T) {
	env := testsupport.New(t)

	// Capacity 2, so the slot itself is not what refuses the second booking.
	slot := env.FutureSlot(3, 2)
	user := env.User(9200)

	users := []int64{user.ID, user.ID}
	results := race(t, env, slot.ID, users)

	winners, duplicates := 0, 0
	for i, r := range results {
		switch {
		case r.ok:
			winners++
		case errors.Is(r.err, service.ErrDuplicateBooking):
			duplicates++
		default:
			t.Errorf("attempt %d failed with %v, want ErrDuplicateBooking", i, r.err)
		}
	}

	if winners != 1 || duplicates != 1 {
		t.Fatalf("got %d winners and %d duplicates, want 1 and 1", winners, duplicates)
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1 — the refused insert must not have held the slot", got)
	}

	env.AssertInvariant()
}

// TestSequentialBookingIsNotEvidence is a comment with a test attached, and it
// demonstrates its own point better than intended.
//
// Booking a full slot one request after the other never reaches the lock at all:
// validate() runs first, slotMessage sees booked_count >= capacity on a row read
// with no lock held, and the attempt is refused as a ValidationError on the slot
// field — the message that renders beside the picker. ErrSlotTaken, the error the
// concurrent losers get, comes only from inside the transaction.
//
// So a sequential test exercises a completely different code path from the one
// that matters, and would pass against an implementation with no lock at all.
// That is what PLAN.md:1590 means by "a sequential test proves nothing here".
func TestSequentialBookingIsNotEvidence(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	first := env.User(9300)
	second := env.User(9301)

	env.Booking(testsupport.BookingInput{UserID: first.ID, SlotID: slot.ID})

	_, err := env.Deps.Booking.Create(context.Background(), service.CreateInput{
		UserID:      second.ID,
		ServiceSlug: testsupport.SlugUrut,
		SlotID:      fmt.Sprint(slot.ID),
		Name:        "Yang kedua",
		Phone:       "081234567890",
		Address:     "Jl. Kedua No. 2, Sleman",
	})

	// A ValidationError, not ErrSlotTaken: this never got as far as the lock.
	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("the second sequential booking got %v, want a ValidationError from "+
			"the pre-lock check", err)
	}
	if msg := ve.Fields["slot"]; msg != "Jadwal tersebut sudah penuh. Pilih jadwal lain." {
		t.Errorf("message = %q, want the full-slot message beside the picker", msg)
	}
	if errors.Is(err, service.ErrSlotTaken) {
		t.Error("a sequential booking reached the in-transaction path; the two are " +
			"meant to be distinguishable")
	}

	env.AssertInvariant()
}

// TestCapacityCannotBeShrunkBelowItsBookings. chk_slots_count would raise a raw
// 4025 (MariaDB constraint violation) and become a 500; the service refuses first
// with a message that names the count, which is what an admin can act on.
func TestCapacityCannotBeShrunkBelowItsBookings(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 2)
	env.Booking(testsupport.BookingInput{UserID: env.User(9400).ID, SlotID: slot.ID})
	env.Booking(testsupport.BookingInput{UserID: env.User(9401).ID, SlotID: slot.ID})

	err := env.Deps.Schedule.Update(context.Background(), slot.ID, service.SlotInput{
		Date:      slot.SlotDate.Format("2006-01-02"),
		StartTime: slot.StartTime,
		EndTime:   slot.EndTime,
		Capacity:  "1",
		IsActive:  true,
	})

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("shrinking capacity below booked_count gave %v, want a ValidationError "+
			"rather than a raw constraint violation", err)
	}
	if msg := ve.Fields["capacity"]; msg == "" {
		t.Errorf("messages = %v, want one on capacity", ve.Fields)
	} else if !strings.Contains(msg, "2") {
		t.Errorf("message %q does not name the booking count", msg)
	}

	if got := env.BookedCount(slot.ID); got != 2 {
		t.Errorf("booked_count = %d after a refused edit, want 2", got)
	}
	env.AssertInvariant()
}
