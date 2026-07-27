package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/payment"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// TestBookToPaidEndToEnd is the composition every other file in this package
// tests a piece of: seed → book → open a payment → a signed settlement arrives →
// the booking is paid.
//
// It exists because the pieces can each be right while the seam between two of
// them is wrong, and because this is the one path where money changes hands.
func TestBookToPaidEndToEnd(t *testing.T) {
	env := testsupport.New(t)

	// --- The customer picks a layanan and a slot -----------------------------

	user := env.User(8900)
	slot := env.FutureSlot(3, 1)

	svc := env.Service(testsupport.SlugMassage)
	if svc.Price != "150000.00" {
		t.Fatalf("the seeded price is %q; this test's amounts assume 150000.00", svc.Price)
	}

	// The picker offers the slot before it is taken.
	slots, err := env.Deps.Booking.AvailableSlots(context.Background(), slot.SlotDate)
	if err != nil {
		t.Fatalf("AvailableSlots: %v", err)
	}
	if !containsSlot(slots, slot.ID) {
		t.Fatal("the picker does not offer a free, in-window slot")
	}

	// --- Booking -------------------------------------------------------------

	booking := env.Booking(testsupport.BookingInput{
		UserID:  user.ID,
		SlotID:  slot.ID,
		Slug:    testsupport.SlugMassage,
		Name:    "Budi Santoso",
		Phone:   "081234567890",
		Address: "Jl. Contoh No. 1",
		Notes:   "Punggung bawah.",
	})

	if booking.PriceAmount != svc.Price {
		t.Errorf("price snapshot = %q, want %q", booking.PriceAmount, svc.Price)
	}
	if !strings.HasPrefix(booking.BookingCode, "TPJ-") {
		t.Errorf("booking code = %q", booking.BookingCode)
	}
	if !booking.ExpiresAt.Valid {
		t.Error("the booking carries no payment deadline")
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1", got)
	}

	// The slot stops being offered the moment it is held.
	slots, err = env.Deps.Booking.AvailableSlots(context.Background(), slot.SlotDate)
	if err != nil {
		t.Fatalf("AvailableSlots: %v", err)
	}
	if containsSlot(slots, slot.ID) {
		t.Error("the picker still offers a slot that is now full")
	}

	// --- Payment -------------------------------------------------------------

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)

	charge, err := env.Deps.Payment.Start(context.Background(), detail, &payer)
	if err != nil {
		t.Fatalf("Payment.Start: %v", err)
	}
	if charge.RedirectURL == "" {
		t.Error("no Snap URL to send the customer to")
	}

	// A Snap token is not a purchase (PLAN.md R7).
	if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusPendingPayment {
		t.Errorf("status = %q after opening a payment, want pending_payment — a Snap "+
			"token means a customer was offered a way to pay, never that they did", got)
	}

	order, ok := env.Gateway.LastOrder()
	if !ok {
		t.Fatal("the gateway was never called")
	}
	if order.GrossAmount != 150000 {
		t.Errorf("Midtrans was asked for %d, want 150000", order.GrossAmount)
	}

	// Drain BEFORE resetting: the booking confirmation is queued, not sent, and
	// resetting the recorder while a job is still in the channel would leave it to
	// be counted against the settlement below.
	drain(t, env)
	if got := env.Mail.Count(); got != 1 {
		t.Errorf("%d mails after booking, want the one confirmation", got)
	}
	env.Mail.Reset()
	env.ClearLogs()

	// --- The notification ----------------------------------------------------

	body := testsupport.Notification(charge.OrderID, booking.PriceAmount,
		testsupport.TestServerKey, payment.StatusSettlement, nil)

	res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
	if err != nil {
		t.Fatalf("ApplyWebhook: %v", err)
	}
	if res.SignatureRejected {
		t.Fatal("the settlement was rejected")
	}

	// --- What must be true afterwards ----------------------------------------

	if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusPaid {
		t.Errorf("status = %q, want paid", got)
	}
	if got := env.CountRows("payments", "order_id = ? AND paid_at IS NOT NULL", charge.OrderID); got != 1 {
		t.Error("paid_at was not stamped")
	}
	// A paid booking still holds its slot.
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1", got)
	}
	// And the ticker leaves it alone.
	released, err := env.Deps.Booking.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if released != 0 {
		t.Errorf("the expiry sweep released %d paid bookings", released)
	}

	drain(t, env)
	msgs := env.Mail.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d mails after the settlement, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Subject, booking.BookingCode) {
		t.Errorf("the receipt does not name the booking: %q", msgs[0].Subject)
	}

	// Nothing on this path may put a secret in the log.
	logs := env.Logs()
	for _, secret := range []string{
		testsupport.TestServerKey,
		testsupport.TestAuthSecret,
		testsupport.TestSessionKey,
		charge.RedirectURL,
		"access_token",
	} {
		if secret != "" && strings.Contains(logs, secret) {
			t.Errorf("the log carries %q", secret)
		}
	}

	env.AssertInvariant()
}

// TestBookToExpiredEndToEnd is the other ending: nobody pays, the ticker releases
// the slot, and it becomes bookable by someone else.
func TestBookToExpiredEndToEnd(t *testing.T) {
	env := testsupport.New(t)

	first := env.User(8910)
	second := env.User(8911)
	slot := env.FutureSlot(3, 1)

	booking := env.Booking(testsupport.BookingInput{UserID: first.ID, SlotID: slot.ID})

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, first.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(first.ID)
	if _, err := env.Deps.Payment.Start(context.Background(), detail, &payer); err != nil {
		t.Fatalf("Payment.Start: %v", err)
	}

	env.ExpireHold(booking.ID)
	if _, err := env.Deps.Booking.Expire(context.Background()); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	if got := env.BookingStatus(booking.ID); got != sqlc.BookingsStatusExpired {
		t.Errorf("status = %q, want expired", got)
	}
	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d, want 0", got)
	}

	// The whole point of releasing it: someone else can now have it.
	next := env.Booking(testsupport.BookingInput{UserID: second.ID, SlotID: slot.ID})
	if next.ID == booking.ID {
		t.Fatal("the second booking reused the first")
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d after the re-booking, want 1", got)
	}

	env.AssertInvariant()
}

// TestBookingRequiresAddress guards the rule the home-visit model turns on: the
// therapist travels to the customer, so a booking with no address names no
// destination and must not reach the slot lock.
//
// It asserts the slot is untouched as well as the error, because the failure
// that matters is not "the message is missing" but "a slot was held for a visit
// nobody can make".
func TestBookingRequiresAddress(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(8920)
	slot := env.FutureSlot(3, 1)

	_, err := env.Deps.Booking.Create(context.Background(), service.CreateInput{
		UserID:      user.ID,
		ServiceSlug: testsupport.SlugUrut,
		SlotID:      fmt.Sprint(slot.ID),
		Name:        "Budi Santoso",
		Phone:       "081234567890",
		Address:     "   ", // whitespace only: trimmed, so still empty
	})

	var ve *service.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Create without an address = %v, want a ValidationError", err)
	}
	if _, ok := ve.Fields["alamat"]; !ok {
		t.Errorf("the error names %v, want a message on \"alamat\"", ve.Fields)
	}

	if got := env.BookedCount(slot.ID); got != 0 {
		t.Errorf("booked_count = %d after a rejected booking, want 0", got)
	}

	env.AssertInvariant()
}

func containsSlot(slots []sqlc.ScheduleSlot, id int64) bool {
	for _, s := range slots {
		if s.ID == id {
			return true
		}
	}
	return false
}
