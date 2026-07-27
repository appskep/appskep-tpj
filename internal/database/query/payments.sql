-- payments and payment_notifications — the Midtrans side.
--
-- Payment is confirmed ONLY by the webhook (PLAN.md R7). A successful Snap token
-- response is not a purchase.
--
-- Midtrans retries notifications, and a retry can arrive while the first is
-- still in flight. Every transition below is guarded so that re-applying a
-- notification affects 0 rows: RowsAffected() == 0 means "already applied",
-- which must return 200 and change nothing else.

-- name: CreatePayment :execresult
INSERT INTO payments (booking_id, order_id, gross_amount, snap_token, snap_redirect_url)
VALUES (?, ?, ?, ?, ?);

-- name: GetPayment :one
SELECT * FROM payments WHERE id = ? LIMIT 1;

-- name: GetPaymentByOrderID :one
SELECT * FROM payments WHERE order_id = ? LIMIT 1;

-- name: GetPaymentByOrderIDForUpdate :one
-- The webhook entry point. This lock, plus the guarded UPDATEs below, is what
-- makes the notification state machine idempotent under concurrent retries.
SELECT * FROM payments WHERE order_id = ? FOR UPDATE;

-- name: GetLatestPaymentForBooking :one
SELECT * FROM payments WHERE booking_id = ? ORDER BY id DESC LIMIT 1;

-- name: GetReusablePaymentForBooking :one
-- create-order.md:93 — repeated taps of "Bayar sekarang" must return the SAME
-- Snap URL rather than minting a new order_id on the shared merchant account.
--
-- The "still within the booking's expires_at" leg of the rule is applied in Go
-- against the already-loaded booking row, which keeps every parameter here
-- directly comparable to a bare column.
SELECT * FROM payments
WHERE booking_id         = ?
  AND paid_at           IS NULL
  AND expired_at        IS NULL
  AND cancelled_at      IS NULL
  AND snap_redirect_url IS NOT NULL
  AND gross_amount       = ?
ORDER BY id DESC
LIMIT 1;

-- name: ListPaymentsForBooking :many
SELECT * FROM payments WHERE booking_id = ? ORDER BY id DESC;

-- name: UpdatePaymentSnap :exec
UPDATE payments SET snap_token = ?, snap_redirect_url = ? WHERE id = ?;

-- ---------------------------------------------------------------------------
-- Idempotent transitions. Each is a no-op once any lifecycle timestamp is set.
-- ---------------------------------------------------------------------------

-- name: MarkPaymentPaid :execresult
-- transaction_status = settlement, or capture + credit_card + fraud_status=accept.
UPDATE payments SET
  transaction_id     = ?,
  transaction_status = ?,
  transaction_time   = ?,
  payment_type       = ?,
  fraud_status       = ?,
  status_code        = ?,
  raw_response       = ?,
  paid_at            = NOW()
WHERE order_id = ? AND paid_at IS NULL AND expired_at IS NULL AND cancelled_at IS NULL;

-- name: MarkPaymentExpired :execresult
-- transaction_status = deny or expire. The caller releases the slot iff this and
-- the matching booking transition both report a real change.
UPDATE payments SET
  transaction_status = ?,
  fraud_status       = ?,
  status_code        = ?,
  raw_response       = ?,
  expired_at         = NOW()
WHERE order_id = ? AND paid_at IS NULL AND expired_at IS NULL AND cancelled_at IS NULL;

-- name: MarkPaymentCancelled :execresult
-- transaction_status = cancel.
UPDATE payments SET
  transaction_status = ?,
  status_code        = ?,
  raw_response       = ?,
  cancelled_at       = NOW()
WHERE order_id = ? AND paid_at IS NULL AND expired_at IS NULL AND cancelled_at IS NULL;

-- name: RecordPaymentProgress :exec
-- transaction_status = pending, and capture + fraud_status = challenge. Records
-- what Midtrans told us, moves no lifecycle timestamp, leaves the booking alone.
-- A challenge is flagged for admin review rather than auto-accepted.
UPDATE payments SET
  transaction_status = ?,
  transaction_id     = ?,
  transaction_time   = ?,
  payment_type       = ?,
  fraud_status       = ?,
  status_code        = ?,
  bank               = ?,
  va_number          = ?,
  raw_response       = ?
WHERE order_id = ? AND paid_at IS NULL;

-- ---------------------------------------------------------------------------
-- Webhook audit log
-- ---------------------------------------------------------------------------

-- name: CreatePaymentNotification :execresult
-- Written BEFORE any validation or processing, for every inbound payload —
-- including invalid signatures and order IDs belonging to other Appskep systems
-- on the shared merchant account. gross_amount is stored as the exact string
-- received because it is an input to the SHA512 signature.
INSERT INTO payment_notifications
  (order_id, transaction_status, fraud_status, status_code, gross_amount,
   signature_valid, payload, remote_ip)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: MarkNotificationProcessed :exec
UPDATE payment_notifications SET processed = 1, process_note = ? WHERE id = ?;

-- name: ListPaymentNotificationsByOrderID :many
SELECT * FROM payment_notifications WHERE order_id = ? ORDER BY id DESC;

-- ---------------------------------------------------------------------------
-- Admin panel (Phase 10)
--
-- A payment has no status column on purpose (see the schema comment): its
-- lifecycle is paid_at / expired_at / cancelled_at. The list filter therefore
-- runs against a derived state, and the same CASE is selected so the page can
-- label a row without re-deriving the rule in Go.
--
-- The date range is `>= ? AND <= ?` on a bare column, never
-- `DATE(p.created_at) BETWEEN ? AND ?`. See the note in bookings.sql: sqlc's
-- MySQL engine mis-binds BETWEEN in both directions here, and only the
-- comparison form generates a call whose argument count matches its SQL. The
-- caller passes an end-of-day instant for the upper bound, since created_at is a
-- DATETIME.
-- ---------------------------------------------------------------------------

-- name: ListPaymentsAdmin :many
SELECT p.id, p.booking_id, p.order_id, p.gross_amount, p.payment_type,
       p.transaction_status, p.fraud_status, p.paid_at, p.expired_at,
       p.cancelled_at, p.created_at,
       CASE WHEN p.paid_at      IS NOT NULL THEN 'paid'
            WHEN p.cancelled_at IS NOT NULL THEN 'cancelled'
            WHEN p.expired_at   IS NOT NULL THEN 'expired'
            ELSE 'pending' END AS state,
       b.booking_code AS booking_code,
       b.status       AS booking_status,
       sv.name        AS service_name,
       u.name         AS user_name,
       u.email        AS user_email
FROM payments p
JOIN bookings b  ON b.id  = p.booking_id
JOIN services sv ON sv.id = b.service_id
JOIN users    u  ON u.id  = b.user_id
WHERE FIND_IN_SET(
        CASE WHEN p.paid_at      IS NOT NULL THEN 'paid'
             WHEN p.cancelled_at IS NOT NULL THEN 'cancelled'
             WHEN p.expired_at   IS NOT NULL THEN 'expired'
             ELSE 'pending' END,
        sqlc.arg(state_list)) > 0
  AND p.created_at >= sqlc.arg(created_from) AND p.created_at <= sqlc.arg(created_to)
  AND (p.order_id     LIKE sqlc.arg(search)
    OR b.booking_code LIKE sqlc.arg(search))
ORDER BY p.id DESC
LIMIT ? OFFSET ?;

-- name: CountPaymentsAdmin :one
SELECT COUNT(*) AS total
FROM payments p
JOIN bookings b ON b.id = p.booking_id
WHERE FIND_IN_SET(
        CASE WHEN p.paid_at      IS NOT NULL THEN 'paid'
             WHEN p.cancelled_at IS NOT NULL THEN 'cancelled'
             WHEN p.expired_at   IS NOT NULL THEN 'expired'
             ELSE 'pending' END,
        sqlc.arg(state_list)) > 0
  AND p.created_at >= sqlc.arg(created_from) AND p.created_at <= sqlc.arg(created_to)
  AND (p.order_id     LIKE sqlc.arg(search)
    OR b.booking_code LIKE sqlc.arg(search));

-- name: ListPaymentsForExport :many
SELECT p.id, p.order_id, p.gross_amount, p.payment_type, p.transaction_id,
       p.transaction_status, p.fraud_status, p.bank, p.va_number,
       p.paid_at, p.expired_at, p.cancelled_at, p.created_at,
       CASE WHEN p.paid_at      IS NOT NULL THEN 'paid'
            WHEN p.cancelled_at IS NOT NULL THEN 'cancelled'
            WHEN p.expired_at   IS NOT NULL THEN 'expired'
            ELSE 'pending' END AS state,
       b.booking_code AS booking_code,
       b.status       AS booking_status,
       sv.name        AS service_name,
       u.name         AS user_name,
       u.email        AS user_email
FROM payments p
JOIN bookings b  ON b.id  = p.booking_id
JOIN services sv ON sv.id = b.service_id
JOIN users    u  ON u.id  = b.user_id
WHERE FIND_IN_SET(
        CASE WHEN p.paid_at      IS NOT NULL THEN 'paid'
             WHEN p.cancelled_at IS NOT NULL THEN 'cancelled'
             WHEN p.expired_at   IS NOT NULL THEN 'expired'
             ELSE 'pending' END,
        sqlc.arg(state_list)) > 0
  AND p.created_at >= sqlc.arg(created_from) AND p.created_at <= sqlc.arg(created_to)
  AND (p.order_id     LIKE sqlc.arg(search)
    OR b.booking_code LIKE sqlc.arg(search))
ORDER BY p.id ASC
LIMIT ?;

-- name: GetPaymentAdminDetail :one
SELECT p.*,
       b.booking_code   AS booking_code,
       b.status         AS booking_status,
       b.price_amount   AS booking_price_amount,
       b.customer_name  AS customer_name,
       b.customer_phone AS customer_phone,
       sv.name          AS service_name,
       sl.slot_date     AS slot_date,
       sl.start_time    AS slot_start_time,
       sl.end_time      AS slot_end_time,
       u.name           AS user_name,
       u.email          AS user_email
FROM payments p
JOIN bookings       b  ON b.id  = p.booking_id
JOIN services       sv ON sv.id = b.service_id
JOIN schedule_slots sl ON sl.id = b.slot_id
JOIN users          u  ON u.id  = b.user_id
WHERE p.id = ?
LIMIT 1;

-- name: SumPaidBetween :one
-- Dashboard revenue. The explicit CAST to DECIMAL is what makes sqlc type this
-- as a Go string: a bare SUM(gross_amount) types as interface{}, and casting to
-- CHAR does too. A string is also what keeps money off float64 all the way to
-- util.Rupiah. COALESCE, because SUM over no rows is NULL, not 0.
SELECT CAST(COALESCE(SUM(gross_amount), 0) AS DECIMAL(14,2)) AS total,
       COUNT(*) AS paid_count
FROM payments
WHERE paid_at BETWEEN ? AND ?;

-- name: ListRecentPaidPayments :many
-- The dashboard's "pembayaran terbaru" card.
SELECT p.id, p.order_id, p.gross_amount, p.payment_type, p.paid_at,
       b.booking_code AS booking_code,
       sv.name        AS service_name,
       u.name         AS user_name
FROM payments p
JOIN bookings b  ON b.id  = p.booking_id
JOIN services sv ON sv.id = b.service_id
JOIN users    u  ON u.id  = b.user_id
WHERE p.paid_at IS NOT NULL
ORDER BY p.paid_at DESC, p.id DESC
LIMIT ?;
