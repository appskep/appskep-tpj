-- bookings — the booking lifecycle.
--
-- Slot-holding statuses: pending_payment, paid, confirmed, completed.
-- Slot-releasing statuses: cancelled, expired.
--
-- EVERY status transition below is :execresult and guarded by the current
-- status. RowsAffected() == 1 IS the definition of "a real status transition",
-- and is the only thing that authorises a ReleaseSlot call. RowsAffected() == 0
-- means the transition was already applied — a Midtrans retry, a double-clicked
-- button, the expiry ticker racing the webhook — in which case do nothing else
-- and report success.

-- name: CreateBooking :execresult
-- active_slot_id is a generated column and must not appear in the column list.
INSERT INTO bookings (booking_code, user_id, service_id, slot_id,
                      customer_name, customer_phone, customer_address, notes,
                      price_amount, status, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending_payment', ?);

-- name: GetBooking :one
SELECT * FROM bookings WHERE id = ? LIMIT 1;

-- name: GetBookingByCode :one
SELECT * FROM bookings WHERE booking_code = ? LIMIT 1;

-- name: GetBookingForUpdate :one
-- If this transaction will also change booked_count, take GetSlotForUpdate
-- first (see the lock order note in schedules.sql).
SELECT * FROM bookings WHERE id = ? FOR UPDATE;

-- name: GetBookingByCodeForUpdate :one
SELECT * FROM bookings WHERE booking_code = ? FOR UPDATE;

-- name: BookingCodeTaken :one
SELECT EXISTS(SELECT 1 FROM bookings WHERE booking_code = ?) AS taken;

-- name: GetSlotHoldingBookingForUser :one
-- Pre-flight duplicate check, so the user gets a friendly message instead of a
-- constraint error. It is only a nicety: the real guarantee is the unique index
-- uq_bookings_active_slot_user, which cannot be bypassed by application code.
SELECT * FROM bookings
WHERE slot_id = ? AND user_id = ? AND status NOT IN ('cancelled', 'expired')
LIMIT 1;

-- name: GetBookingDetailByCode :one
-- The konfirmasi / pembayaran pages: everything needed to render in one round
-- trip.
SELECT b.id, b.booking_code, b.user_id, b.service_id, b.slot_id,
       b.customer_name, b.customer_phone, b.customer_address, b.notes,
       b.price_amount, b.status, b.expires_at, b.cancelled_reason,
       b.created_at, b.updated_at,
       sv.name       AS service_name,
       sv.slug       AS service_slug,
       sv.image_path AS service_image_path,
       sl.slot_date  AS slot_date,
       sl.start_time AS slot_start_time,
       sl.end_time   AS slot_end_time,
       sl.starts_at  AS slot_starts_at
FROM bookings b
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
WHERE b.booking_code = ?
LIMIT 1;

-- ---------------------------------------------------------------------------
-- Status transitions
-- ---------------------------------------------------------------------------

-- name: MarkBookingPaid :execresult
-- Applied only by the Midtrans webhook (settlement, or capture + credit_card +
-- fraud_status=accept). Never from a Snap token response or a client callback.
UPDATE bookings SET status = 'paid'
WHERE id = ? AND status = 'pending_payment';

-- name: ConfirmBooking :execresult
UPDATE bookings SET status = 'confirmed', confirmed_at = NOW()
WHERE id = ? AND status = 'paid';

-- name: CompleteBooking :execresult
UPDATE bookings SET status = 'completed', completed_at = NOW()
WHERE id = ? AND status IN ('paid', 'confirmed');

-- name: CancelBooking :execresult
-- Releases the slot: the caller must call ReleaseSlot iff RowsAffected() == 1.
UPDATE bookings SET status = 'cancelled', cancelled_at = NOW(), cancelled_reason = ?
WHERE id = ? AND status IN ('pending_payment', 'paid', 'confirmed');

-- name: CancelPendingBookingByOwner :execresult
-- Phase 9 user-initiated cancellation. Narrower guard than CancelBooking, with
-- ownership enforced in the WHERE rather than trusted from the handler.
-- Releases the slot: ReleaseSlot iff RowsAffected() == 1.
UPDATE bookings SET status = 'cancelled', cancelled_at = NOW(), cancelled_reason = ?
WHERE id = ? AND user_id = ? AND status = 'pending_payment';

-- name: ExpireBooking :execresult
-- Releases the slot: the caller must call ReleaseSlot iff RowsAffected() == 1.
UPDATE bookings SET status = 'expired'
WHERE id = ? AND status = 'pending_payment';

-- name: RescheduleBooking :execresult
-- Phase 10. The caller must decrement the old slot and increment the new one in
-- the same transaction, taking both slot locks in ascending id order.
UPDATE bookings SET slot_id = ?
WHERE id = ? AND status IN ('pending_payment', 'paid', 'confirmed');

-- ---------------------------------------------------------------------------
-- Lists
-- ---------------------------------------------------------------------------

-- name: ListExpiredPendingBookings :many
-- The Phase 7 expiry ticker's work queue. slot_id is selected so the worker can
-- take the SLOT lock first, preserving the canonical lock order.
--
-- NOW() rather than a timestamp passed from Go: one clock decides expiry, so
-- app-server clock skew cannot expire bookings early or leak slots.
SELECT id, slot_id FROM bookings
WHERE status = 'pending_payment'
  AND expires_at IS NOT NULL
  AND expires_at <= NOW()
ORDER BY id ASC
LIMIT ?;

-- name: ListBookingsForReminder :many
-- The Phase 11 H-1 reminder sweep's work queue: bookings on one slot date that
-- are still coming and have not been reminded yet.
--
-- 'paid' as well as 'confirmed': confirming is an operator action that may never
-- happen, and a customer who has paid is coming regardless. 'completed' is
-- excluded because a completed booking is in the past by definition.
--
-- Only the id is needed — the worker re-reads the whole booking through
-- GetBookingAdminDetail at send time, so a reminder describes the booking as it
-- is when it goes out rather than as it was when it was queued.
SELECT b.id
FROM bookings b
JOIN schedule_slots sl ON sl.id = b.slot_id
WHERE sl.slot_date = ?
  AND b.status IN ('paid', 'confirmed')
  AND b.reminder_sent_at IS NULL
ORDER BY b.id ASC
LIMIT ?;

-- name: MarkBookingReminderSent :execresult
-- Claims one booking for the reminder sweep. RowsAffected() == 1 is the only
-- thing that authorises sending the email — the same rule that guards every
-- status transition above. Claim-then-send is deliberate: a duplicate reminder
-- is worse than a missing one, so a send that fails afterwards is not retried.
UPDATE bookings SET reminder_sent_at = NOW()
WHERE id = ? AND reminder_sent_at IS NULL;

-- name: ListBookingsByUser :many
-- Halaman riwayat. Pass a comma-joined status list; for "all", pass
-- "pending_payment,paid,confirmed,completed,cancelled,expired".
--
-- FIND_IN_SET avoids sqlc.slice(), which the MySQL engine does not support. The
-- CAST is required: an ENUM in a numeric context yields its ordinal, not its
-- label, so FIND_IN_SET would silently match the wrong rows without it.
SELECT b.id, b.booking_code, b.status, b.price_amount, b.expires_at, b.created_at,
       sv.name       AS service_name,
       sv.slug       AS service_slug,
       sl.slot_date  AS slot_date,
       sl.start_time AS slot_start_time,
       sl.end_time   AS slot_end_time
FROM bookings b
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
WHERE b.user_id = ? AND FIND_IN_SET(CAST(b.status AS CHAR), sqlc.arg(status_list)) > 0
ORDER BY b.created_at DESC
LIMIT ? OFFSET ?;

-- name: CountBookingsByUser :one
SELECT COUNT(*) AS total FROM bookings
WHERE user_id = ? AND FIND_IN_SET(CAST(status AS CHAR), sqlc.arg(status_list)) > 0;

-- ---------------------------------------------------------------------------
-- Admin panel (Phase 10)
--
-- One WHERE clause serves every filter combination, following the same
-- techniques as ListBookingsByUser above: a comma-joined status list through
-- FIND_IN_SET (sqlc's MySQL engine has no sqlc.slice()), "%" for an empty search
-- box, and a 0 sentinel for "any layanan".
--
-- BOTH date ranges are always applied, and the caller widens the one the
-- operator did not choose (?tanggal=jadwal|dibuat). That is not a style
-- preference. sqlc's MySQL engine mis-binds BETWEEN here in two different ways,
-- and each produces SQL whose placeholder count does not match the generated
-- call — a runtime failure on the first request, invisible at generate time:
--
--   * `(CASE ... END) BETWEEN ? AND ?` and `DATE(created_at) BETWEEN ? AND ?`
--     emit the placeholders but DROP both parameters. An expression on the left
--     of BETWEEN is not bound at all.
--   * `b.created_at BETWEEN ? AND ?` DUPLICATES each parameter once per joined
--     table that also has a created_at column — four copies here, table
--     qualifier ignored.
--
-- `col >= ? AND col <= ?` binds correctly in every case, and is what all three
-- admin queries below use. Two plain columns are also index-friendly, which the
-- CASE form was not.
--
-- created_at is a DATETIME, so the caller passes an end-of-day instant for the
-- upper bound; a bare date there would cut at midnight and hide the last day.
-- ---------------------------------------------------------------------------

-- name: ListBookingsAdmin :many
SELECT b.id, b.booking_code, b.user_id, b.service_id, b.slot_id,
       b.customer_name, b.customer_phone, b.notes,
       b.price_amount, b.status, b.expires_at, b.created_at,
       sv.name       AS service_name,
       sv.slug       AS service_slug,
       sl.slot_date  AS slot_date,
       sl.start_time AS slot_start_time,
       sl.end_time   AS slot_end_time,
       sl.starts_at  AS slot_starts_at,
       u.name        AS user_name,
       u.email       AS user_email
FROM bookings b
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
JOIN users          u  ON u.id  = b.user_id
WHERE FIND_IN_SET(CAST(b.status AS CHAR), sqlc.arg(status_list)) > 0
  AND (sqlc.arg(service_id) = 0 OR b.service_id = sqlc.arg(service_id))
  AND sl.slot_date >= sqlc.arg(slot_from)    AND sl.slot_date <= sqlc.arg(slot_to)
  AND b.created_at >= sqlc.arg(created_from) AND b.created_at <= sqlc.arg(created_to)
  AND (b.booking_code   LIKE sqlc.arg(search)
    OR b.customer_name  LIKE sqlc.arg(search)
    OR b.customer_phone LIKE sqlc.arg(search))
-- Schedule order, direction chosen by a parameter rather than by a second
-- near-identical query. The unused branch evaluates to a constant NULL, which
-- orders nothing. Ordering is always by the slot's instant, whichever date the
-- operator filtered on: this list is a day sheet, and reading it in schedule
-- order is what an operator is doing with it.
--
-- oldest_first types as interface{} — sqlc's MySQL engine cannot infer a Go type
-- for a parameter compared against a literal. Wrapping it in CAST(... AS
-- UNSIGNED) does type it, but as TWO separate int64 fields that a caller could
-- set to disagreeing values and silently get a wrong order; one untyped field
-- bound twice cannot disagree with itself. The single caller passes 0 or 1.
ORDER BY CASE WHEN sqlc.arg(oldest_first) = 1 THEN sl.starts_at END ASC,
         CASE WHEN sqlc.arg(oldest_first) = 0 THEN sl.starts_at END DESC,
         b.id DESC
LIMIT ? OFFSET ?;

-- name: CountBookingsAdmin :one
SELECT COUNT(*) AS total
FROM bookings b
JOIN schedule_slots sl ON sl.id = b.slot_id
WHERE FIND_IN_SET(CAST(b.status AS CHAR), sqlc.arg(status_list)) > 0
  AND (sqlc.arg(service_id) = 0 OR b.service_id = sqlc.arg(service_id))
  AND sl.slot_date >= sqlc.arg(slot_from)    AND sl.slot_date <= sqlc.arg(slot_to)
  AND b.created_at >= sqlc.arg(created_from) AND b.created_at <= sqlc.arg(created_to)
  AND (b.booking_code   LIKE sqlc.arg(search)
    OR b.customer_name  LIKE sqlc.arg(search)
    OR b.customer_phone LIKE sqlc.arg(search));

-- name: ListBookingsForExport :many
-- The CSV export. Same predicate as the list, no OFFSET, and a LIMIT the service
-- sets so an export cannot pull an unbounded result set into memory.
SELECT b.id, b.booking_code, b.status, b.customer_name, b.customer_phone,
       b.customer_address, b.notes, b.price_amount, b.created_at,
       b.confirmed_at, b.completed_at, b.cancelled_at, b.cancelled_reason,
       sv.name       AS service_name,
       sl.slot_date  AS slot_date,
       sl.start_time AS slot_start_time,
       sl.end_time   AS slot_end_time,
       u.name        AS user_name,
       u.email       AS user_email
FROM bookings b
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
JOIN users          u  ON u.id  = b.user_id
WHERE FIND_IN_SET(CAST(b.status AS CHAR), sqlc.arg(status_list)) > 0
  AND (sqlc.arg(service_id) = 0 OR b.service_id = sqlc.arg(service_id))
  AND sl.slot_date >= sqlc.arg(slot_from)    AND sl.slot_date <= sqlc.arg(slot_to)
  AND b.created_at >= sqlc.arg(created_from) AND b.created_at <= sqlc.arg(created_to)
  AND (b.booking_code   LIKE sqlc.arg(search)
    OR b.customer_name  LIKE sqlc.arg(search)
    OR b.customer_phone LIKE sqlc.arg(search))
ORDER BY sl.slot_date ASC, sl.start_time ASC, b.id ASC
LIMIT ?;

-- name: GetBookingAdminDetail :one
-- Everything the admin detail page renders, in one round trip. The customer's
-- own page uses GetBookingDetailByCode; this one is by id, carries the slot's
-- capacity and the local user record, and has no ownership predicate — the route
-- is behind RequireAdmin.
SELECT b.*,
       sv.name             AS service_name,
       sv.slug             AS service_slug,
       sv.duration_minutes AS service_duration_minutes,
       sl.slot_date        AS slot_date,
       sl.start_time       AS slot_start_time,
       sl.end_time         AS slot_end_time,
       sl.starts_at        AS slot_starts_at,
       sl.capacity         AS slot_capacity,
       sl.booked_count     AS slot_booked_count,
       sl.is_active        AS slot_is_active,
       u.name              AS user_name,
       u.email             AS user_email,
       u.phone             AS user_phone,
       u.appskep_user_id   AS user_appskep_id
FROM bookings b
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
JOIN users          u  ON u.id  = b.user_id
WHERE b.id = ?
LIMIT 1;

-- name: UpdateBookingNotes :exec
-- The only booking field an admin may edit freely. Everything else is either a
-- status transition or a reschedule, both of which move a slot.
UPDATE bookings SET notes = ? WHERE id = ?;

-- name: ListBookingsBySlotDate :many
-- The dashboard day sheet: who is coming on one date, in time order. Released
-- bookings are excluded — a cancelled booking is not on today's list.
SELECT b.id, b.booking_code, b.status, b.customer_name, b.customer_phone,
       b.price_amount,
       sv.name       AS service_name,
       sl.start_time AS slot_start_time,
       sl.end_time   AS slot_end_time
FROM bookings b
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
WHERE sl.slot_date = ? AND b.status NOT IN ('cancelled', 'expired')
ORDER BY sl.start_time ASC, b.id ASC;

-- name: CountBookingsByStatusInRange :many
-- The dashboard's counts by status, over a slot-date range.
SELECT b.status, COUNT(*) AS total
FROM bookings b
JOIN schedule_slots sl ON sl.id = b.slot_id
WHERE sl.slot_date BETWEEN ? AND ?
GROUP BY b.status;
