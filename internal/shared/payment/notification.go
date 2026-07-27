package payment

import (
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// ErrMalformedPayload reports a notification body that is not JSON, or that
// carries no order_id.
//
// It is deliberately not fatal to the caller: an unparseable body is still
// written to the audit log and still answered 200, because a payload we will
// never understand must not be retried by Midtrans forever.
var ErrMalformedPayload = errors.New("payment: malformed notification payload")

// Transaction statuses, as Midtrans spells them.
const (
	StatusCapture    = "capture"
	StatusSettlement = "settlement"
	StatusPending    = "pending"
	StatusDeny       = "deny"
	StatusCancel     = "cancel"
	StatusExpire     = "expire"
	StatusRefund     = "refund"
	StatusPartial    = "partial_refund"
	StatusFailure    = "failure"
)

// Fraud statuses.
const (
	FraudAccept    = "accept"
	FraudChallenge = "challenge"
	FraudDeny      = "deny"
)

// PaymentTypeCreditCard is the one payment type whose "capture" needs a fraud
// check before it counts as money received.
const PaymentTypeCreditCard = "credit_card"

// Outcome is what a notification means for the booking behind it. The mapping
// from Midtrans' vocabulary to ours happens once, here, so the service layer
// switches on four cases instead of on a string it has to re-interpret.
type Outcome int

const (
	// OutcomeProgress: something happened, nothing is decided. The payment row
	// records it; the booking and the slot are untouched. A credit-card capture
	// flagged for review lands here too — a challenge is a human decision, never
	// an automatic acceptance.
	OutcomeProgress Outcome = iota

	// OutcomePaid: the money is real. The only outcome that may mark a booking
	// paid, and it can only ever arrive from Midtrans (R7).
	OutcomePaid

	// OutcomeExpired: deny or expire. Releases the slot.
	OutcomeExpired

	// OutcomeCancelled: cancel. Releases the slot.
	OutcomeCancelled
)

// Outcome classifies a notification.
//
// The default is OutcomeProgress rather than an error, on purpose: Midtrans can
// introduce a transaction_status this build has never heard of, and the safe
// response to an unrecognised one is to record it and touch nothing. Guessing
// would mean either releasing a slot someone paid for or holding one nobody did.
//
// refund and partial_refund are deliberately OutcomeProgress. A refund reverses
// money that was already taken for a booking that may already have been
// delivered; unbooking the slot automatically would be a guess about an event
// that always has a human behind it. Phase 10's admin panel is where a refunded
// booking gets resolved.
func (n *Notification) Outcome() Outcome {
	switch n.TransactionStatus {
	case StatusSettlement:
		return OutcomePaid

	case StatusCapture:
		// A capture is only money when the fraud engine accepted it. For
		// non-card channels Midtrans does not run one, and fraud_status arrives
		// as "accept" — so the card check is expressed as "a challenge or a deny
		// is never paid" rather than as a payment-type whitelist.
		switch n.FraudStatus {
		case FraudChallenge, FraudDeny:
			return OutcomeProgress
		default:
			return OutcomePaid
		}

	case StatusDeny, StatusExpire, StatusFailure:
		return OutcomeExpired

	case StatusCancel:
		return OutcomeCancelled

	default:
		return OutcomeProgress
	}
}

// notificationPayload is the wire shape, covering both the webhook body and the
// /v2/{order_id}/status response — they are the same document.
type notificationPayload struct {
	OrderID           string `json:"order_id"`
	TransactionID     string `json:"transaction_id"`
	TransactionStatus string `json:"transaction_status"`
	TransactionTime   string `json:"transaction_time"`
	PaymentType       string `json:"payment_type"`
	FraudStatus       string `json:"fraud_status"`
	StatusCode        string `json:"status_code"`
	SignatureKey      string `json:"signature_key"`

	// GrossAmount is a string on the wire ("150000.00") and is kept as one.
	GrossAmount string `json:"gross_amount"`

	// The account-identifying fields, which differ per channel.
	VANumbers []struct {
		Bank     string `json:"bank"`
		VANumber string `json:"va_number"`
	} `json:"va_numbers"`
	PermataVANumber string `json:"permata_va_number"`
	BillKey         string `json:"bill_key"`
	BillerCode      string `json:"biller_code"`
	Store           string `json:"store"`
	Acquirer        string `json:"acquirer"`
	Bank            string `json:"bank"`
}

// ParseNotification reads a Midtrans notification body.
//
// It does not verify anything — verification needs the server key and belongs to
// VerifySignature, which the caller runs after the payload has been written to
// the audit log. Parsing first is what lets an invalid signature still be
// recorded with the order it claimed to be for.
func ParseNotification(body []byte) (*Notification, error) {
	var p notificationPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, ErrMalformedPayload
	}
	if strings.TrimSpace(p.OrderID) == "" {
		return nil, ErrMalformedPayload
	}

	n := &Notification{
		OrderID:           p.OrderID,
		TransactionID:     p.TransactionID,
		TransactionStatus: p.TransactionStatus,
		TransactionTime:   p.TransactionTime,
		PaymentType:       p.PaymentType,
		FraudStatus:       p.FraudStatus,
		StatusCode:        p.StatusCode,
		GrossAmount:       p.GrossAmount,
		SignatureKey:      p.SignatureKey,
		Raw:               json.RawMessage(body),
	}
	n.Bank, n.VANumber = p.account()
	return n, nil
}

// account picks the bank and account number out of whichever channel-specific
// field carried them, so the payment page can tell the customer where to pay.
func (p notificationPayload) account() (bank, number string) {
	switch {
	case len(p.VANumbers) > 0:
		return p.VANumbers[0].Bank, p.VANumbers[0].VANumber
	case p.PermataVANumber != "":
		return "permata", p.PermataVANumber
	case p.BillKey != "":
		// Mandiri Bill Payment: the biller code identifies the merchant, the bill
		// key is what the customer types.
		return "mandiri", p.BillKey
	case p.Bank != "":
		return p.Bank, ""
	case p.Store != "":
		return p.Store, ""
	default:
		return p.Acquirer, ""
	}
}

// VerifySignature reports whether a notification was really sent by Midtrans.
//
// signature = SHA512(order_id + status_code + gross_amount + server_key), with
// gross_amount used EXACTLY as received (PLAN.md R6). This is the only thing
// authenticating the webhook — the endpoint is public and has to be, because
// Midtrans cannot hold a credential of ours.
//
// The comparison is constant-time. A byte-by-byte compare on a value an attacker
// can submit repeatedly is a timing oracle for the digest, and the digest is the
// only lock on the door.
func VerifySignature(n *Notification, serverKey string) bool {
	if n == nil || n.SignatureKey == "" {
		return false
	}

	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + serverKey))
	want := hex.EncodeToString(sum[:])

	// Midtrans sends lowercase hex; fold anyway rather than reject a correct
	// signature over a case change upstream.
	got := strings.ToLower(strings.TrimSpace(n.SignatureKey))

	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
