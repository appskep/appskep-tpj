package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Admin-side payment reads (Phase 10).
//
// Reads only. Every write to a payment row still goes through the notification
// state machine in payment.go — the admin panel's one action is Sync, which
// asks Midtrans what happened and feeds the identical `apply`, so an operator
// cannot reach a conclusion the webhook would not have reached on its own.

// Payment lifecycle states, as the list query derives them. A payment has no
// status column: these are computed from paid_at / expired_at / cancelled_at,
// once, in SQL.
const (
	PaymentStatePending   = "pending"
	PaymentStatePaid      = "paid"
	PaymentStateExpired   = "expired"
	PaymentStateCancelled = "cancelled"
)

// allPaymentStates is the FIND_IN_SET argument meaning "no filter".
const allPaymentStates = PaymentStatePending + "," + PaymentStatePaid + "," +
	PaymentStateExpired + "," + PaymentStateCancelled

// paymentStateFilters is the whitelist a request's ?status= resolves through.
//
// Same reasoning as bookingStatusFilters: the value is a bind parameter either
// way, so this is not about injection. It is about an unrecognised value meaning
// "everything" instead of returning an empty list that reads as "there are no
// payments".
var paymentStateFilters = map[string]string{
	PaymentStatePending:   PaymentStatePending,
	PaymentStatePaid:      PaymentStatePaid,
	PaymentStateExpired:   PaymentStateExpired,
	PaymentStateCancelled: PaymentStateCancelled,
}

// PaymentStateFilter resolves a request's filter value, and reports whether it
// named a real state.
func PaymentStateFilter(state string) (list string, filtered bool) {
	if s, ok := paymentStateFilters[state]; ok {
		return s, true
	}
	return allPaymentStates, false
}

// AdminPaymentQuery is one page of the admin payments list. From/To apply to
// created_at — a payment's own timeline, not the booking's.
type AdminPaymentQuery struct {
	State  string
	From   time.Time
	To     time.Time
	Search string

	Page     int
	PageSize int
}

// AdminPaymentResult is that page plus what the pagination partial needs.
type AdminPaymentResult struct {
	Items      []sqlc.ListPaymentsAdminRow
	Page       int
	TotalPages int
	Total      int64
}

type paymentFilter struct {
	stateList   string
	createdFrom time.Time
	createdTo   time.Time
	search      string
}

func (p *Payment) filterFor(q AdminPaymentQuery) paymentFilter {
	stateList, _ := PaymentStateFilter(q.State)

	search := "%"
	if term := strings.TrimSpace(q.Search); term != "" {
		search = "%" + util.EscapeLike(term) + "%"
	}

	from := time.Date(1970, 1, 1, 0, 0, 0, 0, p.loc)
	to := time.Date(2999, 12, 31, 23, 59, 59, 0, p.loc)
	if !q.From.IsZero() {
		from = startOfDay(q.From, p.loc)
	}
	if !q.To.IsZero() {
		// created_at is a DATETIME; a bare date here would hide everything
		// created after midnight on the last day of the range.
		to = endOfDay(q.To, p.loc)
	}

	return paymentFilter{stateList: stateList, createdFrom: from, createdTo: to, search: search}
}

// AdminList returns one page of payments across every booking.
func (p *Payment) AdminList(ctx context.Context, q AdminPaymentQuery) (AdminPaymentResult, error) {
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}

	f := p.filterFor(q)

	total, err := p.store.Queries.CountPaymentsAdmin(ctx, sqlc.CountPaymentsAdminParams{
		StateList:   f.stateList,
		CreatedFrom: f.createdFrom,
		CreatedTo:   f.createdTo,
		Search:      f.search,
	})
	if err != nil {
		return AdminPaymentResult{}, fmt.Errorf("counting payments: %w", err)
	}

	totalPages := int((total + int64(q.PageSize) - 1) / int64(q.PageSize))
	if totalPages > 0 && q.Page > totalPages {
		q.Page = totalPages
	}

	items, err := p.store.Queries.ListPaymentsAdmin(ctx, sqlc.ListPaymentsAdminParams{
		StateList:   f.stateList,
		CreatedFrom: f.createdFrom,
		CreatedTo:   f.createdTo,
		Search:      f.search,
		Limit:       int32(q.PageSize),
		Offset:      int32((q.Page - 1) * q.PageSize),
	})
	if err != nil {
		return AdminPaymentResult{}, fmt.Errorf("listing payments: %w", err)
	}

	return AdminPaymentResult{Items: items, Page: q.Page, TotalPages: totalPages, Total: total}, nil
}

// Export returns every payment matching the filter, and whether the cap
// truncated it.
func (p *Payment) Export(ctx context.Context, q AdminPaymentQuery) ([]sqlc.ListPaymentsForExportRow, bool, error) {
	f := p.filterFor(q)

	rows, err := p.store.Queries.ListPaymentsForExport(ctx, sqlc.ListPaymentsForExportParams{
		StateList:   f.stateList,
		CreatedFrom: f.createdFrom,
		CreatedTo:   f.createdTo,
		Search:      f.search,
		Limit:       exportLimit + 1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("exporting payments: %w", err)
	}

	if len(rows) > exportLimit {
		return rows[:exportLimit], true, nil
	}
	return rows, false, nil
}

// AdminDetail returns one payment with its booking and customer joined in.
func (p *Payment) AdminDetail(ctx context.Context, id int64) (sqlc.GetPaymentAdminDetailRow, error) {
	row, err := p.store.Queries.GetPaymentAdminDetail(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.GetPaymentAdminDetailRow{}, ErrNotFound
	}
	if err != nil {
		return sqlc.GetPaymentAdminDetailRow{}, fmt.Errorf("getting payment %d: %w", id, err)
	}
	return row, nil
}

// Notifications returns the raw webhook audit trail for one order — every
// payload Midtrans sent about it, valid signature or not.
//
// This is the page that makes a payment dispute answerable: it shows what
// arrived, when, whether it verified, and what was done about it.
func (p *Payment) Notifications(ctx context.Context, orderID string) ([]sqlc.PaymentNotification, error) {
	rows, err := p.store.Queries.ListPaymentNotificationsByOrderID(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("listing notifications for %s: %w", orderID, err)
	}
	return rows, nil
}

// ForBooking lists every payment attempt against one booking, newest first, for
// the admin booking detail page.
func (p *Payment) ForBooking(ctx context.Context, bookingID int64) ([]sqlc.Payment, error) {
	rows, err := p.store.Queries.ListPaymentsForBooking(ctx, bookingID)
	if err != nil {
		return nil, fmt.Errorf("listing payments for booking %d: %w", bookingID, err)
	}
	return rows, nil
}

// PaymentState derives the lifecycle state of a payment row in Go, for the one
// caller that has a sqlc.Payment rather than a list row: the booking detail
// page. The list query computes the identical CASE in SQL.
func PaymentState(p sqlc.Payment) string {
	switch {
	case p.PaidAt.Valid:
		return PaymentStatePaid
	case p.CancelledAt.Valid:
		return PaymentStateCancelled
	case p.ExpiredAt.Valid:
		return PaymentStateExpired
	}
	return PaymentStatePending
}
