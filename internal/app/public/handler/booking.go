package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/util"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Booking serves the customer booking flow and the payment page that follows it.
//
// The flow is one URL, not four. Its state — which layanan, which date, which
// slot — travels in the query string on the way in and as hidden fields on the
// way out, so every step is bookmarkable, the back button works, and nothing
// depends on a server-side draft that would have to be expired too.
//
// The review step is a POST to this same route without konfirmasi=1, following
// the pattern the jadwal generator and the bulk delete already settled in
// Phase 5. PLAN.md sketched a separate GET /booking/review; that would have put
// the customer's name, phone and address in a query string, and from there into
// access logs and the Referer header of every asset the page loads.
type Booking struct {
	deps *app.Deps
}

func NewBooking(deps *app.Deps) *Booking {
	return &Booking{deps: deps}
}

// bookingFormValues holds the submitted strings verbatim.
//
// Never the parsed values: a rejected form has to re-render exactly what the
// user typed, including the parts that failed to parse. Field names match the
// HTML input names, which are also the ValidationError keys.
type bookingFormValues struct {
	Layanan string
	Tanggal string
	Slot    string
	Nama    string
	Telepon string
	Alamat  string
	Catatan string
}

// bookingData is the /booking payload.
type bookingData struct {
	// Step is the furthest step the current state unlocks: 1 pick layanan,
	// 2 pick jadwal, 3 fill in data. Steps behind it stay on the page as a
	// summary the user can click back into.
	Step int

	Services []sqlc.Service
	// Service is nil until a layanan resolves, which is what keeps Step at 1.
	Service *sqlc.Service

	Dates []dateOption
	// Date is zero until one is chosen. Derived from the chosen slot when there
	// is one, so the two can never disagree.
	Date  time.Time
	Slots []slotOption
	// Slot is nil until one is chosen and still free.
	Slot *sqlc.ScheduleSlot

	Form   bookingFormValues
	Errors map[string]string

	// Review renders the confirmation panel instead of the submit button. Set only
	// by a POST that validated cleanly.
	Review bool
	// Notice is a warning above the form: the slot was taken, the layanan closed.
	// Distinct from Errors, which sit beside individual inputs.
	Notice string
	// AlreadyBooked warns before the commit that this user already holds this
	// slot. uq_bookings_active_slot_user is what actually refuses it.
	AlreadyBooked bool

	Terms      string
	ExpiryMins int

	EmptyDates    emptyState
	EmptySlots    emptyState
	EmptyServices emptyState
}

// dateOption and slotOption are one entry in each picker, with the link and the
// selected state resolved here rather than in the template.
//
// The template could neither build the query string safely nor compare two
// time.Time values for "same day" — and there is no dict helper to assemble a
// payload with, by design. A handler-side struct is the established answer
// (Phase 4's confirmDialog).
type dateOption struct {
	Date     time.Time
	Free     int64
	Href     string
	Selected bool
}

type slotOption struct {
	Slot     sqlc.ScheduleSlot
	Href     string
	Selected bool
	// Left is how many places remain. Shown only when the slot holds more than
	// one, where "2 tersisa" is information rather than noise.
	Left int32
}

// paymentData is the /booking/{code}/pembayaran payload, and the payload of the
// payment_status fragment the Turbo Frame reloads.
//
// One struct for both, because the fragment is the same template in both cases —
// the page renders it inline and the poller re-renders it alone, so the two
// cannot show different things for the same booking.
type paymentData struct {
	Booking sqlc.GetBookingDetailByCodeRow
	// Pending is whether this booking is still waiting to be paid, and so whether
	// the deadline block applies at all. Decided here rather than by comparing an
	// enum against a literal in the template.
	Pending bool
	// Expired reports that the hold has already lapsed. Taken from the clock, not
	// from the status: the ticker runs every minute, so there is a window where
	// the booking is dead but still reads as pending, and inviting a payment
	// during it would be a lie.
	Expired bool
	// Paid covers paid, confirmed and completed — every state in which the money
	// has arrived and the page should stop asking for it.
	Paid bool

	// Payment is the newest attempt, or nil when none has been started. It is what
	// lets the page name the channel and the virtual account the customer is
	// waiting to transfer to.
	Payment *sqlc.Payment

	// Poll asks the page to keep reloading the frame. True only while there is
	// something left to wait for; a terminal booking stops the polling rather
	// than having the script decide when to give up.
	Poll bool

	// FrameSrc is the frame's src, and it is set ONLY when rendering the whole
	// page. A frame that answers its own request carrying a src equal to that
	// request's URL makes Turbo throw "source URL which references itself" and
	// empty the frame — so the fragment leaves it blank and only the page points
	// the frame at /status.
	FrameSrc string

	// Notice is a message above the payment panel — a refused "bayar" or the
	// result of a manual status check.
	Notice string
}

// Form renders the booking page at whatever step the query string unlocks.
func (h *Booking) Form(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	values := bookingFormValues{
		Layanan: q.Get("layanan"),
		Tanggal: q.Get("tanggal"),
		Slot:    q.Get("slot"),
	}

	data, err := h.load(r.Context(), values)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "booking: loading form", "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// Prefill from the local profile. Name and email come from Appskep; phone and
	// address are the two fields the user owns here, and re-typing them for every
	// booking is the kind of friction that loses one.
	if u := auth.UserFrom(r.Context()); u != nil {
		data.Form.Nama = u.Name
		data.Form.Telepon = u.Phone
		data.Form.Alamat = u.Address
	}

	h.render(w, r, http.StatusOK, data)
}

// Submit handles both phases of the form: the review, and the commit.
func (h *Booking) Submit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.deps.ErrorPage(w, r, http.StatusBadRequest)
		return
	}

	user := auth.UserFrom(r.Context())
	if user == nil {
		// RequireAuth guarantees this cannot happen; refusing rather than
		// dereferencing keeps a routing mistake from becoming a panic.
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	values := bookingFormValues{
		Layanan: r.PostFormValue("layanan"),
		Tanggal: r.PostFormValue("tanggal"),
		Slot:    r.PostFormValue("slot"),
		Nama:    r.PostFormValue("nama"),
		Telepon: r.PostFormValue("telepon"),
		Alamat:  r.PostFormValue("alamat"),
		Catatan: r.PostFormValue("catatan"),
	}

	in := service.CreateInput{
		UserID:      user.ID,
		ServiceSlug: values.Layanan,
		SlotID:      values.Slot,
		Name:        values.Nama,
		Phone:       values.Telepon,
		Address:     values.Alamat,
		Notes:       values.Catatan,
	}

	// Phase two: the user has seen the review and pressed confirm.
	if r.PostFormValue("konfirmasi") == "1" {
		booking, err := h.deps.Booking.Create(r.Context(), in)
		if err != nil {
			h.formError(w, r, err, values)
			return
		}

		h.deps.Log.InfoContext(r.Context(), "booking created",
			"booking_code", booking.BookingCode,
			"user_id", user.ID,
			"slot_id", booking.SlotID,
		)

		// FlashRedirect, not SaveFlashAndRedirect: the latter replaces the whole
		// session with the flash and would sign the user out on the way to paying.
		h.deps.FlashRedirect(w, r, "/booking/"+booking.BookingCode+"/pembayaran",
			model.FlashSuccess("Booking dibuat. Selesaikan pembayaran sebelum batas waktu."))
		return
	}

	// Phase one: validate and show the review. Nothing is written, and passing
	// here is not a promise the commit will — Create re-checks everything under
	// the slot lock, where the answer can still be different.
	if _, _, err := h.deps.Booking.Validate(r.Context(), in); err != nil {
		h.formError(w, r, err, values)
		return
	}

	data, err := h.load(r.Context(), values)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "booking: loading review", "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	data.Review = true

	if data.Slot != nil {
		held, herr := h.deps.Booking.Held(r.Context(), data.Slot.ID, user.ID)
		if herr != nil {
			// Advisory only. Losing it costs a warning, not the page — and the
			// unique index still refuses the duplicate at the commit.
			h.deps.Log.ErrorContext(r.Context(), "booking: checking existing hold", "error", herr)
		} else {
			data.AlreadyBooked = held
		}
	}

	h.render(w, r, http.StatusOK, data)
}

// Payment renders the payment page for one booking.
//
// It is the pending-only action page. A booking with nothing left to pay belongs
// on konfirmasi, which describes a booking in any status — two pages rendering
// the same paid/cancelled/expired states would be two places for that copy to
// drift. The condition is the status alone, not the clock: a lapsed hold still
// has a payment panel to explain and a "periksa" button to offer until the
// ticker moves it, which it does within the minute.
func (h *Booking) Payment(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	code := chi.URLParam(r, "code")
	booking, ok := h.loadBooking(w, r, code, user)
	if !ok {
		return
	}
	if booking.Status != sqlc.BookingsStatusPendingPayment {
		http.Redirect(w, r, "/booking/"+booking.BookingCode+"/konfirmasi", http.StatusSeeOther)
		return
	}

	data, ok := h.paymentDataFor(r, booking, "")
	if !ok {
		return
	}
	h.renderPayment(w, r, http.StatusOK, data)
}

// Status answers the Turbo Frame that the payment page polls.
//
// It reads the database and nothing else — it never calls Midtrans. Payment is
// confirmed by the webhook (R7), and a poller that asked the gateway on every
// tick would put an unauthenticated-ish load on someone else's API for a fact
// that is already on its way to us.
func (h *Booking) Status(w http.ResponseWriter, r *http.Request) {
	data, ok := h.paymentState(w, r, "")
	if !ok {
		return
	}

	h.deps.View.RenderFragment(w, r, http.StatusOK, "public/pembayaran", "payment_status", &view.View{
		Page: view.Page{Title: "Status pembayaran " + data.Booking.BookingCode, NoIndex: true},
		Data: data,
	})
}

// Pay opens (or re-opens) the Midtrans order and sends the customer to it.
//
// Same URL as the GET, following the one-URL shape the booking flow settled in
// Phase 7 — and it answers with a redirect, so Turbo is happy and the form needs
// no data-turbo="false".
//
// It never marks anything paid. A Snap URL means the customer was offered a way
// to pay; only the webhook may conclude that they did (R7).
func (h *Booking) Pay(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	code := chi.URLParam(r, "code")
	booking, ok := h.loadBooking(w, r, code, user)
	if !ok {
		return
	}

	charge, err := h.deps.Payment.Start(r.Context(), booking, user)
	switch {
	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return

	case errors.Is(err, service.ErrNotPayable):
		h.paymentNotice(w, r, code, user,
			"Booking ini sudah tidak menunggu pembayaran.")
		return

	case errors.Is(err, service.ErrPaymentWindowClosed):
		h.paymentNotice(w, r, code, user,
			"Waktu pembayaran untuk booking ini sudah habis. Silakan booking ulang.")
		return

	case err != nil:
		// A Midtrans that is down or slow lands here. It is a 502-shaped problem,
		// but the customer is on a page that still works — so it re-renders with a
		// notice and the WhatsApp fallback rather than replacing it with an error.
		h.deps.Log.ErrorContext(r.Context(), "booking: starting payment",
			"code", code, "error", err)
		h.paymentNotice(w, r, code, user,
			"Gagal menghubungi layanan pembayaran. Coba lagi sebentar lagi, atau hubungi kami lewat WhatsApp.")
		return
	}

	h.deps.Log.InfoContext(r.Context(), "payment started",
		"booking_code", code, "order_id", charge.OrderID, "reused", charge.Reused)

	// 303 rather than 302: the customer arrived by POST and must land on Midtrans
	// with a GET.
	http.Redirect(w, r, charge.RedirectURL, http.StatusSeeOther)
}

// Check re-syncs one booking's payment against Midtrans, by hand.
//
// The remedy for a notification that never arrived — and, on a development
// machine Midtrans cannot call back into, the only way to finish a payment at
// all. It applies the identical state machine the webhook does, so it cannot
// reach a conclusion the webhook would not have.
func (h *Booking) Check(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	code := chi.URLParam(r, "code")
	booking, ok := h.loadBooking(w, r, code, user)
	if !ok {
		return
	}

	target := "/booking/" + booking.BookingCode + "/pembayaran"

	latest, err := h.deps.Payment.LatestForBooking(r.Context(), booking.ID)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "booking: reading latest payment", "code", code, "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	if latest == nil {
		h.deps.FlashRedirect(w, r, target,
			model.FlashInfo("Belum ada pembayaran yang dimulai untuk booking ini."))
		return
	}

	switch err := h.deps.Payment.Sync(r.Context(), latest.OrderID); {
	case errors.Is(err, service.ErrOrderNotFound):
		h.deps.FlashRedirect(w, r, target,
			model.FlashInfo("Pembayaran belum tercatat di Midtrans. Tekan Bayar sekarang untuk melanjutkan."))

	case err != nil:
		h.deps.Log.ErrorContext(r.Context(), "booking: syncing payment",
			"code", code, "order_id", latest.OrderID, "error", err)
		h.deps.FlashRedirect(w, r, target,
			model.FlashError("Gagal memeriksa status pembayaran. Coba lagi sebentar lagi."))

	default:
		h.deps.FlashRedirect(w, r, target,
			model.FlashSuccess("Status pembayaran diperbarui."))
	}
}

// loadBooking resolves {code} for the signed-in user, or writes the 404.
//
// Someone else's booking and a code that does not exist are indistinguishable —
// the rule lives in Booking.DetailForUser, and a 403 here would confirm that a
// guessed code was real.
func (h *Booking) loadBooking(
	w http.ResponseWriter,
	r *http.Request,
	code string,
	user *model.User,
) (sqlc.GetBookingDetailByCodeRow, bool) {
	booking, err := h.deps.Booking.DetailForUser(r.Context(), code, user.ID, user.IsAdmin())
	if errors.Is(err, service.ErrNotFound) {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return sqlc.GetBookingDetailByCodeRow{}, false
	}
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "booking: getting booking", "code", code, "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return sqlc.GetBookingDetailByCodeRow{}, false
	}
	return booking, true
}

// paymentState assembles everything the payment page and its frame render.
func (h *Booking) paymentState(
	w http.ResponseWriter,
	r *http.Request,
	notice string,
) (paymentData, bool) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return paymentData{}, false
	}

	booking, ok := h.loadBooking(w, r, chi.URLParam(r, "code"), user)
	if !ok {
		return paymentData{}, false
	}
	return h.paymentDataFor(r, booking, notice)
}

func (h *Booking) paymentDataFor(
	r *http.Request,
	booking sqlc.GetBookingDetailByCodeRow,
	notice string,
) (paymentData, bool) {
	data := paymentData{
		Booking: booking,
		Pending: booking.Status == sqlc.BookingsStatusPendingPayment && booking.ExpiresAt.Valid,
		Notice:  notice,
	}

	switch booking.Status {
	case sqlc.BookingsStatusPaid, sqlc.BookingsStatusConfirmed, sqlc.BookingsStatusCompleted:
		data.Paid = true
	case sqlc.BookingsStatusExpired:
		// The ticker has already been through. Without this the page falls to the
		// generic "no longer awaiting payment" copy and stops telling the customer
		// the one thing they need to know: the slot was let go, book again.
		data.Expired = true
	}
	if data.Pending {
		// Taken from the clock as well as the status, because the ticker runs on
		// an interval — there is a window in which the booking is dead and still
		// reads as pending, and inviting a payment during it would be a lie.
		data.Expired = booking.ExpiresAt.Time.Before(time.Now().In(h.deps.Cfg.App.Location))
	}

	// Keep polling only while there is an answer still coming: the booking is
	// live, its deadline has not passed, and a payment has been started. Nothing
	// started means nothing can arrive, and a terminal booking is already final.
	latest, err := h.deps.Payment.LatestForBooking(r.Context(), booking.ID)
	if err != nil {
		// Advisory. The page's job is to show the booking; losing the payment row
		// costs the channel details, not the page.
		h.deps.Log.ErrorContext(r.Context(), "booking: reading latest payment",
			"code", booking.BookingCode, "error", err)
	} else {
		data.Payment = latest
	}

	data.Poll = data.Pending && !data.Expired && data.Payment != nil
	return data, true
}

// paymentNotice re-renders the payment page with a message, at 422.
//
// A refused "Bayar sekarang" never redirects: the reason has to be on the page
// the customer is looking at, beside the booking it refers to.
func (h *Booking) paymentNotice(
	w http.ResponseWriter,
	r *http.Request,
	code string,
	user *model.User,
	notice string,
) {
	booking, ok := h.loadBooking(w, r, code, user)
	if !ok {
		return
	}
	data, ok := h.paymentDataFor(r, booking, notice)
	if !ok {
		return
	}
	h.renderPayment(w, r, http.StatusUnprocessableEntity, data)
}

func (h *Booking) renderPayment(w http.ResponseWriter, r *http.Request, status int, data paymentData) {
	// Only the full page points the frame at /status; see paymentData.FrameSrc.
	data.FrameSrc = "/booking/" + data.Booking.BookingCode + "/status"

	h.deps.View.Render(w, r, status, "public/pembayaran", &view.View{
		Page: view.Page{
			Title:       "Pembayaran " + data.Booking.BookingCode,
			Description: "Detail pembayaran booking " + data.Booking.BookingCode + ".",
			// A booking page is nobody's search result, and robots.txt already
			// disallows /booking. This is the belt to that pair of braces.
			NoIndex: true,
		},
		Data: data,
	})
}

// load resolves the form's state into everything the page renders.
//
// Shared by the GET, the review and every error re-render, so the three cannot
// show different lists for the same inputs. It also self-corrects: a layanan
// slug that no longer resolves drops the page back to step 1, and a slot that is
// no longer free drops it back to step 2, rather than rendering a choice the
// user can no longer make.
func (h *Booking) load(ctx context.Context, v bookingFormValues) (bookingData, error) {
	data := bookingData{
		Form:       v,
		Errors:     map[string]string{},
		Terms:      h.deps.Settings.String(service.KeyBookingTerms, ""),
		ExpiryMins: h.deps.Settings.Int(service.KeyPaymentExpiryMinutes, h.deps.Cfg.Midtrans.ExpiryMinutes),
		EmptyServices: emptyState{
			Icon:  "sparkles",
			Title: "Belum ada layanan yang bisa dibooking",
			Body:  "Daftar layanan sedang diperbarui. Tanya jadwal langsung lewat WhatsApp sementara ini.",
		},
		EmptyDates: emptyState{
			Icon:  "calendar-days",
			Title: "Belum ada jadwal tersedia",
			Body:  "Semua jadwal terdekat sudah terisi. Hubungi kami lewat WhatsApp untuk menanyakan jadwal tambahan.",
		},
		EmptySlots: emptyState{
			Icon:  "clock",
			Title: "Tidak ada slot di tanggal ini",
			Body:  "Pilih tanggal lain di atas.",
		},
	}

	var err error
	if data.Services, err = h.deps.Booking.BookableServices(ctx); err != nil {
		return data, err
	}

	// Step 1 — layanan.
	if slug := strings.TrimSpace(v.Layanan); slug != "" {
		svc, serr := h.deps.Booking.BookableBySlug(ctx, slug)
		switch {
		case errors.Is(serr, service.ErrNotFound):
			// Deactivated or marked coming-soon since the link was made. Fall back
			// to the picker rather than 404 — the visitor still wants to book.
			data.Form.Layanan = ""
		case serr != nil:
			return data, serr
		default:
			data.Service = &svc
		}
	}
	if data.Service == nil {
		data.Step = 1
		return data, nil
	}

	// Step 2 — jadwal.
	dates, err := h.deps.Booking.AvailableDates(ctx)
	if err != nil {
		return data, err
	}

	slotID := parseSlotID(v.Slot)

	// A chosen slot decides the date. Taking it from the slot rather than from
	// ?tanggal= means the two cannot contradict each other, however the URL was
	// assembled.
	if slotID > 0 {
		slot, gerr := h.deps.Schedule.Get(ctx, slotID)
		switch {
		case errors.Is(gerr, service.ErrNotFound):
			data.Form.Slot = ""
			slotID = 0
		case gerr != nil:
			return data, gerr
		default:
			data.Date = slot.SlotDate
		}
	}
	if data.Date.IsZero() {
		data.Date = h.deps.Schedule.ParseDate(v.Tanggal)
	}

	selectedDate := ""
	if !data.Date.IsZero() {
		selectedDate = util.DateISO(data.Date)
		data.Form.Tanggal = selectedDate
	}

	data.Dates = make([]dateOption, 0, len(dates))
	for _, d := range dates {
		iso := util.DateISO(d.Date)
		data.Dates = append(data.Dates, dateOption{
			Date:     d.Date,
			Free:     d.Free,
			Href:     bookingHref(data.Service.Slug, iso, 0),
			Selected: iso == selectedDate,
		})
	}

	if !data.Date.IsZero() {
		slots, serr := h.deps.Booking.AvailableSlots(ctx, data.Date)
		if serr != nil {
			return data, serr
		}

		// Step 3 — the slot, but only if it is still on offer. Matching against
		// the freshly loaded list is what makes a slot someone else just took fall
		// out of the form rather than survive in a hidden field.
		data.Slots = make([]slotOption, 0, len(slots))
		for _, s := range slots {
			data.Slots = append(data.Slots, slotOption{
				Slot:     s,
				Href:     bookingHref(data.Service.Slug, selectedDate, s.ID),
				Selected: s.ID == slotID,
				Left:     s.Capacity - s.BookedCount,
			})
		}

		for i := range data.Slots {
			if data.Slots[i].Selected {
				chosen := data.Slots[i].Slot
				data.Slot = &chosen
				break
			}
		}
	}
	if slotID > 0 && data.Slot == nil {
		data.Form.Slot = ""
	}

	data.Step = 2
	if data.Slot != nil {
		data.Step = 3
	}
	return data, nil
}

// formError maps a rejected submit onto a re-rendered form.
//
// Every branch re-renders with the submitted values intact; none of them
// redirects, because a redirect would throw away what the user typed. The lists
// are reloaded first, so a race that took the slot shows the picker as it is
// now rather than as it was when the page was built.
func (h *Booking) formError(w http.ResponseWriter, r *http.Request, err error, v bookingFormValues) {
	if errors.Is(err, service.ErrNotFound) {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return
	}

	var ve *service.ValidationError
	isValidation := errors.As(err, &ve)
	known := isValidation ||
		errors.Is(err, service.ErrSlotTaken) ||
		errors.Is(err, service.ErrDuplicateBooking) ||
		errors.Is(err, service.ErrNotBookable)

	if !known {
		h.deps.Log.ErrorContext(r.Context(), "booking: creating booking", "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	data, lerr := h.load(r.Context(), v)
	if lerr != nil {
		h.deps.Log.ErrorContext(r.Context(), "booking: reloading rejected form", "error", lerr)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	switch {
	case isValidation:
		data.Errors = ve.Fields

		// "layanan" and "slot" are pickers, not inputs — there is no field for a
		// message to sit beside, and choosing one drops the page back a step so
		// the message would render into a section that is no longer on screen.
		// Promoting them to the notice is what stops a rejected submit from
		// returning a 422 that says nothing at all. Found by verification, not by
		// reading the code: deactivating a slot mid-flow produced exactly that
		// silent page.
		for _, field := range []string{"slot", "layanan"} {
			if msg, ok := ve.Fields[field]; ok {
				data.Notice = msg
				break
			}
		}

	case errors.Is(err, service.ErrSlotTaken):
		// load() has already dropped the slot and rebuilt the list for its date,
		// so the user lands on a current picker with everything else preserved.
		data.Notice = "Slot itu baru saja terisi. Silakan pilih jadwal lain."

	case errors.Is(err, service.ErrDuplicateBooking):
		data.AlreadyBooked = true
		data.Notice = "Anda sudah punya booking aktif untuk jadwal ini. Cek halaman Riwayat."

	case errors.Is(err, service.ErrNotBookable):
		data.Form.Layanan = ""
		data.Service = nil
		data.Step = 1
		data.Notice = "Layanan itu sedang tidak bisa dibooking. Silakan pilih layanan lain."
	}

	h.render(w, r, http.StatusUnprocessableEntity, data)
}

func (h *Booking) render(w http.ResponseWriter, r *http.Request, status int, data bookingData) {
	title := "Booking"
	if data.Service != nil {
		title = "Booking " + data.Service.Name
	}

	h.deps.View.Render(w, r, status, "public/booking", &view.View{
		Page: view.Page{
			Title:       title,
			Description: "Pilih layanan, jadwal, dan isi data untuk booking terapi di Terapi Pemuda Jompo.",
			NoIndex:     true,
		},
		Data: data,
	})
}

// bookingHref builds a link back into this page at a given step.
//
// Through net/url rather than string concatenation: a slug is admin-supplied
// text, and while Slugify keeps today's slugs tame, a URL assembled by hand is
// the kind of thing that stops being safe long after the person who wrote it
// stopped looking.
func bookingHref(slug, dateISO string, slotID int64) string {
	q := url.Values{}
	q.Set("layanan", slug)
	if dateISO != "" {
		q.Set("tanggal", dateISO)
	}
	if slotID > 0 {
		q.Set("slot", strconv.FormatInt(slotID, 10))
	}
	return "/booking?" + q.Encode()
}

// parseSlotID reads a slot id from a query parameter or a hidden field. Anything
// unparseable is 0 — "nothing chosen" — because a hand-typed ?slot=abc is a
// visitor at step 2, not an error worth a page.
func parseSlotID(raw string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
