package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
)

// Booking is the public booking flow, and the only caller of the slot locking
// machinery Phase 1 built.
//
// Everything else in this package validates a form. This type does that too, but
// its real job is Create: the one transaction in the system where two users can
// be racing for the same row, and where losing the race silently means taking
// money for a service that cannot be delivered. See PLAN.md § Design note: slot
// concurrency.
//
// The reads below (BookableServices, AvailableDates, AvailableSlots) are
// advisory. They decide what the form offers, nothing more — every one of their
// conditions is re-checked inside Create under the slot's row lock, because
// anything a page renders is already out of date by the time it is submitted.
type Booking struct {
	store    *repository.Store
	settings *Settings
	// schedule supplies Window(), so the booking window has exactly one
	// definition and the layanan page cannot advertise a date this type refuses.
	schedule *Schedule
	// expiryFallbackMinutes is PAYMENT_EXPIRY_MINUTES from the environment. The
	// payment_expiry_minutes settings row wins when present and parseable; this is
	// the fallback, per CLAUDE.md § Settings vs environment.
	expiryFallbackMinutes int
	// loc is the application timezone; every instant this type writes is built in
	// it, matching the pinned driver loc.
	loc *time.Location
	// email queues what a customer is told. Every call to it sits AFTER the
	// commit that made a transition real, never inside a transaction, and can
	// never fail the action it describes — Audit.Record's rule (Phase 11).
	email *Email
}

func NewBooking(
	store *repository.Store,
	settings *Settings,
	schedule *Schedule,
	email *Email,
	expiryFallbackMinutes int,
	loc *time.Location,
) *Booking {
	return &Booking{
		store:                 store,
		settings:              settings,
		schedule:              schedule,
		email:                 email,
		expiryFallbackMinutes: expiryFallbackMinutes,
		loc:                   loc,
	}
}

// Field bounds, mirroring the bookings table.
const (
	maxCustomerNameLen    = 150 // customer_name VARCHAR(150)
	maxCustomerPhoneLen   = 30  // customer_phone VARCHAR(30)
	maxCustomerAddressLen = 500 // customer_address VARCHAR(500)
	maxNotesLen           = 500 // notes VARCHAR(500)

	// minPhoneDigits rejects a phone number that could not reach anyone. Indonesian
	// mobile numbers are 10-13 digits; 8 leaves room for a landline written with an
	// area code and does not fight a format nobody predicted.
	minPhoneDigits = 8

	// defaultExpiryMinutes backs up both the settings row and the environment. It
	// is only reached if config somehow yielded a non-positive value, which
	// validate() already refuses at boot.
	defaultExpiryMinutes = 60

	// codeRandomLen is how many random characters a booking code carries after the
	// date. Four over a 31-character alphabet is ~923k combinations per day —
	// enough that a collision is rare, and uq_bookings_code catches the rest.
	codeRandomLen = 4

	// maxCodeAttempts bounds the collision retry inside one transaction.
	maxCodeAttempts = 5
)

// CreateInput is one submitted booking form, still as raw strings. Same shape
// and same reason as ServiceInput and SlotInput: the service owns the parsing
// rules and the Indonesian messages that describe them, so no two call sites can
// disagree about what a valid phone number is.
type CreateInput struct {
	// UserID is the local users.id of the signed-in user. Not a form field — the
	// handler takes it from the verified session, never from the request body.
	UserID int64

	ServiceSlug string
	SlotID      string
	Name        string
	Phone       string
	Address     string
	Notes       string
}

// parsedBooking is a validated CreateInput, carrying the rows the form resolved
// to so the transaction does not have to look them up twice.
//
// The service and slot here are a snapshot from *before* the lock. Create
// re-reads both inside the transaction and trusts only what it finds there;
// these copies exist to render the review panel.
type parsedBooking struct {
	service sqlc.Service
	slot    sqlc.ScheduleSlot
	name    string
	phone   string
	address sql.NullString
	notes   sql.NullString
}

// AvailableDate is one date the picker may offer.
type AvailableDate struct {
	Date time.Time
	Free int64
}

// BookableServices lists what step 1 may offer.
//
// Coming-soon services are excluded here, not filtered in the template: they are
// listed on the public site with a label but must not be bookable (PLAN.md
// Phase 4), and a rule enforced in SQL cannot be forgotten at a call site.
func (b *Booking) BookableServices(ctx context.Context) ([]sqlc.Service, error) {
	services, err := b.store.Queries.ListBookableServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing bookable services: %w", err)
	}
	return services, nil
}

// BookableBySlug resolves the ?layanan= parameter, or ErrNotFound.
//
// GetActiveServiceBySlug carries the is_active predicate; the coming-soon check
// is applied here because no query needs both variants. An inactive and a
// coming-soon service are both simply "not bookable" to this flow.
func (b *Booking) BookableBySlug(ctx context.Context, slug string) (sqlc.Service, error) {
	svc, err := b.store.Queries.GetActiveServiceBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.Service{}, ErrNotFound
	}
	if err != nil {
		return sqlc.Service{}, fmt.Errorf("getting bookable service %q: %w", slug, err)
	}
	if svc.IsComingSoon {
		return sqlc.Service{}, ErrNotFound
	}
	return svc, nil
}

// AvailableDates lists the dates inside the booking window that still have at
// least one free slot.
func (b *Booking) AvailableDates(ctx context.Context) ([]AvailableDate, error) {
	startsAt, until := b.schedule.Window()

	rows, err := b.store.Queries.ListAvailableDates(ctx, sqlc.ListAvailableDatesParams{
		StartsAt: startsAt,
		SlotDate: until,
	})
	if err != nil {
		return nil, fmt.Errorf("listing available dates: %w", err)
	}

	dates := make([]AvailableDate, 0, len(rows))
	for _, row := range rows {
		dates = append(dates, AvailableDate{Date: row.SlotDate, Free: row.FreeSlots})
	}
	return dates, nil
}

// AvailableSlots lists the free slots on one date.
//
// A date outside the window returns nothing rather than an error: the query
// string is user-supplied, and a hand-typed ?tanggal= from next year is a picker
// with no options, not a failure worth a page.
func (b *Booking) AvailableSlots(ctx context.Context, date time.Time) ([]sqlc.ScheduleSlot, error) {
	if date.IsZero() {
		return nil, nil
	}

	startsAt, until := b.schedule.Window()
	day := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, b.loc)
	if day.After(until) {
		return nil, nil
	}

	slots, err := b.store.Queries.ListAvailableSlotsByDate(ctx, sqlc.ListAvailableSlotsByDateParams{
		SlotDate: day,
		StartsAt: startsAt,
	})
	if err != nil {
		return nil, fmt.Errorf("listing slots for %s: %w", day.Format(dateLayout), err)
	}
	return slots, nil
}

// Held reports whether this user already holds a live booking for this slot.
//
// Advisory, like every other read here: uq_bookings_active_slot_user is what
// actually prevents the duplicate. This exists so the review step can say so
// before the user commits, instead of the database saying it afterwards.
func (b *Booking) Held(ctx context.Context, slotID, userID int64) (bool, error) {
	_, err := b.store.Queries.GetSlotHoldingBookingForUser(ctx, sqlc.GetSlotHoldingBookingForUserParams{
		SlotID: slotID,
		UserID: userID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking existing booking on slot %d: %w", slotID, err)
	}
	return true, nil
}

// DetailForUser returns one booking with its layanan and slot joined in, for the
// pembayaran and konfirmasi pages.
//
// The ownership rule lives here rather than in the handler so it cannot be
// forgotten by the next page that needs a booking. A booking belonging to
// someone else is ErrNotFound, not a permission error: a 403 would confirm that
// the code exists, which is the one fact a stranger guessing codes wants.
func (b *Booking) DetailForUser(
	ctx context.Context,
	code string,
	userID int64,
	isAdmin bool,
) (sqlc.GetBookingDetailByCodeRow, error) {
	row, err := b.store.Queries.GetBookingDetailByCode(ctx, code)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.GetBookingDetailByCodeRow{}, ErrNotFound
	}
	if err != nil {
		return sqlc.GetBookingDetailByCodeRow{}, fmt.Errorf("getting booking %q: %w", code, err)
	}
	if row.UserID != userID && !isAdmin {
		return sqlc.GetBookingDetailByCodeRow{}, ErrNotFound
	}
	return row, nil
}

// Validate parses and checks the form without writing anything, for the review
// step. Create runs the same checks again, so a review that passed is not a
// promise the commit will.
func (b *Booking) Validate(ctx context.Context, in CreateInput) (sqlc.Service, sqlc.ScheduleSlot, error) {
	p, ve, err := b.validate(ctx, in)
	if err != nil {
		return sqlc.Service{}, sqlc.ScheduleSlot{}, err
	}
	if ve != nil {
		return sqlc.Service{}, sqlc.ScheduleSlot{}, ve
	}
	return p.service, p.slot, nil
}

// validate applies every field rule at once so one submit reports every problem.
//
// The third return is for infrastructure failures — a lookup that could not run
// at all — which are not the user's fault and must not be rendered as a field
// message.
func (b *Booking) validate(ctx context.Context, in CreateInput) (parsedBooking, *ValidationError, error) {
	ve := NewValidationError()
	var p parsedBooking

	// Layanan.
	slug := strings.TrimSpace(in.ServiceSlug)
	if slug == "" {
		ve.Add("layanan", "Pilih layanan terlebih dahulu.")
	} else {
		svc, err := b.BookableBySlug(ctx, slug)
		switch {
		case errors.Is(err, ErrNotFound):
			ve.Add("layanan", "Layanan tidak tersedia untuk booking. Pilih layanan lain.")
		case err != nil:
			return parsedBooking{}, nil, err
		default:
			p.service = svc
		}
	}

	// Jadwal.
	raw := strings.TrimSpace(in.SlotID)
	switch id, err := strconv.ParseInt(raw, 10, 64); {
	case raw == "":
		ve.Add("slot", "Pilih jadwal terlebih dahulu.")
	case err != nil || id <= 0:
		ve.Add("slot", "Jadwal tidak valid. Pilih jadwal dari daftar.")
	default:
		slot, gerr := b.store.Queries.GetSlot(ctx, id)
		switch {
		case errors.Is(gerr, sql.ErrNoRows):
			ve.Add("slot", "Jadwal tidak ditemukan. Pilih jadwal lain.")
		case gerr != nil:
			return parsedBooking{}, nil, fmt.Errorf("getting slot %d: %w", id, gerr)
		default:
			if msg := b.slotMessage(slot); msg != "" {
				ve.Add("slot", msg)
			} else {
				p.slot = slot
			}
		}
	}

	// Nama.
	p.name = strings.TrimSpace(in.Name)
	requiredMaxLen(ve, "nama", "Nama", p.name, maxCustomerNameLen)

	// Telepon.
	p.phone = strings.TrimSpace(in.Phone)
	if requiredMaxLen(ve, "telepon", "Nomor telepon", p.phone, maxCustomerPhoneLen) {
		phone(ve, "telepon", p.phone)
	}

	// Alamat — required. The therapist travels to the customer, so this is the
	// destination, not context: a booking without one cannot be carried out.
	addr := strings.TrimSpace(in.Address)
	if requiredMaxLen(ve, "alamat", "Alamat", addr, maxCustomerAddressLen) {
		p.address = sql.NullString{String: addr, Valid: true}
	}

	// Catatan — optional.
	if notes := strings.TrimSpace(in.Notes); notes != "" {
		if maxLen(ve, "catatan", "Catatan", notes, maxNotesLen) {
			p.notes = sql.NullString{String: notes, Valid: true}
		}
	}

	if ve.Any() {
		return parsedBooking{}, ve, nil
	}
	return p, nil, nil
}

// slotMessage returns why a slot cannot be booked, or "" when it can.
//
// Used twice: once in validate, to put the reason beside the picker, and once
// inside the transaction, where the same conditions are re-read under the row
// lock. Keeping it in one function is what stops the two from drifting into
// disagreement about, say, whether the lead time is inclusive.
func (b *Booking) slotMessage(slot sqlc.ScheduleSlot) string {
	startsAt, until := b.schedule.Window()

	switch {
	case !slot.IsActive:
		return "Jadwal tersebut sudah tidak tersedia. Pilih jadwal lain."
	case slot.BookedCount >= slot.Capacity:
		return "Jadwal tersebut sudah penuh. Pilih jadwal lain."
	case slot.StartsAt.Before(startsAt):
		return "Jadwal tersebut terlalu dekat atau sudah lewat. Pilih jadwal lain."
	case slot.SlotDate.After(until):
		return "Jadwal tersebut terlalu jauh ke depan. Pilih jadwal lain."
	}
	return ""
}

// Create books a slot.
//
// THE transaction. Statement order is not stylistic — it is the canonical lock
// order (schedule_slots -> bookings -> payments, schedules.sql:4-8) and the rule
// that the slot's FOR UPDATE is taken BEFORE the capacity check. A second
// request for the same capacity-1 slot blocks at step 2 until the first commits,
// and then correctly sees the slot as full.
//
// Everything validate() already checked is checked again here, against rows read
// inside the lock. The earlier pass exists to produce good messages; this one is
// what is actually true.
func (b *Booking) Create(ctx context.Context, in CreateInput) (sqlc.Booking, error) {
	p, ve, err := b.validate(ctx, in)
	if err != nil {
		return sqlc.Booking{}, err
	}
	if ve != nil {
		return sqlc.Booking{}, ve
	}

	// One retry on a deadlock or lock-wait timeout. The expiry ticker takes the
	// same slot locks, so the two can collide under load; InnoDB rolls the loser
	// back whole, which makes a retry safe rather than merely hopeful.
	var booking sqlc.Booking
	for attempt := range 2 {
		booking, err = b.create(ctx, in.UserID, p)
		if err == nil {
			// After the commit, never inside it: the transaction above holds a slot
			// lock, and nothing that talks to another machine belongs under one.
			b.email.Notify(ctx, EmailBookingCreated, booking.ID)
			return booking, nil
		}
		if attempt == 0 && repository.IsRetryable(err) {
			continue
		}
		return sqlc.Booking{}, err
	}
	return sqlc.Booking{}, err
}

func (b *Booking) create(ctx context.Context, userID int64, p parsedBooking) (sqlc.Booking, error) {
	expiresAt := time.Now().In(b.loc).Add(b.expiry())

	var booking sqlc.Booking

	err := b.store.WithTx(ctx, func(q *sqlc.Queries) error {
		// 1. THE LOCK — literally the first statement, before the capacity check
		//    and before every other read.
		//
		//    "First" is not stylistic and it is not only about lock ordering. Under
		//    REPEATABLE READ the transaction's snapshot is established by its first
		//    consistent read, and a locking read that then finds a row newer than
		//    that snapshot fails outright: MariaDB raises ER_CHECKREAD (1020,
		//    "Record has changed since last read"). A plain SELECT of the layanan
		//    placed above this line takes no lock and looks harmless, but it sets
		//    the snapshot — and every request that queued behind the winner then
		//    died with a 500 instead of a friendly "slot baru saja terisi". That
		//    was measured, not theorised: 19 of 20 racers hit it.
		slot, err := q.GetSlotForUpdate(ctx, p.slot.ID)
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted between render and submit. Same remedy as a full slot: pick
			// another one. A 404 page would be a worse answer to "it just went away".
			return ErrSlotTaken
		}
		if err != nil {
			return fmt.Errorf("locking slot %d: %w", p.slot.ID, err)
		}

		// 2. Every slot condition re-checked, now against the locked row.
		if b.slotMessage(slot) != "" {
			return ErrSlotTaken
		}

		// 3. The layanan, re-read inside the transaction so an admin deactivating
		//    it mid-flow is caught. Safe here, after the lock, for the reason
		//    spelled out above.
		svc, err := q.GetService(ctx, p.service.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotBookable
		}
		if err != nil {
			return fmt.Errorf("re-reading service %d: %w", p.service.ID, err)
		}
		if !svc.IsActive || svc.IsComingSoon {
			return ErrNotBookable
		}

		// 4. The increment. Its WHERE clause is defence in depth — the lock above is
		//    what makes this correct — but a zero here means the row moved anyway,
		//    and the booking must not be written.
		res, err := q.HoldSlot(ctx, slot.ID)
		if err != nil {
			return fmt.Errorf("holding slot %d: %w", slot.ID, err)
		}
		held, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading hold result for slot %d: %w", slot.ID, err)
		}
		if held != 1 {
			return ErrSlotTaken
		}

		// 5. The booking. price_amount is snapshotted from the service read in this
		//    same transaction: a later price edit must not change what a past
		//    customer agreed to pay.
		id, err := insertBooking(ctx, q, sqlc.CreateBookingParams{
			UserID:          userID,
			ServiceID:       svc.ID,
			SlotID:          slot.ID,
			CustomerName:    p.name,
			CustomerPhone:   p.phone,
			CustomerAddress: p.address,
			Notes:           p.notes,
			PriceAmount:     svc.Price,
			ExpiresAt:       sql.NullTime{Time: expiresAt, Valid: true},
		}, slot.SlotDate)
		if err != nil {
			return err
		}

		booking, err = q.GetBooking(ctx, id)
		if err != nil {
			return fmt.Errorf("re-reading booking %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return sqlc.Booking{}, err
	}
	return booking, nil
}

// insertBooking allocates a booking code and inserts the row, retrying the code
// on a collision.
//
// The collision retry stays inside the transaction because InnoDB rolls back the
// failed statement, not the transaction — so the slot hold taken above survives,
// and a second code can be tried without giving the slot up and re-acquiring it.
func insertBooking(ctx context.Context, q *sqlc.Queries, arg sqlc.CreateBookingParams, day time.Time) (int64, error) {
	for range maxCodeAttempts {
		code, err := newBookingCode(day)
		if err != nil {
			return 0, fmt.Errorf("generating booking code: %w", err)
		}

		// The friendly check. It takes no lock, so uq_bookings_code below is what
		// actually guarantees uniqueness; this only keeps the common case off the
		// error path.
		taken, err := q.BookingCodeTaken(ctx, code)
		if err != nil {
			return 0, fmt.Errorf("checking booking code: %w", err)
		}
		if taken {
			continue
		}

		arg.BookingCode = code
		res, err := q.CreateBooking(ctx, arg)
		switch {
		case err == nil:
			id, lerr := res.LastInsertId()
			if lerr != nil {
				return 0, fmt.Errorf("reading booking id: %w", lerr)
			}
			return id, nil

		case repository.IsDuplicateKeyOn(err, "uq_bookings_active_slot_user"):
			// The second line of defence firing: this user already holds a live
			// booking for this slot. A double-submitted form reaches here even if
			// every check in Go were removed.
			return 0, ErrDuplicateBooking

		case repository.IsDuplicateKeyOn(err, "uq_bookings_code"):
			continue

		default:
			return 0, fmt.Errorf("creating booking: %w", err)
		}
	}
	return 0, errors.New("creating booking: no free booking code after retries")
}

// expiry is how long a new booking holds its slot unpaid.
func (b *Booking) expiry() time.Duration {
	fallback := b.expiryFallbackMinutes
	if fallback <= 0 {
		fallback = defaultExpiryMinutes
	}
	minutes := b.settings.Int(KeyPaymentExpiryMinutes, fallback)
	if minutes <= 0 {
		minutes = fallback
	}
	return time.Duration(minutes) * time.Minute
}

// Sweep bounds, keeping one expiry pass finite.
const (
	// expireBatch is how many expired bookings one query returns.
	expireBatch = 100

	// maxSweepPasses bounds the loop. A booking that fails to expire would
	// otherwise be returned by every pass forever, since its status never moves.
	maxSweepPasses = 20
)

// Expire releases the slots of unpaid bookings that ran out of time, and reports
// how many it released.
//
// The lock order is the whole point of the shape below. ListExpiredPendingBookings
// deliberately returns slot_id so this can lock the slot FIRST — a ticker that
// locked the booking and then reached for the slot would deadlock against
// Create, which locks them the other way round.
//
// One transaction per booking, so a single bad row cannot block the batch. And
// the release is authorised by exactly one thing: ExpireBooking reporting one
// affected row. Zero means someone else already moved this booking — the
// Midtrans webhook in Phase 8, or the user cancelling — and the slot has already
// been given back by whoever did. Releasing again would drive booked_count below
// the truth.
func (b *Booking) Expire(ctx context.Context) (int, error) {
	released := 0

	for range maxSweepPasses {
		rows, err := b.store.Queries.ListExpiredPendingBookings(ctx, expireBatch)
		if err != nil {
			return released, fmt.Errorf("listing expired bookings: %w", err)
		}
		if len(rows) == 0 {
			return released, nil
		}

		for _, row := range rows {
			did, err := b.expireOne(ctx, row.ID, row.SlotID)
			if err != nil {
				return released, err
			}
			if did {
				// Only on a real transition. RowsAffected() == 1 inside expireOne is
				// what says this ticker, rather than the webhook or the customer, is
				// the one that ended this booking — and so the one that should say so.
				b.email.Notify(ctx, EmailBookingExpired, row.ID)
				released++
			}
		}

		if len(rows) < expireBatch {
			return released, nil
		}
	}
	return released, nil
}

// ---------------------------------------------------------------------------
// Riwayat (Phase 9)
// ---------------------------------------------------------------------------

// allBookingStatuses is the FIND_IN_SET argument meaning "no filter", spelled
// out because ListBookingsByUser takes a comma-joined list rather than a
// nullable predicate — sqlc's MySQL engine has no sqlc.slice().
const allBookingStatuses = "pending_payment,paid,confirmed,completed,cancelled,expired"

// bookingStatusFilters is the whitelist a request's ?status= is resolved through.
//
// The value reaches SQL as a bind parameter, so this is not about injection. It
// is about what an unrecognised value should do: mapping through a whitelist
// means a hand-typed or stale query string falls back to "everything", where
// passing it through would silently return an empty list that looks like "you
// have no bookings".
var bookingStatusFilters = map[string]string{
	string(sqlc.BookingsStatusPendingPayment): string(sqlc.BookingsStatusPendingPayment),
	string(sqlc.BookingsStatusPaid):           string(sqlc.BookingsStatusPaid),
	string(sqlc.BookingsStatusConfirmed):      string(sqlc.BookingsStatusConfirmed),
	string(sqlc.BookingsStatusCompleted):      string(sqlc.BookingsStatusCompleted),
	string(sqlc.BookingsStatusCancelled):      string(sqlc.BookingsStatusCancelled),
	string(sqlc.BookingsStatusExpired):        string(sqlc.BookingsStatusExpired),
}

// BookingStatusFilter resolves a request's filter value to the status list the
// query wants, and reports whether it named a real status. The handler needs the
// second return to decide which empty state to render: "no bookings yet" and
// "none with this status" want different copy and different call to action.
func BookingStatusFilter(status string) (list string, filtered bool) {
	if s, ok := bookingStatusFilters[status]; ok {
		return s, true
	}
	return allBookingStatuses, false
}

// BookingHistoryQuery selects one page of a user's bookings.
type BookingHistoryQuery struct {
	UserID int64
	// Status is the raw request value. Anything unrecognised means "all".
	Status   string
	Page     int
	PageSize int
}

// BookingHistoryResult is that page plus what the pagination partial needs.
//
// Page and TotalPages are int, not the int32 sqlc uses, because the partial
// reaches them through `add`, which is func(int, int) int.
type BookingHistoryResult struct {
	Items      []sqlc.ListBookingsByUserRow
	Page       int
	TotalPages int
	Total      int64
}

// History is the riwayat list: one user's bookings, newest first.
//
// Same shape as Catalog.List — count, clamp, then read — because the pagination
// partial is the same one and the clamping rule is what stops a stale ?page=
// from rendering an empty table with a working "next".
func (b *Booking) History(ctx context.Context, q BookingHistoryQuery) (BookingHistoryResult, error) {
	if q.PageSize <= 0 {
		// Defensive only: the handler passes cfg.App.PageSize. Same fallback as
		// Catalog.List, so the two lists page identically.
		q.PageSize = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}

	statusList, _ := BookingStatusFilter(q.Status)

	total, err := b.store.Queries.CountBookingsByUser(ctx, sqlc.CountBookingsByUserParams{
		UserID:     q.UserID,
		StatusList: statusList,
	})
	if err != nil {
		return BookingHistoryResult{}, fmt.Errorf("counting bookings for user %d: %w", q.UserID, err)
	}

	totalPages := int((total + int64(q.PageSize) - 1) / int64(q.PageSize))
	// Clamp after counting: showing the last page beats showing an empty one.
	if totalPages > 0 && q.Page > totalPages {
		q.Page = totalPages
	}

	items, err := b.store.Queries.ListBookingsByUser(ctx, sqlc.ListBookingsByUserParams{
		UserID:     q.UserID,
		StatusList: statusList,
		Limit:      int32(q.PageSize),
		Offset:     int32((q.Page - 1) * q.PageSize),
	})
	if err != nil {
		return BookingHistoryResult{}, fmt.Errorf("listing bookings for user %d: %w", q.UserID, err)
	}

	return BookingHistoryResult{
		Items:      items,
		Page:       q.Page,
		TotalPages: totalPages,
		Total:      total,
	}, nil
}

// ---------------------------------------------------------------------------
// User-initiated cancellation (Phase 9)
// ---------------------------------------------------------------------------

// cancelReasonByUser is what a customer cancellation records.
//
// Fixed copy rather than a free-text field: the cancellation runs from the
// confirm partial, whose form carries only the CSRF token, so there is nowhere
// to type a reason. Fits bookings.cancelled_reason VARCHAR(255) with room to
// spare.
const cancelReasonByUser = "Dibatalkan oleh pelanggan"

// CancelForUser cancels the caller's own pending_payment booking and gives the
// slot back. It returns the booking's id — so the caller can cancel the Midtrans
// order that may still be open against it — and whether a real transition
// happened.
//
// There is deliberately no isAdmin parameter, unlike DetailForUser. An admin
// cancelling someone else's booking is Phase 10's job and wants a reason, an
// audit entry and the wider CancelBooking guard; passing userID straight into the
// WHERE here is what makes ownership the database's guarantee rather than the
// handler's.
//
// moved == false is a success, not a failure: it means the expiry ticker or the
// Midtrans webhook moved this booking first, and whoever did has already released
// the slot.
func (b *Booking) CancelForUser(ctx context.Context, code string, userID int64) (bookingID int64, moved bool, err error) {
	// Resolved outside the transaction purely to learn slot_id, the same shape
	// ListExpiredPendingBookings gives the expiry ticker: the slot lock has to be
	// the transaction's first statement, so the slot has to be known before it
	// opens.
	row, err := b.store.Queries.GetBookingByCode(ctx, code)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("getting booking %q: %w", code, err)
	}

	// Someone else's booking is a 404, never a 403 — the same rule DetailForUser
	// carries, for the same reason: a 403 confirms that a guessed code is real.
	if row.UserID != userID {
		return 0, false, ErrNotFound
	}
	// Pre-flight only, for the message. The guard that decides anything is the
	// status predicate inside CancelPendingBookingByOwner.
	if row.Status != sqlc.BookingsStatusPendingPayment {
		return row.ID, false, ErrNotPayable
	}

	err = b.store.WithTx(ctx, func(q *sqlc.Queries) error {
		// Slot lock first, and "first" is not only about lock order: a plain SELECT
		// ahead of it establishes the REPEATABLE READ snapshot, and a locking read
		// that then meets a newer row fails with ER_CHECKREAD (1020) instead of
		// blocking. Its value is not read — the lock itself is the point.
		if _, err := q.GetSlotForUpdate(ctx, row.SlotID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// fk_bookings_slot is RESTRICT, so this should be unreachable. Leave
				// the booking alone rather than cancelling it without being able to
				// release anything.
				return nil
			}
			return fmt.Errorf("locking slot %d: %w", row.SlotID, err)
		}

		res, err := q.CancelPendingBookingByOwner(ctx, sqlc.CancelPendingBookingByOwnerParams{
			CancelledReason: nullString(cancelReasonByUser),
			ID:              row.ID,
			UserID:          userID,
		})
		if err != nil {
			return fmt.Errorf("cancelling booking %d: %w", row.ID, err)
		}
		did, err := changed(res)
		if err != nil {
			return fmt.Errorf("reading cancellation result for booking %d: %w", row.ID, err)
		}
		if !did {
			// Already moved by the ticker or the webhook. Success, and nothing to
			// release: whoever moved it gave the slot back.
			return nil
		}

		if err := q.ReleaseSlot(ctx, row.SlotID); err != nil {
			return fmt.Errorf("releasing slot %d: %w", row.SlotID, err)
		}
		moved = true
		return nil
	})
	if err != nil {
		return row.ID, false, err
	}

	if moved {
		// Only when this call is the one that cancelled it. moved == false means
		// the ticker or the webhook got there first, and that path sends its own
		// mail — sending here too would tell the customer twice.
		//
		// wasPaid is false by construction: CancelPendingBookingByOwner is guarded
		// on pending_payment, so a booking that reaches here was never paid for.
		b.email.NotifyCancelled(ctx, row.ID, false)
	}
	return row.ID, moved, nil
}

func (b *Booking) expireOne(ctx context.Context, bookingID, slotID int64) (bool, error) {
	moved := false

	err := b.store.WithTx(ctx, func(q *sqlc.Queries) error {
		// Slot lock first. Its value is not read — the lock itself is the point.
		if _, err := q.GetSlotForUpdate(ctx, slotID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// fk_bookings_slot is RESTRICT, so a slot with bookings cannot be
				// deleted and this should be unreachable. Leave the booking alone
				// rather than expiring it without being able to release anything.
				return nil
			}
			return fmt.Errorf("locking slot %d: %w", slotID, err)
		}

		res, err := q.ExpireBooking(ctx, bookingID)
		if err != nil {
			return fmt.Errorf("expiring booking %d: %w", bookingID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading expiry result for booking %d: %w", bookingID, err)
		}
		if n != 1 {
			// Already moved by someone else. Success, and nothing to release.
			return nil
		}

		if err := q.ReleaseSlot(ctx, slotID); err != nil {
			return fmt.Errorf("releasing slot %d: %w", slotID, err)
		}
		moved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
}

// newBookingCode builds a code of the form TPJ-20260727-A1B2.
//
// The date is the slot's date rather than today's, so a code read aloud over the
// phone says when the customer is coming.
func newBookingCode(day time.Time) (string, error) {
	suffix, err := randomCode(codeRandomLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("TPJ-%s-%s", day.Format("20060102"), suffix), nil
}

// codeAlphabet omits 0, 1, O, I and L. A booking code gets read over the phone
// and typed back in; those five are the pairs that get confused doing it.
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// randomCode returns n characters drawn uniformly from codeAlphabet.
//
// Bytes at or above 248 (the largest multiple of 31 below 256) are discarded
// rather than folded, so no character is more likely than another. crypto/rand
// rather than math/rand because a guessable booking code is a way to read
// someone else's booking.
func randomCode(n int) (string, error) {
	const limit = 248 // 8 * len(codeAlphabet)

	out := make([]byte, 0, n)
	buf := make([]byte, n)

	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, c := range buf {
			if c >= limit {
				continue
			}
			out = append(out, codeAlphabet[int(c)%len(codeAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
