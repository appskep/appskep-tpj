// Package app holds what every subsystem needs in common.
package app

import (
	"log/slog"

	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/middleware"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Deps is the set of process-wide dependencies handed to each subsystem's Routes
// function and from there to its handlers.
//
// One struct rather than positional parameters: Phases 3 to 11 each add another
// shared dependency (session store, payment gateway, email service, background
// ticker), and threading those positionally would mean editing every Routes
// signature and every handler constructor five more times. Adding a field here
// costs nothing at the call sites that do not use it.
//
// Constructed once in main and treated as read-only afterwards. Everything in it
// is safe for concurrent use.
type Deps struct {
	Cfg      *config.Config
	Store    *repository.Store
	Log      *slog.Logger
	View     *view.Renderer
	Settings *service.Settings
	// Catalog is the business logic behind the layanan module: validation, slug
	// allocation and image storage.
	Catalog *service.Catalog
	// Schedule is the business logic behind the jadwal module: slot validation,
	// the range and calendar reads, and the generator.
	Schedule *service.Schedule
	// Booking is the public booking flow — the slot-locking transaction and the
	// expiry sweep behind RunExpiryTicker.
	Booking *service.Booking
	// Payment opens Midtrans orders and applies the notifications that come back.
	// It is the only thing in the system allowed to mark a booking paid.
	Payment *service.Payment
	// Profile is the locally-owned half of a user record: phone, address and
	// avatar. Name and email belong to Appskep and are never written here.
	Profile *service.Profile
	// Users is the admin view of the local mirror: the role and is_active flags,
	// with the self-demotion and last-admin guards.
	Users *service.Users
	// Dashboard assembles the admin landing screen's aggregates.
	Dashboard *service.Dashboard
	// Audit writes the activity_logs trail behind admin actions. It never fails
	// the action it records.
	Audit *service.Audit
	// Email is what the customer is told. Handlers never call it — the dispatch
	// points are in the service layer, after the commit that made a transition
	// real. Deps carries it for RunReminderTicker and for main's worker goroutine.
	Email *service.Email
	// Auth verifies Appskep tokens and owns the local users mirror.
	Auth *auth.Service
	// Session reads and writes the signed session cookie.
	Session *auth.SessionManager
	// Limits holds the per-route rate limiters. They carry state — the buckets —
	// so they are built once in main and shared, not per request.
	Limits Limits
}

// Limits are the rate limiters applied to the routes that cost something on the
// way through. Each keeps its own buckets, so exhausting the booking budget does
// not lock a customer out of checking their payment status.
type Limits struct {
	// Booking guards the commit step, which takes a slot lock.
	Booking *middleware.Limiter
	// Payment guards both actions that call Midtrans.
	Payment *middleware.Limiter
	// Webhook guards the notification endpoint. Generous by design: Midtrans
	// retries legitimately, and a dropped notification means a customer who paid
	// and is still shown as unpaid.
	Webhook *middleware.Limiter
}

// NewLimits builds the limiters from config. maxKeys bounds each map; 10k
// distinct client addresses is far past any real traffic here and still small.
func NewLimits(cfg *config.Config) Limits {
	const maxKeys = 10_000
	return Limits{
		Booking: middleware.NewLimiter(cfg.Server.RateLimitBooking, maxKeys),
		Payment: middleware.NewLimiter(cfg.Server.RateLimitPayment, maxKeys),
		Webhook: middleware.NewLimiter(cfg.Server.RateLimitWebhook, maxKeys),
	}
}
