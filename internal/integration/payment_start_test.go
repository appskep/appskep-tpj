package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// Payment.Start is the outbound half: what this system tells Midtrans. A Charge
// it returns means a customer was offered a way to pay, never that they did.

// start opens a payment for a fresh booking and returns the charge.
func start(t *testing.T, env *testsupport.Env, appskepID uint64) (*service.Charge, int64) {
	t.Helper()

	user := env.User(appskepID)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

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
	return charge, booking.ID
}

// TestStartBuildsTheOrder checks every field the shared merchant account and the
// expiry design depend on.
func TestStartBuildsTheOrder(t *testing.T) {
	env := testsupport.New(t)

	charge, _ := start(t, env, 9900)

	order, ok := env.Gateway.LastOrder()
	if !ok {
		t.Fatal("Start returned without calling the gateway")
	}

	// PLAN.md R5: the prefix is what separates our transactions from ukom's and
	// every other Appskep system's on the shared account.
	if !strings.HasPrefix(order.ID, "tpj-") {
		t.Errorf("order_id = %q, want the tpj- prefix", order.ID)
	}
	if order.ID != charge.OrderID {
		t.Errorf("the charge names order %q but the gateway was given %q", charge.OrderID, order.ID)
	}
	// Whole rupiah: Midtrans has no concept of sen for IDR.
	if order.GrossAmount != 75000 {
		t.Errorf("gross_amount = %d, want 75000", order.GrossAmount)
	}

	// Both URLs come from APP_URL, never from a request header — the same reason
	// client_base_url does not.
	if order.NotificationURL != "http://localhost:8080/midtrans/notification" {
		t.Errorf("notification URL = %q", order.NotificationURL)
	}
	if !strings.HasPrefix(order.FinishURL, "http://localhost:8080/booking/") {
		t.Errorf("finish URL = %q", order.FinishURL)
	}
	// Konfirmasi, not pembayaran: the customer is done paying, and konfirmasi is
	// the page that describes a booking in any status.
	if !strings.HasSuffix(order.FinishURL, "/konfirmasi") {
		t.Errorf("finish URL = %q, want it to end at konfirmasi", order.FinishURL)
	}

	// Without an expiry Midtrans defaults to 24 hours and would happily collect
	// payment for a slot the ticker released an hour in.
	if order.Expiry <= 0 {
		t.Error("no expiry was sent — Midtrans would keep taking money for a released slot")
	}
	if order.Expiry > time.Hour {
		t.Errorf("expiry = %v, want it inside the 60-minute hold", order.Expiry)
	}

	if order.ItemName == "" || order.ItemID == "" {
		t.Errorf("the Snap page would show a bare total: item = %q/%q", order.ItemID, order.ItemName)
	}
	if order.CustomerEmail == "" {
		t.Error("no customer email was sent")
	}
}

// TestStartUsesTheBookingsPriceSnapshot is the whole point of
// bookings.price_amount: reuse matches what the customer agreed to, never the
// layanan's current price.
func TestStartUsesTheBookingsPriceSnapshot(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9910)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	// The admin raises the price after the booking was made.
	env.Exec("UPDATE services SET price = '999000.00' WHERE slug = ?", testsupport.SlugUrut)

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)
	if _, err := env.Deps.Payment.Start(context.Background(), detail, &payer); err != nil {
		t.Fatalf("Payment.Start: %v", err)
	}

	order, _ := env.Gateway.LastOrder()
	if order.GrossAmount != 75000 {
		t.Errorf("charged %d, want the 75000 snapshotted on the booking — a later "+
			"price edit must not change what a past customer agreed to pay",
			order.GrossAmount)
	}
}

// TestStartReusesAnOpenOrder. Repeated taps of "Bayar sekarang" must not mint a
// new order_id on a shared merchant account.
func TestStartReusesAnOpenOrder(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9920)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)

	first, err := env.Deps.Payment.Start(context.Background(), detail, &payer)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if first.Reused {
		t.Error("the first Start reported a reuse")
	}

	for i := range 3 {
		again, err := env.Deps.Payment.Start(context.Background(), detail, &payer)
		if err != nil {
			t.Fatalf("Start %d: %v", i+2, err)
		}
		if !again.Reused {
			t.Errorf("tap %d minted a new order rather than reusing", i+2)
		}
		if again.OrderID != first.OrderID {
			t.Errorf("tap %d used order %q, want %q", i+2, again.OrderID, first.OrderID)
		}
	}

	if got := env.Gateway.OrderCount(); got != 1 {
		t.Errorf("the gateway was called %d times for four taps, want 1", got)
	}
	if got := env.CountRows("payments", "booking_id = ?", booking.ID); got != 1 {
		t.Errorf("%d payment rows for one booking, want 1", got)
	}
}

// TestStartRefusesWhenTheHoldIsNearlyGone. Midtrans is told to stop accepting
// payment when the hold ends; starting a payment with minutes left would invite
// the customer to pay for a slot the ticker is about to release.
func TestStartRefusesWhenTheHoldIsNearlyGone(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9930)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	// Two minutes left, under the five-minute floor.
	env.Exec("UPDATE bookings SET expires_at = DATE_ADD(NOW(), INTERVAL 2 MINUTE) WHERE id = ?",
		booking.ID)

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)

	_, err = env.Deps.Payment.Start(context.Background(), detail, &payer)
	if !errors.Is(err, service.ErrPaymentWindowClosed) {
		t.Fatalf("Start got %v, want ErrPaymentWindowClosed", err)
	}
	if got := env.Gateway.OrderCount(); got != 0 {
		t.Error("an order was opened at Midtrans for a hold about to lapse")
	}
}

// TestStartRefusesANonPendingBooking: pembayaran is only the payment step.
func TestStartRefusesANonPendingBooking(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9940)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)

	for _, status := range []string{"paid", "cancelled", "expired", "completed"} {
		env.Exec("UPDATE bookings SET status = ? WHERE id = ?", status, booking.ID)

		_, err := env.Deps.Payment.Start(context.Background(), detail, &payer)
		if !errors.Is(err, service.ErrNotPayable) {
			t.Errorf("Start on a %s booking got %v, want ErrNotPayable", status, err)
		}
	}
	if got := env.Gateway.OrderCount(); got != 0 {
		t.Errorf("%d orders were opened for unpayable bookings", got)
	}
}

// TestStartLeavesTheBookingAloneWhenTheGatewayFails. The payment row is written
// before Midtrans is called, deliberately — an order id Midtrans knows and we do
// not would be a notification we could never match. The reverse is inert.
func TestStartLeavesTheBookingAloneWhenTheGatewayFails(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9950)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	env.Gateway.CreateErr = errors.New("midtrans is down")

	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, user.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	payer := env.UserModel(user.ID)

	if _, err := env.Deps.Payment.Start(context.Background(), detail, &payer); err == nil {
		t.Fatal("Start reported success against a failing gateway")
	}

	if got := env.BookingStatus(booking.ID); got != "pending_payment" {
		t.Errorf("status = %q after a failed Snap call, want pending_payment", got)
	}
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d, want 1 — the hold survives a gateway failure", got)
	}
	// The orphaned row has no redirect URL, so GetReusablePaymentForBooking will
	// not offer it and the next tap mints a fresh order.
	if got := env.CountRows("payments", "booking_id = ? AND snap_redirect_url IS NULL", booking.ID); got != 1 {
		t.Errorf("%d inert payment rows, want 1", got)
	}

	env.Gateway.CreateErr = nil
	again, err := env.Deps.Payment.Start(context.Background(), detail, &payer)
	if err != nil {
		t.Fatalf("the retry after a gateway failure: %v", err)
	}
	if again.Reused {
		t.Error("the retry reused the payment row whose Snap call had failed")
	}

	env.AssertInvariant()
}

// TestStartRefusesSomeoneElsesBooking. DetailForUser has already enforced
// ownership; refusing again rather than trusting keeps a future caller that
// forgets from being able to pay for someone else's booking.
func TestStartRefusesSomeoneElsesBooking(t *testing.T) {
	env := testsupport.New(t)

	owner := env.User(9960)
	stranger := env.User(9961)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: owner.ID, SlotID: slot.ID})

	// Resolved as the owner, then handed a different user — the shape a future
	// refactor could produce by accident.
	detail, err := env.Deps.Booking.DetailForUser(context.Background(),
		booking.BookingCode, owner.ID, false)
	if err != nil {
		t.Fatalf("DetailForUser: %v", err)
	}
	other := env.UserModel(stranger.ID)

	if _, err := env.Deps.Payment.Start(context.Background(), detail, &other); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Start got %v, want ErrNotFound", err)
	}
	if got := env.Gateway.OrderCount(); got != 0 {
		t.Error("an order was opened for someone else's booking")
	}
}
