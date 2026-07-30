package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/midtrans/midtrans-go"
	"github.com/midtrans/midtrans-go/coreapi"
	"github.com/midtrans/midtrans-go/snap"

	"github.com/remorac/appskep-tpj/internal/shared/config"
)

// ErrOrderNotFound reports that Midtrans has never heard of an order ID.
//
// Its own answer is a 404 with status_code 404, which is a perfectly ordinary
// outcome for a re-sync of an order whose Snap call failed halfway — not a
// failure worth an error page.
var ErrOrderNotFound = errors.New("payment: order not found at midtrans")

// ErrNotCancelable reports an order Midtrans refuses to cancel: already settled,
// already cancelled, or otherwise in a state its API will not move (HTTP 412).
//
// Benign for the only caller. Cancellation releases the slot in its own committed
// transaction and the gateway call is the courtesy that follows; an order that
// cannot be cancelled is either already dead or already paid, and money arriving
// for a released slot is Phase 8's logged manual-action path, not this one's.
var ErrNotCancelable = errors.New("payment: order cannot be cancelled")

// Midtrans is the Snap implementation of Gateway.
//
// It holds no client. snap.Client and coreapi.Client are value types that carry
// their transport, and this package's transport carries the caller's context —
// so each call builds its own pair. They are three struct fields wide; the cost
// is nothing next to the HTTP round trip, and the alternative is a shared client
// whose deadline belongs to whichever request created it.
type Midtrans struct {
	serverKey string
	env       midtrans.EnvironmentType
	timeout   time.Duration
	// enabledPayments scopes the Snap page to the channels TPJ accepts. It is
	// per transaction because the merchant account is shared: its account-wide
	// channel list belongs to another Appskep system and must not be touched.
	enabledPayments []snap.SnapPaymentType
}

// NewMidtrans builds the gateway from configuration.
//
// Environment selection follows the Appskep convention (PLAN.md R4): the literal
// "midtrans.Production" means production, and anything else — including a typo —
// means sandbox. Failing safe matters more here than failing loudly: the error
// case is charging real cards from a staging box.
func NewMidtrans(cfg config.MidtransConfig) *Midtrans {
	env := midtrans.Sandbox
	if cfg.IsProduction() {
		env = midtrans.Production
	}
	// SnapPaymentType is a bare string type, so a channel the pinned SDK has no
	// constant for — "other_qris", which predates v1.3.8 by years — converts
	// like any other. config validated the names; this only changes their type.
	var payments []snap.SnapPaymentType
	for _, p := range cfg.EnabledPayments {
		payments = append(payments, snap.SnapPaymentType(p))
	}
	return &Midtrans{
		serverKey:       cfg.ServerKey,
		env:             env,
		timeout:         cfg.Timeout,
		enabledPayments: payments,
	}
}

// ServerKey exposes the key the signature check needs. It is not otherwise read
// outside this package.
func (m *Midtrans) ServerKey() string { return m.serverKey }

// CreateTransaction opens a Snap transaction and returns its token and hosted
// payment URL.
func (m *Midtrans) CreateTransaction(ctx context.Context, o Order) (*Charge, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	req := m.snapRequest(o)

	client := m.snapClient(ctx)
	if o.NotificationURL != "" {
		// X-Append-Notification, not X-Override-Notification: the account-wide
		// notification URL points at another Appskep system on this shared
		// merchant account, and overriding it would silently stop their
		// payments from being confirmed.
		client.Options.SetPaymentAppendNotification(o.NotificationURL)
	}

	res, merr := client.CreateTransaction(req)
	if merr != nil {
		return nil, wrap("creating snap transaction", merr)
	}
	if res == nil {
		return nil, fmt.Errorf("payment: snap returned no response for %s", o.ID)
	}
	if res.RedirectURL == "" {
		// Midtrans answers a rejected request with 2xx and error_messages more
		// often than with a status code, so the absence of a URL is the real
		// failure signal.
		return nil, fmt.Errorf("payment: snap returned no redirect url for %s: %s",
			o.ID, strings.Join(res.ErrorMessages, "; "))
	}

	return &Charge{Token: res.Token, RedirectURL: res.RedirectURL}, nil
}

// snapRequest builds the Snap transaction body for one order.
//
// Separate from CreateTransaction so the assembled request can be marshalled and
// asserted without a network: everything that decides what the customer is shown
// lives here, and the only alternative to reading it in a test is reading it on
// Midtrans' hosted page.
func (m *Midtrans) snapRequest(o Order) *snap.Request {
	req := &snap.Request{
		TransactionDetails: midtrans.TransactionDetails{
			OrderID:  o.ID,
			GrossAmt: o.GrossAmount,
		},
		// One line item, so the Snap page names the layanan instead of showing a
		// bare total. Midtrans rejects the request when the item prices do not
		// sum to gross_amount, which is why there is exactly one and it carries
		// the same number.
		Items: &[]midtrans.ItemDetails{{
			ID:    o.ItemID,
			Name:  itemName(o.ItemName),
			Price: o.GrossAmount,
			Qty:   1,
		}},
		CustomerDetail: &midtrans.CustomerDetails{
			FName: o.CustomerName,
			Email: o.CustomerEmail,
			Phone: o.CustomerPhone,
		},
	}

	if o.FinishURL != "" {
		req.Callbacks = &snap.Callbacks{Finish: o.FinishURL}
	}
	if exp := expiryMinutes(o.Expiry); exp > 0 {
		// Whole minutes: Midtrans' smallest expiry unit. Truncating rather than
		// rounding up is deliberate — Midtrans must stop accepting payment
		// before our ticker releases the slot, never after.
		req.Expiry = &snap.ExpiryDetails{Unit: "minute", Duration: exp}
	}
	if len(m.enabledPayments) > 0 {
		// Scoping the channels is per transaction, never account-wide, because
		// the merchant account is shared. A single-entry list additionally makes
		// Snap skip its method picker and open that channel's page directly,
		// which is why a QRIS-only deployment stays a one-line env change even
		// though the default now carries the e-wallet deeplinks too.
		req.EnabledPayments = m.enabledPayments
	}

	return req
}

// GetStatus asks Midtrans what happened to an order.
//
// The response document is the same one a webhook delivers, signature included,
// so it is converted into a Notification and handed to the identical state
// machine. That is what stops a manual re-sync from becoming a second, subtly
// different, interpretation of the same statuses.
func (m *Midtrans) GetStatus(ctx context.Context, orderID string) (*Notification, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	res, merr := m.coreClient(ctx).CheckTransaction(orderID)
	if merr != nil {
		if merr.StatusCode == http.StatusNotFound || (res != nil && res.StatusCode == "404") {
			return nil, ErrOrderNotFound
		}
		return nil, wrap("checking transaction status", merr)
	}
	if res == nil {
		return nil, ErrOrderNotFound
	}
	if res.StatusCode == "404" {
		return nil, ErrOrderNotFound
	}

	n := &Notification{
		OrderID:           res.OrderID,
		TransactionID:     res.TransactionID,
		TransactionStatus: res.TransactionStatus,
		TransactionTime:   res.TransactionTime,
		PaymentType:       res.PaymentType,
		FraudStatus:       res.FraudStatus,
		StatusCode:        res.StatusCode,
		GrossAmount:       res.GrossAmount,
		SignatureKey:      res.SignatureKey,
		Bank:              res.Bank,
	}

	switch {
	case len(res.VaNumbers) > 0:
		n.Bank, n.VANumber = res.VaNumbers[0].Bank, res.VaNumbers[0].VANumber
	case res.PermataVaNumber != "":
		n.Bank, n.VANumber = "permata", res.PermataVaNumber
	case res.BillKey != "":
		n.Bank, n.VANumber = "mandiri", res.BillKey
	case n.Bank == "":
		n.Bank = res.Acquirer
	}

	// Re-encoded rather than captured: the SDK decodes into a struct and does not
	// keep the bytes. What is stored is therefore this build's view of the
	// response, which is what the payment row's raw_response should hold for a
	// re-sync — the webhook path stores the true original.
	if raw, err := json.Marshal(res); err == nil {
		n.Raw = raw
	}
	return n, nil
}

// Cancel stops Midtrans accepting payment for an order.
//
// Two of its outcomes are ordinary rather than exceptional, and both are reported
// as sentinels so the caller can swallow them without inspecting an HTTP status:
//
//   - 404 is the COMMON case. A Snap token the customer never used means no
//     transaction was ever charged, so there is nothing at Midtrans to cancel —
//     and there is also nothing that could take their money later, which is the
//     whole point of the call.
//   - 412 means the order reached a state the API will not move, most often
//     because it settled. That is the money-after-release case, and it belongs to
//     the webhook's logged manual-action path, not here.
func (m *Midtrans) Cancel(ctx context.Context, orderID string) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	res, merr := m.coreClient(ctx).CancelTransaction(orderID)
	if merr != nil {
		// The concrete pointer is checked before wrap, never an error-typed
		// variable holding a nil *midtrans.Error — the typed-nil trap wrap documents.
		switch {
		case merr.StatusCode == http.StatusNotFound || (res != nil && res.StatusCode == "404"):
			return ErrOrderNotFound
		case merr.StatusCode == http.StatusPreconditionFailed || (res != nil && res.StatusCode == "412"):
			return ErrNotCancelable
		}
		return wrap("cancelling transaction", merr)
	}
	if res == nil {
		return ErrOrderNotFound
	}

	// Midtrans answers a refused request with 2xx and a status_code in the body
	// more often than with an HTTP status, exactly as CreateTransaction does.
	switch res.StatusCode {
	case "404":
		return ErrOrderNotFound
	case "412":
		return ErrNotCancelable
	}
	return nil
}

func (m *Midtrans) snapClient(ctx context.Context) snap.Client {
	return snap.Client{
		ServerKey:  m.serverKey,
		Env:        m.env,
		HttpClient: &httpClient{ctx: ctx, do: &http.Client{Timeout: m.timeout}},
		Options:    &midtrans.ConfigOptions{},
	}
}

func (m *Midtrans) coreClient(ctx context.Context) coreapi.Client {
	return coreapi.Client{
		ServerKey:  m.serverKey,
		Env:        m.env,
		HttpClient: &httpClient{ctx: ctx, do: &http.Client{Timeout: m.timeout}},
		Options:    &midtrans.ConfigOptions{},
	}
}

// maxItemNameLen is Midtrans' limit on item_details[].name. A longer name is
// rejected outright, so a layanan renamed to something long would break checkout
// rather than merely look untidy.
const maxItemNameLen = 50

func itemName(s string) string {
	r := []rune(s)
	if len(r) <= maxItemNameLen {
		return s
	}
	return strings.TrimSpace(string(r[:maxItemNameLen-1])) + "…"
}

// expiryMinutes converts a duration to whole minutes, saturating rather than
// overflowing. Zero disables the expiry block, which the caller only wants when
// it genuinely has no deadline to impose.
func expiryMinutes(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	mins := int64(d / time.Minute)
	if mins > math.MaxInt32 {
		return math.MaxInt32
	}
	return mins
}

// wrap turns a *midtrans.Error into a plain error.
//
// The SDK returns a concrete pointer type, so `if err != nil` on an error-typed
// variable holding a nil *midtrans.Error is true — the classic typed-nil trap.
// Every call site here checks the concrete pointer and then calls this, so no
// nil pointer is ever wrapped into a non-nil error.
func wrap(what string, e *midtrans.Error) error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("payment: %s: %s (status %d)", what, e.GetMessage(), e.StatusCode)
}
