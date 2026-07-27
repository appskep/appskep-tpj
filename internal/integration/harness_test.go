// Package integration holds every test that needs a real MariaDB.
//
// One package rather than one per subsystem, for three reasons. testsupport
// imports service, so an in-package service test could not import the harness
// without an import cycle. One package means one test database and one schema
// apply per run. And `go test ./...` runs packages in parallel, so several
// DB-backed packages would clobber each other's rows.
//
// Nothing here reaches for an unexported function. Every entry point these tests
// drive — Booking.Create, Booking.Expire, CancelForUser, Reschedule,
// Payment.ApplyWebhook, Payment.Start, Schedule.Apply, Email.SweepReminders — is
// exported, which is also the seam a caller in production would use.
package integration

import (
	"testing"

	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// TestHarness checks the harness itself before anything relies on it: the seed
// loaded, the fixtures work, and the invariant holds on a database nobody has
// touched yet.
//
// It is first on purpose. A failure here means every other failure in this
// package is uninformative.
func TestHarness(t *testing.T) {
	env := testsupport.New(t)

	// The seed's own arithmetic: 17 days x 4 slots.
	if got := env.CountRows("schedule_slots", ""); got != 68 {
		t.Errorf("seeded slots = %d, want 68", got)
	}
	if got := env.CountRows("services", ""); got != 3 {
		t.Errorf("seeded services = %d, want 3", got)
	}
	if got := env.CountRows("settings", ""); got != 21 {
		t.Errorf("seeded settings = %d, want 21", got)
	}
	// seed_dev.sql:84-87 — no bookings and no payments are seeded, deliberately,
	// so the concurrency test starts from a clean counter.
	if got := env.CountRows("bookings", ""); got != 0 {
		t.Errorf("seeded bookings = %d, want 0", got)
	}

	svc := env.Service(testsupport.SlugUrut)
	if svc.Price != "75000.00" {
		t.Errorf("urut price = %q, want %q", svc.Price, "75000.00")
	}

	admin := env.Admin()
	if admin.AppskepUserID != 1 {
		t.Errorf("seeded admin appskep id = %d, want 1", admin.AppskepUserID)
	}

	// A fixture slot must not collide with a seeded one, and must be bookable.
	slot := env.FutureSlot(3, 1)
	if slot.Capacity != 1 || slot.BookedCount != 0 {
		t.Errorf("fixture slot = capacity %d booked %d, want 1/0", slot.Capacity, slot.BookedCount)
	}

	user := env.User(4242)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count after one booking = %d, want 1", got)
	}
	if got := env.BookingStatus(booking.ID); got != "pending_payment" {
		t.Errorf("new booking status = %q, want pending_payment", got)
	}
	if booking.PriceAmount != svc.Price {
		t.Errorf("price snapshot = %q, want %q", booking.PriceAmount, svc.Price)
	}

	env.AssertInvariant()
}

// TestHarnessResetIsolates proves Reset actually isolates: this test runs after
// TestHarness, which left a booking behind, and must still see a clean slate.
func TestHarnessResetIsolates(t *testing.T) {
	env := testsupport.New(t)

	if got := env.CountRows("bookings", ""); got != 0 {
		t.Errorf("bookings leaked from the previous test: %d", got)
	}
	if got := env.CountRows("schedule_slots", ""); got != 68 {
		t.Errorf("slots after reset = %d, want the seeded 68", got)
	}
	env.AssertInvariant()
}
