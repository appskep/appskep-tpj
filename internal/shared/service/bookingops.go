package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Admin-side booking operations (Phase 10).
//
// These hang off *Booking rather than a new type so that the rules about a
// booking live in one place: AdminCancel and CancelForUser must agree about what
// releasing a slot means, and Reschedule has to obey the same lock order Create
// does. A separate AdminBooking type would have made that agreement a matter of
// two authors remembering.
//
// Two of the methods here move booked_count — AdminCancel and Reschedule — and
// both follow the rule the whole system rests on: the slot lock is the
// transaction's first statement, and RowsAffected() == 1 on a status-guarded
// UPDATE is the only thing that authorises touching the counter.

// exportLimit bounds a CSV export. A clinic's whole history is far below this;
// the cap exists so a hand-typed date range cannot pull an unbounded result set
// into memory. The handler reports when it bites rather than silently truncating.
const exportLimit = 5000

// rescheduleWindowDays is how far ahead the reschedule picker looks.
const rescheduleWindowDays = 60

// rescheduleOptionLimit bounds that picker. Two months of four daily slots is
// ~240; the cap is a safety net, not a business rule.
const rescheduleOptionLimit = 400

// maxCancelReasonLen matches bookings.cancelled_reason VARCHAR(255).
const maxCancelReasonLen = 255

// Date modes for the admin list's range filter.
const (
	DateModeSlot    = "jadwal"
	DateModeCreated = "dibuat"
)

// AdminBookingQuery is one page of the admin booking list.
//
// Every field is optional. A zero From/To means "no bound on that end", which
// the query expresses by widening rather than by branching — see the note at the
// top of the admin section in bookings.sql for why the SQL has no conditional
// predicates.
type AdminBookingQuery struct {
	// Status is the raw request value. Anything unrecognised means "all", the
	// same whitelist behaviour as the riwayat filter.
	Status string
	// ServiceID is 0 for "every layanan".
	ServiceID int64
	// DateMode selects which column From/To apply to: DateModeSlot (the day the
	// customer is coming) or DateModeCreated (when the booking was made).
	DateMode string
	From     time.Time
	To       time.Time
	Search   string
	// OldestFirst flips the schedule ordering. Default is newest first.
	OldestFirst bool

	Page     int
	PageSize int
}

// AdminBookingResult is one page plus what the pagination partial needs.
type AdminBookingResult struct {
	Items      []sqlc.ListBookingsAdminRow
	Page       int
	TotalPages int
	Total      int64
}

// bookingFilter is one resolved AdminBookingQuery: every predicate the SQL wants,
// already widened and escaped.
//
// Built once and shared by the list, the count and the export, so a CSV cannot
// contain a different set of rows from the page that offered it.
type bookingFilter struct {
	statusList  string
	serviceID   int64
	slotFrom    time.Time
	slotTo      time.Time
	createdFrom time.Time
	createdTo   time.Time
	search      string
}

// wideFrom and wideTo stand in for "no bound". They are dates rather than
// time.Time zero values because the columns they are compared against are DATE
// and DATETIME, and MySQL's DATETIME range starts at 1000-01-01.
func (b *Booking) wideBounds() (time.Time, time.Time) {
	return time.Date(1970, 1, 1, 0, 0, 0, 0, b.loc),
		time.Date(2999, 12, 31, 23, 59, 59, 0, b.loc)
}

func (b *Booking) filterFor(q AdminBookingQuery) bookingFilter {
	statusList, _ := BookingStatusFilter(q.Status)

	// "%" for an empty box, per the query's comment. EscapeLike keeps a typed %
	// or _ a literal character rather than a wildcard that silently matches
	// everything.
	search := "%"
	if term := strings.TrimSpace(q.Search); term != "" {
		search = "%" + util.EscapeLike(term) + "%"
	}

	lo, hi := b.wideBounds()
	f := bookingFilter{
		statusList:  statusList,
		serviceID:   q.ServiceID,
		slotFrom:    lo,
		slotTo:      hi,
		createdFrom: lo,
		createdTo:   hi,
		search:      search,
	}

	from, to := q.From, q.To
	if q.DateMode == DateModeCreated {
		if !from.IsZero() {
			f.createdFrom = startOfDay(from, b.loc)
		}
		if !to.IsZero() {
			// End of day, not midnight: created_at is a DATETIME, and a bare date
			// as the upper bound would exclude everything booked after 00:00:00 on
			// the last day of the range.
			f.createdTo = endOfDay(to, b.loc)
		}
		return f
	}

	if !from.IsZero() {
		f.slotFrom = startOfDay(from, b.loc)
	}
	if !to.IsZero() {
		f.slotTo = startOfDay(to, b.loc)
	}
	return f
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func endOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, loc)
}

// AdminList returns one page of bookings across every customer.
//
// Same count-clamp-read shape as Catalog.List and Booking.History, because the
// pagination partial is the same one: clamping after the count is what stops a
// stale ?page= from rendering an empty table with a working "next".
func (b *Booking) AdminList(ctx context.Context, q AdminBookingQuery) (AdminBookingResult, error) {
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}

	f := b.filterFor(q)

	total, err := b.store.Queries.CountBookingsAdmin(ctx, sqlc.CountBookingsAdminParams{
		StatusList:  f.statusList,
		ServiceID:   f.serviceID,
		SlotFrom:    f.slotFrom,
		SlotTo:      f.slotTo,
		CreatedFrom: f.createdFrom,
		CreatedTo:   f.createdTo,
		Search:      f.search,
	})
	if err != nil {
		return AdminBookingResult{}, fmt.Errorf("counting bookings: %w", err)
	}

	totalPages := int((total + int64(q.PageSize) - 1) / int64(q.PageSize))
	if totalPages > 0 && q.Page > totalPages {
		q.Page = totalPages
	}

	// oldest_first types as interface{} (see the query comment). Sending an int
	// keeps the comparison numeric on the server side.
	oldest := 0
	if q.OldestFirst {
		oldest = 1
	}

	items, err := b.store.Queries.ListBookingsAdmin(ctx, sqlc.ListBookingsAdminParams{
		StatusList:  f.statusList,
		ServiceID:   f.serviceID,
		SlotFrom:    f.slotFrom,
		SlotTo:      f.slotTo,
		CreatedFrom: f.createdFrom,
		CreatedTo:   f.createdTo,
		Search:      f.search,
		OldestFirst: oldest,
		Limit:       int32(q.PageSize),
		Offset:      int32((q.Page - 1) * q.PageSize),
	})
	if err != nil {
		return AdminBookingResult{}, fmt.Errorf("listing bookings: %w", err)
	}

	return AdminBookingResult{
		Items:      items,
		Page:       q.Page,
		TotalPages: totalPages,
		Total:      total,
	}, nil
}

// Export returns every booking matching the filter, for the CSV writer, and
// reports whether the cap truncated the result.
func (b *Booking) Export(ctx context.Context, q AdminBookingQuery) ([]sqlc.ListBookingsForExportRow, bool, error) {
	f := b.filterFor(q)

	// One more than the cap, so a full page is distinguishable from an exact fit.
	rows, err := b.store.Queries.ListBookingsForExport(ctx, sqlc.ListBookingsForExportParams{
		StatusList:  f.statusList,
		ServiceID:   f.serviceID,
		SlotFrom:    f.slotFrom,
		SlotTo:      f.slotTo,
		CreatedFrom: f.createdFrom,
		CreatedTo:   f.createdTo,
		Search:      f.search,
		Limit:       exportLimit + 1,
	})
	if err != nil {
		return nil, false, fmt.Errorf("exporting bookings: %w", err)
	}

	if len(rows) > exportLimit {
		return rows[:exportLimit], true, nil
	}
	return rows, false, nil
}

// AdminDetail returns one booking with its layanan, slot and customer joined in.
//
// No ownership predicate, unlike DetailForUser: this is reached only through the
// /admin subtree, which RequireAdmin wraps in its entirety.
func (b *Booking) AdminDetail(ctx context.Context, id int64) (sqlc.GetBookingAdminDetailRow, error) {
	row, err := b.store.Queries.GetBookingAdminDetail(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.GetBookingAdminDetailRow{}, ErrNotFound
	}
	if err != nil {
		return sqlc.GetBookingAdminDetailRow{}, fmt.Errorf("getting booking %d: %w", id, err)
	}
	return row, nil
}

// Timeline returns the audit entries for one booking, newest first.
func (b *Booking) Timeline(ctx context.Context, id int64, limit int32) ([]sqlc.ActivityLog, error) {
	rows, err := b.store.Queries.ListActivityLogsForEntity(ctx, sqlc.ListActivityLogsForEntityParams{
		Entity:   nullString(EntityBooking),
		EntityID: sql.NullInt64{Int64: id, Valid: true},
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing activity for booking %d: %w", id, err)
	}
	return rows, nil
}

// UpdateNotes edits the internal note on a booking. It is the only booking field
// an admin may change freely — everything else is a status transition or a
// reschedule, and both of those move a slot.
func (b *Booking) UpdateNotes(ctx context.Context, id int64, notes string) error {
	if _, err := b.get(ctx, id); err != nil {
		return err
	}

	notes = strings.TrimSpace(notes)
	ve := NewValidationError()
	if !maxLen(ve, "catatan", "Catatan", notes, maxNotesLen) {
		return ve
	}

	var value sql.NullString
	if notes != "" {
		value = sql.NullString{String: notes, Valid: true}
	}

	if err := b.store.Queries.UpdateBookingNotes(ctx, sqlc.UpdateBookingNotesParams{
		Notes: value,
		ID:    id,
	}); err != nil {
		return fmt.Errorf("updating notes on booking %d: %w", id, err)
	}
	return nil
}

// Confirm and Complete move a booking forward. Neither touches the slot: both
// the statuses they leave and the ones they enter hold it.
//
// The bool is whether a real transition happened. False means the guard in the
// UPDATE refused — the booking was not in the status this action starts from,
// usually because the operator's page was stale or the button was double-clicked
// — and the handler reports that as information, never as an error.
func (b *Booking) Confirm(ctx context.Context, id int64) (bool, error) {
	return b.transition(ctx, id, b.store.Queries.ConfirmBooking, "confirming")
}

func (b *Booking) Complete(ctx context.Context, id int64) (bool, error) {
	return b.transition(ctx, id, b.store.Queries.CompleteBooking, "completing")
}

func (b *Booking) transition(
	ctx context.Context,
	id int64,
	apply func(context.Context, int64) (sql.Result, error),
	what string,
) (bool, error) {
	if _, err := b.get(ctx, id); err != nil {
		return false, err
	}

	res, err := apply(ctx, id)
	if err != nil {
		return false, fmt.Errorf("%s booking %d: %w", what, id, err)
	}
	moved, err := changed(res)
	if err != nil {
		return false, fmt.Errorf("reading %s result for booking %d: %w", what, id, err)
	}
	return moved, nil
}

// AdminCancel cancels any booking that still holds its slot, records a reason,
// and gives the slot back.
//
// It is CancelForUser with a wider status guard and no ownership predicate:
// pending_payment, paid and confirmed can all be cancelled here, because an
// operator sometimes has to free a slot for a booking that was already paid for.
// There is no refund path in v1 (PLAN.md Q6) — the UI says so, and the reason is
// recorded so a human can act on it.
//
// The returned status is the one the booking held BEFORE the cancellation, so
// the caller knows whether there is still a live Midtrans order to close: only a
// pending_payment booking can have a Snap page open that would otherwise keep
// taking money for a slot that has just been given back.
//
// moved == false is a success, not a failure: the expiry ticker or the webhook
// moved this booking first, and whoever did has already released the slot.
func (b *Booking) AdminCancel(ctx context.Context, id int64, reason string) (was sqlc.BookingsStatus, moved bool, err error) {
	// Resolved outside the transaction purely to learn slot_id: the slot lock has
	// to be the transaction's first statement, so the slot must be known before it
	// opens. Same shape as CancelForUser and the expiry ticker.
	row, err := b.get(ctx, id)
	if err != nil {
		return "", false, err
	}

	reason = strings.TrimSpace(reason)
	ve := NewValidationError()
	// "Alasan pembatalan" when empty, "Alasan" in the length message: the first
	// says what to type, the second names the box it is already in.
	if required(ve, "alasan", "Alasan pembatalan", reason) {
		maxLen(ve, "alasan", "Alasan", reason, maxCancelReasonLen)
	}
	if ve.Any() {
		return row.Status, false, ve
	}

	// Pre-flight only, for the message. The guard that decides anything is the
	// status predicate inside CancelBooking.
	if !canCancelOrReschedule(row.Status) {
		return row.Status, false, ErrBookingFinal
	}

	err = b.store.WithTx(ctx, func(q *sqlc.Queries) error {
		// Slot lock first, and "first" is not only about lock order: a plain SELECT
		// ahead of it establishes the REPEATABLE READ snapshot, and a locking read
		// that then meets a newer row fails with ER_CHECKREAD (1020) instead of
		// blocking. Its value is not read — the lock itself is the point.
		if _, lerr := q.GetSlotForUpdate(ctx, row.SlotID); lerr != nil {
			if errors.Is(lerr, sql.ErrNoRows) {
				// fk_bookings_slot is RESTRICT, so this should be unreachable. Leave
				// the booking alone rather than cancelling it without being able to
				// release anything.
				return nil
			}
			return fmt.Errorf("locking slot %d: %w", row.SlotID, lerr)
		}

		res, cerr := q.CancelBooking(ctx, sqlc.CancelBookingParams{
			CancelledReason: nullString(reason),
			ID:              row.ID,
		})
		if cerr != nil {
			return fmt.Errorf("cancelling booking %d: %w", row.ID, cerr)
		}
		did, cerr := changed(res)
		if cerr != nil {
			return fmt.Errorf("reading cancellation result for booking %d: %w", row.ID, cerr)
		}
		if !did {
			// Already moved by the ticker, the webhook or another operator. Success,
			// and nothing to release: whoever moved it gave the slot back.
			return nil
		}

		if rerr := q.ReleaseSlot(ctx, row.SlotID); rerr != nil {
			return fmt.Errorf("releasing slot %d: %w", row.SlotID, rerr)
		}
		moved = true
		return nil
	})
	if err != nil {
		return row.Status, false, err
	}

	if moved {
		// row.Status is the status held BEFORE the cancellation, which is the only
		// way to know whether the mail has to mention a refund: the booking now
		// reads "cancelled" whatever it was. paid and confirmed both mean money
		// changed hands.
		b.email.NotifyCancelled(ctx, row.ID, row.Status != sqlc.BookingsStatusPendingPayment)
	}
	return row.Status, moved, nil
}

// RescheduleOptions lists the slots a booking may be moved to: active, with room
// left, and not yet started.
//
// It deliberately ignores booking_lead_time_minutes, which Schedule.Window
// applies to the public picker. That rule protects the clinic from a customer
// booking themselves in with no notice; an operator moving someone by phone into
// a slot half an hour out is doing their job.
func (b *Booking) RescheduleOptions(ctx context.Context) ([]sqlc.ScheduleSlot, error) {
	now := time.Now().In(b.loc)

	rows, err := b.store.Queries.ListFreeSlotsBetween(ctx, sqlc.ListFreeSlotsBetweenParams{
		FromStartsAt: sql.NullTime{Time: now, Valid: true},
		ToStartsAt:   sql.NullTime{Time: now.AddDate(0, 0, rescheduleWindowDays), Valid: true},
		Limit:        rescheduleOptionLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing reschedule options: %w", err)
	}
	return rows, nil
}

// Reschedule moves a booking to another slot.
//
// The first transaction in this system that locks TWO slots, and the lock order
// is what makes it safe: both are taken with GetSlotForUpdate in ascending id
// order, as the transaction's first two statements. Two operators moving
// bookings between the same pair of slots in opposite directions would otherwise
// deadlock, each holding the slot the other needs.
//
// Everything after the locks is the booking transaction in miniature: re-read
// under the lock, re-check the target, increment the new slot, move the booking,
// and release the old one — each step authorised by the one before it. The price
// snapshot is never touched: a customer owes what they agreed to, whatever the
// layanan costs now and whenever they are now coming.
func (b *Booking) Reschedule(ctx context.Context, bookingID, newSlotID int64) error {
	row, err := b.get(ctx, bookingID)
	if err != nil {
		return err
	}
	if !canCancelOrReschedule(row.Status) {
		return ErrBookingFinal
	}
	if newSlotID == row.SlotID {
		ve := NewValidationError()
		ve.Add("slot", "Jadwal baru sama dengan jadwal saat ini.")
		return ve
	}

	// One retry on a deadlock or lock-wait timeout, for the same reason Create
	// retries: InnoDB rolls the loser back whole, which makes a second attempt
	// safe rather than merely hopeful. A slot that moved under us is retried on
	// the same terms.
	for attempt := range 2 {
		var previous sqlc.ScheduleSlot
		previous, err = b.reschedule(ctx, row, newSlotID)
		if err == nil {
			// After the commit, carrying the slot the booking has just left: once
			// the move lands, the old time is no longer readable from the booking.
			b.email.NotifyRescheduled(ctx, row.ID, previous)
			return nil
		}
		if attempt == 0 && (repository.IsRetryable(err) || errors.Is(err, errSlotMoved)) {
			// Re-read: the retry has to lock the pair the booking actually holds now.
			row, err = b.get(ctx, bookingID)
			if err != nil {
				return err
			}
			if !canCancelOrReschedule(row.Status) {
				return ErrBookingFinal
			}
			if newSlotID == row.SlotID {
				return nil
			}
			continue
		}
		if errors.Is(err, errSlotMoved) {
			return ErrBookingFinal
		}
		return err
	}
	return err
}

// reschedule performs one attempt and returns the slot the booking was moved
// away from, which Phase 11's notification needs and nothing can read back once
// the move has committed.
func (b *Booking) reschedule(ctx context.Context, row sqlc.Booking, newSlotID int64) (sqlc.ScheduleSlot, error) {
	oldSlotID := row.SlotID

	// Ascending id: the deadlock-avoidance order. Both locks are taken before any
	// other statement, so this transaction also establishes its REPEATABLE READ
	// snapshot on a locking read (see the note in Booking.create).
	ids := []int64{oldSlotID, newSlotID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var previous sqlc.ScheduleSlot

	err := b.store.WithTx(ctx, func(q *sqlc.Queries) error {
		var target sqlc.ScheduleSlot
		for _, id := range ids {
			slot, err := q.GetSlotForUpdate(ctx, id)
			if errors.Is(err, sql.ErrNoRows) {
				if id == newSlotID {
					ve := NewValidationError()
					ve.Add("slot", "Jadwal tujuan tidak ditemukan. Pilih jadwal lain.")
					return ve
				}
				// fk_bookings_slot is RESTRICT: the slot a booking holds cannot be
				// deleted, so this is unreachable.
				return fmt.Errorf("locking slot %d: %w", id, err)
			}
			if err != nil {
				return fmt.Errorf("locking slot %d: %w", id, err)
			}
			if id == newSlotID {
				target = slot
			}
			if id == oldSlotID {
				previous = slot
			}
		}

		// Re-read the booking under the locks. If it moved slots in between, the
		// pair we locked is wrong and releasing oldSlotID would decrement a slot
		// this booking no longer holds.
		current, err := q.GetBookingForUpdate(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("locking booking %d: %w", row.ID, err)
		}
		if current.SlotID != oldSlotID {
			return errSlotMoved
		}
		if !canCancelOrReschedule(current.Status) {
			return ErrBookingFinal
		}

		// The target's conditions, re-checked against the locked row. The lead time
		// is deliberately not applied (see RescheduleOptions); "already started" is.
		switch {
		case !target.IsActive:
			return newSlotValidationError("Jadwal tujuan sudah tidak aktif. Pilih jadwal lain.")
		case target.BookedCount >= target.Capacity:
			return newSlotValidationError("Jadwal tujuan sudah penuh. Pilih jadwal lain.")
		case !target.StartsAt.After(time.Now().In(b.loc)):
			return newSlotValidationError("Jadwal tujuan sudah lewat. Pilih jadwal lain.")
		}

		// Increment first: if the new slot cannot take the booking, nothing else
		// should happen. The WHERE clause is defence in depth — the lock above is
		// what makes this correct.
		held, err := q.HoldSlot(ctx, newSlotID)
		if err != nil {
			return fmt.Errorf("holding slot %d: %w", newSlotID, err)
		}
		n, err := changed(held)
		if err != nil {
			return fmt.Errorf("reading hold result for slot %d: %w", newSlotID, err)
		}
		if !n {
			return ErrSlotTaken
		}

		res, err := q.RescheduleBooking(ctx, sqlc.RescheduleBookingParams{
			SlotID: newSlotID,
			ID:     row.ID,
		})
		switch {
		case repository.IsDuplicateKeyOn(err, "uq_bookings_active_slot_user"):
			// This customer already holds a live booking on the target slot. The
			// transaction rolls back, so the hold taken above is undone.
			return ErrDuplicateBooking
		case err != nil:
			return fmt.Errorf("rescheduling booking %d: %w", row.ID, err)
		}

		moved, err := changed(res)
		if err != nil {
			return fmt.Errorf("reading reschedule result for booking %d: %w", row.ID, err)
		}
		if !moved {
			// The status changed under the lock — impossible with the re-read above,
			// but the counter must never be left incremented for a booking that did
			// not move. Rolling back is what undoes the hold.
			return ErrBookingFinal
		}

		// Only now, and only because the booking really moved.
		if err := q.ReleaseSlot(ctx, oldSlotID); err != nil {
			return fmt.Errorf("releasing slot %d: %w", oldSlotID, err)
		}
		return nil
	})
	if err != nil {
		return sqlc.ScheduleSlot{}, err
	}
	return previous, nil
}

func newSlotValidationError(msg string) error {
	ve := NewValidationError()
	ve.Add("slot", msg)
	return ve
}

// get reads one booking by id, mapping a missing row to ErrNotFound.
func (b *Booking) get(ctx context.Context, id int64) (sqlc.Booking, error) {
	row, err := b.store.Queries.GetBooking(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Booking{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Booking{}, fmt.Errorf("getting booking %d: %w", id, err)
	}
	return row, nil
}

// canCancelOrReschedule reports whether an operator may still move or cancel a
// booking: the exact set CancelBooking and RescheduleBooking are guarded on, so
// the two share one predicate and ErrBookingFinal has one meaning.
//
// It is NOT the set the booked_count invariant is defined by, and the difference
// is `completed`. Only 'cancelled' and 'expired' release a slot — that rule lives
// in the bookings.active_slot_id generated column, and a completed booking is
// still counted in booked_count by it. So this predicate must never be used to
// decide whether to call ReleaseSlot: doing so would release the slot of every
// completed booking and drive the counter below the truth.
//
// It was named holdsSlot until Phase 13, whose test read the old name and the old
// doc comment and expected `completed` to be true. The behaviour at all four call
// sites was right; the name was the trap.
func canCancelOrReschedule(s sqlc.BookingsStatus) bool {
	switch s {
	case sqlc.BookingsStatusPendingPayment, sqlc.BookingsStatusPaid, sqlc.BookingsStatusConfirmed:
		return true
	}
	return false
}
