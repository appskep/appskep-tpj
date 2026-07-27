package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Schedule is the business logic behind the admin penjadwalan module: the rules
// that decide whether a slot may exist at all, and the generator that fills a
// date range with them.
//
// As with Catalog, validation lives here rather than in the handler so that the
// single-slot form and the generator apply the same rules to the same values —
// two copies of "durasi 15–480" would eventually disagree, and the generator is
// precisely where a disagreement would be multiplied by three hundred rows.
//
// Everything this type writes is bounded by the schema's own constraints
// (uq_slots_date_start, chk_slots_window, chk_slots_count). The checks here
// exist so the admin gets a message beside the field instead of a 500 from a
// constraint violation — the constraints remain what actually enforces the
// rules, because a check in Go takes no lock.
type Schedule struct {
	store    *repository.Store
	settings *Settings
	// loc is the application timezone. Every date this type parses or builds is
	// constructed in it, so a DATE column round-trips to the same calendar day it
	// left as — the driver loc is pinned to the same zone (config.DBConfig.DSN).
	loc *time.Location
}

func NewSchedule(store *repository.Store, settings *Settings, loc *time.Location) *Schedule {
	return &Schedule{store: store, settings: settings, loc: loc}
}

// Bounds, mirroring the schema and keeping one generator run finite.
const (
	// MinSlotMinutes and MaxSlotMinutes bound a single slot's length. They match
	// the layanan duration bounds because a slot exists to hold one service.
	MinSlotMinutes = 15
	MaxSlotMinutes = 480

	// MinCapacity and MaxCapacity bound parallel bookings in one slot. Capacity
	// models parallel therapists (0001_schema.sql), so the ceiling is a sanity
	// bound on a typo, not a business limit.
	MinCapacity = 1
	MaxCapacity = 20

	// MaxBreakMinutes bounds the gap between generated slots.
	MaxBreakMinutes = 240

	// maxRangeDays bounds any admin range read. ListRange filters by status in Go,
	// so the range has to be small enough that pulling it whole is cheap.
	maxRangeDays = 92

	// maxGenerateDays and maxGeneratedSlots bound one generator run. PLAN.md
	// Phase 5 requires "a guard on max generated rows": without it a mistyped year
	// in the end date turns one submit into a hundred thousand inserts inside a
	// single transaction.
	maxGenerateDays   = 180
	maxGeneratedSlots = 500

	// maxNoteLen matches schedule_slots.note VARCHAR(255).
	maxNoteLen = 255

	// dateLayout is what <input type="date"> submits and what the filters carry.
	dateLayout = "2006-01-02"
)

// Status filter values. They appear in query strings, so they are Indonesian
// like the rest of the admin URLs.
const (
	StatusAll      = ""
	StatusActive   = "aktif"
	StatusInactive = "nonaktif"
	StatusFull     = "penuh"
	StatusPast     = "lewat"
)

// SlotInput is one submitted slot form, still as raw strings — the same shape
// ServiceInput has, and for the same reason: the service owns the parsing rules
// and the messages that describe them.
type SlotInput struct {
	Date      string
	StartTime string
	EndTime   string
	Capacity  string
	Note      string
	IsActive  bool
}

// parsedSlot is a validated SlotInput in the types the database wants.
type parsedSlot struct {
	date     time.Time
	start    string // "HH:MM:SS", as a MySQL TIME column takes
	end      string
	capacity int32
	note     sql.NullString
}

// Get returns one slot, or ErrNotFound.
func (s *Schedule) Get(ctx context.Context, id int64) (sqlc.ScheduleSlot, error) {
	slot, err := s.store.Queries.GetSlot(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.ScheduleSlot{}, ErrNotFound
	}
	if err != nil {
		return sqlc.ScheduleSlot{}, fmt.Errorf("getting slot %d: %w", id, err)
	}
	return slot, nil
}

// Locked reports whether a slot's date and times may still be changed.
//
// A slot with bookings holds a promise already made to a customer: moving it
// would reschedule them silently, with no notification and no record that the
// time they agreed to was ever different. Capacity and note stay editable, and
// per-booking reschedule is Phase 10's job with a flow of its own.
func Locked(slot sqlc.ScheduleSlot) bool { return slot.BookedCount > 0 }

// RangeQuery is one filtered window of the admin list.
type RangeQuery struct {
	From, To time.Time
	Status   string
}

// DayGroup is one date's slots, so the list renders a date heading once rather
// than repeating it on every row.
type DayGroup struct {
	Date  time.Time
	Slots []sqlc.ScheduleSlot
}

// RangeResult is the filtered window plus the totals the page reports.
type RangeResult struct {
	Days     []DayGroup
	From, To time.Time
	Total    int
	Clamped  bool
}

// ListRange returns the slots in a date window, grouped by date and filtered by
// status.
//
// The status filter is applied in Go rather than in SQL. sqlc cannot express a
// predicate that changes shape per request without a query per combination, and
// the window is bounded to maxRangeDays — a few hundred rows — so filtering
// after the read costs nothing and keeps one query serving every filter.
func (s *Schedule) ListRange(ctx context.Context, q RangeQuery) (RangeResult, error) {
	from := s.dayOf(q.From)
	to := s.dayOf(q.To)

	if to.Before(from) {
		to = from
	}
	clamped := false
	if limit := from.AddDate(0, 0, maxRangeDays); to.After(limit) {
		to = limit
		clamped = true
	}

	slots, err := s.store.Queries.ListSlotsByDateRange(ctx, sqlc.ListSlotsByDateRangeParams{
		FromSlotDate: from,
		ToSlotDate:   to,
	})
	if err != nil {
		return RangeResult{}, fmt.Errorf("listing slots %s..%s: %w",
			from.Format(dateLayout), to.Format(dateLayout), err)
	}

	now := time.Now().In(s.loc)
	res := RangeResult{From: from, To: to, Clamped: clamped}

	// The query already orders by date then start time, so a group closes as soon
	// as the date changes and no sorting is needed here.
	for _, slot := range slots {
		if !matchesStatus(slot, q.Status, now) {
			continue
		}
		res.Total++

		n := len(res.Days)
		if n == 0 || !res.Days[n-1].Date.Equal(slot.SlotDate) {
			res.Days = append(res.Days, DayGroup{Date: slot.SlotDate})
			n++
		}
		res.Days[n-1].Slots = append(res.Days[n-1].Slots, slot)
	}

	return res, nil
}

// matchesStatus applies one status filter to one slot. "penuh" and "lewat"
// describe a slot's state rather than a column, which is why this is a function
// and not a WHERE clause.
func matchesStatus(slot sqlc.ScheduleSlot, status string, now time.Time) bool {
	switch status {
	case StatusActive:
		return slot.IsActive
	case StatusInactive:
		return !slot.IsActive
	case StatusFull:
		return slot.BookedCount >= slot.Capacity
	case StatusPast:
		return slot.StartsAt.Before(now)
	default:
		return true
	}
}

// DayCell is one square of the month grid.
type DayCell struct {
	Date time.Time
	Day  int
	// Outside marks a neighbouring month's day, shown to complete the week.
	Outside  bool
	IsToday  bool
	IsPast   bool
	Total    int
	Booked   int
	Capacity int
	// Inactive counts deactivated slots, so a day that looks scheduled but is
	// switched off does not read as available.
	Inactive int
}

// Full reports a day with no free places left, which the grid tints differently
// from a day that simply has no slots.
func (c DayCell) Full() bool { return c.Total > 0 && c.Booked >= c.Capacity }

// MonthView is the calendar: whole weeks, Monday first.
type MonthView struct {
	Anchor time.Time
	Weeks  [][]DayCell
	Prev   string // "2006-01" for the month links
	Next   string
	Total  int
}

// Month builds the calendar grid for the month containing anchor.
//
// Weeks start on Monday because the working week does; time.Weekday starts on
// Sunday, which is the one place that difference has to be handled, and it is
// handled here rather than in the template.
func (s *Schedule) Month(ctx context.Context, anchor time.Time) (MonthView, error) {
	first := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, s.loc)
	last := first.AddDate(0, 1, -1)

	// Back up to the Monday on or before the 1st, then forward to the Sunday on or
	// after the last — the grid always holds whole weeks.
	gridStart := first.AddDate(0, 0, -mondayOffset(first.Weekday()))
	gridEnd := last.AddDate(0, 0, 6-mondayOffset(last.Weekday()))

	slots, err := s.store.Queries.ListSlotsByDateRange(ctx, sqlc.ListSlotsByDateRangeParams{
		FromSlotDate: gridStart,
		ToSlotDate:   gridEnd,
	})
	if err != nil {
		return MonthView{}, fmt.Errorf("listing slots for %s: %w", first.Format("2006-01"), err)
	}

	// One pass into a per-date bucket, keyed by the formatted date rather than the
	// time.Time: two values for the same calendar day are only equal if their
	// wall clock and location match exactly, and the key sidesteps that entirely.
	type agg struct{ total, booked, capacity, inactive int }
	byDate := make(map[string]*agg, len(slots))
	for _, slot := range slots {
		key := slot.SlotDate.Format(dateLayout)
		a := byDate[key]
		if a == nil {
			a = &agg{}
			byDate[key] = a
		}
		a.total++
		a.booked += int(slot.BookedCount)
		a.capacity += int(slot.Capacity)
		if !slot.IsActive {
			a.inactive++
		}
	}

	today := s.dayOf(time.Now().In(s.loc))
	view := MonthView{
		Anchor: first,
		Prev:   first.AddDate(0, -1, 0).Format("2006-01"),
		Next:   first.AddDate(0, 1, 0).Format("2006-01"),
	}

	for d := gridStart; !d.After(gridEnd); d = d.AddDate(0, 0, 7) {
		week := make([]DayCell, 0, 7)
		for i := range 7 {
			day := d.AddDate(0, 0, i)
			cell := DayCell{
				Date:    day,
				Day:     day.Day(),
				Outside: day.Month() != first.Month(),
				IsToday: day.Equal(today),
				IsPast:  day.Before(today),
			}
			if a := byDate[day.Format(dateLayout)]; a != nil {
				cell.Total, cell.Booked, cell.Capacity, cell.Inactive = a.total, a.booked, a.capacity, a.inactive
				if !cell.Outside {
					view.Total += a.total
				}
			}
			week = append(week, cell)
		}
		view.Weeks = append(view.Weeks, week)
	}

	return view, nil
}

// mondayOffset converts a time.Weekday into "days since Monday", which is what a
// Monday-first grid indexes on. time.Sunday is 0, so it is the one value that
// does not simply shift down by one.
func mondayOffset(w time.Weekday) int {
	if w == time.Sunday {
		return 6
	}
	return int(w) - 1
}

// Upcoming availability, for the public layanan detail page.
const (
	// maxUpcomingTimes caps how many start times one day shows before the rest
	// become a "+N lagi" count. A generous schedule would otherwise turn the
	// section into a wall of times on a phone.
	maxUpcomingTimes = 4

	// defaultLeadMinutes and defaultDaysAhead back the settings rows up. They are
	// the seeded values, so a settings table missing the key behaves like the
	// shipped default rather than like "no lead time at all".
	defaultLeadMinutes = 120
	defaultDaysAhead   = 30
)

// Window is the range a visitor may book into: nothing sooner than the lead time
// from now, nothing later than the max-days-ahead setting.
//
// It exists as one function because two callers must agree on it. The layanan
// detail page advertises availability with it and the Phase 7 booking
// transaction re-checks against it under the row lock; if they diverged, the
// preview would offer a date the form then refuses, or — worse — the form would
// accept a slot the lead time was meant to protect.
//
// startsAt is a wall-clock instant compared against schedule_slots.starts_at.
// until is a date compared against slot_date, so it is the last bookable day
// inclusive, not an instant.
func (s *Schedule) Window() (startsAt, until time.Time) {
	lead := s.settings.Int(KeyBookingLeadMinutes, defaultLeadMinutes)
	ahead := s.settings.Int(KeyBookingMaxDaysAhead, defaultDaysAhead)

	startsAt = time.Now().In(s.loc).Add(time.Duration(lead) * time.Minute)
	until = s.Today().AddDate(0, 0, ahead)
	return startsAt, until
}

// UpcomingDay is one date that still has free slots.
type UpcomingDay struct {
	Date  time.Time
	Free  int64
	Times []string
	// More is how many start times were dropped past maxUpcomingTimes.
	More int
}

// Upcoming returns the nearest maxDays dates that still have a free slot, each
// with its first few start times.
//
// Read-only and advisory: it is what the detail page shows to answer "when can I
// come", not a reservation. Phase 7's booking transaction re-checks everything
// under a row lock, so a slot listed here filling up between the render and the
// submit is expected and handled there.
//
// The same two settings the booking flow will use bound the window, so the
// preview cannot advertise a date the booking form would then refuse.
func (s *Schedule) Upcoming(ctx context.Context, maxDays int) ([]UpcomingDay, error) {
	if maxDays <= 0 {
		return nil, nil
	}

	startsAt, until := s.Window()

	dates, err := s.store.Queries.ListAvailableDates(ctx, sqlc.ListAvailableDatesParams{
		StartsAt: startsAt,
		SlotDate: until,
	})
	if err != nil {
		return nil, fmt.Errorf("listing available dates: %w", err)
	}
	if len(dates) > maxDays {
		dates = dates[:maxDays]
	}

	days := make([]UpcomingDay, 0, len(dates))
	for _, d := range dates {
		slots, err := s.store.Queries.ListAvailableSlotsByDate(ctx, sqlc.ListAvailableSlotsByDateParams{
			SlotDate: d.SlotDate,
			StartsAt: startsAt,
		})
		if err != nil {
			return nil, fmt.Errorf("listing slots for %s: %w", d.SlotDate.Format(dateLayout), err)
		}

		day := UpcomingDay{Date: d.SlotDate, Free: d.FreeSlots}
		for _, slot := range slots {
			if len(day.Times) == maxUpcomingTimes {
				day.More = len(slots) - maxUpcomingTimes
				break
			}
			day.Times = append(day.Times, util.TimeRange(slot.StartTime, slot.EndTime))
		}
		days = append(days, day)
	}
	return days, nil
}

// Create validates the form and inserts one slot.
func (s *Schedule) Create(ctx context.Context, in SlotInput) (int64, error) {
	p, ve := s.validate(in, nil)
	if ve != nil {
		return 0, ve
	}

	res, err := s.store.Queries.CreateSlot(ctx, sqlc.CreateSlotParams{
		SlotDate:  p.date,
		StartTime: p.start,
		EndTime:   p.end,
		Capacity:  p.capacity,
		IsActive:  in.IsActive,
		Note:      p.note,
		// service_id stays NULL: every slot is global until PLAN.md Q4 flips, and
		// uq_slots_date_start is keyed on (slot_date, start_time) for exactly that
		// reason — see the note in 0001_schema.sql before changing this.
	})
	if err != nil {
		if converted := slotConstraintError(err); converted != nil {
			return 0, converted
		}
		return 0, fmt.Errorf("creating slot: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("creating slot: %w", err)
	}
	return id, nil
}

// Update validates the form and saves one slot.
//
// A slot that already has bookings keeps its date and times whatever the
// submission says. The form renders those inputs disabled, but a disabled input
// is an affordance rather than a guarantee — a hand-written POST must not be
// able to move a slot out from under a customer who has paid for it.
func (s *Schedule) Update(ctx context.Context, id int64, in SlotInput) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	p, ve := s.validate(in, &current)
	if ve != nil {
		return ve
	}

	if err := s.store.Queries.UpdateSlot(ctx, sqlc.UpdateSlotParams{
		SlotDate:  p.date,
		StartTime: p.start,
		EndTime:   p.end,
		Capacity:  p.capacity,
		IsActive:  in.IsActive,
		Note:      p.note,
		ID:        id,
	}); err != nil {
		if converted := slotConstraintError(err); converted != nil {
			return converted
		}
		return fmt.Errorf("updating slot %d: %w", id, err)
	}
	return nil
}

// validate applies every rule at once so one submit reports every problem.
//
// current is nil when creating. When it is a slot that already holds bookings,
// the date and times are taken from it and their rules are skipped entirely —
// the form does not submit disabled inputs, so validating them would reject a
// perfectly good capacity change with "Tanggal wajib diisi."
func (s *Schedule) validate(in SlotInput, current *sqlc.ScheduleSlot) (parsedSlot, *ValidationError) {
	ve := NewValidationError()
	var p parsedSlot

	if current != nil && Locked(*current) {
		p.date, p.start, p.end = current.SlotDate, current.StartTime, current.EndTime
	} else {
		s.validateWhen(in, &p, ve)
	}

	// The floor is the booking count, not MinCapacity: shrinking a slot below what
	// is already booked violates chk_slots_count, and the message has to say why.
	floor := int32(MinCapacity)
	if current != nil && current.BookedCount > floor {
		floor = current.BookedCount
	}

	switch c, err := strconv.Atoi(strings.TrimSpace(in.Capacity)); {
	case strings.TrimSpace(in.Capacity) == "":
		ve.Add("capacity", "Kapasitas wajib diisi.")
	case err != nil:
		ve.Add("capacity", "Kapasitas harus berupa angka.")
	case c < MinCapacity || c > MaxCapacity:
		ve.Add("capacity", fmt.Sprintf("Kapasitas harus antara %d dan %d.", MinCapacity, MaxCapacity))
	case int32(c) < floor:
		ve.Add("capacity", fmt.Sprintf(
			"Kapasitas tidak boleh lebih kecil dari jumlah booking yang sudah ada (%d).", floor))
	default:
		p.capacity = int32(c)
	}

	if note := strings.TrimSpace(in.Note); note != "" {
		if maxLen(ve, "note", "Catatan", note, maxNoteLen) {
			p.note = sql.NullString{String: note, Valid: true}
		}
	}

	if ve.Any() {
		return parsedSlot{}, ve
	}
	return p, nil
}

// validateWhen parses the date and the two clock values, and checks the window
// they describe. Split out because the booked-slot lock skips all of it.
func (s *Schedule) validateWhen(in SlotInput, p *parsedSlot, ve *ValidationError) {
	date, err := s.parseDate(in.Date)
	switch {
	case strings.TrimSpace(in.Date) == "":
		ve.Add("date", "Tanggal wajib diisi.")
	case err != nil:
		ve.Add("date", "Format tanggal tidak valid.")
	default:
		p.date = date
	}

	start, serr := util.ParseClock(in.StartTime)
	if serr != nil {
		ve.Add("start_time", "Jam mulai tidak valid. Contoh: 08:00")
	} else {
		p.start = util.FormatClock(start)
	}

	end, eerr := util.ParseClock(in.EndTime)
	if eerr != nil {
		ve.Add("end_time", "Jam selesai tidak valid. Contoh: 10:30")
	} else {
		p.end = util.FormatClock(end)
	}

	if serr != nil || eerr != nil {
		return
	}

	// chk_slots_window is end_time > start_time: a slot cannot cross midnight,
	// because both columns are TIME values on one date.
	switch d := end - start; {
	case d <= 0:
		ve.Add("end_time", "Jam selesai harus setelah jam mulai.")
	case d < MinSlotMinutes || d > MaxSlotMinutes:
		ve.Add("end_time", fmt.Sprintf("Durasi slot harus antara %d dan %d menit.",
			MinSlotMinutes, MaxSlotMinutes))
	}
}

// slotConstraintError translates the two constraint violations an admin can
// actually cause into messages beside the field that caused them, and returns
// nil for anything else so the caller can report a real failure.
func slotConstraintError(err error) error {
	switch {
	case repository.IsDuplicateKeyOn(err, "uq_slots_date_start"):
		ve := NewValidationError()
		ve.Add("start_time", "Sudah ada slot yang dimulai pada jam ini.")
		return ve
	case repository.IsCheckViolation(err):
		// Reachable when a booking lands between the Go check and this write.
		ve := NewValidationError()
		ve.Add("capacity", "Kapasitas tidak boleh lebih kecil dari jumlah booking yang sudah ada.")
		return ve
	}
	return nil
}

// SetActive flips is_active and returns the refreshed row, which the turbo
// stream re-renders the toggle from.
func (s *Schedule) SetActive(ctx context.Context, id int64, v bool) (sqlc.ScheduleSlot, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return sqlc.ScheduleSlot{}, err
	}
	if err := s.store.Queries.SetSlotActive(ctx, sqlc.SetSlotActiveParams{
		IsActive: v,
		ID:       id,
	}); err != nil {
		return sqlc.ScheduleSlot{}, fmt.Errorf("setting slot %d active: %w", id, err)
	}
	return s.Get(ctx, id)
}

// Delete removes a slot no booking holds, returning ErrHasBookings otherwise.
//
// As in Catalog.Delete, the read and the delete are two round-trips, so a
// booking can land in between. DeleteSlotIfUnbooked's own `booked_count = 0`
// predicate catches exactly that and is reported as the same error — the check
// is the friendly path, the predicate is the correct one.
func (s *Schedule) Delete(ctx context.Context, id int64) error {
	slot, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if slot.BookedCount > 0 {
		return ErrHasBookings
	}

	res, err := s.store.Queries.DeleteSlotIfUnbooked(ctx, id)
	if err != nil {
		return fmt.Errorf("deleting slot %d: %w", id, err)
	}
	if rows, rerr := res.RowsAffected(); rerr == nil && rows == 0 {
		return ErrHasBookings
	}
	return nil
}

// BulkResult reports what a range action did and what it refused to touch.
type BulkResult struct {
	Affected int64
	Skipped  int64
}

// SetActiveInRange activates or deactivates every slot in a date window.
//
// Deactivating is always safe: it only hides a slot from public availability and
// leaves existing bookings alone, which is exactly what an admin closing a week
// wants.
func (s *Schedule) SetActiveInRange(ctx context.Context, from, to time.Time, v bool) (BulkResult, error) {
	f, t, ve := s.parseWindow(from, to)
	if ve != nil {
		return BulkResult{}, ve
	}

	res, err := s.store.Queries.SetSlotActiveInRange(ctx, sqlc.SetSlotActiveInRangeParams{
		IsActive:     v,
		FromSlotDate: f,
		ToSlotDate:   t,
	})
	if err != nil {
		return BulkResult{}, fmt.Errorf("setting slots active in range: %w", err)
	}

	n, _ := res.RowsAffected()
	return BulkResult{Affected: n}, nil
}

// PreviewDeleteInRange reports what a bulk delete would remove and what it would
// refuse, without removing anything.
//
// A range delete is the one admin action here that cannot be undone, so it is
// confirmed against real numbers rather than against a generic "are you sure" —
// "37 dihapus, 3 dilewati" is a sentence an admin can actually check.
func (s *Schedule) PreviewDeleteInRange(ctx context.Context, from, to time.Time) (BulkResult, error) {
	f, t, ve := s.parseWindow(from, to)
	if ve != nil {
		return BulkResult{}, ve
	}

	total, err := s.store.Queries.CountSlotsInRange(ctx, sqlc.CountSlotsInRangeParams{
		FromSlotDate: f,
		ToSlotDate:   t,
	})
	if err != nil {
		return BulkResult{}, fmt.Errorf("counting slots in range: %w", err)
	}

	booked, err := s.store.Queries.CountBookedSlotsInRange(ctx, sqlc.CountBookedSlotsInRangeParams{
		FromSlotDate: f,
		ToSlotDate:   t,
	})
	if err != nil {
		return BulkResult{}, fmt.Errorf("counting booked slots in range: %w", err)
	}

	return BulkResult{Affected: total - booked, Skipped: booked}, nil
}

// DeleteInRange removes every unbooked slot in a date window and reports how
// many booked ones it left behind.
//
// The count runs first so the flash can name both numbers; a slot booked between
// the count and the delete simply survives, which is the outcome either way.
func (s *Schedule) DeleteInRange(ctx context.Context, from, to time.Time) (BulkResult, error) {
	f, t, ve := s.parseWindow(from, to)
	if ve != nil {
		return BulkResult{}, ve
	}

	booked, err := s.store.Queries.CountBookedSlotsInRange(ctx, sqlc.CountBookedSlotsInRangeParams{
		FromSlotDate: f,
		ToSlotDate:   t,
	})
	if err != nil {
		return BulkResult{}, fmt.Errorf("counting booked slots in range: %w", err)
	}

	res, err := s.store.Queries.DeleteUnbookedSlotsInRange(ctx, sqlc.DeleteUnbookedSlotsInRangeParams{
		FromSlotDate: f,
		ToSlotDate:   t,
	})
	if err != nil {
		return BulkResult{}, fmt.Errorf("deleting slots in range: %w", err)
	}

	n, _ := res.RowsAffected()
	return BulkResult{Affected: n, Skipped: booked}, nil
}

// parseWindow normalises and bounds a bulk action's date range.
func (s *Schedule) parseWindow(from, to time.Time) (time.Time, time.Time, *ValidationError) {
	f, t := s.dayOf(from), s.dayOf(to)

	ve := NewValidationError()
	switch {
	case f.IsZero():
		ve.Add("bulk_from", "Tanggal awal wajib diisi.")
	case t.IsZero():
		ve.Add("bulk_to", "Tanggal akhir wajib diisi.")
	case t.Before(f):
		ve.Add("bulk_to", "Tanggal akhir tidak boleh sebelum tanggal awal.")
	case t.After(f.AddDate(0, 0, maxGenerateDays)):
		ve.Add("bulk_to", fmt.Sprintf("Rentang maksimal %d hari.", maxGenerateDays))
	}
	if ve.Any() {
		return time.Time{}, time.Time{}, ve
	}
	return f, t, nil
}

// GenerateInput is the generator form, as raw strings.
type GenerateInput struct {
	From        string
	To          string
	WindowStart string
	WindowEnd   string
	Duration    string
	Break       string
	Capacity    string
	// Weekdays holds time.Weekday numbers as strings ("0" is Sunday). The form
	// orders the checkboxes Monday-first for reading; the values stay aligned with
	// time.Weekday so no mapping is needed here.
	Weekdays []string
	IsActive bool
}

// Candidate is one slot the generator would create.
type Candidate struct {
	Date  time.Time
	Start string
	End   string
	// Exists marks a candidate the unique index will skip, so the preview can say
	// what a run will actually change.
	Exists bool
}

// GeneratePlan is the preview: what a run would create, without creating it.
type GeneratePlan struct {
	Candidates    []Candidate
	NewCount      int
	ExistingCount int
	Days          int
	PerDay        int
}

// GenerateResult is what a run did.
type GenerateResult struct {
	Created int
	Skipped int
}

// GenerateDefaults prefills the generator form from the settings table, falling
// back to the values seeded there.
func (s *Schedule) GenerateDefaults() GenerateInput {
	today := s.dayOf(time.Now().In(s.loc))
	duration := s.settings.Int(KeySlotDefaultDuration, 150)
	capacity := s.settings.Int(KeySlotDefaultCapacity, 1)

	return GenerateInput{
		From:        today.Format(dateLayout),
		To:          today.AddDate(0, 0, 13).Format(dateLayout),
		WindowStart: "08:00",
		WindowEnd:   "20:00",
		Duration:    strconv.Itoa(duration),
		Break:       "0",
		Capacity:    strconv.Itoa(capacity),
		Weekdays:    []string{"1", "2", "3", "4", "5", "6", "0"},
		IsActive:    true,
	}
}

// Plan expands the form into the slots it describes and marks the ones that
// already exist. It writes nothing.
func (s *Schedule) Plan(ctx context.Context, in GenerateInput) (GeneratePlan, error) {
	cands, from, to, ve := s.expand(in)
	if ve != nil {
		return GeneratePlan{}, ve
	}

	existing, err := s.store.Queries.ListSlotsByDateRange(ctx, sqlc.ListSlotsByDateRangeParams{
		FromSlotDate: from,
		ToSlotDate:   to,
	})
	if err != nil {
		return GeneratePlan{}, fmt.Errorf("listing existing slots for plan: %w", err)
	}

	// Keyed the same way the unique index is — (slot_date, start_time) — so the
	// preview's idea of "already there" is the database's idea of it.
	taken := make(map[string]bool, len(existing))
	for _, slot := range existing {
		taken[slotKey(slot.SlotDate, slot.StartTime)] = true
	}

	plan := GeneratePlan{Candidates: cands, Days: countDays(cands)}
	for i := range plan.Candidates {
		c := &plan.Candidates[i]
		c.Exists = taken[slotKey(c.Date, c.Start)]
		if c.Exists {
			plan.ExistingCount++
		} else {
			plan.NewCount++
		}
	}
	if plan.Days > 0 {
		plan.PerDay = len(cands) / plan.Days
	}

	return plan, nil
}

// Apply creates the slots the form describes, skipping the ones already there.
//
// Idempotency is the unique index plus INSERT IGNORE, not a check in Go:
// RowsAffected() == 1 means the row was created and 0 means it already existed,
// so re-running an identical request reports "0 dibuat, N dilewati" and changes
// nothing at all. One transaction, so a failure part-way leaves no half-filled
// schedule behind.
func (s *Schedule) Apply(ctx context.Context, in GenerateInput) (GenerateResult, error) {
	cands, _, _, ve := s.expand(in)
	if ve != nil {
		return GenerateResult{}, ve
	}

	capacity, _ := strconv.Atoi(strings.TrimSpace(in.Capacity))

	var res GenerateResult
	err := s.store.WithTx(ctx, func(q *sqlc.Queries) error {
		res = GenerateResult{}
		for _, c := range cands {
			r, err := q.CreateSlotIfAbsent(ctx, sqlc.CreateSlotIfAbsentParams{
				SlotDate:  c.Date,
				StartTime: c.Start,
				EndTime:   c.End,
				Capacity:  int32(capacity),
				IsActive:  in.IsActive,
			})
			if err != nil {
				return err
			}
			if n, err := r.RowsAffected(); err == nil && n == 1 {
				res.Created++
			} else {
				res.Skipped++
			}
		}
		return nil
	})
	if err != nil {
		return GenerateResult{}, fmt.Errorf("generating slots: %w", err)
	}

	return res, nil
}

// expand turns the generator form into the exact list of slots it describes.
//
// Plan and Apply both go through here, so the preview cannot describe a
// different set from the one that gets written — which is the only thing that
// makes a preview worth showing.
func (s *Schedule) expand(in GenerateInput) ([]Candidate, time.Time, time.Time, *ValidationError) {
	ve := NewValidationError()

	from, ferr := s.parseDate(in.From)
	if strings.TrimSpace(in.From) == "" {
		ve.Add("from", "Tanggal awal wajib diisi.")
	} else if ferr != nil {
		ve.Add("from", "Format tanggal awal tidak valid.")
	}

	to, terr := s.parseDate(in.To)
	if strings.TrimSpace(in.To) == "" {
		ve.Add("to", "Tanggal akhir wajib diisi.")
	} else if terr != nil {
		ve.Add("to", "Format tanggal akhir tidak valid.")
	}

	if ferr == nil && terr == nil {
		switch {
		case to.Before(from):
			ve.Add("to", "Tanggal akhir tidak boleh sebelum tanggal awal.")
		case to.After(from.AddDate(0, 0, maxGenerateDays)):
			ve.Add("to", fmt.Sprintf("Rentang maksimal %d hari sekali generate.", maxGenerateDays))
		}
	}

	windowStart, wserr := util.ParseClock(in.WindowStart)
	if wserr != nil {
		ve.Add("window_start", "Jam buka tidak valid. Contoh: 08:00")
	}
	windowEnd, weerr := util.ParseClock(in.WindowEnd)
	if weerr != nil {
		ve.Add("window_end", "Jam tutup tidak valid. Contoh: 20:00")
	}
	if wserr == nil && weerr == nil && windowEnd <= windowStart {
		ve.Add("window_end", "Jam tutup harus setelah jam buka.")
	}

	duration := s.intField(in.Duration, "duration", "Durasi", MinSlotMinutes, MaxSlotMinutes, ve)
	brk := s.intField(in.Break, "break", "Jeda", 0, MaxBreakMinutes, ve)
	s.intField(in.Capacity, "capacity", "Kapasitas", MinCapacity, MaxCapacity, ve)

	days := weekdaySet(in.Weekdays)
	if len(days) == 0 {
		ve.Add("weekdays", "Pilih minimal satu hari.")
	}

	if ve.Any() {
		return nil, time.Time{}, time.Time{}, ve
	}

	var cands []Candidate
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if !days[d.Weekday()] {
			continue
		}
		// `t+duration <= windowEnd` is what keeps a slot from spilling past closing
		// time: the last slot of the day is the last one that fits whole.
		for t := windowStart; t+duration <= windowEnd; t += duration + brk {
			cands = append(cands, Candidate{
				Date:  d,
				Start: util.FormatClock(t),
				End:   util.FormatClock(t + duration),
			})
			if len(cands) > maxGeneratedSlots {
				ve.Add("to", fmt.Sprintf(
					"Terlalu banyak slot. Maksimal %d sekali generate — persempit rentang tanggal "+
						"atau jam operasionalnya.", maxGeneratedSlots))
				return nil, time.Time{}, time.Time{}, ve
			}
		}
	}

	if len(cands) == 0 {
		ve.Add("window_end", "Tidak ada slot yang muat di jam operasional ini.")
		return nil, time.Time{}, time.Time{}, ve
	}

	return cands, from, to, nil
}

// intField parses one bounded integer field, adding its own message on failure.
// label is the Indonesian field name the message names.
func (s *Schedule) intField(raw, field, label string, min, max int, ve *ValidationError) int {
	v := strings.TrimSpace(raw)
	if !required(ve, field, label, v) {
		return 0
	}

	n, err := strconv.Atoi(v)
	switch {
	case err != nil:
		ve.Add(field, label+" harus berupa angka.")
		return 0
	case n < min || n > max:
		ve.Add(field, fmt.Sprintf("%s harus antara %d dan %d menit.", label, min, max))
		return 0
	}
	return n
}

// weekdaySet reads the checked weekday boxes. An unparseable or out-of-range
// value is dropped rather than rejected: it can only come from a hand-edited
// form, and the "pick at least one day" check already covers an empty result.
func weekdaySet(values []string) map[time.Weekday]bool {
	days := make(map[time.Weekday]bool, len(values))
	for _, v := range values {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 || n > 6 {
			continue
		}
		days[time.Weekday(n)] = true
	}
	return days
}

// slotKey is the (slot_date, start_time) identity uq_slots_date_start enforces.
func slotKey(date time.Time, start string) string {
	return date.Format(dateLayout) + "|" + start
}

// countDays counts the distinct dates in a candidate list.
func countDays(cands []Candidate) int {
	n := 0
	var prev string
	for _, c := range cands {
		if key := c.Date.Format(dateLayout); key != prev {
			n++
			prev = key
		}
	}
	return n
}

// ParseDate reads a form or query-string date in the application timezone,
// returning the zero time when it is empty or malformed. Handlers use it for
// filter values, where a bad date should fall back to a default rather than
// reject the page.
func (s *Schedule) ParseDate(v string) time.Time {
	t, err := s.parseDate(v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ParseMonth reads a "2006-01" anchor, falling back to the current month.
func (s *Schedule) ParseMonth(v string) time.Time {
	t, err := time.ParseInLocation("2006-01", strings.TrimSpace(v), s.loc)
	if err != nil {
		return s.dayOf(time.Now().In(s.loc))
	}
	return t
}

// Today is the current date in the application timezone, for filter defaults.
func (s *Schedule) Today() time.Time { return s.dayOf(time.Now().In(s.loc)) }

func (s *Schedule) parseDate(v string) (time.Time, error) {
	return time.ParseInLocation(dateLayout, strings.TrimSpace(v), s.loc)
}

// dayOf strips the time of day, so a value compares equal to a DATE column read
// back out of the database.
func (s *Schedule) dayOf(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	t = t.In(s.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.loc)
}
