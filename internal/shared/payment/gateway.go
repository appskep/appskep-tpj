// Package payment is the boundary between this system and Midtrans.
//
// Everything here is about one external service, behind one interface, and
// nothing in it touches the database — CLAUDE.md requires every external call to
// sit behind an interface and take a context timeout, and keeping the gateway
// free of storage is what lets the payment service be read as a state machine
// rather than as a mix of HTTP and SQL.
//
// Two facts about the account shape this package. It is Appskep's SHARED
// Midtrans merchant account, so our order IDs carry the "tpj-" prefix (PLAN.md
// R5) and our webhook URL is attached per transaction rather than configured
// account-wide. And payment is confirmed ONLY by a notification (R7): a Charge
// returned by CreateTransaction means a customer was offered a way to pay, never
// that they did.
package payment

import (
	"context"
	"encoding/json"
	"time"
)

// Gateway is the payment provider. One implementation (Midtrans Snap) today; the
// interface exists so the service layer can be exercised without the network and
// so a provider change is one file.
type Gateway interface {
	// CreateTransaction opens a payment for one booking and returns where to send
	// the customer. It never reports that anything was paid.
	CreateTransaction(ctx context.Context, o Order) (*Charge, error)

	// GetStatus asks Midtrans what actually happened to an order. It is the
	// manual remedy for a notification that never arrived, and it returns the
	// same shape a notification does so both feed one state machine.
	GetStatus(ctx context.Context, orderID string) (*Notification, error)

	// Cancel stops Midtrans accepting payment for an order.
	//
	// It exists for user-initiated cancellation (Phase 9): the slot goes back the
	// moment the booking is cancelled, and without this the Snap page the customer
	// still has open keeps taking money for it until the order's own expiry. It
	// reports nothing about what was paid — only the notification does that (R7).
	Cancel(ctx context.Context, orderID string) error
}

// Order is one payment attempt, in the terms Midtrans needs.
type Order struct {
	// ID is the Midtrans order_id: "tpj-<uuid-v4>", already prefixed.
	ID string

	// GrossAmount is whole rupiah. Midtrans has no concept of sen for IDR, and
	// util.PriceToRupiah is what converts the stored DECIMAL string without ever
	// building a float64.
	GrossAmount int64

	// ItemName and ItemID describe the layanan, so the Snap page shows what is
	// being bought rather than a bare total.
	ItemName string
	ItemID   string

	CustomerName  string
	CustomerEmail string
	CustomerPhone string

	// FinishURL is where Snap sends the browser after the customer is done. It is
	// set per transaction, so it never disturbs the account-wide redirect the
	// other Appskep systems rely on.
	FinishURL string

	// NotificationURL is attached to this transaction only, via
	// X-Append-Notification. The account-wide notification URL belongs to another
	// Appskep system and must not be changed.
	NotificationURL string

	// Expiry is how long Midtrans keeps accepting payment. It is derived from the
	// booking's remaining hold, so Midtrans stops taking money at the same moment
	// the expiry ticker releases the slot. Left zero, Midtrans defaults to 24
	// hours and would happily collect payment for a slot given to someone else.
	Expiry time.Duration
}

// Charge is where to send the customer to pay.
type Charge struct {
	Token       string
	RedirectURL string
}

// Notification is what Midtrans reports about an order, from either direction:
// an inbound webhook or an outbound status query. Both carry the same fields and
// the same signature, so the service layer has one input type and one state
// machine rather than two that can drift.
type Notification struct {
	OrderID           string
	TransactionID     string
	TransactionStatus string
	TransactionTime   string
	PaymentType       string
	FraudStatus       string
	StatusCode        string

	// GrossAmount is kept as the EXACT string received ("150000.00"). It is an
	// input to the SHA512 signature, and normalising it — even to an equal
	// numeric value — would make every signature fail to verify.
	GrossAmount string

	SignatureKey string

	Bank     string
	VANumber string

	// Raw is the JSON this was parsed from, stored verbatim on the payment row
	// and in the notification audit log. Midtrans sends fields this struct does
	// not model, and the ones that matter next are usually among them.
	Raw json.RawMessage
}
