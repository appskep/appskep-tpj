package service

import (
	"errors"
	"sort"
	"strings"
)

// This file establishes how the service layer reports failure to a handler. It
// is the first such convention in the codebase — Phases 5, 7, 9 and 10 all have
// forms to validate, and each should copy this rather than invent its own.
//
// The distinction that matters is between a mistake the user can fix and a
// failure they cannot. A ValidationError is the former: the handler re-renders
// the form with the messages beside their inputs and a 422. Everything else is
// the latter: it is logged with its context and becomes the styled 500 page.

// ErrNotFound reports a row that does not exist.
//
// The service layer returns this instead of passing sql.ErrNoRows up, so a
// handler can tell a 404 from a 500 without importing database/sql — CLAUDE.md
// reserves that import for main.go and shared/repository.
var ErrNotFound = errors.New("service: not found")

// ErrHasBookings blocks deleting a record some booking still references.
//
// PLAN.md Phase 4 offered "soft delete or block delete when bookings exist".
// Blocking is what the schema already implements: fk_bookings_service is
// RESTRICT, and there is no deleted_at column to soft-delete into. The handler
// turns this into an offer to deactivate instead, which is the outcome the admin
// actually wants.
var ErrHasBookings = errors.New("service: still referenced by bookings")

// ErrSlotTaken reports that a slot filled up, or was deactivated, between the
// moment the page rendered it as free and the moment the booking was submitted.
//
// This is the expected outcome of the race the whole locking design exists to
// make safe, not a malfunction: the loser of two concurrent bookings on a
// capacity-1 slot gets this. It is therefore a friendly re-render of the slot
// picker — "slot baru saja terisi, pilih jadwal lain" — and never an error page.
var ErrSlotTaken = errors.New("service: slot no longer available")

// ErrDuplicateBooking reports that this user already holds a live booking for
// this slot.
//
// The check that produces it is uq_bookings_active_slot_user, via the
// active_slot_id generated column — so a double-submitted form is refused by the
// database itself even if every check in Go were removed. Distinct from
// ErrSlotTaken because the remedy is different: the user does not need another
// slot, they need to be told they already have this one.
var ErrDuplicateBooking = errors.New("service: user already booked this slot")

// ErrNotBookable reports that the layanan stopped being bookable — deactivated
// or marked coming-soon — between the page render and the submit.
//
// is_coming_soon services are listed publicly but must not be booked (PLAN.md
// Phase 4). ListBookableServices keeps them out of the picker; this is the
// re-check inside the booking transaction that makes the rule hold even when the
// admin flips the flag mid-flow.
var ErrNotBookable = errors.New("service: layanan is not bookable")

// ErrNotPayable reports a booking that cannot be paid for: it is already paid,
// or it was cancelled or expired.
//
// Distinct from ErrPaymentWindowClosed because the remedy differs — a paid
// booking needs nothing, an expired one needs booking again.
var ErrNotPayable = errors.New("service: booking is not awaiting payment")

// ErrPaymentWindowClosed reports that the booking's hold has run out, or has so
// little left that starting a payment would invite the customer to pay for a
// slot the expiry ticker is about to release.
//
// Midtrans is told to stop accepting payment at the same moment we release the
// slot (payment.Order.Expiry), so the window this guards is the one where the
// two would be too close together to be honest about.
var ErrPaymentWindowClosed = errors.New("service: payment window has closed")

// ErrBookingFinal reports a booking that no longer holds its slot — completed,
// cancelled or expired — for an action that only makes sense while it does.
//
// It covers exactly the set CancelBooking and RescheduleBooking are guarded on
// (pending_payment, paid, confirmed), so the admin cancel and the reschedule
// share one sentinel rather than two that would have to be kept in agreement.
var ErrBookingFinal = errors.New("service: booking no longer holds its slot")

// ErrLastAdmin blocks demoting or deactivating the only remaining admin.
//
// TPJ owns no credentials, so there is no password reset to get back in with:
// an empty admin list is repaired by editing the database or the ADMIN_USER_IDS
// allowlist and logging in again. Refusing is cheaper than explaining that.
var ErrLastAdmin = errors.New("service: cannot remove the last admin")

// ErrSelfDemotion blocks an admin from removing their own access. Same class of
// mistake as ErrLastAdmin, one step earlier: a single misclick would otherwise
// end the session that made it, mid-task.
var ErrSelfDemotion = errors.New("service: admins cannot demote or deactivate themselves")

// ErrOrderNotFound reports a re-sync for an order Midtrans has never seen.
//
// It happens legitimately: a payment row is written before the Snap call, so a
// failed call leaves an order ID that exists here and nowhere else.
var ErrOrderNotFound = errors.New("service: order not found at the payment gateway")

// ValidationError carries one user-facing message per rejected form field, so a
// handler can re-render the submitted form with each message beside its own
// input rather than replacing the page with a single generic complaint.
//
// Messages are in Bahasa Indonesia because they are shown to the user verbatim.
// The Error() string, which only ever reaches a log, stays in English like the
// rest of the code.
type ValidationError struct {
	// Fields maps a form field name to its message. The field names are the HTML
	// input names, so a template looks a message up with the same string the
	// input carries and the two cannot drift apart.
	Fields map[string]string
}

// NewValidationError returns an empty error ready to collect messages. Callers
// build one up across every field and check Any() once, so a user sees every
// problem at once instead of fixing them one submit at a time.
func NewValidationError() *ValidationError {
	return &ValidationError{Fields: make(map[string]string)}
}

// Add records a message for a field. The first message for a field wins, so a
// cheap check (required) can run before an expensive one (uniqueness) without
// the second overwriting the more useful first.
func (e *ValidationError) Add(field, msg string) {
	if _, exists := e.Fields[field]; !exists {
		e.Fields[field] = msg
	}
}

// Any reports whether anything was rejected.
func (e *ValidationError) Any() bool { return len(e.Fields) > 0 }

// Error names the rejected fields without repeating their messages, which are
// user-facing copy and would only add noise to a log line.
func (e *ValidationError) Error() string {
	names := make([]string, 0, len(e.Fields))
	for f := range e.Fields {
		names = append(names, f)
	}
	sort.Strings(names) // stable output, so log lines for the same failure match
	return "validation failed: " + strings.Join(names, ", ")
}
