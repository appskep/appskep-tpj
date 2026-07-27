package handler

import (
	"context"
	"errors"
	"log/slog"
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
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Booking is the admin booking module: the operational list, one booking's
// detail page, and the five actions an operator can take on it.
//
// Two of those actions move a slot. Neither does so here — AdminCancel and
// Reschedule are service methods, because the lock order and the
// RowsAffected() == 1 rule are properties of the system rather than of this
// page. What this file owns is which error becomes which status code and which
// Indonesian sentence.
type Booking struct {
	deps *app.Deps
}

func NewBooking(deps *app.Deps) *Booking {
	return &Booking{deps: deps}
}

const bookingPath = "/admin/booking"

// bookingFilterValues is the submitted filter, kept as strings so the form
// re-renders exactly what was typed.
type bookingFilterValues struct {
	Status    string
	ServiceID string
	DateMode  string
	From      string
	To        string
	Search    string
	Sort      string
}

// query rebuilds the filter as a query string, so pagination links, the export
// link and the sort toggle all carry the active filter without any of them
// assembling it separately.
func (f bookingFilterValues) query() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("status", f.Status)
	set("layanan", f.ServiceID)
	set("tanggal", f.DateMode)
	set("dari", f.From)
	set("sampai", f.To)
	set("q", f.Search)
	set("urut", f.Sort)
	return v
}

// bookingRow is one listed booking with its link already built.
type bookingRow struct {
	Booking sqlc.ListBookingsAdminRow
	Href    string
}

// serviceOption is one entry in the layanan filter.
type serviceOption struct {
	ID       int64
	Name     string
	Selected bool
}

type bookingListData struct {
	Rows     []bookingRow
	Filters  []filterChip
	Services []serviceOption
	Values   bookingFilterValues
	// DateModes are the two chips that pick which date the range applies to.
	DateModes []filterChip
	// SortHref flips the ordering while keeping every other filter.
	SortHref    string
	OldestFirst bool
	ExportHref  string
	Total       int64
	Pager       pager
	Empty       emptyState
}

// bookingDetailData is everything the detail page renders. Every branch the
// template takes is decided here — a template compares nothing against an enum
// literal, the same rule Phase 7 settled for the public pages.
type bookingDetailData struct {
	Booking  sqlc.GetBookingAdminDetailRow
	Payments []paymentRow
	Timeline []timelineEntry

	// The four actions, each with whether it currently applies.
	CanConfirm    bool
	CanComplete   bool
	CanCancel     bool
	CanReschedule bool

	// PaidWarning is set when cancelling would release a slot the customer has
	// already paid for. There is no automated refund in v1, so the dialog has to
	// say so in the amount the customer actually paid.
	PaidWarning bool

	SlotOptions []slotOption
	Errors      map[string]string
	// NotesValue is the submitted note on a rejected save, so a 422 re-render
	// does not blank what the operator typed.
	NotesValue string
}

// paymentRow is one payment attempt on the detail page.
type paymentRow struct {
	Payment sqlc.Payment
	State   string
	Href    string
}

// timelineEntry is one activity_logs row, with its action already translated.
type timelineEntry struct {
	At     time.Time
	Label  string
	Actor  string
	Detail string
}

// slotOption is one candidate slot in the reschedule picker.
type slotOption struct {
	ID    int64
	Label string
	Free  int32
}

// List renders the operational booking list.
func (h *Booking) List(w http.ResponseWriter, r *http.Request) {
	values := h.filterValues(r)

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	q := h.query(values)
	q.Page = page
	q.PageSize = h.deps.Cfg.App.PageSize

	result, err := h.deps.Booking.AdminList(r.Context(), q)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin booking: listing", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	services, err := h.serviceOptions(r, values.ServiceID)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin booking: listing services", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	rows := make([]bookingRow, 0, len(result.Items))
	for _, b := range result.Items {
		rows = append(rows, bookingRow{
			Booking: b,
			Href:    bookingPath + "/" + strconv.FormatInt(b.ID, 10),
		})
	}

	h.deps.View.Render(w, r, http.StatusOK, "admin/booking", &view.View{
		Page: view.Page{Title: "Booking"},
		Data: bookingListData{
			Rows:        rows,
			Filters:     h.statusChips(values),
			Services:    services,
			Values:      values,
			DateModes:   h.dateModeChips(values),
			SortHref:    h.sortHref(values),
			OldestFirst: values.Sort == "lama",
			ExportHref:  bookingPath + "/ekspor?" + values.query().Encode(),
			Total:       result.Total,
			Pager: pager{
				Page:       result.Page,
				TotalPages: result.TotalPages,
				BaseURL:    baseURL(bookingPath, values.query()),
			},
			Empty: bookingEmptyState(values),
		},
	})
}

// Detail renders one booking with its payments, its timeline and its actions.
func (h *Booking) Detail(w http.ResponseWriter, r *http.Request) {
	h.renderDetail(w, r, http.StatusOK, nil, "")
}

// UpdateNotes saves the internal note.
func (h *Booking) UpdateNotes(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	notes := r.PostFormValue("catatan")

	switch err := h.deps.Booking.UpdateNotes(r.Context(), id, notes); {
	case err == nil:
		h.audit(r, service.ActionBookingNotes, id, nil)
		h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashSuccess("Catatan disimpan."))
	default:
		var ve *service.ValidationError
		switch {
		case errors.As(err, &ve):
			h.renderDetail(w, r, http.StatusUnprocessableEntity, ve.Fields, notes)
		case errors.Is(err, service.ErrNotFound):
			h.deps.ErrorPage(w, r, http.StatusNotFound)
		default:
			h.deps.Log.ErrorContext(r.Context(), "admin booking: notes", slog.Any("error", err))
			h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		}
	}
}

// Confirm and Complete move a booking forward. Neither touches a slot.
func (h *Booking) Confirm(w http.ResponseWriter, r *http.Request) {
	h.move(w, r, h.deps.Booking.Confirm, service.ActionBookingConfirm,
		"Booking dikonfirmasi.",
		"Booking tidak dalam status yang bisa dikonfirmasi. Halaman sudah diperbarui.")
}

func (h *Booking) Complete(w http.ResponseWriter, r *http.Request) {
	h.move(w, r, h.deps.Booking.Complete, service.ActionBookingComplete,
		"Booking ditandai selesai.",
		"Booking tidak dalam status yang bisa diselesaikan. Halaman sudah diperbarui.")
}

func (h *Booking) move(
	w http.ResponseWriter,
	r *http.Request,
	apply func(ctx context.Context, id int64) (bool, error),
	action, okMsg, noopMsg string,
) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	moved, err := apply(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "admin booking: "+action, slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// A no-op is information, not a failure: the operator's page was stale, or
	// the button was pressed twice. Saying so beats an error page for something
	// that is already true.
	if !moved {
		h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashInfo(noopMsg))
		return
	}

	h.audit(r, action, id, nil)
	h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashSuccess(okMsg))
}

// Cancel releases a booking's slot and records why.
//
// The Midtrans order is closed afterwards, outside the transaction and only for
// a booking that was still awaiting payment — the one case where a Snap page the
// customer left open could still take money for a slot that has just been given
// back. A booking that was already paid has a settled order: Midtrans answers
// 412 and there is nothing to close.
func (h *Booking) Cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	reason := r.PostFormValue("alasan")

	was, moved, err := h.deps.Booking.AdminCancel(r.Context(), id, reason)
	if err != nil {
		var ve *service.ValidationError
		switch {
		case errors.As(err, &ve):
			h.renderDetail(w, r, http.StatusUnprocessableEntity, ve.Fields, "")
		case errors.Is(err, service.ErrNotFound):
			h.deps.ErrorPage(w, r, http.StatusNotFound)
		case errors.Is(err, service.ErrBookingFinal):
			h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashInfo(
				"Booking sudah tidak aktif, jadi tidak ada yang dibatalkan."))
		default:
			h.deps.Log.ErrorContext(r.Context(), "admin booking: cancelling", slog.Any("error", err))
			h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		}
		return
	}

	if !moved {
		// The ticker or the webhook got there first and already released the slot.
		h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashInfo(
			"Booking sudah dibatalkan sebelumnya. Tidak ada perubahan."))
		return
	}

	h.audit(r, service.ActionBookingCancel, id, map[string]any{
		"from":   string(was),
		"reason": strings.TrimSpace(reason),
	})

	if was == sqlc.BookingsStatusPendingPayment {
		h.cancelOrder(r, id)
	}

	msg := "Booking dibatalkan dan slot dikembalikan."
	if was != sqlc.BookingsStatusPendingPayment {
		// Say it plainly: the money is real and this system will not move it.
		msg = "Booking dibatalkan dan slot dikembalikan. Pembayaran yang sudah masuk harus dikembalikan secara manual."
	}
	h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashSuccess(msg))
}

// Reschedule moves a booking to another slot.
func (h *Booking) Reschedule(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	slotID, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("slot")), 10, 64)
	if slotID < 1 {
		h.renderDetail(w, r, http.StatusUnprocessableEntity,
			map[string]string{"slot": "Pilih jadwal tujuan terlebih dahulu."}, "")
		return
	}

	err := h.deps.Booking.Reschedule(r.Context(), id, slotID)
	if err != nil {
		var ve *service.ValidationError
		switch {
		case errors.As(err, &ve):
			h.renderDetail(w, r, http.StatusUnprocessableEntity, ve.Fields, "")
		case errors.Is(err, service.ErrNotFound):
			h.deps.ErrorPage(w, r, http.StatusNotFound)
		case errors.Is(err, service.ErrSlotTaken):
			h.renderDetail(w, r, http.StatusUnprocessableEntity,
				map[string]string{"slot": "Jadwal tujuan baru saja terisi. Pilih jadwal lain."}, "")
		case errors.Is(err, service.ErrDuplicateBooking):
			h.renderDetail(w, r, http.StatusUnprocessableEntity,
				map[string]string{"slot": "Pelanggan ini sudah punya booking aktif di jadwal tersebut."}, "")
		case errors.Is(err, service.ErrBookingFinal):
			h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashWarning(
				"Booking sudah tidak aktif, jadi jadwalnya tidak bisa dipindahkan."))
		default:
			h.deps.Log.ErrorContext(r.Context(), "admin booking: rescheduling", slog.Any("error", err))
			h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		}
		return
	}

	h.audit(r, service.ActionBookingReschedule, id, map[string]any{"to_slot": slotID})
	h.deps.FlashRedirect(w, r, h.detailPath(id), model.FlashSuccess("Jadwal booking dipindahkan."))
}

// Export writes the filtered bookings as CSV.
func (h *Booking) Export(w http.ResponseWriter, r *http.Request) {
	values := h.filterValues(r)

	rows, truncated, err := h.deps.Booking.Export(r.Context(), h.query(values))
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin booking: exporting", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	records := make([][]string, 0, len(rows)+1)
	records = append(records, []string{
		"Kode", "Status", "Layanan", "Tanggal", "Mulai", "Selesai",
		"Nama", "Telepon", "Alamat", "Catatan", "Harga",
		"Akun", "Email", "Dibuat", "Dikonfirmasi", "Selesai", "Dibatalkan", "Alasan",
	})
	for _, b := range rows {
		records = append(records, []string{
			b.BookingCode,
			string(b.Status),
			b.ServiceName,
			csvDate(b.SlotDate),
			b.SlotStartTime,
			b.SlotEndTime,
			b.CustomerName,
			b.CustomerPhone,
			b.CustomerAddress.String,
			b.Notes.String,
			b.PriceAmount,
			b.UserName,
			b.UserEmail,
			csvTime(b.CreatedAt),
			csvNullTime(b.ConfirmedAt),
			csvNullTime(b.CompletedAt),
			csvNullTime(b.CancelledAt),
			b.CancelledReason.String,
		})
	}

	h.writeCSV(w, r, "booking", records, truncated)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Booking) filterValues(r *http.Request) bookingFilterValues {
	q := r.URL.Query()

	mode := strings.TrimSpace(q.Get("tanggal"))
	if mode != service.DateModeCreated {
		mode = service.DateModeSlot
	}

	sort := strings.TrimSpace(q.Get("urut"))
	if sort != "lama" {
		sort = ""
	}

	return bookingFilterValues{
		Status:    strings.TrimSpace(q.Get("status")),
		ServiceID: strings.TrimSpace(q.Get("layanan")),
		DateMode:  mode,
		From:      strings.TrimSpace(q.Get("dari")),
		To:        strings.TrimSpace(q.Get("sampai")),
		Search:    strings.TrimSpace(q.Get("q")),
		Sort:      sort,
	}
}

func (h *Booking) query(v bookingFilterValues) service.AdminBookingQuery {
	serviceID, _ := strconv.ParseInt(v.ServiceID, 10, 64)
	if serviceID < 0 {
		serviceID = 0
	}

	return service.AdminBookingQuery{
		Status:      v.Status,
		ServiceID:   serviceID,
		DateMode:    v.DateMode,
		From:        h.deps.Schedule.ParseDate(v.From),
		To:          h.deps.Schedule.ParseDate(v.To),
		Search:      v.Search,
		OldestFirst: v.Sort == "lama",
	}
}

// statusChips builds the filter row. Labels come from view.StatusBadge, never
// retyped here, so a chip and the badge it filters to cannot call one status two
// different things.
func (h *Booking) statusChips(v bookingFilterValues) []filterChip {
	statuses := []sqlc.BookingsStatus{
		sqlc.BookingsStatusPendingPayment,
		sqlc.BookingsStatusPaid,
		sqlc.BookingsStatusConfirmed,
		sqlc.BookingsStatusCompleted,
		sqlc.BookingsStatusCancelled,
		sqlc.BookingsStatusExpired,
	}

	_, filtered := service.BookingStatusFilter(v.Status)

	href := func(status string) string {
		q := v.query()
		if status == "" {
			q.Del("status")
		} else {
			q.Set("status", status)
		}
		q.Del("page")
		return baseURLNoTrailer(bookingPath, q)
	}

	out := []filterChip{{Label: "Semua", Href: href(""), Selected: !filtered}}
	for _, s := range statuses {
		out = append(out, filterChip{
			Label:    view.StatusBadge(s).Label,
			Href:     href(string(s)),
			Selected: filtered && v.Status == string(s),
		})
	}
	return out
}

func (h *Booking) dateModeChips(v bookingFilterValues) []filterChip {
	href := func(mode string) string {
		q := v.query()
		q.Set("tanggal", mode)
		q.Del("page")
		return baseURLNoTrailer(bookingPath, q)
	}
	return []filterChip{
		{Label: "Tanggal jadwal", Href: href(service.DateModeSlot), Selected: v.DateMode != service.DateModeCreated},
		{Label: "Tanggal dibuat", Href: href(service.DateModeCreated), Selected: v.DateMode == service.DateModeCreated},
	}
}

func (h *Booking) sortHref(v bookingFilterValues) string {
	q := v.query()
	if v.Sort == "lama" {
		q.Del("urut")
	} else {
		q.Set("urut", "lama")
	}
	q.Del("page")
	return baseURLNoTrailer(bookingPath, q)
}

// serviceOptions lists every layanan for the filter, including inactive ones:
// past bookings reference them, and a filter that cannot name them would hide
// its own rows.
func (h *Booking) serviceOptions(r *http.Request, selected string) ([]serviceOption, error) {
	result, err := h.deps.Catalog.List(r.Context(), service.ListQuery{Page: 1, PageSize: 200})
	if err != nil {
		return nil, err
	}

	out := make([]serviceOption, 0, len(result.Items))
	for _, s := range result.Items {
		out = append(out, serviceOption{
			ID:       s.ID,
			Name:     s.Name,
			Selected: selected == strconv.FormatInt(s.ID, 10),
		})
	}
	return out, nil
}

// renderDetail loads and renders one booking. It is the single render path for
// the detail page, so a 422 from any of the four forms comes back with the same
// page fully populated rather than a stripped-down error view.
func (h *Booking) renderDetail(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	fieldErrors map[string]string,
	notesValue string,
) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	booking, err := h.deps.Booking.AdminDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "admin booking: loading", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	payments, err := h.deps.Payment.ForBooking(r.Context(), id)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin booking: payments", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	rows := make([]paymentRow, 0, len(payments))
	for _, p := range payments {
		rows = append(rows, paymentRow{
			Payment: p,
			State:   service.PaymentState(p),
			Href:    "/admin/pembayaran/" + strconv.FormatInt(p.ID, 10),
		})
	}

	// The timeline is decoration: a booking whose audit trail cannot be read is
	// still a booking an operator has to act on.
	var timeline []timelineEntry
	if logs, terr := h.deps.Booking.Timeline(r.Context(), id, 50); terr != nil {
		h.deps.Log.WarnContext(r.Context(), "admin booking: timeline", slog.Any("error", terr))
	} else {
		timeline = buildTimeline(logs)
	}

	live := booking.Status == sqlc.BookingsStatusPendingPayment ||
		booking.Status == sqlc.BookingsStatusPaid ||
		booking.Status == sqlc.BookingsStatusConfirmed

	var options []slotOption
	if live {
		slots, serr := h.deps.Booking.RescheduleOptions(r.Context())
		if serr != nil {
			h.deps.Log.WarnContext(r.Context(), "admin booking: reschedule options", slog.Any("error", serr))
		} else {
			options = slotOptions(slots, booking.SlotID)
		}
	}

	if notesValue == "" {
		notesValue = booking.Notes.String
	}

	h.deps.View.Render(w, r, status, "admin/booking-detail", &view.View{
		Page: view.Page{Title: booking.BookingCode},
		Data: bookingDetailData{
			Booking:       booking,
			Payments:      rows,
			Timeline:      timeline,
			CanConfirm:    booking.Status == sqlc.BookingsStatusPaid,
			CanComplete:   booking.Status == sqlc.BookingsStatusPaid || booking.Status == sqlc.BookingsStatusConfirmed,
			CanCancel:     live,
			CanReschedule: live,
			PaidWarning:   booking.Status != sqlc.BookingsStatusPendingPayment && live,
			SlotOptions:   options,
			Errors:        orEmpty(fieldErrors),
			NotesValue:    notesValue,
		},
	})
}

// cancelOrder closes a still-open Midtrans order after the cancellation has
// committed.
//
// Best-effort by design, exactly as Phase 9's customer cancellation: the slot is
// already released, so a gateway failure must not be reported as a failed
// cancellation. context.WithoutCancel because the operator is already following
// the redirect.
func (h *Booking) cancelOrder(r *http.Request, bookingID int64) {
	ctx := withoutCancel(r.Context())
	if err := h.deps.Payment.CancelOrder(ctx, bookingID); err != nil {
		h.deps.Log.WarnContext(ctx, "admin booking: cancelling gateway order",
			slog.Int64("booking_id", bookingID), slog.Any("error", err))
	}
}

func (h *Booking) audit(r *http.Request, action string, id int64, meta map[string]any) {
	var actor int64
	if u := auth.UserFrom(r.Context()); u != nil {
		actor = u.ID
	}
	h.deps.Audit.Record(r.Context(), service.Entry{
		ActorID:  actor,
		Action:   action,
		Entity:   service.EntityBooking,
		EntityID: id,
		Meta:     meta,
		IP:       clientIP(r),
	})
}

func (h *Booking) detailPath(id int64) string {
	return bookingPath + "/" + strconv.FormatInt(id, 10)
}

func (h *Booking) idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return 0, false
	}
	return id, true
}

func bookingEmptyState(v bookingFilterValues) emptyState {
	filtered := v.Status != "" || v.Search != "" || v.From != "" || v.To != "" || v.ServiceID != ""
	if filtered {
		return emptyState{
			Icon:        "search",
			Title:       "Tidak ada booking yang cocok",
			Body:        "Coba ubah filter atau rentang tanggalnya.",
			ActionLabel: "Tampilkan semua",
			ActionHref:  bookingPath,
		}
	}
	return emptyState{
		Icon:  "receipt",
		Title: "Belum ada booking",
		Body:  "Booking dari situs publik akan muncul di sini.",
	}
}
