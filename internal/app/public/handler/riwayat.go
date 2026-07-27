package handler

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Riwayat is the customer's own booking history.
type Riwayat struct {
	deps *app.Deps
}

func NewRiwayat(deps *app.Deps) *Riwayat {
	return &Riwayat{deps: deps}
}

const riwayatPath = "/riwayat"

// pager is the pagination partial's payload. Page and TotalPages are int because
// the partial reaches them through `add`, which is func(int, int) int.
//
// Declared per package rather than shared, the same way emptyState already is:
// three fields whose only contract is with one template are cheaper to repeat
// than to couple two subsystems over.
type pager struct {
	Page       int
	TotalPages int
	BaseURL    string
}

// filterOption is one status chip.
type filterOption struct {
	Label    string
	Href     string
	Selected bool
}

// riwayatRow is one booking in the list, with every decision already made.
type riwayatRow struct {
	Booking sqlc.ListBookingsByUserRow
	Href    string
	// CanPay drives the "Bayar sekarang" shortcut. Pending, with a deadline, and
	// that deadline still ahead — the same three conditions the konfirmasi page
	// uses, because a row that offers to pay must lead to a page that accepts.
	CanPay  bool
	PayHref string
}

type riwayatData struct {
	Rows    []riwayatRow
	Filters []filterOption
	Total   int64
	Pager   pager
	Empty   emptyState
}

// List renders one page of the signed-in user's bookings.
func (h *Riwayat) List(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		// RequireAuth guarantees this cannot happen; refusing rather than
		// dereferencing keeps a routing mistake from becoming a panic.
		h.deps.ErrorPage(w, r, http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	status := q.Get("status")

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	result, err := h.deps.Booking.History(r.Context(), service.BookingHistoryQuery{
		UserID:   user.ID,
		Status:   status,
		Page:     page,
		PageSize: h.deps.Cfg.App.PageSize,
	})
	if err != nil {
		h.deps.Log.ErrorContext(r.Context(), "riwayat: listing bookings",
			"user_id", user.ID, "error", err)
		h.deps.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}

	// The service decides what the filter resolved to, so a value it treated as
	// "all" cannot leave a chip looking selected.
	_, filtered := service.BookingStatusFilter(status)
	if !filtered {
		status = ""
	}

	h.deps.View.Render(w, r, http.StatusOK, "public/riwayat", &view.View{
		Page: view.Page{
			Title:       "Riwayat booking",
			Description: "Daftar booking terapi kamu.",
			// Behind auth, and robots.txt already disallows /riwayat.
			NoIndex: true,
		},
		Data: riwayatData{
			Rows:    h.rows(result.Items),
			Filters: riwayatFilters(status),
			Total:   result.Total,
			Pager: pager{
				Page:       result.Page,
				TotalPages: result.TotalPages,
				BaseURL:    riwayatBaseURL(status),
			},
			Empty: riwayatEmpty(status != ""),
		},
	})
}

func (h *Riwayat) rows(items []sqlc.ListBookingsByUserRow) []riwayatRow {
	now := time.Now().In(h.deps.Cfg.App.Location)

	rows := make([]riwayatRow, 0, len(items))
	for _, b := range items {
		row := riwayatRow{
			Booking: b,
			Href:    "/booking/" + b.BookingCode + "/konfirmasi",
		}
		// From the clock as well as the status: the ticker runs on an interval, so
		// a booking can still read as pending for up to a minute after its hold
		// ended. Offering to pay for it would be a lie.
		if b.Status == sqlc.BookingsStatusPendingPayment &&
			b.ExpiresAt.Valid && b.ExpiresAt.Time.After(now) {
			row.CanPay = true
			row.PayHref = "/booking/" + b.BookingCode + "/pembayaran"
		}
		rows = append(rows, row)
	}
	return rows
}

// riwayatFilters builds the status chips.
//
// The labels come from statusBadge's table rather than being retyped here, so a
// chip and the badge on the row it filters to can never disagree about what a
// status is called.
func riwayatFilters(selected string) []filterOption {
	statuses := []sqlc.BookingsStatus{
		sqlc.BookingsStatusPendingPayment,
		sqlc.BookingsStatusPaid,
		sqlc.BookingsStatusConfirmed,
		sqlc.BookingsStatusCompleted,
		sqlc.BookingsStatusCancelled,
		sqlc.BookingsStatusExpired,
	}

	opts := make([]filterOption, 0, len(statuses)+1)
	opts = append(opts, filterOption{
		Label:    "Semua",
		Href:     riwayatPath,
		Selected: selected == "",
	})
	for _, s := range statuses {
		opts = append(opts, filterOption{
			Label:    view.StatusBadge(s).Label,
			Href:     riwayatPath + "?status=" + url.QueryEscape(string(s)),
			Selected: selected == string(s),
		})
	}
	return opts
}

// riwayatBaseURL builds the prefix the pagination partial appends "page=" to. It
// must carry the active filter and end in ? or &, or paging would silently drop
// it and show the user a different list than the one they were paging through.
func riwayatBaseURL(status string) string {
	if status == "" {
		return riwayatPath + "?"
	}
	return riwayatPath + "?status=" + url.QueryEscape(status) + "&"
}

// riwayatEmpty picks the copy for an empty list. "You have no bookings" and
// "none match this filter" want different words and different buttons — offering
// /layanan to someone who has ten cancelled bookings is answering a question
// they did not ask.
func riwayatEmpty(filtered bool) emptyState {
	if filtered {
		return emptyState{
			Icon:        "filter",
			Title:       "Tidak ada booking dengan status ini",
			Body:        "Coba pilih status lain untuk melihat booking kamu yang lain.",
			ActionLabel: "Lihat semua booking",
			ActionHref:  riwayatPath,
		}
	}
	return emptyState{
		Icon:        "calendar",
		Title:       "Belum ada booking",
		Body:        "Booking pertama kamu akan muncul di sini setelah dibuat.",
		ActionLabel: "Lihat layanan",
		ActionHref:  "/layanan",
	}
}
