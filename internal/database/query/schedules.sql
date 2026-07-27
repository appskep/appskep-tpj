-- schedule_slots — bookable time slots, and the slot side of the concurrency
-- design.
--
-- CANONICAL LOCK ORDER: schedule_slots -> bookings -> payments.
-- Any transaction that will change a slot's booked_count must take that slot's
-- FOR UPDATE lock FIRST, before locking any booking or payment row. Booking
-- creation naturally locks slot-then-booking; a naive expiry ticker would lock
-- booking-then-slot, and the two together deadlock.

-- name: GetSlot :one
SELECT * FROM schedule_slots WHERE id = ? LIMIT 1;

-- name: GetSlotForUpdate :one
-- THE lock. This must be the first statement of the booking transaction, taken
-- BEFORE the capacity check — not after. That ordering is the entire reason two
-- concurrent bookings on a capacity-1 slot cannot both succeed: the second
-- request blocks here until the first commits, and then correctly sees the slot
-- as full. See PLAN.md § Design note: slot concurrency.
SELECT * FROM schedule_slots WHERE id = ? FOR UPDATE;

-- name: ListAvailableSlotsByDate :many
-- The public availability query (booking step 2, loaded into a Turbo Frame).
-- `starts_at >= ?` receives now + booking_lead_time_minutes, computed in Go, so
-- past and too-soon slots disappear without an INTERVAL expression that sqlc
-- cannot type.
--
-- There is no service_id predicate: every slot is global today (PLAN.md Q4), so
-- it would always be true while costing a nullable parameter at every call site.
-- It gets added in the same change that flips Q4.
SELECT * FROM schedule_slots
WHERE slot_date    = ?
  AND is_active    = 1
  AND booked_count < capacity
  AND starts_at   >= ?
ORDER BY start_time ASC;

-- name: ListAvailableDates :many
-- The date picker: which dates still have at least one free slot.
SELECT slot_date, COUNT(*) AS free_slots
FROM schedule_slots
WHERE is_active    = 1
  AND booked_count < capacity
  AND starts_at   >= ?
  AND slot_date   <= ?
GROUP BY slot_date
ORDER BY slot_date ASC;

-- name: ListSlotsByDateRange :many
-- The admin calendar / list view.
SELECT * FROM schedule_slots
WHERE slot_date BETWEEN ? AND ?
ORDER BY slot_date ASC, start_time ASC;

-- name: ListFreeSlotsBetween :many
-- The Phase 10 reschedule picker: every slot with room left, between two
-- instants.
--
-- It deliberately does NOT apply booking_lead_time_minutes. That rule protects a
-- customer from booking themselves into a slot the clinic cannot prepare for; an
-- admin moving someone to a slot half an hour out is a phone call, not a
-- mistake. The caller passes `now` as the lower bound, so a slot that has
-- already started is still excluded.
SELECT * FROM schedule_slots
WHERE is_active    = 1
  AND booked_count < capacity
  AND starts_at BETWEEN ? AND ?
ORDER BY starts_at ASC
LIMIT ?;

-- name: CreateSlot :execresult
INSERT INTO schedule_slots (service_id, slot_date, start_time, end_time, capacity, is_active, note)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: CreateSlotIfAbsent :execresult
-- The Phase 5 generator. RowsAffected() == 0 means "already existed, skipped" —
-- that is the created/skipped counter, and what makes a re-run a no-op.
INSERT IGNORE INTO schedule_slots (service_id, slot_date, start_time, end_time, capacity, is_active, note)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSlot :exec
-- Lowering capacity below booked_count violates chk_slots_count and surfaces as
-- a constraint error, which the handler renders as "kapasitas tidak boleh lebih
-- kecil dari jumlah booking".
--
-- is_active is written here as well as by SetSlotActive: the edit form owns the
-- flag alongside every other field, and leaving it out would make saving the
-- form silently discard a status change the admin just made.
UPDATE schedule_slots SET
  slot_date  = ?,
  start_time = ?,
  end_time   = ?,
  capacity   = ?,
  is_active  = ?,
  note       = ?
WHERE id = ?;

-- name: SetSlotActive :exec
UPDATE schedule_slots SET is_active = ? WHERE id = ?;

-- name: SetSlotActiveInRange :execresult
UPDATE schedule_slots SET is_active = ? WHERE slot_date BETWEEN ? AND ?;

-- name: DeleteSlotIfUnbooked :execresult
-- RowsAffected() == 0 means the slot had bookings and was left alone.
DELETE FROM schedule_slots WHERE id = ? AND booked_count = 0;

-- name: DeleteUnbookedSlotsInRange :execresult
DELETE FROM schedule_slots WHERE slot_date BETWEEN ? AND ? AND booked_count = 0;

-- name: CountSlotsInRange :one
-- How many slots a date range holds, without pulling them. The admin calendar
-- uses it to decide whether a range is worth rendering at all.
SELECT COUNT(*) AS total FROM schedule_slots WHERE slot_date BETWEEN ? AND ?;

-- name: CountBookedSlotsInRange :one
-- How many slots in a range a bulk delete will refuse to touch.
--
-- DeleteUnbookedSlotsInRange reports what it deleted but not what it left
-- alone, so without this the admin is told "12 dihapus" with no hint that 3
-- booked slots survived. Run it before the delete, in the same request.
SELECT COUNT(*) AS total FROM schedule_slots
WHERE slot_date BETWEEN ? AND ? AND booked_count > 0;

-- name: HoldSlot :execresult
-- The increment. Runs inside the booking transaction, after GetSlotForUpdate.
--
-- The WHERE clause is defence in depth, not the primary safety mechanism — the
-- row lock is what makes this correct. RowsAffected() == 0 means the slot filled
-- or was deactivated between page load and submit; render "slot baru saja
-- terisi" back into the Turbo Frame rather than an error page.
UPDATE schedule_slots SET booked_count = booked_count + 1
WHERE id = ? AND is_active = 1 AND booked_count < capacity;

-- name: ReleaseSlot :exec
-- The guarded decrement. The CALLER must have observed a real status transition
-- first — RowsAffected() == 1 from ExpireBooking or CancelBooking — inside the
-- same transaction. Never call this unconditionally: the expiry ticker and the
-- Midtrans webhook can both reach the same booking, and only the one that
-- actually moved the status may release the slot.
--
-- GREATEST(..., 0) means even a logic error cannot drive the counter negative.
UPDATE schedule_slots SET booked_count = GREATEST(booked_count - 1, 0) WHERE id = ?;

-- name: CountSlotHoldingBookings :one
-- The invariant: this must always equal schedule_slots.booked_count. Asserted by
-- the Phase 13 concurrency tests.
SELECT COUNT(*) AS total FROM bookings
WHERE slot_id = ? AND status NOT IN ('cancelled', 'expired');

-- name: RecountSlotBooked :exec
-- Repair tool for the admin panel and tests. Not part of any normal write path;
-- if this ever changes a row in production, something upstream is broken.
UPDATE schedule_slots s
SET s.booked_count = (
  SELECT COUNT(*) FROM bookings b
  WHERE b.slot_id = s.id AND b.status NOT IN ('cancelled', 'expired')
)
WHERE s.id = ?;
