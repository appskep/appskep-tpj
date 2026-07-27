package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Pembayaran is the admin payments module.
//
// It is read-only apart from one button. Nothing here marks a booking paid:
// that is the webhook's job and only the webhook's (PLAN.md R7). The re-sync
// asks Midtrans what happened and feeds the identical state machine, so an
// operator pressing it cannot reach a conclusion the notification would not have
// reached on its own.
type Pembayaran struct {
	deps *app.Deps
}

func NewPembayaran(deps *app.Deps) *Pembayaran {
	return &Pembayaran{deps: deps}
}

const pembayaranPath = "/admin/pembayaran"

type pembayaranFilterValues struct {
	State  string
	From   string
	To     string
	Search string
}

func (f pembayaranFilterValues) query() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("status", f.State)
	set("dari", f.From)
	set("sampai", f.To)
	set("q", f.Search)
	return v
}

type pembayaranRow struct {
	Payment sqlc.ListPaymentsAdminRow
	Href    string
	// BookingHref links to the booking this payment belongs to, so an operator
	// can act on it without searching for the code.
	BookingHref string
}

type pembayaranListData struct {
	Rows       []pembayaranRow
	Filters    []filterChip
	Values     pembayaranFilterValues
	ExportHref string
	Total      int64
	Pager      pager
	Empty      emptyState
}

type pembayaranDetailData struct {
	Payment sqlc.GetPaymentAdminDetailRow
	State   string
	// Notifications is the raw webhook trail for this order — what arrived, when,
	// whether it verified, and what was decided. This is the page that makes a
	// payment dispute answerable.
	Notifications []notificationRow
	BookingHref   string
	// RawResponse is the gateway's last body, pretty-printed. Rendered as text by
	// the template, never as markup.
	RawResponse string
	// CanSync is false once the payment reached a final state: there is nothing
	// left for Midtrans to tell us.
	CanSync bool
}

type notificationRow struct {
	Notification sqlc.PaymentNotification
	Payload      string
}

// List renders the payments list.
func (h *Pembayaran) List(w http.ResponseWriter, r *http.Request) {
	values := h.filterValues(r)

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	q := h.query(values)
	q.Page = page
	q.PageSize = h.deps.Cfg.App.PageSize

	result, err := h.deps.Payment.AdminList(r.Context(), q)
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin pembayaran: listing", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	rows := make([]pembayaranRow, 0, len(result.Items))
	for _, p := range result.Items {
		rows = append(rows, pembayaranRow{
			Payment:     p,
			Href:        pembayaranPath + "/" + strconv.FormatInt(p.ID, 10),
			BookingHref: bookingPath + "/" + strconv.FormatInt(p.BookingID, 10),
		})
	}

	h.deps.View.Render(w, r, http.StatusOK, "admin/pembayaran", &view.View{
		Page: view.Page{Title: "Pembayaran"},
		Data: pembayaranListData{
			Rows:       rows,
			Filters:    h.stateChips(values),
			Values:     values,
			ExportHref: pembayaranPath + "/ekspor?" + values.query().Encode(),
			Total:      result.Total,
			Pager: pager{
				Page:       result.Page,
				TotalPages: result.TotalPages,
				BaseURL:    baseURL(pembayaranPath, values.query()),
			},
			Empty: pembayaranEmptyState(values),
		},
	})
}

// Detail renders one payment with its webhook trail.
func (h *Pembayaran) Detail(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	p, err := h.deps.Payment.AdminDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "admin pembayaran: loading", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// The trail is context, not the record itself: a payment page that 500s
	// because its audit log could not be read helps nobody.
	var trail []notificationRow
	if notes, nerr := h.deps.Payment.Notifications(r.Context(), p.OrderID); nerr != nil {
		h.deps.Log.WarnContext(r.Context(), "admin pembayaran: notifications", slog.Any("error", nerr))
	} else {
		trail = make([]notificationRow, 0, len(notes))
		for _, n := range notes {
			trail = append(trail, notificationRow{
				Notification: n,
				Payload:      prettyJSON(string(n.Payload)),
			})
		}
	}

	state := statePayment(p)

	h.deps.View.Render(w, r, http.StatusOK, "admin/pembayaran-detail", &view.View{
		Page: view.Page{Title: p.OrderID},
		Data: pembayaranDetailData{
			Payment:       p,
			State:         state,
			Notifications: trail,
			BookingHref:   bookingPath + "/" + strconv.FormatInt(p.BookingID, 10),
			RawResponse:   prettyJSON(p.RawResponse.String),
			CanSync:       state == service.PaymentStatePending,
		},
	})
}

// Sync asks Midtrans for the current status of one order and applies it.
//
// The admin half of the same method the customer's "Saya sudah bayar" button
// calls, which is the point: one state machine, reachable from both sides,
// incapable of disagreeing with itself.
func (h *Pembayaran) Sync(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}

	p, err := h.deps.Payment.AdminDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			h.deps.ErrorPage(w, r, http.StatusNotFound)
			return
		}
		h.deps.Log.ErrorContext(r.Context(), "admin pembayaran: loading for sync", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	dest := pembayaranPath + "/" + strconv.FormatInt(id, 10)

	switch err := h.deps.Payment.Sync(r.Context(), p.OrderID); {
	case err == nil:
		h.audit(r, service.ActionPaymentSync, id, map[string]any{"order_id": p.OrderID})
		h.deps.FlashRedirect(w, r, dest, model.FlashSuccess("Status pembayaran diperbarui dari Midtrans."))

	case errors.Is(err, service.ErrOrderNotFound):
		// Ordinary, not a failure: a Snap token the customer never used means
		// Midtrans has no transaction under that order id at all.
		h.deps.FlashRedirect(w, r, dest, model.FlashInfo(
			"Midtrans belum punya transaksi untuk order ini — kemungkinan pelanggan belum membuka halaman pembayaran."))

	case errors.Is(err, service.ErrNotFound):
		h.deps.ErrorPage(w, r, http.StatusNotFound)

	default:
		h.deps.Log.ErrorContext(r.Context(), "admin pembayaran: syncing",
			slog.String("order_id", p.OrderID), slog.Any("error", err))
		h.deps.FlashRedirect(w, r, dest, model.FlashError(
			"Gagal menghubungi Midtrans. Coba lagi beberapa saat lagi."))
	}
}

// Export writes the filtered payments as CSV.
func (h *Pembayaran) Export(w http.ResponseWriter, r *http.Request) {
	values := h.filterValues(r)

	rows, truncated, err := h.deps.Payment.Export(r.Context(), h.query(values))
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "admin pembayaran: exporting", slog.Any("error", err))
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	records := make([][]string, 0, len(rows)+1)
	records = append(records, []string{
		"Order ID", "Status", "Kode booking", "Status booking", "Layanan",
		"Nama", "Email", "Jumlah", "Metode", "Bank", "VA",
		"ID transaksi", "Status transaksi", "Fraud",
		"Dibuat", "Dibayar", "Kedaluwarsa", "Dibatalkan",
	})
	for _, p := range rows {
		records = append(records, []string{
			p.OrderID,
			p.State,
			p.BookingCode,
			string(p.BookingStatus),
			p.ServiceName,
			p.UserName,
			p.UserEmail,
			p.GrossAmount,
			p.PaymentType.String,
			p.Bank.String,
			p.VaNumber.String,
			p.TransactionID.String,
			p.TransactionStatus.String,
			p.FraudStatus.String,
			csvTime(p.CreatedAt),
			csvNullTime(p.PaidAt),
			csvNullTime(p.ExpiredAt),
			csvNullTime(p.CancelledAt),
		})
	}

	h.writeCSV(w, r, "pembayaran", records, truncated)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Pembayaran) filterValues(r *http.Request) pembayaranFilterValues {
	q := r.URL.Query()
	return pembayaranFilterValues{
		State:  strings.TrimSpace(q.Get("status")),
		From:   strings.TrimSpace(q.Get("dari")),
		To:     strings.TrimSpace(q.Get("sampai")),
		Search: strings.TrimSpace(q.Get("q")),
	}
}

func (h *Pembayaran) query(v pembayaranFilterValues) service.AdminPaymentQuery {
	return service.AdminPaymentQuery{
		State:  v.State,
		From:   h.deps.Schedule.ParseDate(v.From),
		To:     h.deps.Schedule.ParseDate(v.To),
		Search: v.Search,
	}
}

func (h *Pembayaran) stateChips(v pembayaranFilterValues) []filterChip {
	states := []string{
		service.PaymentStatePending,
		service.PaymentStatePaid,
		service.PaymentStateExpired,
		service.PaymentStateCancelled,
	}

	_, filtered := service.PaymentStateFilter(v.State)

	href := func(state string) string {
		q := v.query()
		if state == "" {
			q.Del("status")
		} else {
			q.Set("status", state)
		}
		q.Del("page")
		return baseURLNoTrailer(pembayaranPath, q)
	}

	out := []filterChip{{Label: "Semua", Href: href(""), Selected: !filtered}}
	for _, s := range states {
		out = append(out, filterChip{
			Label:    view.PaymentBadge(s).Label,
			Href:     href(s),
			Selected: filtered && v.State == s,
		})
	}
	return out
}

func (h *Pembayaran) audit(r *http.Request, action string, id int64, meta map[string]any) {
	var actor int64
	if u := auth.UserFrom(r.Context()); u != nil {
		actor = u.ID
	}
	h.deps.Audit.Record(r.Context(), service.Entry{
		ActorID:  actor,
		Action:   action,
		Entity:   service.EntityPayment,
		EntityID: id,
		Meta:     meta,
		IP:       clientIP(r),
	})
}

func (h *Pembayaran) idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		h.deps.ErrorPage(w, r, http.StatusNotFound)
		return 0, false
	}
	return id, true
}

// statePayment derives the lifecycle state of a detail row. The list query
// computes the identical CASE in SQL; this is the one row that arrives without
// it.
func statePayment(p sqlc.GetPaymentAdminDetailRow) string {
	switch {
	case p.PaidAt.Valid:
		return service.PaymentStatePaid
	case p.CancelledAt.Valid:
		return service.PaymentStateCancelled
	case p.ExpiredAt.Valid:
		return service.PaymentStateExpired
	}
	return service.PaymentStatePending
}

func pembayaranEmptyState(v pembayaranFilterValues) emptyState {
	if v.State != "" || v.Search != "" || v.From != "" || v.To != "" {
		return emptyState{
			Icon:        "search",
			Title:       "Tidak ada pembayaran yang cocok",
			Body:        "Coba ubah filter atau rentang tanggalnya.",
			ActionLabel: "Tampilkan semua",
			ActionHref:  pembayaranPath,
		}
	}
	return emptyState{
		Icon:  "credit-card",
		Title: "Belum ada pembayaran",
		Body:  "Pembayaran muncul di sini setelah pelanggan membuka halaman pembayaran.",
	}
}
