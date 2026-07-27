package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/payment"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Payment is the money side of a booking: opening a Midtrans order, and applying
// what Midtrans later says happened to it.
//
// Two rules govern everything here, and neither is negotiable.
//
// A booking becomes paid ONLY because Midtrans said so (PLAN.md R7). Start
// returns a URL, which means a customer was offered a way to pay — never that
// they did. Nothing on the Snap response path touches bookings.status.
//
// And a slot is released by exactly one authority per event. The chain is:
// the payment transition must report a real change before the booking
// transition is attempted, and the booking transition must report a real change
// before the slot is released. Both are RowsAffected() == 1 against a
// status-guarded UPDATE, which is what lets a Midtrans retry, the expiry ticker
// and a user cancellation all reach the same booking without any of them
// double-counting.
type Payment struct {
	store *repository.Store
	gw    payment.Gateway
	log   *slog.Logger
	// email queues what a customer is told. Every notice it raises is decided
	// inside a transaction and enqueued after that transaction commits — see
	// pendingEmail and applyOnce.
	email *Email

	// cfg carries the order-ID prefix that separates our transactions from the
	// other Appskep systems on the shared merchant account, and the server key
	// the signature check needs.
	cfg config.MidtransConfig

	// appURL is where Midtrans sends the customer back to, and where it posts
	// notifications. From APP_URL, never from a request header — the same reason
	// client_base_url is not (see internal/app/auth.go).
	appURL string

	loc *time.Location
}

func NewPayment(
	store *repository.Store,
	gw payment.Gateway,
	log *slog.Logger,
	email *Email,
	cfg config.MidtransConfig,
	appURL string,
	loc *time.Location,
) *Payment {
	return &Payment{store: store, gw: gw, log: log, email: email, cfg: cfg, appURL: appURL, loc: loc}
}

// pendingEmail is a notice a transaction decided on but must not send.
//
// The state machine below runs entirely inside one transaction holding a slot
// lock, and nothing that talks to another machine may run under one. So the
// transition records what the customer should be told, and applyOnce enqueues it
// after the commit — the same shape as "the gateway order is cancelled after the
// transaction commits, never inside it". A zero Kind means there is nothing to
// send: a retry that changed no rows tells the customer nothing.
type pendingEmail struct {
	kind      EmailKind
	bookingID int64
	// wasPaid records whether the booking had already been paid for when it was
	// cancelled, which decides whether the mail mentions a refund.
	wasPaid bool
}

const (
	// minPaymentWindow is how much of the booking's hold must remain before a
	// payment may be started. Below it, the customer would be sent to a Snap page
	// that expires while they are reading it — and might still pay, for a slot the
	// ticker has already given away.
	minPaymentWindow = 5 * time.Minute

	// maxNoteLen matches payment_notifications.process_note VARCHAR(255).
	maxProcessNoteLen = 255
)

// Charge is where to send a customer to pay, plus whether this is the order they
// were already given.
type Charge struct {
	OrderID     string
	RedirectURL string
	// Reused reports that an existing, still-valid Snap order was returned rather
	// than a new one being minted. Repeated taps of "Bayar sekarang" must not
	// create a new order_id on a shared merchant account (create-order.md:93).
	Reused bool
}

// Start opens — or re-opens — the payment for one booking.
//
// The booking argument comes from Booking.DetailForUser, so ownership has
// already been enforced; every rule that decides whether money may be taken is
// nonetheless re-checked here against a locked row, because the page that
// rendered the button is by definition out of date.
//
// The ordering below is the same one create-order.md §5 describes, and it is
// deliberate: the payment row is written BEFORE Midtrans is called. An order ID
// that Midtrans knows and we do not would be a notification we could never
// match; the reverse — a row whose Snap call failed — is inert, because
// GetReusablePaymentForBooking requires a redirect URL and this row has none.
func (p *Payment) Start(
	ctx context.Context,
	booking sqlc.GetBookingDetailByCodeRow,
	user *model.User,
) (*Charge, error) {
	var (
		row     sqlc.Payment
		reuse   bool
		expires time.Time
	)

	// Tx1: decide, under the booking's row lock, whether to reuse or mint.
	//
	// No slot lock is taken and none is needed — this transaction cannot change
	// booked_count, so it never enters the schedule_slots -> bookings -> payments
	// order that would otherwise bind it.
	err := p.store.WithTx(ctx, func(q *sqlc.Queries) error {
		bk, err := q.GetBookingForUpdate(ctx, booking.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("locking booking %d: %w", booking.ID, err)
		}
		if bk.UserID != user.ID {
			// Cannot normally happen — the caller resolved the booking through
			// DetailForUser. Refusing rather than trusting keeps a future caller
			// that forgets from being able to pay for someone else's booking.
			return ErrNotFound
		}
		if bk.Status != sqlc.BookingsStatusPendingPayment {
			return ErrNotPayable
		}
		if !bk.ExpiresAt.Valid {
			return ErrPaymentWindowClosed
		}

		expires = bk.ExpiresAt.Time
		if time.Until(expires) < minPaymentWindow {
			return ErrPaymentWindowClosed
		}

		// Reuse: same booking, same amount, no lifecycle timestamp, and a Snap URL
		// to hand back. The query carries every one of those conditions except the
		// deadline, which its own comment leaves here so each parameter stays
		// directly comparable to a bare column.
		existing, err := q.GetReusablePaymentForBooking(ctx, sqlc.GetReusablePaymentForBookingParams{
			BookingID:   bk.ID,
			GrossAmount: bk.PriceAmount,
		})
		switch {
		case err == nil:
			row, reuse = existing, true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("looking for a reusable payment on booking %d: %w", bk.ID, err)
		}

		orderID := p.cfg.OrderID(uuid.NewString())
		res, err := q.CreatePayment(ctx, sqlc.CreatePaymentParams{
			BookingID:   bk.ID,
			OrderID:     orderID,
			GrossAmount: bk.PriceAmount,
		})
		if err != nil {
			return fmt.Errorf("creating payment for booking %d: %w", bk.ID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading payment id: %w", err)
		}

		row, err = q.GetPayment(ctx, id)
		if err != nil {
			return fmt.Errorf("re-reading payment %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if reuse {
		return &Charge{OrderID: row.OrderID, RedirectURL: row.SnapRedirectUrl.String, Reused: true}, nil
	}

	amount, err := util.PriceToRupiah(row.GrossAmount)
	if err != nil {
		return nil, fmt.Errorf("converting %q for order %s: %w", row.GrossAmount, row.OrderID, err)
	}

	// The external call, outside every transaction: a row lock is never held
	// across a network round trip to somebody else's service.
	charge, err := p.gw.CreateTransaction(ctx, payment.Order{
		ID:            row.OrderID,
		GrossAmount:   amount,
		ItemID:        strconv.FormatInt(booking.ServiceID, 10),
		ItemName:      booking.ServiceName,
		CustomerName:  booking.CustomerName,
		CustomerEmail: user.Email,
		CustomerPhone: booking.CustomerPhone,
		// Konfirmasi, not pembayaran: the customer is done paying, and konfirmasi
		// is the page that describes a booking in any status. Per transaction, so
		// it never disturbs the account-wide redirect another Appskep system uses.
		FinishURL:       p.appURL + "/booking/" + booking.BookingCode + "/konfirmasi",
		NotificationURL: p.WebhookURL(),
		// Midtrans stops accepting payment when our hold ends, so it cannot
		// collect money for a slot the expiry ticker has already released.
		Expiry: time.Until(expires),
	})
	if err != nil {
		return nil, err
	}

	if err := p.store.Queries.UpdatePaymentSnap(ctx, sqlc.UpdatePaymentSnapParams{
		ID:              row.ID,
		SnapToken:       nullString(charge.Token),
		SnapRedirectUrl: nullString(charge.RedirectURL),
	}); err != nil {
		// The order exists at Midtrans and the customer can still be sent to it;
		// what is lost is the ability to reuse it, which costs one extra order on
		// the next tap rather than a failed payment.
		p.log.ErrorContext(ctx, "payment: storing snap result",
			slog.String("order_id", row.OrderID), slog.Any("error", err))
	}

	return &Charge{OrderID: row.OrderID, RedirectURL: charge.RedirectURL}, nil
}

// WebhookURL is where Midtrans should post notifications for our transactions.
//
// It is attached per transaction (X-Append-Notification) rather than configured
// on the account, because the account-wide setting belongs to another Appskep
// system and changing it would stop their payments being confirmed.
func (p *Payment) WebhookURL() string { return p.appURL + "/api/webhook/midtrans" }

// LatestForBooking returns the newest payment attempt, for the payment page.
// A booking with no attempt yet is not an error.
func (p *Payment) LatestForBooking(ctx context.Context, bookingID int64) (*sqlc.Payment, error) {
	row, err := p.store.Queries.GetLatestPaymentForBooking(ctx, bookingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting latest payment for booking %d: %w", bookingID, err)
	}
	return &row, nil
}

// CancelOrder closes the newest Midtrans order for a booking, so a Snap page the
// customer still has open cannot take money for a slot that has been given back.
//
// It is called AFTER the cancellation transaction commits, and is best-effort by
// design: the slot is already released, so a gateway failure must not undo
// anything or be reported to the customer as a failed cancellation. The caller
// logs and carries on.
//
// Two Midtrans answers are ordinary and mean "nothing to do": ErrOrderNotFound
// (the Snap token was never used to charge anything — the common case) and
// ErrNotCancelable (the order already reached a final state). Neither is
// returned as an error.
func (p *Payment) CancelOrder(ctx context.Context, bookingID int64) error {
	row, err := p.LatestForBooking(ctx, bookingID)
	if err != nil {
		return err
	}
	if row == nil || !row.SnapToken.Valid {
		// No payment attempt was ever opened, so there is nothing at Midtrans.
		return nil
	}
	if row.PaidAt.Valid {
		// Never cancel an order that was paid. The money is real; what to do about
		// a paid booking whose slot was released is the webhook's logged
		// manual-action path, not a gateway call.
		return nil
	}

	switch err := p.gw.Cancel(ctx, row.OrderID); {
	case errors.Is(err, payment.ErrOrderNotFound), errors.Is(err, payment.ErrNotCancelable):
		return nil
	case err != nil:
		return fmt.Errorf("cancelling order %s: %w", row.OrderID, err)
	}

	// Record it locally rather than waiting for the cancel notification. Midtrans
	// cannot reach a development machine at all, and in production the webhook
	// finds the payment already final, reports zero rows and changes nothing —
	// the Phase 8 idempotency rule unchanged.
	res, err := p.store.Queries.MarkPaymentCancelled(ctx, sqlc.MarkPaymentCancelledParams{
		TransactionStatus: nullString("cancel"),
		OrderID:           row.OrderID,
	})
	if err != nil {
		return fmt.Errorf("recording cancellation for order %s: %w", row.OrderID, err)
	}
	if _, err := changed(res); err != nil {
		return fmt.Errorf("reading cancellation result for order %s: %w", row.OrderID, err)
	}
	return nil
}

// WebhookResult tells the handler what to answer.
type WebhookResult struct {
	// SignatureRejected means the payload claimed to be a Midtrans notification
	// for one of our orders and its signature did not verify. It is the ONLY
	// outcome that must not answer 200: a wrong server key has to be visible at
	// both ends rather than silently swallowing every payment.
	//
	// A payload that is not ours at all — another Appskep system's order ID, or a
	// body that is not JSON — is not "rejected". Nothing we can ever accept must
	// be answered with a status that makes Midtrans retry it for hours.
	SignatureRejected bool

	// Note is the one-line summary written to payment_notifications.process_note.
	Note string
}

// ApplyWebhook is the inbound notification path: audit, authenticate, apply.
//
// The audit row is written FIRST, unconditionally, for every payload — bad JSON,
// forged signatures and other Appskep systems' order IDs included. A notification
// that was refused is exactly the one worth having a record of, and deciding
// whether to keep it after deciding whether to trust it would lose precisely
// those.
func (p *Payment) ApplyWebhook(ctx context.Context, raw []byte, remoteIP string) (WebhookResult, error) {
	n, perr := payment.ParseNotification(raw)

	notifID, err := p.recordNotification(ctx, n, raw, nullString(remoteIP))
	if err != nil {
		return WebhookResult{}, err
	}

	if perr != nil {
		// Unparseable, or carrying no order_id. Nothing to apply and nothing to
		// verify; the body is already preserved above.
		return p.finish(ctx, notifID, WebhookResult{Note: "payload tidak bisa dibaca"})
	}

	if !p.cfg.OwnsOrderID(n.OrderID) {
		// Another Appskep system on the shared merchant account. Not ours, not an
		// error, and not something Midtrans should keep retrying.
		return p.finish(ctx, notifID, WebhookResult{Note: "order_id bukan milik " + p.cfg.Prefix})
	}

	if !payment.VerifySignature(n, p.cfg.ServerKey) {
		p.log.WarnContext(ctx, "payment: notification signature rejected",
			slog.String("order_id", n.OrderID), slog.String("remote", remoteIP))
		return p.finish(ctx, notifID, WebhookResult{
			SignatureRejected: true,
			Note:              "signature tidak valid",
		})
	}

	note, err := p.apply(ctx, n)
	if err != nil {
		return WebhookResult{}, err
	}
	return p.finish(ctx, notifID, WebhookResult{Note: note})
}

// Sync asks Midtrans what happened to an order and applies the answer.
//
// The manual remedy for a notification that never arrived — and the only way to
// complete a payment from a development machine Midtrans cannot call back into.
// It reaches the identical state machine, so a re-sync can never interpret a
// status differently from the webhook that should have delivered it.
func (p *Payment) Sync(ctx context.Context, orderID string) error {
	if !p.cfg.OwnsOrderID(orderID) {
		return ErrNotFound
	}

	n, err := p.gw.GetStatus(ctx, orderID)
	if errors.Is(err, payment.ErrOrderNotFound) {
		return ErrOrderNotFound
	}
	if err != nil {
		return err
	}

	// Recorded like a webhook, with no remote IP: the audit log is the history of
	// what the gateway told us, and a re-sync is part of that history.
	notifID, err := p.recordNotification(ctx, n, n.Raw, sql.NullString{})
	if err != nil {
		return err
	}

	note, err := p.apply(ctx, n)
	if err != nil {
		return err
	}
	_, err = p.finish(ctx, notifID, WebhookResult{Note: "sync: " + note})
	return err
}

// apply is the state machine. One transaction, entered in the canonical lock
// order, idempotent under every retry.
func (p *Payment) apply(ctx context.Context, n *payment.Notification) (string, error) {
	// Resolve the slot BEFORE opening the transaction. The transaction's first
	// statement has to be the slot lock, and the slot is only reachable through
	// the payment and the booking — so those two reads happen out here, exactly
	// as ListExpiredPendingBookings feeds the expiry ticker's per-booking
	// transaction with a slot_id.
	pay, err := p.store.Queries.GetPaymentByOrderID(ctx, n.OrderID)
	if errors.Is(err, sql.ErrNoRows) {
		return "order tidak dikenal", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting payment %s: %w", n.OrderID, err)
	}

	bk, err := p.store.Queries.GetBooking(ctx, pay.BookingID)
	if err != nil {
		return "", fmt.Errorf("getting booking %d for %s: %w", pay.BookingID, n.OrderID, err)
	}

	// One retry, for the same reason Booking.Create has one: the expiry ticker
	// takes these locks in the same order and the two can collide under load.
	// Also covers the reschedule race guarded below.
	var note string
	for attempt := range 2 {
		note, err = p.applyOnce(ctx, n, bk.SlotID)
		if err == nil {
			return note, nil
		}
		if attempt == 0 && (repository.IsRetryable(err) || errors.Is(err, errSlotMoved)) {
			// Re-read: if the booking was rescheduled, its slot changed under us.
			if bk, err = p.store.Queries.GetBooking(ctx, pay.BookingID); err != nil {
				return "", fmt.Errorf("re-reading booking %d: %w", pay.BookingID, err)
			}
			continue
		}
		return "", err
	}
	return "", err
}

// errSlotMoved reports that the booking changed slots between the pre-read and
// the lock — Phase 10's reschedule doing its job. Releasing the slot we happened
// to lock would then decrement a slot this booking no longer holds.
var errSlotMoved = errors.New("service: booking moved to another slot")

func (p *Payment) applyOnce(ctx context.Context, n *payment.Notification, slotID int64) (string, error) {
	var (
		note   string
		notify pendingEmail
	)

	err := p.store.WithTx(ctx, func(q *sqlc.Queries) error {
		// 1. THE LOCK — the literal first statement of the transaction.
		//
		//    Not merely lock ordering: under REPEATABLE READ the first consistent
		//    read fixes the snapshot, and a locking read that then meets a newer
		//    row fails with MariaDB ER_CHECKREAD (1020) instead of blocking. A
		//    plain SELECT above this line looks harmless and turns a queued
		//    racer into a 500. Measured in Phase 7: 19 of 20.
		if _, err := q.GetSlotForUpdate(ctx, slotID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// fk_bookings_slot is RESTRICT, so a slot with bookings cannot be
				// deleted; unreachable in practice.
				return fmt.Errorf("slot %d not found", slotID)
			}
			return fmt.Errorf("locking slot %d: %w", slotID, err)
		}

		// 2. The payment, by order_id. Locked before the booking would invert the
		//    canonical order, so the booking comes first.
		pay, err := q.GetPaymentByOrderID(ctx, n.OrderID)
		if err != nil {
			return fmt.Errorf("re-reading payment %s: %w", n.OrderID, err)
		}

		bk, err := q.GetBookingForUpdate(ctx, pay.BookingID)
		if err != nil {
			return fmt.Errorf("locking booking %d: %w", pay.BookingID, err)
		}
		if bk.SlotID != slotID {
			return errSlotMoved
		}

		locked, err := q.GetPaymentByOrderIDForUpdate(ctx, n.OrderID)
		if err != nil {
			return fmt.Errorf("locking payment %s: %w", n.OrderID, err)
		}

		// 3. The amount Midtrans reports must be the amount we recorded. The
		//    signature already covers gross_amount, so a mismatch is not forgery —
		//    it means our own row and the order at Midtrans have drifted, and
		//    marking that booking paid would accept whatever was actually charged.
		if !sameAmount(n.GrossAmount, locked.GrossAmount) {
			p.log.ErrorContext(ctx, "payment: gross_amount mismatch",
				slog.String("order_id", n.OrderID),
				slog.String("expected", locked.GrossAmount),
				slog.String("received", n.GrossAmount))
			note = "nominal tidak cocok, tidak diproses"
			return nil
		}

		note, err = p.transition(ctx, q, n, bk, slotID, &notify)
		return err
	})
	if err != nil {
		return "", err
	}

	// After the commit, and only for a transition that really happened.
	switch notify.kind {
	case "":
	case EmailBookingCancelled:
		p.email.NotifyCancelled(ctx, notify.bookingID, notify.wasPaid)
	default:
		p.email.Notify(ctx, notify.kind, notify.bookingID)
	}

	return note, nil
}

// transition applies one notification to the payment, the booking and the slot,
// in that order, each step authorised by the one before it.
//
// notify is filled in — never sent from — when a transition really happened; the
// caller enqueues it after the commit.
func (p *Payment) transition(
	ctx context.Context,
	q *sqlc.Queries,
	n *payment.Notification,
	bk sqlc.Booking,
	slotID int64,
	notify *pendingEmail,
) (string, error) {
	raw := nullJSON(n.Raw)

	switch n.Outcome() {
	case payment.OutcomePaid:
		res, err := q.MarkPaymentPaid(ctx, sqlc.MarkPaymentPaidParams{
			OrderID:           n.OrderID,
			TransactionID:     nullString(clip(n.TransactionID, 100)),
			TransactionStatus: nullString(clip(n.TransactionStatus, 50)),
			TransactionTime:   p.parseTime(n.TransactionTime),
			PaymentType:       nullString(clip(n.PaymentType, 50)),
			FraudStatus:       nullString(clip(n.FraudStatus, 30)),
			StatusCode:        nullString(clip(n.StatusCode, 10)),
			RawResponse:       raw,
		})
		if err != nil {
			return "", fmt.Errorf("marking payment %s paid: %w", n.OrderID, err)
		}
		moved, err := changed(res)
		if err != nil {
			return "", err
		}
		if !moved {
			// Already settled, or already dead. Either way this notification has
			// nothing left to do, and the booking must not be touched.
			return "pembayaran sudah final sebelumnya", nil
		}

		bres, err := q.MarkBookingPaid(ctx, bk.ID)
		if err != nil {
			return "", fmt.Errorf("marking booking %d paid: %w", bk.ID, err)
		}
		bmoved, err := changed(bres)
		if err != nil {
			return "", err
		}
		if !bmoved {
			// The money is real and the payment row now says so, but the booking
			// could not accept it — the expiry ticker or a cancellation got there
			// first, and its slot has been given back to whoever wanted it. There
			// is nothing safe to do automatically: re-taking the slot would evict
			// a second customer, and v1 has no refund path (PLAN.md Q6). It is
			// logged loudly and left for a human.
			p.log.ErrorContext(ctx, "payment: paid after the slot was released — needs manual action",
				slog.String("order_id", n.OrderID),
				slog.String("booking_code", bk.BookingCode),
				slog.String("booking_status", string(bk.Status)))
			return "DIBAYAR setelah booking " + string(bk.Status) + " — perlu tindakan admin", nil
		}

		p.log.InfoContext(ctx, "booking paid",
			slog.String("booking_code", bk.BookingCode),
			slog.String("order_id", n.OrderID),
			slog.String("payment_type", n.PaymentType))
		*notify = pendingEmail{kind: EmailPaymentReceived, bookingID: bk.ID}
		return "pembayaran diterima, booking menjadi paid", nil

	case payment.OutcomeExpired:
		res, err := q.MarkPaymentExpired(ctx, sqlc.MarkPaymentExpiredParams{
			OrderID:           n.OrderID,
			TransactionStatus: nullString(clip(n.TransactionStatus, 50)),
			FraudStatus:       nullString(clip(n.FraudStatus, 30)),
			StatusCode:        nullString(clip(n.StatusCode, 10)),
			RawResponse:       raw,
		})
		if err != nil {
			return "", fmt.Errorf("marking payment %s expired: %w", n.OrderID, err)
		}
		return p.release(ctx, q, res, bk, slotID, "", "kedaluwarsa/ditolak", notify)

	case payment.OutcomeCancelled:
		res, err := q.MarkPaymentCancelled(ctx, sqlc.MarkPaymentCancelledParams{
			OrderID:           n.OrderID,
			TransactionStatus: nullString(clip(n.TransactionStatus, 50)),
			StatusCode:        nullString(clip(n.StatusCode, 10)),
			RawResponse:       raw,
		})
		if err != nil {
			return "", fmt.Errorf("marking payment %s cancelled: %w", n.OrderID, err)
		}
		return p.release(ctx, q, res, bk, slotID, "Pembayaran dibatalkan.", "dibatalkan", notify)

	default:
		// pending, a challenged capture, and anything Midtrans invents later.
		// Recorded, nothing decided: the booking keeps its slot until a terminal
		// status arrives or the ticker takes it.
		if err := q.RecordPaymentProgress(ctx, sqlc.RecordPaymentProgressParams{
			OrderID:           n.OrderID,
			TransactionStatus: nullString(clip(n.TransactionStatus, 50)),
			TransactionID:     nullString(clip(n.TransactionID, 100)),
			TransactionTime:   p.parseTime(n.TransactionTime),
			PaymentType:       nullString(clip(n.PaymentType, 50)),
			FraudStatus:       nullString(clip(n.FraudStatus, 30)),
			StatusCode:        nullString(clip(n.StatusCode, 10)),
			Bank:              nullString(clip(n.Bank, 50)),
			VaNumber:          nullString(clip(n.VANumber, 64)),
			RawResponse:       raw,
		}); err != nil {
			return "", fmt.Errorf("recording progress for %s: %w", n.OrderID, err)
		}
		if n.FraudStatus == payment.FraudChallenge {
			p.log.WarnContext(ctx, "payment: capture flagged for review",
				slog.String("order_id", n.OrderID),
				slog.String("booking_code", bk.BookingCode))
			return "capture ditandai challenge, menunggu review", nil
		}
		return "status " + n.TransactionStatus + " dicatat", nil
	}
}

// release ends a booking and gives its slot back, but only along the full chain
// of authorisations: the payment moved, therefore try the booking; the booking
// moved, therefore release the slot.
//
// Skipping either guard is how booked_count drifts below the truth. The expiry
// ticker can reach the same booking a second before this does, and the second
// arrival must change nothing at all.
func (p *Payment) release(
	ctx context.Context,
	q *sqlc.Queries,
	res sql.Result,
	bk sqlc.Booking,
	slotID int64,
	cancelReason string,
	what string,
	notify *pendingEmail,
) (string, error) {
	moved, err := changed(res)
	if err != nil {
		return "", err
	}
	if !moved {
		return "pembayaran sudah final sebelumnya", nil
	}

	var bres sql.Result
	if cancelReason != "" {
		bres, err = q.CancelBooking(ctx, sqlc.CancelBookingParams{
			ID:              bk.ID,
			CancelledReason: nullString(cancelReason),
		})
	} else {
		bres, err = q.ExpireBooking(ctx, bk.ID)
	}
	if err != nil {
		return "", fmt.Errorf("ending booking %d (%s): %w", bk.ID, what, err)
	}

	bmoved, err := changed(bres)
	if err != nil {
		return "", err
	}
	if !bmoved {
		// Already released by the ticker or by the user. Releasing again would
		// decrement a slot that was decremented once for this booking.
		return "pembayaran " + what + ", booking sudah " + string(bk.Status), nil
	}

	if err := q.ReleaseSlot(ctx, slotID); err != nil {
		return "", fmt.Errorf("releasing slot %d: %w", slotID, err)
	}

	// The booking really ended, here, so this call is the one that tells the
	// customer. cancelReason is what distinguishes the two endings, exactly as it
	// does for the booking transition above.
	//
	// bk.Status is the status held before this transaction: a gateway
	// cancellation of a booking that was already paid has money to explain.
	if cancelReason != "" {
		*notify = pendingEmail{
			kind:      EmailBookingCancelled,
			bookingID: bk.ID,
			wasPaid:   bk.Status != sqlc.BookingsStatusPendingPayment,
		}
	} else {
		*notify = pendingEmail{kind: EmailBookingExpired, bookingID: bk.ID}
	}

	p.log.InfoContext(ctx, "booking released after payment "+what,
		slog.String("booking_code", bk.BookingCode),
		slog.Int64("slot_id", slotID))
	return "pembayaran " + what + ", slot dilepas", nil
}

// recordNotification writes the audit row that every inbound payload gets,
// whether or not it is ours and whether or not it verifies.
func (p *Payment) recordNotification(
	ctx context.Context,
	n *payment.Notification,
	raw []byte,
	remoteIP sql.NullString,
) (int64, error) {
	arg := sqlc.CreatePaymentNotificationParams{
		Payload:  validJSON(raw),
		RemoteIp: remoteIP,
	}
	if n != nil {
		arg.OrderID = clip(n.OrderID, 64)
		arg.TransactionStatus = nullString(clip(n.TransactionStatus, 50))
		arg.FraudStatus = nullString(clip(n.FraudStatus, 30))
		arg.StatusCode = nullString(clip(n.StatusCode, 10))
		arg.GrossAmount = nullString(clip(n.GrossAmount, 32))
		arg.SignatureValid = payment.VerifySignature(n, p.cfg.ServerKey)
	}

	res, err := p.store.Queries.CreatePaymentNotification(ctx, arg)
	if err != nil {
		return 0, fmt.Errorf("recording notification for %q: %w", arg.OrderID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading notification id: %w", err)
	}
	return id, nil
}

// finish stamps the audit row with what was decided and returns the result.
func (p *Payment) finish(ctx context.Context, notifID int64, r WebhookResult) (WebhookResult, error) {
	if err := p.store.Queries.MarkNotificationProcessed(ctx, sqlc.MarkNotificationProcessedParams{
		ID:          notifID,
		ProcessNote: nullString(clip(r.Note, maxProcessNoteLen)),
	}); err != nil {
		return r, fmt.Errorf("marking notification %d processed: %w", notifID, err)
	}
	return r, nil
}

// parseTime reads Midtrans' "2006-01-02 15:04:05". It reports the value in the
// application timezone, which is also the session timezone MySQL is pinned to,
// so the DATETIME column stores the same wall clock Midtrans meant.
func (p *Payment) parseTime(s string) sql.NullTime {
	if s == "" {
		return sql.NullTime{}
	}
	t, err := time.ParseInLocation(time.DateTime, s, p.loc)
	if err != nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// changed turns a guarded UPDATE's result into "did this actually transition".
//
// RowsAffected() == 1 is the definition of a real status transition, and the
// only thing that authorises the step after it.
func changed(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading rows affected: %w", err)
	}
	return n == 1, nil
}

// sameAmount compares two DECIMAL strings by value, textually. "150000.00" and
// "150000" are the same amount written two ways.
func sameAmount(a, b string) bool {
	x, errA := util.PriceToRupiah(a)
	y, errB := util.PriceToRupiah(b)
	return errA == nil && errB == nil && x == y
}

// validJSON keeps a JSON column valid. MariaDB implements JSON as LONGTEXT with
// an implicit json_valid() check, so a body that is not JSON has to be wrapped
// rather than stored raw — and it must be stored, because an unparseable
// notification is exactly the one worth reading later.
//
// An EMPTY body is wrapped too, not nulled. payment_notifications.payload is NOT
// NULL, so returning nil here made a zero-length POST to the public webhook fail
// its own audit insert and answer 500 — which is the status that tells Midtrans
// to retry, so an empty body would have been retried forever and logged at ERROR
// each time. A zero-length request is something anything on the internet can send
// to an unauthenticated endpoint; it must land on the same "we will never
// understand this, stop sending it" path as any other garbage. Found by Phase
// 13's webhook test.
func validJSON(raw []byte) json.RawMessage {
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	wrapped, err := json.Marshal(map[string]string{"raw": string(raw)})
	if err != nil {
		return nil
	}
	return wrapped
}

// nullJSON is validJSON for payments.raw_response, which is nullable and so is
// mapped to NullString (see sqlc.yaml). An empty body writes NULL rather than an
// empty string, which json_valid() would reject.
//
// The emptiness test is on the input, not on validJSON's output: that column is
// NULL until Midtrans answers, which is the ordinary case, and wrapping nothing
// into {"raw":""} would fill it with noise. payment_notifications.payload is NOT
// NULL and takes the wrapped form instead — the two columns want opposite things
// from an empty body, which is why there are two functions.
func nullJSON(raw []byte) sql.NullString {
	if len(raw) == 0 {
		return sql.NullString{}
	}
	v := validJSON(raw)
	if len(v) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(v), Valid: true}
}

// clip bounds a value to its column width. Midtrans is free to send a longer
// string than the schema anticipated, and strict mode turns that into a failed
// INSERT on a webhook we are obliged to accept.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
