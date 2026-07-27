package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// The konfirmasi page and the cancellation that lives on it.
//
// These are methods on Booking rather than on a type of their own so they reuse
// loadBooking unchanged — the helper that makes someone else's booking a 404
// rather than a 403. A second type would mean a second copy of that rule, and a
// copy is exactly how it comes to differ.
//
// Konfirmasi is the canonical page for a booking in ANY status: riwayat links
// here, Midtrans returns the customer here, and /pembayaran redirects here as
// soon as there is nothing left to pay. That division is why this page renders no
// payment panel and starts no order.

// confirmDialog is the confirm partial's payload: {ID, Title, Body,
// ConfirmLabel, Action, CSRF}. Built in the handler because there is no dict
// helper, by design — a partial receives exactly one value whose shape is
// visible in Go.
type confirmDialog struct {
	ID           string
	Title        string
	Body         string
	ConfirmLabel string
	Action       string
	CSRF         string
}

// konfirmasiData is the /booking/{code}/konfirmasi payload.
//
// Every branch the template takes is decided here. The rule is Phase 7's: a
// template compares nothing against an enum literal and does no clock
// arithmetic, so there is one place where "can this still be paid" is defined.
type konfirmasiData struct {
	Booking sqlc.GetBookingDetailByCodeRow

	// Payment is the newest attempt, or nil when none was ever started. It names
	// the channel the customer used; its absence is ordinary, not an error.
	Payment *sqlc.Payment

	// Live reports that this booking still holds its slot — the customer is
	// expected at the clinic. Slot-holding statuses only.
	Live bool
	// Paid covers paid, confirmed and completed: the money has arrived.
	Paid bool
	// Cancelled and Expired are the two ways a booking let its slot go. They are
	// separate because the copy differs: one was the customer's decision.
	Cancelled bool
	Expired   bool

	// Payable is whether the deadline block still applies: pending, with a
	// deadline, and that deadline still ahead of us. Taken from the clock as well
	// as the status, because the ticker runs on an interval and there is a window
	// where a dead booking still reads as pending.
	Payable bool
	// Lapsed is pending_payment whose deadline has passed but which the ticker has
	// not reached yet. The slot is gone in all but name; the page must not invite
	// a payment for it.
	Lapsed bool

	PayHref      string
	CancelDialog confirmDialog
}

// Konfirmasi renders one booking, whatever state it is in.
func (h *Booking) Konfirmasi(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		// RequireAuth guarantees this cannot happen; refusing rather than
		// dereferencing keeps a routing mistake from becoming a panic.
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	booking, ok := h.loadBooking(w, r, chi.URLParam(r, "code"), user)
	if !ok {
		return
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/konfirmasi", &view.View{
		Page: view.Page{
			Title:       "Booking " + booking.BookingCode,
			Description: "Detail booking " + booking.BookingCode + ".",
			// A booking page is nobody's search result, and robots.txt already
			// disallows /booking. This is the belt to that pair of braces.
			NoIndex: true,
		},
		Data: h.konfirmasiDataFor(r, booking),
	})
}

// Cancel is the customer giving a slot back.
//
// It answers with a redirect on every outcome it can describe, so the confirm
// dialog's form needs no data-turbo="false" — the attribute is for a POST that
// replies 200, which Turbo silently discards.
func (h *Booking) Cancel(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	code := chi.URLParam(r, "code")
	target := "/booking/" + code + "/konfirmasi"

	bookingID, moved, err := h.deps.Booking.CancelForUser(r.Context(), code, user.ID)
	switch {
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return
	case errors.Is(err, service.ErrNotPayable):
		h.deps.FlashRedirect(w, r, target,
			model.FlashInfo("Booking ini sudah tidak bisa dibatalkan."))
		return
	case err != nil:
		h.deps.Log.ErrorContext(r.Context(), "booking: cancelling booking",
			"code", code, "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	if !moved {
		// The ticker or the webhook got there first, and whoever did released the
		// slot. Nothing to undo, and nothing at Midtrans worth a round trip.
		h.deps.FlashRedirect(w, r, target,
			model.FlashInfo("Booking ini sudah tidak menunggu pembayaran."))
		return
	}

	h.cancelOrder(r, bookingID, code)

	h.deps.FlashRedirect(w, r, target,
		model.FlashSuccess("Booking dibatalkan. Jadwal sudah dilepas dan bisa dipilih lagi."))
}

// cancelOrder closes any Midtrans order still open against a cancelled booking.
//
// Best-effort, and deliberately so: the cancellation transaction has already
// committed and the slot is already back, so a gateway failure must not be
// reported to the customer as a failed cancellation or retried into one. It is
// logged at WARN and the page says the booking was cancelled, because it was.
//
// context.WithoutCancel, because the request's context dies with the redirect
// the customer is already following. The gateway applies its own timeout, so
// this cannot hang.
func (h *Booking) cancelOrder(r *http.Request, bookingID int64, code string) {
	ctx := context.WithoutCancel(r.Context())

	if err := h.deps.Payment.CancelOrder(ctx, bookingID); err != nil {
		h.deps.Log.WarnContext(ctx, "booking: cancelling midtrans order after cancellation",
			"code", code, "booking_id", bookingID, "error", err)
	}
}

// konfirmasiDataFor resolves everything the page branches on.
func (h *Booking) konfirmasiDataFor(
	r *http.Request,
	booking sqlc.GetBookingDetailByCodeRow,
) konfirmasiData {
	data := konfirmasiData{Booking: booking}

	switch booking.Status {
	case sqlc.BookingsStatusPendingPayment:
		data.Live = true
	case sqlc.BookingsStatusPaid, sqlc.BookingsStatusConfirmed, sqlc.BookingsStatusCompleted:
		data.Live, data.Paid = true, true
	case sqlc.BookingsStatusCancelled:
		data.Cancelled = true
	case sqlc.BookingsStatusExpired:
		data.Expired = true
	}

	if booking.Status == sqlc.BookingsStatusPendingPayment && booking.ExpiresAt.Valid {
		if booking.ExpiresAt.Time.After(time.Now().In(h.deps.Cfg.App.Location)) {
			data.Payable = true
		} else {
			data.Lapsed = true
		}
	}

	if data.Payable {
		data.PayHref = "/booking/" + booking.BookingCode + "/pembayaran"
		data.CancelDialog = confirmDialog{
			// The confirm partial is invoked with this struct, so $ inside it is
			// this value, not the page envelope — the token comes in here.
			CSRF:         auth.MaskedTokenFrom(r.Context()),
			ID:           "cancel-booking",
			Title:        "Batalkan booking ini?",
			Body:         "Jadwal yang kamu pilih akan dilepas dan bisa diambil orang lain. Tindakan ini tidak bisa dibatalkan.",
			ConfirmLabel: "Ya, batalkan",
			Action:       "/booking/" + booking.BookingCode + "/batal",
		}
	}

	// Advisory: the page's job is to describe the booking, and losing the payment
	// row costs the channel line, not the page.
	latest, err := h.deps.Payment.LatestForBooking(r.Context(), booking.ID)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "booking: reading latest payment",
			"code", booking.BookingCode, "error", err)
	} else {
		data.Payment = latest
	}

	// The WhatsApp link is not built here: waLink + printf on .Site.WhatsApp is
	// what every other public page uses, and a second way to compose the same
	// href would only be a thing to keep in sync.
	return data
}
