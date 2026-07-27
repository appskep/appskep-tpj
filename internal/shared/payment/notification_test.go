package payment_test

import (
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/payment"
)

// sign computes the digest Midtrans sends, independently of VerifySignature's own
// arithmetic — the point of a signature test is that both sides agree, and a
// helper that called the function under test would agree with itself.
func sign(orderID, statusCode, grossAmount, serverKey string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + serverKey))
	return hex.EncodeToString(sum[:])
}

const testKey = "SB-Mid-server-testing-only"

func TestVerifySignature(t *testing.T) {
	const (
		orderID = "tpj-4f0a1e9c-8b2d-4a77-9f1e-2c3d4e5f6a7b"
		status  = "200"
		// As Midtrans sends it: two decimal places, as a string. PLAN.md R6 requires
		// this value be used EXACTLY as received.
		amount = "150000.00"
	)
	good := sign(orderID, status, amount, testKey)

	valid := func() *payment.Notification {
		return &payment.Notification{
			OrderID:      orderID,
			StatusCode:   status,
			GrossAmount:  amount,
			SignatureKey: good,
		}
	}

	t.Run("a genuine notification verifies", func(t *testing.T) {
		if !payment.VerifySignature(valid(), testKey) {
			t.Error("VerifySignature rejected a correctly signed notification")
		}
	})

	t.Run("uppercase hex still verifies", func(t *testing.T) {
		n := valid()
		n.SignatureKey = strings.ToUpper(good)
		if !payment.VerifySignature(n, testKey) {
			t.Error("a correct signature was rejected over a case change upstream")
		}
	})

	t.Run("surrounding whitespace still verifies", func(t *testing.T) {
		n := valid()
		n.SignatureKey = "  " + good + "\n"
		if !payment.VerifySignature(n, testKey) {
			t.Error("a correct signature was rejected over surrounding whitespace")
		}
	})

	// The attack this exists to stop: raising the amount on a notification whose
	// signature was captured from a smaller, genuine one.
	t.Run("a tampered amount is refused", func(t *testing.T) {
		n := valid()
		n.GrossAmount = "1500000.00"
		if payment.VerifySignature(n, testKey) {
			t.Error("VerifySignature accepted a notification whose gross_amount was changed")
		}
	})

	// Numerically equal, textually different. The digest is over the string, so
	// normalising the amount anywhere on this path breaks every signature — which
	// is why Notification.GrossAmount is documented as the exact received value.
	t.Run("an equal but rewritten amount is refused", func(t *testing.T) {
		n := valid()
		n.GrossAmount = "150000"
		if payment.VerifySignature(n, testKey) {
			t.Error("VerifySignature accepted a re-formatted gross_amount — " +
				"the digest is over the string, not the number")
		}
	})

	t.Run("a tampered order id is refused", func(t *testing.T) {
		n := valid()
		n.OrderID = "tpj-00000000-0000-0000-0000-000000000000"
		if payment.VerifySignature(n, testKey) {
			t.Error("VerifySignature accepted a notification whose order_id was changed")
		}
	})

	t.Run("a tampered status code is refused", func(t *testing.T) {
		n := valid()
		n.StatusCode = "201"
		if payment.VerifySignature(n, testKey) {
			t.Error("VerifySignature accepted a notification whose status_code was changed")
		}
	})

	// A misconfigured MIDTRANS_SERVER_KEY must be visible at both ends rather than
	// silently swallowing every payment: this is the one webhook outcome that
	// answers 401.
	t.Run("the wrong server key is refused", func(t *testing.T) {
		if payment.VerifySignature(valid(), "SB-Mid-server-some-other-key") {
			t.Error("VerifySignature accepted a signature made with a different key")
		}
	})

	t.Run("an empty server key is refused", func(t *testing.T) {
		if payment.VerifySignature(valid(), "") {
			t.Error("VerifySignature accepted a signature against an empty server key")
		}
	})

	t.Run("a missing signature is refused", func(t *testing.T) {
		n := valid()
		n.SignatureKey = ""
		if payment.VerifySignature(n, testKey) {
			t.Error("VerifySignature accepted a notification carrying no signature")
		}
	})

	t.Run("a nil notification is refused", func(t *testing.T) {
		if payment.VerifySignature(nil, testKey) {
			t.Error("VerifySignature accepted nil")
		}
	})

	t.Run("a truncated signature is refused", func(t *testing.T) {
		n := valid()
		n.SignatureKey = good[:len(good)-1]
		if payment.VerifySignature(n, testKey) {
			t.Error("VerifySignature accepted a truncated digest")
		}
	})
}

func TestParseNotification(t *testing.T) {
	t.Run("a full settlement body", func(t *testing.T) {
		body := []byte(`{
			"order_id": "tpj-abc",
			"transaction_id": "txn-1",
			"transaction_status": "settlement",
			"transaction_time": "2026-07-27 10:00:00",
			"payment_type": "bank_transfer",
			"fraud_status": "accept",
			"status_code": "200",
			"gross_amount": "150000.00",
			"signature_key": "deadbeef"
		}`)

		n, err := payment.ParseNotification(body)
		if err != nil {
			t.Fatalf("ParseNotification: %v", err)
		}
		if n.OrderID != "tpj-abc" || n.TransactionStatus != "settlement" {
			t.Errorf("parsed %+v, want order tpj-abc / settlement", n)
		}
		if n.GrossAmount != "150000.00" {
			t.Errorf("GrossAmount = %q, want the exact string received", n.GrossAmount)
		}
		// Raw is stored verbatim on the payment row and in the audit log, because
		// Midtrans sends fields this struct does not model.
		if string(n.Raw) != string(body) {
			t.Error("Raw does not carry the body verbatim")
		}
	})

	t.Run("bad JSON", func(t *testing.T) {
		_, err := payment.ParseNotification([]byte("not json at all"))
		if !errors.Is(err, payment.ErrMalformedPayload) {
			t.Fatalf("error = %v, want ErrMalformedPayload", err)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		_, err := payment.ParseNotification(nil)
		if !errors.Is(err, payment.ErrMalformedPayload) {
			t.Fatalf("error = %v, want ErrMalformedPayload", err)
		}
	})

	// Valid JSON with nothing to act on. It is still written to the audit log and
	// still answered 200, because a body we will never understand must not be
	// retried by Midtrans for hours.
	t.Run("missing order_id", func(t *testing.T) {
		_, err := payment.ParseNotification([]byte(`{"transaction_status":"settlement"}`))
		if !errors.Is(err, payment.ErrMalformedPayload) {
			t.Fatalf("error = %v, want ErrMalformedPayload", err)
		}
	})

	t.Run("blank order_id", func(t *testing.T) {
		_, err := payment.ParseNotification([]byte(`{"order_id":"   "}`))
		if !errors.Is(err, payment.ErrMalformedPayload) {
			t.Fatalf("error = %v, want ErrMalformedPayload", err)
		}
	})
}

// TestParseNotificationAccountLadder covers the channel-specific fields the
// payment page tells the customer where to pay with. The precedence is
// va_numbers, then permata, then a Mandiri bill key, then bank, store, acquirer.
func TestParseNotificationAccountLadder(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		bank, acct string
	}{
		{
			name: "virtual account",
			body: `{"order_id":"tpj-a","va_numbers":[{"bank":"bca","va_number":"12345678"}]}`,
			bank: "bca", acct: "12345678",
		},
		{
			name: "permata has its own field",
			body: `{"order_id":"tpj-a","permata_va_number":"87654321"}`,
			bank: "permata", acct: "87654321",
		},
		{
			// The biller code identifies the merchant; the bill key is what the
			// customer types.
			name: "mandiri bill payment",
			body: `{"order_id":"tpj-a","bill_key":"99887766","biller_code":"70012"}`,
			bank: "mandiri", acct: "99887766",
		},
		{
			name: "bank without an account number",
			body: `{"order_id":"tpj-a","bank":"bni"}`,
			bank: "bni", acct: "",
		},
		{
			name: "convenience store",
			body: `{"order_id":"tpj-a","store":"indomaret"}`,
			bank: "indomaret", acct: "",
		},
		{
			name: "acquirer is the last resort",
			body: `{"order_id":"tpj-a","acquirer":"gopay"}`,
			bank: "gopay", acct: "",
		},
		{
			name: "nothing identifying at all",
			body: `{"order_id":"tpj-a"}`,
			bank: "", acct: "",
		},
		{
			name: "va_numbers wins over a bank field",
			body: `{"order_id":"tpj-a","bank":"bni","va_numbers":[{"bank":"bca","va_number":"1"}]}`,
			bank: "bca", acct: "1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := payment.ParseNotification([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseNotification: %v", err)
			}
			if n.Bank != tc.bank || n.VANumber != tc.acct {
				t.Errorf("bank/account = %q/%q, want %q/%q", n.Bank, n.VANumber, tc.bank, tc.acct)
			}
		})
	}
}

// TestOutcome is the state machine's classifier, and the table is exhaustive over
// every transaction_status Midtrans documents, crossed with every fraud_status
// where it matters.
//
// The default is OutcomeProgress rather than an error, deliberately: Midtrans can
// introduce a status this build has never heard of, and the safe answer to an
// unrecognised one is to record it and touch nothing. Guessing would mean either
// releasing a slot someone paid for or holding one nobody did.
func TestOutcome(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		fraud       string
		paymentType string
		want        payment.Outcome
		why         string
	}{
		{
			name: "settlement is money", status: payment.StatusSettlement,
			want: payment.OutcomePaid,
			why:  "the only outcome that may mark a booking paid",
		},
		{
			name: "capture accepted is money", status: payment.StatusCapture, fraud: payment.FraudAccept,
			paymentType: payment.PaymentTypeCreditCard,
			want:        payment.OutcomePaid,
		},
		{
			// A challenge is a human decision, never an automatic acceptance.
			name: "capture challenged is not money yet", status: payment.StatusCapture,
			fraud: payment.FraudChallenge, paymentType: payment.PaymentTypeCreditCard,
			want: payment.OutcomeProgress,
			why:  "a challenge is reviewed by a person",
		},
		{
			name: "capture denied is not money", status: payment.StatusCapture,
			fraud: payment.FraudDeny, paymentType: payment.PaymentTypeCreditCard,
			want: payment.OutcomeProgress,
		},
		{
			// Non-card channels get no fraud engine, and Midtrans sends "accept".
			// The rule is expressed as "a challenge or a deny is never paid" rather
			// than as a payment-type whitelist, so this still counts.
			name: "capture on a non-card channel", status: payment.StatusCapture,
			fraud: payment.FraudAccept, paymentType: "bank_transfer",
			want: payment.OutcomePaid,
		},
		{
			name: "capture with no fraud status at all", status: payment.StatusCapture,
			want: payment.OutcomePaid,
		},

		{name: "deny releases", status: payment.StatusDeny, want: payment.OutcomeExpired},
		{name: "expire releases", status: payment.StatusExpire, want: payment.OutcomeExpired},
		{name: "failure releases", status: payment.StatusFailure, want: payment.OutcomeExpired},
		{name: "cancel releases", status: payment.StatusCancel, want: payment.OutcomeCancelled},

		{
			name: "pending is progress", status: payment.StatusPending,
			want: payment.OutcomeProgress,
		},
		{
			// A refund reverses money already taken for a booking that may already
			// have been delivered. Unbooking the slot automatically would be a guess
			// about an event that always has a human behind it.
			name: "refund does not unbook", status: payment.StatusRefund,
			want: payment.OutcomeProgress,
			why:  "PLAN.md Q6: no automated refund in v1",
		},
		{
			name: "partial refund does not unbook", status: payment.StatusPartial,
			want: payment.OutcomeProgress,
		},
		{
			name: "a status this build has never seen", status: "some_future_status",
			want: payment.OutcomeProgress,
			why:  "record it and touch nothing",
		},
		{name: "empty status", status: "", want: payment.OutcomeProgress},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &payment.Notification{
				TransactionStatus: tc.status,
				FraudStatus:       tc.fraud,
				PaymentType:       tc.paymentType,
			}
			if got := n.Outcome(); got != tc.want {
				t.Errorf("Outcome(%q/%q) = %v, want %v (%s)",
					tc.status, tc.fraud, got, tc.want, tc.why)
			}
		})
	}
}
