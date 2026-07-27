// Package handler holds the admin panel handlers.
package handler

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// Dashboard serves the admin landing screen.
type Dashboard struct {
	deps *app.Deps
}

func NewDashboard(deps *app.Deps) *Dashboard {
	return &Dashboard{deps: deps}
}

// statusTile is one count in a breakdown, with its label, colour and the list
// link that shows exactly those rows.
//
// The label and class come from view.StatusBadge rather than being retyped here,
// so a tile and the badge on the row it links to cannot disagree.
type statusTile struct {
	Label string
	Class string
	Total int64
	Href  string
}

// daySheetRow is one booking on today's list.
type daySheetRow struct {
	Booking sqlc.ListBookingsBySlotDateRow
	Href    string
}

type dashboardData struct {
	Today time.Time

	DaySheet      []daySheetRow
	DaySheetTotal int64

	TodayTiles    []statusTile
	UpcomingTiles []statusTile
	MonthTiles    []statusTile

	MonthRevenue string
	MonthPaid    int64
	MonthLabel   time.Time

	RecentPayments []recentPaymentRow

	// Pending is the number that usually needs acting on: bookings holding a slot
	// with the money not yet in.
	Pending     int64
	PendingHref string

	TodayHref    string
	UpcomingHref string
}

type recentPaymentRow struct {
	Payment sqlc.ListRecentPaidPaymentsRow
	Href    string
}

// Get renders the dashboard.
//
// Every figure comes from service.Dashboard, which logs and drops a card rather
// than failing: a panel with one section missing is more useful at the start of
// a shift than a 500 page.
func (h *Dashboard) Get(w http.ResponseWriter, r *http.Request) {
	d := h.deps.Dashboard.Load(r.Context())

	sheet := make([]daySheetRow, 0, len(d.TodayBookings))
	for _, b := range d.TodayBookings {
		sheet = append(sheet, daySheetRow{
			Booking: b,
			Href:    bookingPath + "/" + strconv.FormatInt(b.ID, 10),
		})
	}

	recent := make([]recentPaymentRow, 0, len(d.RecentPayments))
	for _, p := range d.RecentPayments {
		recent = append(recent, recentPaymentRow{
			Payment: p,
			Href:    pembayaranPath + "/" + strconv.FormatInt(p.ID, 10),
		})
	}

	today := d.Today
	week := today.AddDate(0, 0, service.UpcomingDays)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
	monthEnd := monthStart.AddDate(0, 1, -1)

	h.deps.View.Render(w, r, http.StatusOK, "admin/dashboard", &view.View{
		Page: view.Page{Title: "Dasbor", NoIndex: true},
		Data: dashboardData{
			Today:         today,
			DaySheet:      sheet,
			DaySheetTotal: int64(len(sheet)),

			TodayTiles:    tiles(d.TodayStatuses, today, today),
			UpcomingTiles: tiles(d.UpcomingStatuses, today, week),
			MonthTiles:    tiles(d.MonthStatuses, monthStart, monthEnd),

			MonthRevenue: d.MonthRevenue,
			MonthPaid:    d.MonthPaid,
			MonthLabel:   monthStart,

			RecentPayments: recent,

			Pending: d.Pending,
			PendingHref: rangeHref(today, week,
				string(sqlc.BookingsStatusPendingPayment)),

			TodayHref:    rangeHref(today, today, ""),
			UpcomingHref: rangeHref(today, week, ""),
		},
	})
}

// tiles turns a status breakdown into the linked counts the page renders.
//
// Statuses with no bookings are dropped rather than shown as zero: a shift
// starts with a glance, and six zeroes are noise around the one number that is
// not.
func tiles(counts []service.StatusCount, from, to time.Time) []statusTile {
	out := make([]statusTile, 0, len(counts))
	for _, c := range counts {
		if c.Total == 0 {
			continue
		}
		badge := view.StatusBadge(c.Status)
		out = append(out, statusTile{
			Label: badge.Label,
			Class: badge.Class,
			Total: c.Total,
			Href:  rangeHref(from, to, string(c.Status)),
		})
	}
	return out
}

// rangeHref links to the booking list filtered to the same rows a tile counted:
// the same slot-date range, the same status. A dashboard number that leads
// somewhere showing a different number would be worse than no link.
func rangeHref(from, to time.Time, status string) string {
	q := url.Values{}
	q.Set("tanggal", service.DateModeSlot)
	q.Set("dari", from.Format("2006-01-02"))
	q.Set("sampai", to.Format("2006-01-02"))
	q.Set("urut", "lama")
	if status != "" {
		q.Set("status", status)
	}
	return bookingPath + "?" + q.Encode()
}
