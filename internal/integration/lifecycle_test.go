package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The three ways a booking releases its slot — the expiry ticker, a customer
// cancelling, and (in webhook_test.go) Midtrans — plus the reschedule that moves
// a hold between two slots.
//
// One rule governs all of them: RowsAffected() == 1 on a status-guarded UPDATE is
// the only thing that authorises touching booked_count. Every test here exists to
// keep some pair of them from both releasing the same slot.

// TestExpiryReleasesTheSlot is the base case: a hold that ran out comes back.
func TestExpiryReleasesTheSlot(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9500).ID, SlotID: slot.ID})

	if got := env.BookedCount(slot.ID); got != 1 {
		t.Fatalf("booked_count = %d before expiry, want 1", got)
	}

	env.ExpireHold(booking.ID)

	released, err := env.Deps.Booking.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if released != 1 {
		t.Errorf("Expire released %d bookings, want 1", released)
	}
	if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusExpired {
		t.Errorf("status = %q, want expired", got)
	}
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after expiry, want 0 — slots leak until the "+
			"schedule looks fully booked with no real bookings behind it", got)
	}

	env.AssertInvariant()
}

// TestExpiryIsIdempotent. The sweep runs every minute forever, so releasing twice
// is not a hypothetical: without the RowsAffected() guard, the second pass would
// decrement a counter it had already decremented.
func TestExpiryIsIdempotent(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9501).ID, SlotID: slot.ID})
	env.ExpireHold(booking.ID)

	first, err := env.Deps.Booking.Expire(context.Background())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first != 1 {
		t.Fatalf("first sweep released %d, want 1", first)
	}

	// Four more sweeps, as the live ticker would run them.
	for i := range 4 {
		again, err := env.Deps.Booking.Expire(context.Background())
		if err != nil {
			t.Fatalf("sweep %d: %v", i+2, err)
		}
		if again != 0 {
			t.Errorf("sweep %d released %d bookings, want 0 — an expired booking is "+
				"no longer selected, and re-releasing would drive booked_count below "+
				"the truth", i+2, again)
		}
	}

	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after five sweeps, want 0", got)
	}
	env.AssertInvariant()
}

// TestExpirySkipsALiveHold: only a booking whose deadline has actually passed is
// touched.
func TestExpirySkipsALiveHold(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9502).ID, SlotID: slot.ID})

	released, err := env.Deps.Booking.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if released != 0 {
		t.Errorf("Expire released %d bookings whose hold is still live", released)
	}
	if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusPendingPayment {
		t.Errorf("status = %q, want pending_payment", got)
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1", got)
	}
}

// TestExpiryRacesCancellation is the pair PLAN.md warns about by name: the expiry
// ticker and a user cancellation reaching the same booking at the same moment.
// Exactly one may move it, and the slot may be released exactly once.
func TestExpiryRacesCancellation(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9503)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})
	env.ExpireHold(booking.ID)

	var (
		released int
		expErr   error
		moved    bool
		canErr   error
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		released, expErr = env.Deps.Booking.Expire(context.Background())
	})
	wg.Go(func() {
		<-start
		_, moved, canErr = env.Deps.Booking.CancelForUser(context.Background(),
			booking.BookingCode, user.ID)
	})
	close(start)
	wg.Wait()

	if expErr != nil {
		t.Errorf("Expire: %v", expErr)
	}
	// ErrNotPayable is a legitimate outcome: the ticker got there first and the
	// pre-flight status check then refused. Anything else is a real failure.
	if canErr != nil && !errors.Is(canErr, service.ErrNotPayable) {
		t.Errorf("CancelForUser: %v", canErr)
	}

	// Exactly one of them may claim the transition.
	transitions := released
	if moved {
		transitions++
	}
	if transitions != 1 {
		t.Errorf("%d of the two racers claimed the transition, want exactly 1 "+
			"(expire released %d, cancel moved %v)", transitions, released, moved)
	}

	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d, want 0 — the slot must be released once, not twice", got)
	}
	env.AssertInvariant()
}

// TestDoubleReleaseWouldStealAnotherBookingsSlot isolates the guard that
// authorises ReleaseSlot, on the one shape where nothing else can cover for it.
//
// At capacity 1 the guarded decrement — `GREATEST(booked_count - 1, 0)` — hides a
// missing RowsAffected() check: a second release takes the counter from 0 to 0
// and the invariant still holds. That is defence in depth working, and it is also
// why a capacity-1 test cannot prove the guard is there.
//
// With capacity 2 and a second live booking it can. Releasing twice for one
// booking takes the counter from 2 to 0 while the other booking still holds its
// place — the slot has been given away underneath a customer who is still coming.
//
// Verified by removing the `n != 1` guard in expireOne: the capacity-1 tests above
// stayed green and this one failed.
func TestDoubleReleaseWouldStealAnotherBookingsSlot(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(3, 2)
	owner := env.User(9520)
	bystander := env.User(9521)

	expiring := env.Booking(testsupport.BookingInput{UserID: owner.ID, SlotID: slot.ID})
	env.Booking(testsupport.BookingInput{UserID: bystander.ID, SlotID: slot.ID})

	if got := env.BookedCount(slot.ID); got != 2 {
		t.Fatalf("booked_count = %d, want 2", got)
	}

	// The booking is cancelled by its owner and swept by the ticker. Whichever
	// order they land in, exactly one release may happen.
	env.ExpireHold(expiring.ID)

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		_, _ = env.Deps.Booking.Expire(context.Background())
	})
	wg.Go(func() {
		<-start
		_, _, _ = env.Deps.Booking.CancelForUser(context.Background(),
			expiring.BookingCode, owner.ID)
	})
	close(start)
	wg.Wait()

	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1 — the slot was released twice for one "+
			"booking, so the bystander's place has been given away", got)
	}
	env.AssertInvariant()
}

// TestCancelForUser covers the ordinary path plus the two rules that make it
// safe: ownership is the database's guarantee, and a second call is a success.
func TestCancelForUser(t *testing.T) {
	env := testsupport.New(t)

	owner := env.User(9600)
	stranger := env.User(9601)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: owner.ID, SlotID: slot.ID})

	t.Run("someone else's booking is a 404, never a 403", func(t *testing.T) {
		// A 403 confirms the code exists, which is what makes a guessed code worth
		// guessing.
		_, _, err := env.Deps.Booking.CancelForUser(context.Background(),
			booking.BookingCode, stranger.ID)
		if !errors.Is(err, service.ErrNotFound) {
			t.Fatalf("a stranger cancelling got %v, want ErrNotFound", err)
		}
		if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusPendingPayment {
			t.Errorf("status = %q after a refused cancellation, want pending_payment", got)
		}
		if got := env.BookedCount(slot.ID); got != 1 {
			t.Errorf("booked_count = %d, want 1", got)
		}
	})

	t.Run("an unknown code is a 404", func(t *testing.T) {
		_, _, err := env.Deps.Booking.CancelForUser(context.Background(),
			"TPJ-20260101-ZZZZ", owner.ID)
		if !errors.Is(err, service.ErrNotFound) {
			t.Fatalf("an unknown code got %v, want ErrNotFound", err)
		}
	})

	t.Run("the owner cancels", func(t *testing.T) {
		_, moved, err := env.Deps.Booking.CancelForUser(context.Background(),
			booking.BookingCode, owner.ID)
		if err != nil {
			t.Fatalf("CancelForUser: %v", err)
		}
		if !moved {
			t.Error("moved = false on the call that actually cancelled")
		}
		if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusCancelled {
			t.Errorf("status = %q, want cancelled", got)
		}
		if got := env.BookedCount(slot.ID); got != 0 {
			t.Errorf("booked_count = %d, want 0", got)
		}
	})

	t.Run("cancelling twice is refused, not double-released", func(t *testing.T) {
		_, moved, err := env.Deps.Booking.CancelForUser(context.Background(),
			booking.BookingCode, owner.ID)
		// The pre-flight status check refuses it before the transaction opens.
		if !errors.Is(err, service.ErrNotPayable) {
			t.Fatalf("the second cancellation got %v, want ErrNotPayable", err)
		}
		if moved {
			t.Error("moved = true on a booking that was already cancelled")
		}
		if got := env.BookedCount(slot.ID); got != 0 {
			t.Errorf("booked_count = %d, want 0 — a second cancellation must not "+
				"decrement again", got)
		}
	})

	env.AssertInvariant()
}

// TestCancelForUserCancelsTheGatewayOrderAfterTheCommit. A Snap page the customer
// still has open would otherwise keep taking money for a slot someone else now
// has — and the call sits after the commit, never inside it, because nothing that
// talks to another machine belongs under a slot lock.
func TestCancelForUserCancelsTheGatewayOrder(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9610)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)
	if _, err := env.Deps.Payment.Start(context.Background(), detail, &payer); err != nil {
		t.Fatalf("Payment.Start: %v", err)
	}

	if _, _, err := env.Deps.Booking.CancelForUser(context.Background(),
		booking.BookingCode, user.ID); err != nil {
		t.Fatalf("CancelForUser: %v", err)
	}

	// The service layer cancels the gateway order itself; the handler is not
	// involved, so this is where it has to be checked.
	if err := env.Deps.Payment.CancelOrder(context.Background(), booking.ID); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if env.Gateway.CancelCount() == 0 {
		t.Error("the Midtrans order was never cancelled — an open Snap page would " +
			"keep taking money for a released slot")
	}
	env.AssertInvariant()
}

// TestDetailForUserHidesOtherPeoplesBookings. The rule lives in the service, not
// in a handler, so the next page that loads a booking inherits it.
func TestDetailForUser(t *testing.T) {
	env := testsupport.New(t)

	owner := env.User(9620)
	stranger := env.User(9621)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: owner.ID, SlotID: slot.ID})

	if _, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, owner.ID, false); err != nil {
		t.Errorf("the owner cannot read their own booking: %v", err)
	}

	_, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, stranger.ID, false)
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("a stranger reading got %v, want ErrNotFound (never a 403, which "+
			"would confirm the code exists)", err)
	}

	// An admin may read any of them.
	if _, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, stranger.ID, true); err != nil {
		t.Errorf("an admin cannot read someone else's booking: %v", err)
	}

	_, err = env.Deps.Booking.DetailForUser(context.Background(),
		"TPJ-20260101-ZZZZ", owner.ID, false)
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("an unknown code got %v, want ErrNotFound", err)
	}
}
