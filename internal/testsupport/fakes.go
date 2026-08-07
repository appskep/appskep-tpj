package testsupport

import (
	"context"
	"sync"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/mail"
	"github.com/remorac/appskep-tpj/internal/shared/payment"
)

// FakeAccount stands in for the Appskep account API (auth.Account).
//
// It records every call so a test can assert the exact fields pushed, and its
// programmable errors let a test drive the rejection branch (return an
// *auth.ErrAccountRejected) or a transport failure (any other error) without a
// network. Mutex-guarded to stay honest under -race, like FakeGateway.
type FakeAccount struct {
	mu sync.Mutex

	// Profiles records every UpdateProfile argument; Tokens the bearer token each
	// call carried, so a test can assert the signed-in user's own token was used.
	Profiles []auth.AccountProfile
	Tokens   []string
	// Passwords records every SetPassword new-password argument.
	Passwords []string

	// The programmable failures, returned as-is.
	UpdateErr   error
	PasswordErr error
}

func NewFakeAccount() *FakeAccount { return &FakeAccount{} }

func (a *FakeAccount) UpdateProfile(_ context.Context, token string, in auth.AccountProfile) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Tokens = append(a.Tokens, token)
	if a.UpdateErr != nil {
		return a.UpdateErr
	}
	a.Profiles = append(a.Profiles, in)
	return nil
}

func (a *FakeAccount) SetPassword(_ context.Context, token, newPassword, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Tokens = append(a.Tokens, token)
	if a.PasswordErr != nil {
		return a.PasswordErr
	}
	a.Passwords = append(a.Passwords, newPassword)
	return nil
}

// LastProfile returns the most recent accepted UpdateProfile argument.
func (a *FakeAccount) LastProfile() (auth.AccountProfile, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.Profiles) == 0 {
		return auth.AccountProfile{}, false
	}
	return a.Profiles[len(a.Profiles)-1], true
}

// ProfileCount is how many UpdateProfile calls were accepted (UpdateErr nil).
func (a *FakeAccount) ProfileCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.Profiles)
}

// PasswordCount is how many SetPassword calls were accepted.
func (a *FakeAccount) PasswordCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.Passwords)
}

// compile-time assertion that FakeAccount satisfies the interface.
var _ auth.Account = (*FakeAccount)(nil)

// FakeGateway stands in for Midtrans.
//
// payment.Gateway's own doc comment says the interface exists "so the service
// layer can be exercised without the network", which is what this is. Every
// method is mutex-guarded because the concurrency tests drive Booking.Create and
// Payment.Start from many goroutines under -race.
type FakeGateway struct {
	mu sync.Mutex

	// Orders records every CreateTransaction argument, in order. The assertions
	// that matter live here: the tpj- prefix, the amount taken from the booking's
	// snapshot rather than the layanan's current price, and a non-zero Expiry
	// derived from the remaining hold.
	Orders []payment.Order

	// Cancelled records every Cancel argument.
	Cancelled []string

	// Statuses answers GetStatus, keyed by order id.
	Statuses map[string]*payment.Notification

	// Charge is what CreateTransaction returns when CreateErr is nil. A zero
	// RedirectURL is what the real gateway treats as a failure, so tests that
	// want a successful charge must set one.
	Charge payment.Charge

	// The programmable failures. CreateErr and CancelErr are returned as-is, so a
	// test can hand back payment.ErrOrderNotFound or payment.ErrNotCancelable and
	// check that the service swallows them.
	CreateErr error
	StatusErr error
	CancelErr error
}

func NewFakeGateway() *FakeGateway {
	return &FakeGateway{
		Statuses: make(map[string]*payment.Notification),
		Charge: payment.Charge{
			Token:       "fake-snap-token",
			RedirectURL: "https://app.sandbox.midtrans.com/snap/v2/vtweb/fake",
		},
	}
}

func (g *FakeGateway) CreateTransaction(_ context.Context, o payment.Order) (*payment.Charge, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.Orders = append(g.Orders, o)
	if g.CreateErr != nil {
		return nil, g.CreateErr
	}
	charge := g.Charge
	return &charge, nil
}

func (g *FakeGateway) GetStatus(_ context.Context, orderID string) (*payment.Notification, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.StatusErr != nil {
		return nil, g.StatusErr
	}
	n, ok := g.Statuses[orderID]
	if !ok {
		return nil, payment.ErrOrderNotFound
	}
	return n, nil
}

func (g *FakeGateway) Cancel(_ context.Context, orderID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.Cancelled = append(g.Cancelled, orderID)
	return g.CancelErr
}

// LastOrder returns the most recent CreateTransaction argument, and whether
// there was one.
func (g *FakeGateway) LastOrder() (payment.Order, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.Orders) == 0 {
		return payment.Order{}, false
	}
	return g.Orders[len(g.Orders)-1], true
}

// OrderCount is how many payments were opened.
func (g *FakeGateway) OrderCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.Orders)
}

// CancelCount is how many gateway cancellations were attempted.
func (g *FakeGateway) CancelCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.Cancelled)
}

// SetStatus programmes the answer GetStatus will give for one order, for the
// re-sync tests.
func (g *FakeGateway) SetStatus(orderID string, n *payment.Notification) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Statuses[orderID] = n
}

// RecordingSender is a mail.Sender that keeps every message.
//
// It replaces mail.NoOp rather than extending it, because "exactly one mail was
// sent" is an assertion several tests need and NoOp discards the evidence. The
// email worker sends from its own goroutine, so this is mutex-guarded too.
type RecordingSender struct {
	mu sync.Mutex

	// Sent is every delivered message, in order.
	Sent []mail.Message

	// Err, when set, fails every send. A failed send must produce a WARN and
	// nothing else — never a failed booking.
	Err error

	// Delay, when set, stalls each send. Used to prove the shutdown drain has a
	// grace period rather than blocking forever.
	Delay time.Duration
}

func NewRecordingSender() *RecordingSender { return &RecordingSender{} }

func (s *RecordingSender) Send(ctx context.Context, m mail.Message) error {
	s.mu.Lock()
	delay, err := s.Delay, s.Err
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sent = append(s.Sent, m)
	return nil
}

// Count is how many messages were delivered.
func (s *RecordingSender) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Sent)
}

// Messages returns a copy of everything delivered.
func (s *RecordingSender) Messages() []mail.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mail.Message(nil), s.Sent...)
}

// Subjects returns the subject line of every delivered message, which is the
// cheapest way to assert which mail went out.
func (s *RecordingSender) Subjects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.Sent))
	for _, m := range s.Sent {
		out = append(out, m.Subject)
	}
	return out
}

// Reset forgets everything sent so far.
func (s *RecordingSender) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Sent = nil
}
