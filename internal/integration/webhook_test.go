package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/payment"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// Payment is confirmed ONLY by a notification (PLAN.md R7). Everything in this
// file goes through Payment.ApplyWebhook with a correctly signed body, because
// that is the one path by which a booking can become paid.

// paid is a booking with a live payment attached, ready for a notification.
type paid struct {
	bookingID int64
	slotID    int64
	orderID   string
	amount    string
	code      string
}

// openPayment creates a booking and opens a Snap order against the fake gateway,
// leaving everything a notification needs.
func openPayment(t *testing.T, env *testsupport.Env, appskepID uint64) paid {
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

	return paid{
		bookingID: booking.ID,
		slotID:    slot.ID,
		orderID:   charge.OrderID,
		// As Midtrans reports it: the booking's snapshot, in whole rupiah with two
		// decimals. The digest is over this exact string.
		amount: booking.PriceAmount,
		code:   booking.BookingCode,
	}
}

// notify delivers a signed notification and returns the result.
func notify(t *testing.T, env *testsupport.Env, p paid, status string, extra map[string]any) service_WebhookResult {
	t.Helper()

	body := testsupport.Notification(p.orderID, p.amount, testsupport.TestServerKey, status, extra)

	res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
	if err != nil {
		t.Fatalf("ApplyWebhook(%s): %v", status, err)
	}
	return service_WebhookResult{SignatureRejected: res.SignatureRejected, Note: res.Note}
}

// service_WebhookResult mirrors service.WebhookResult so the helper above can
// return it without the test file importing the type for one field.
type service_WebhookResult struct {
	SignatureRejected bool
	Note              string
}

// TestSettlementPaysTheBooking is the ordinary success: money is real, the
// booking becomes paid, and the slot stays held.
func TestSettlementPaysTheBooking(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9800)

	res := notify(t, env, p, payment.StatusSettlement, nil)

	if res.SignatureRejected {
		t.Fatal("a correctly signed notification was rejected")
	}
	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusPaid {
		t.Errorf("status = %q, want paid", got)
	}
	if got := env.CountRows("payments", "order_id = ? AND paid_at IS NOT NULL", p.orderID); got != 1 {
		t.Errorf("%d payment rows carry paid_at, want 1", got)
	}
	if got := env.BookedCount(p.slotID); got != 1 {
		t.Errorf("booked_count = %d, want 1 — a paid booking holds its slot", got)
	}
	// Every inbound payload is recorded, whatever it turns out to be.
	if got := env.CountRows("payment_notifications", "order_id = ?", p.orderID); got != 1 {
		t.Errorf("%d audit rows, want 1", got)
	}

	env.AssertInvariant()
}

// TestSettlementIsIdempotent. Midtrans re-sends notifications, and a retry can
// arrive while the first is still in flight. Applying the same settlement twice
// must produce one paid booking and no second side effect.
func TestSettlementIsIdempotent(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9801)

	notify(t, env, p, payment.StatusSettlement, nil)
	env.Mail.Reset()

	var paidAtFirst string
	if err := env.Store.DB().QueryRowContext(context.Background(),
		"SELECT paid_at FROM payments WHERE order_id = ?", p.orderID).Scan(&paidAtFirst); err != nil {
		t.Fatalf("reading paid_at: %v", err)
	}

	// Four more deliveries, as a retrying Midtrans would send them.
	for i := range 4 {
		res := notify(t, env, p, payment.StatusSettlement, nil)
		if res.SignatureRejected {
			t.Fatalf("retry %d was rejected", i+1)
		}
		// The note says so explicitly: the transition already happened.
		if !strings.Contains(res.Note, "final") {
			t.Errorf("retry %d note = %q, want it to report an already-final payment",
				i+1, res.Note)
		}
	}

	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusPaid {
		t.Errorf("status = %q, want paid", got)
	}
	if got := env.CountRows("payments", "order_id = ?", p.orderID); got != 1 {
		t.Errorf("%d payment rows, want 1", got)
	}

	var paidAtNow string
	if err := env.Store.DB().QueryRowContext(context.Background(),
		"SELECT paid_at FROM payments WHERE order_id = ?", p.orderID).Scan(&paidAtNow); err != nil {
		t.Fatalf("re-reading paid_at: %v", err)
	}
	if paidAtNow != paidAtFirst {
		t.Errorf("paid_at moved from %q to %q on a retry", paidAtFirst, paidAtNow)
	}

	// The trigger is a real transition, not a request: four retries that changed
	// nothing must tell the customer nothing.
	if got := env.Mail.Count(); got != 0 {
		t.Errorf("four retries queued %d mails, want 0 — RowsAffected() == 1 is what "+
			"authorises a notice", got)
	}

	// Every delivery is still recorded, which is the point of the audit log.
	if got := env.CountRows("payment_notifications", "order_id = ?", p.orderID); got != 5 {
		t.Errorf("%d audit rows for five deliveries, want 5", got)
	}

	env.AssertInvariant()
}

// TestNotificationStateMachine is the table over every transaction_status
// Midtrans documents, crossed with fraud_status where it matters, asserting the
// booking status AND booked_count each one leaves behind.
func TestNotificationStateMachine(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		fraud       string
		paymentType string
		wantStatus  sqlc.BookingsStatus
		wantHeld    int32
		why         string
	}{
		{
			name: "settlement", status: payment.StatusSettlement,
			wantStatus: sqlc.BookingsStatusPaid, wantHeld: 1,
		},
		{
			name: "capture accepted", status: payment.StatusCapture, fraud: payment.FraudAccept,
			paymentType: payment.PaymentTypeCreditCard,
			wantStatus:  sqlc.BookingsStatusPaid, wantHeld: 1,
		},
		{
			name: "capture challenged", status: payment.StatusCapture, fraud: payment.FraudChallenge,
			paymentType: payment.PaymentTypeCreditCard,
			wantStatus:  sqlc.BookingsStatusPendingPayment, wantHeld: 1,
			why: "a challenge is a human decision, never an automatic acceptance",
		},
		{
			name: "capture denied", status: payment.StatusCapture, fraud: payment.FraudDeny,
			paymentType: payment.PaymentTypeCreditCard,
			wantStatus:  sqlc.BookingsStatusPendingPayment, wantHeld: 1,
		},
		{
			name: "deny", status: payment.StatusDeny,
			wantStatus: sqlc.BookingsStatusExpired, wantHeld: 0,
		},
		{
			name: "expire", status: payment.StatusExpire,
			wantStatus: sqlc.BookingsStatusExpired, wantHeld: 0,
		},
		{
			name: "failure", status: payment.StatusFailure,
			wantStatus: sqlc.BookingsStatusExpired, wantHeld: 0,
		},
		{
			name: "cancel", status: payment.StatusCancel,
			wantStatus: sqlc.BookingsStatusCancelled, wantHeld: 0,
		},
		{
			name: "pending", status: payment.StatusPending,
			wantStatus: sqlc.BookingsStatusPendingPayment, wantHeld: 1,
			why: "the customer has been given a way to pay and has not yet",
		},
		{
			name: "refund", status: payment.StatusRefund,
			wantStatus: sqlc.BookingsStatusPendingPayment, wantHeld: 1,
			why: "a refund always has a human behind it; unbooking automatically " +
				"would be a guess (PLAN.md Q6)",
		},
		{
			name: "partial refund", status: payment.StatusPartial,
			wantStatus: sqlc.BookingsStatusPendingPayment, wantHeld: 1,
		},
		{
			name: "a status this build has never seen", status: "some_future_status",
			wantStatus: sqlc.BookingsStatusPendingPayment, wantHeld: 1,
			why: "record it and touch nothing — guessing would either release a slot " +
				"someone paid for or hold one nobody did",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := testsupport.New(t)
			p := openPayment(t, env, uint64(9810+i))

			extra := map[string]any{}
			if tc.fraud != "" {
				extra["fraud_status"] = tc.fraud
			}
			if tc.paymentType != "" {
				extra["payment_type"] = tc.paymentType
			}

			res := notify(t, env, p, tc.status, extra)
			if res.SignatureRejected {
				t.Fatal("a correctly signed notification was rejected")
			}

			if got := env.BookingStatus(p.bookingID); got != tc.wantStatus {
				t.Errorf("status = %q, want %q — %s", got, tc.wantStatus, tc.why)
			}
			if got := env.BookedCount(p.slotID); got != tc.wantHeld {
				t.Errorf("booked_count = %d, want %d", got, tc.wantHeld)
			}
			env.AssertInvariant()
		})
	}
}

// TestForeignPrefixIsIgnoredCleanly. Notifications for other Appskep systems
// arrive constantly on the shared merchant account. They must be recorded and
// answered 200 — never treated as an error Midtrans should retry for hours.
func TestForeignPrefixIsIgnoredCleanly(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9830)

	body := testsupport.Notification("ukom-4f0a1e9c-8b2d", "150000.00",
		testsupport.TestServerKey, payment.StatusSettlement, nil)

	res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
	if err != nil {
		t.Fatalf("a foreign prefix produced an error: %v", err)
	}
	if res.SignatureRejected {
		t.Error("a foreign order was reported as a signature failure — that would " +
			"answer 401 and make Midtrans retry another system's notification at us")
	}
	if !strings.Contains(res.Note, "tpj") {
		t.Errorf("note = %q, want it to say the prefix is not ours", res.Note)
	}

	// Recorded, because a notification that was refused is exactly the one worth
	// having a record of.
	if got := env.CountRows("payment_notifications", "order_id = ?", "ukom-4f0a1e9c-8b2d"); got != 1 {
		t.Errorf("%d audit rows for the foreign notification, want 1", got)
	}
	// And ours is untouched.
	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusPendingPayment {
		t.Errorf("our booking moved to %q on another system's notification", got)
	}
	env.AssertInvariant()
}

// TestGarbagePayloadIsRecordedAndAccepted: a body we will never understand must
// not be retried forever, and must still be preserved.
func TestGarbagePayloadIsRecordedAndAccepted(t *testing.T) {
	env := testsupport.New(t)

	for _, body := range [][]byte{
		[]byte("this is not json"),
		[]byte(`{"transaction_status":"settlement"}`), // valid JSON, no order_id
		[]byte(`{"order_id":"   "}`),
		{},
	} {
		res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
		if err != nil {
			t.Fatalf("ApplyWebhook(%q) errored: %v", body, err)
		}
		if res.SignatureRejected {
			t.Errorf("ApplyWebhook(%q) reported a signature failure", body)
		}
		if !strings.Contains(res.Note, "tidak bisa dibaca") {
			t.Errorf("note for %q = %q", body, res.Note)
		}
	}

	// MariaDB implements JSON as LONGTEXT with an implicit json_valid() check, so
	// an unparseable body has to be wrapped rather than dropped — and it is the one
	// worth reading later.
	if got := env.CountRows("payment_notifications", ""); got != 4 {
		t.Errorf("%d audit rows for four unreadable payloads, want 4", got)
	}
}

// TestBadSignatureIsRejected is the only outcome that must not answer 200: a
// wrong server key has to be visible at both ends rather than silently swallowing
// every payment.
func TestBadSignatureIsRejected(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9840)

	// Signed with a key that is not ours.
	body := testsupport.Notification(p.orderID, p.amount, "SB-Mid-server-attacker",
		payment.StatusSettlement, nil)

	res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
	if err != nil {
		t.Fatalf("ApplyWebhook: %v", err)
	}
	if !res.SignatureRejected {
		t.Fatal("a forged notification was accepted — this is the whole authentication " +
			"of a public endpoint")
	}
	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusPendingPayment {
		t.Errorf("a forged notification moved the booking to %q", got)
	}
	if got := env.CountRows("payments", "order_id = ? AND paid_at IS NOT NULL", p.orderID); got != 0 {
		t.Error("a forged notification stamped paid_at")
	}
	// Still recorded, with the verdict.
	if got := env.CountRows("payment_notifications", "order_id = ? AND signature_valid = 0", p.orderID); got != 1 {
		t.Errorf("%d rejected-signature audit rows, want 1", got)
	}
	env.AssertInvariant()
}

// TestAmountMismatchIsNotProcessed. The signature covers gross_amount, so a
// mismatch here means the amount changed on OUR side between opening the order
// and the notification arriving — worth refusing and logging rather than
// applying.
func TestAmountMismatchIsNotProcessed(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9850)

	// A correctly signed notification for a different amount: the signature
	// verifies, the amount does not match the payment row.
	body := testsupport.Notification(p.orderID, "1.00", testsupport.TestServerKey,
		payment.StatusSettlement, nil)

	res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
	if err != nil {
		t.Fatalf("ApplyWebhook: %v", err)
	}
	if res.SignatureRejected {
		t.Fatal("the signature was rejected; this test is about the amount check")
	}
	if !strings.Contains(res.Note, "nominal") {
		t.Errorf("note = %q, want it to name the amount mismatch", res.Note)
	}
	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusPendingPayment {
		t.Errorf("a mismatched amount moved the booking to %q", got)
	}
	env.AssertInvariant()
}

// TestMoneyAfterTheSlotWasReleased is Phase 8's logged manual-action path.
//
// The ticker released the slot, someone else may now hold it, and then the money
// arrives. paid_at is stamped because the money is real; the booking stays
// expired; the slot is never re-taken. There is no automated refund in v1, so it
// is logged at ERROR and left for a human.
func TestMoneyAfterTheSlotWasReleased(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9860)

	env.ExpireHold(p.bookingID)
	if _, err := env.Deps.Booking.Expire(context.Background()); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if got := env.BookedCount(p.slotID); got != 0 {
		t.Fatalf("booked_count = %d after expiry, want 0", got)
	}
	env.ClearLogs()

	res := notify(t, env, p, payment.StatusSettlement, nil)
	if res.SignatureRejected {
		t.Fatal("the notification was rejected")
	}

	// The money is real.
	if got := env.CountRows("payments", "order_id = ? AND paid_at IS NOT NULL", p.orderID); got != 1 {
		t.Error("paid_at was not stamped — the money arrived and the record must say so")
	}
	// The booking does not come back.
	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusExpired {
		t.Errorf("status = %q, want expired — a released slot may already belong to "+
			"someone else", got)
	}
	// And the slot is not re-taken.
	if got := env.BookedCount(p.slotID); got != 0 {
		t.Errorf("booked_count = %d, want 0 — re-taking the slot would double-book it", got)
	}
	if !strings.Contains(res.Note, "tindakan admin") {
		t.Errorf("note = %q, want it to ask for manual action", res.Note)
	}
	// Left for a human, and visible.
	if !strings.Contains(env.Logs(), "ERROR") {
		t.Error("money after a release was not logged at ERROR — nobody would ever see it")
	}

	env.AssertInvariant()
}

// TestUnknownOrderIsAccepted: an order id that verifies but names nothing here.
// Nothing to apply, and nothing Midtrans should keep retrying.
func TestUnknownOrderIsAccepted(t *testing.T) {
	env := testsupport.New(t)

	body := testsupport.Notification("tpj-00000000-0000-0000-0000-000000000000",
		"150000.00", testsupport.TestServerKey, payment.StatusSettlement, nil)

	res, err := env.Deps.Payment.ApplyWebhook(context.Background(), body, "203.0.113.7")
	if err != nil {
		t.Fatalf("ApplyWebhook: %v", err)
	}
	if res.SignatureRejected {
		t.Error("an unknown order was reported as a signature failure")
	}
	if !strings.Contains(res.Note, "tidak dikenal") {
		t.Errorf("note = %q", res.Note)
	}
}

// TestSyncReachesTheSameStateMachine. A re-sync is the manual remedy for a
// notification that never arrived — and the only way to complete a payment from a
// development machine Midtrans cannot call back into. It must never interpret a
// status differently from the webhook that should have delivered it.
func TestSyncReachesTheSameStateMachine(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 9870)

	env.Gateway.SetStatus(p.orderID, &payment.Notification{
		OrderID:           p.orderID,
		TransactionStatus: payment.StatusSettlement,
		TransactionID:     "txn-sync",
		GrossAmount:       p.amount,
		StatusCode:        "200",
		PaymentType:       "bank_transfer",
		FraudStatus:       payment.FraudAccept,
		Raw:               []byte(`{"order_id":"` + p.orderID + `"}`),
	})

	if err := env.Deps.Payment.Sync(context.Background(), p.orderID); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if got := env.BookingStatus(p.bookingID); got != sqlc.BookingsStatusPaid {
		t.Errorf("status = %q after a re-sync reporting settlement, want paid", got)
	}
	if got := env.BookedCount(p.slotID); got != 1 {
		t.Errorf("booked_count = %d, want 1", got)
	}
	// Recorded like a webhook: the audit log is the history of what the gateway
	// told us, and a re-sync is part of that history.
	if got := env.CountRows("payment_notifications", "order_id = ?", p.orderID); got != 1 {
		t.Errorf("%d audit rows, want 1", got)
	}
	env.AssertInvariant()
}

// TestSyncIgnoresAForeignOrder: the prefix check applies on the way out too.
func TestSyncIgnoresAForeignOrder(t *testing.T) {
	env := testsupport.New(t)

	if err := env.Deps.Payment.Sync(context.Background(), "ukom-abc"); err == nil {
		t.Fatal("Sync accepted another system's order id")
	}
}
