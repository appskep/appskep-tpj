package service

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
)

// Dashboard assembles the admin landing screen.
//
// Six independent reads, and none of them is allowed to take the page down: a
// failure logs and drops its own card, exactly as Schedule.Upcoming does on the
// layanan page. The dashboard's job is to orient an operator at the start of a
// shift, and a panel that 500s because one aggregate hiccupped is worse than a
// panel with one section missing.
type Dashboard struct {
	store *repository.Store
	log   *slog.Logger
	loc   *time.Location
}

func NewDashboard(store *repository.Store, log *slog.Logger, loc *time.Location) *Dashboard {
	return &Dashboard{store: store, log: log, loc: loc}
}

// recentPaymentLimit is how many payments the "terbaru" card shows.
const recentPaymentLimit = 8

// UpcomingDays is how far ahead the "jadwal mendatang" counts look. Exported
// because the dashboard's tiles link to the booking list filtered to the same
// range, and a number that links to a different range would be worse than one
// that links nowhere.
const UpcomingDays = 7

// StatusCount is one bar of the status breakdown.
type StatusCount struct {
	Status sqlc.BookingsStatus
	Total  int64
}

// DashboardData is everything the page renders. Every field is independently
// optional: a nil slice or a zero count means that read failed and was logged.
type DashboardData struct {
	Today time.Time

	// TodayBookings is the day sheet — who is coming today, in time order,
	// excluding released bookings.
	TodayBookings []sqlc.ListBookingsBySlotDateRow

	// TodayStatuses and MonthStatuses count bookings by status over the day and
	// over the calendar month, both by slot date.
	TodayStatuses []StatusCount
	MonthStatuses []StatusCount

	// UpcomingStatuses covers today through the next week, so an operator can
	// see what is committed ahead.
	UpcomingStatuses []StatusCount

	// MonthRevenue is the sum of gross_amount over payments PAID this calendar
	// month, as a decimal string. Money never becomes a float64 on the way to
	// the page: SumPaidBetween casts it, util.Rupiah renders it.
	MonthRevenue string
	MonthPaid    int64

	// RecentPayments is the newest settled payments across every booking.
	RecentPayments []sqlc.ListRecentPaidPaymentsRow

	// Pending is how many bookings are waiting for payment right now — the one
	// number that usually needs acting on.
	Pending int64
}

// Load reads everything the dashboard shows.
//
// It returns no error: every failure is logged and leaves its own field zero.
// The caller renders the page regardless.
func (d *Dashboard) Load(ctx context.Context) DashboardData {
	now := time.Now().In(d.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, d.loc)

	data := DashboardData{Today: today, MonthRevenue: "0.00"}

	if rows, err := d.store.Queries.ListBookingsBySlotDate(ctx, today); err != nil {
		d.warn(ctx, "day sheet", err)
	} else {
		data.TodayBookings = rows
	}

	data.TodayStatuses = d.statuses(ctx, "today", today, today)

	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, d.loc)
	monthEnd := monthStart.AddDate(0, 1, 0).AddDate(0, 0, -1)
	data.MonthStatuses = d.statuses(ctx, "month", monthStart, monthEnd)

	data.UpcomingStatuses = d.statuses(ctx, "upcoming", today, today.AddDate(0, 0, UpcomingDays))

	// Revenue is bounded by paid_at, not by slot date: it is money received this
	// month, which is what a monthly figure means. A booking paid in July for a
	// session in August belongs to July's revenue.
	if row, err := d.store.Queries.SumPaidBetween(ctx, sqlc.SumPaidBetweenParams{
		FromPaidAt: sql.NullTime{Time: monthStart, Valid: true},
		ToPaidAt:   sql.NullTime{Time: endOfDay(monthEnd, d.loc), Valid: true},
	}); err != nil {
		d.warn(ctx, "month revenue", err)
	} else {
		data.MonthRevenue = row.Total
		data.MonthPaid = row.PaidCount
	}

	if rows, err := d.store.Queries.ListRecentPaidPayments(ctx, recentPaymentLimit); err != nil {
		d.warn(ctx, "recent payments", err)
	} else {
		data.RecentPayments = rows
	}

	for _, s := range data.UpcomingStatuses {
		if s.Status == sqlc.BookingsStatusPendingPayment {
			data.Pending = s.Total
		}
	}

	return data
}

func (d *Dashboard) statuses(ctx context.Context, what string, from, to time.Time) []StatusCount {
	rows, err := d.store.Queries.CountBookingsByStatusInRange(ctx, sqlc.CountBookingsByStatusInRangeParams{
		FromSlotDate: from,
		ToSlotDate:   to,
	})
	if err != nil {
		d.warn(ctx, what+" status counts", err)
		return nil
	}

	out := make([]StatusCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, StatusCount{Status: row.Status, Total: row.Total})
	}
	return out
}

func (d *Dashboard) warn(ctx context.Context, what string, err error) {
	d.log.WarnContext(ctx, "dashboard: reading "+what, slog.Any("error", err))
}
