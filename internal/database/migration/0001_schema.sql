-- 0001_schema.sql — Appskep TPJ schema.
--
-- This file is the single source of truth for the schema until go-live (Phase
-- 14). Schema changes are made by EDITING THIS FILE and running `make db-fresh`;
-- there are no 0002_*.sql files before go-live. `make migrate` re-pipes every
-- 0*.sql on each run, so every statement here is `CREATE TABLE IF NOT EXISTS`
-- and the file is safe to re-apply.
--
-- sqlc parses this exact file (see sqlc.yaml). Keep it pure DDL: no SET, no USE,
-- no DROP.
--
-- Conventions:
--   * InnoDB + utf8mb4_unicode_ci everywhere.
--   * DATETIME, never TIMESTAMP — no implicit server-side timezone conversion
--     and no 2038 problem. The session clock is pinned to +07:00 by the driver
--     (see internal/shared/config/config.go DSN), so NOW() is Jakarta wall time.
--   * Signed BIGINT primary/foreign keys, so Go sees int64 and LastInsertId()
--     needs no cast. The one unsigned column is users.appskep_user_id.
--   * Money is DECIMAL(12,2); sqlc maps it to a Go string, never float64.
--   * Foreign keys are RESTRICT/RESTRICT: booking and payment rows are financial
--     records and must never disappear behind a cascade.

-- ---------------------------------------------------------------------------
-- users — a local mirror of the Appskep identity. TPJ owns NO credentials.
--
-- There is deliberately no password_hash column and no email_verifications or
-- password_resets table: credentials, verification and password resets are owned
-- by the Appskep SSO service (PLAN.md R1/R2). Do not add them.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
  id              BIGINT               NOT NULL AUTO_INCREMENT,
  -- The `user_id` claim from the Appskep JWT. This is the only identity key we
  -- trust and the key the login upsert matches on.
  appskep_user_id BIGINT UNSIGNED      NOT NULL,
  email           VARCHAR(255)         NOT NULL,
  name            VARCHAR(150)         NOT NULL,
  phone           VARCHAR(30)          NULL,
  address         VARCHAR(500)         NULL,
  avatar_path     VARCHAR(255)         NULL,
  role            ENUM('user','admin') NOT NULL DEFAULT 'user',
  is_active       TINYINT(1)           NOT NULL DEFAULT 1,
  last_login_at   DATETIME             NULL,
  created_at      DATETIME             NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      DATETIME             NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uq_users_appskep_user_id (appskep_user_id),
  -- email is indexed but NOT unique on purpose: Appskep owns identity, and a
  -- unique constraint here would hard-fail the login upsert for a real user if
  -- Appskep ever emitted two accounts sharing an address — a lockout we could
  -- not fix from this side.
  KEY idx_users_email (email),
  KEY idx_users_role_active (role, is_active)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- services — the layanan offered. Managed entirely from /admin/layanan.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS services (
  id               BIGINT        NOT NULL AUTO_INCREMENT,
  slug             VARCHAR(150)  NOT NULL,
  name             VARCHAR(150)  NOT NULL,
  description      TEXT          NULL,
  price            DECIMAL(12,2) NOT NULL DEFAULT 0.00,
  -- 150 = the spec's "2,5 jam untuk 1 jadwal".
  duration_minutes INT           NOT NULL DEFAULT 150,
  image_path       VARCHAR(255)  NULL,
  is_active        TINYINT(1)    NOT NULL DEFAULT 1,
  -- Shown on the public site with a "coming soon" label, but not bookable.
  is_coming_soon   TINYINT(1)    NOT NULL DEFAULT 0,
  sort_order       INT           NOT NULL DEFAULT 0,
  created_at       DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at       DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uq_services_slug (slug),
  KEY idx_services_active_sort (is_active, sort_order, id),
  -- Mirrors the Phase 4 handler validation, so a bug there cannot write garbage.
  CONSTRAINT chk_services_price    CHECK (price >= 0),
  CONSTRAINT chk_services_duration CHECK (duration_minutes BETWEEN 15 AND 480)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- schedule_slots — bookable time slots.
--
-- Slots are global (PLAN.md Q4): service_id is NULL today and exists so
-- per-service slots can arrive later without a table rewrite.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schedule_slots (
  id           BIGINT       NOT NULL AUTO_INCREMENT,
  service_id   BIGINT       NULL,
  slot_date    DATE         NOT NULL,
  start_time   TIME         NOT NULL,
  end_time     TIME         NOT NULL,
  -- starts_at/ends_at collapse the timezone-less DATE+TIME pair into one
  -- comparable, indexable DATETIME. Two reasons they exist:
  --   1. "is this slot still in the future" becomes `starts_at >= ?`, a bind
  --      parameter next to a bare column — the only shape sqlc's MySQL engine
  --      can type-infer. Against TIMESTAMP(slot_date, start_time) it would emit
  --      an untyped interface{} parameter instead.
  --   2. They give availability and calendar queries an ordering key to index.
  -- MariaDB accepts no NULL/NOT NULL clause on a generated column, so these are
  -- nominally nullable. They can never actually be NULL — both base columns are
  -- NOT NULL — so sqlc.yaml overrides them back to a non-null Go time.Time.
  starts_at    DATETIME     GENERATED ALWAYS AS (TIMESTAMP(slot_date, start_time)) STORED,
  ends_at      DATETIME     GENERATED ALWAYS AS (TIMESTAMP(slot_date, end_time))   STORED,
  -- Parallel therapists are modelled as capacity, not as extra rows.
  capacity     INT          NOT NULL DEFAULT 1,
  booked_count INT          NOT NULL DEFAULT 0,
  is_active    TINYINT(1)   NOT NULL DEFAULT 1,
  note         VARCHAR(255) NULL,
  created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  -- Global slot start times are unique. NOTE: this is deliberately NOT
  -- UNIQUE(service_id, slot_date, start_time). service_id is NULL for every row
  -- today, and MySQL unique indexes do not collide on NULLs — that index would
  -- silently permit unlimited duplicates and break the Phase 5 generator's
  -- "re-running creates zero duplicates" guarantee. When Q4 flips to per-service
  -- slots, add `service_key BIGINT NOT NULL GENERATED ALWAYS AS
  -- (COALESCE(service_id, 0)) STORED` and key the index on that.
  UNIQUE KEY uq_slots_date_start (slot_date, start_time),
  KEY idx_slots_date_active (slot_date, is_active),
  KEY idx_slots_starts_at (starts_at),
  KEY idx_slots_service (service_id),
  CONSTRAINT fk_slots_service FOREIGN KEY (service_id) REFERENCES services (id)
    ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT chk_slots_window CHECK (end_time > start_time),
  -- Defence in depth for the concurrency design: even if the SELECT ... FOR
  -- UPDATE logic in the booking transaction regresses, the engine refuses to
  -- overbook. Consequence to handle in Phase 5: an admin lowering capacity below
  -- booked_count gets a constraint error, which must be rendered as
  -- "kapasitas tidak boleh lebih kecil dari jumlah booking".
  CONSTRAINT chk_slots_count  CHECK (booked_count >= 0 AND booked_count <= capacity)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- bookings
--
-- Slot-holding vs slot-releasing statuses — the rule the whole concurrency
-- design rests on:
--   HOLD the slot:    pending_payment, paid, confirmed, completed
--   RELEASE the slot: cancelled, expired
-- The invariant is therefore
--   schedule_slots.booked_count == COUNT(bookings for that slot whose status is
--                                        NOT IN ('cancelled','expired'))
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bookings (
  id               BIGINT        NOT NULL AUTO_INCREMENT,
  -- e.g. TPJ-20260726-A1B2
  booking_code     VARCHAR(32)   NOT NULL,
  user_id          BIGINT        NOT NULL,
  service_id       BIGINT        NOT NULL,
  slot_id          BIGINT        NOT NULL,
  customer_name    VARCHAR(150)  NOT NULL,
  customer_phone   VARCHAR(30)   NOT NULL,
  customer_address VARCHAR(500)  NULL,
  notes            VARCHAR(500)  NULL,
  -- Snapshot of services.price at booking time. A later price change must never
  -- alter what a past customer owes.
  price_amount     DECIMAL(12,2) NOT NULL,
  status           ENUM('pending_payment','paid','confirmed','completed','cancelled','expired')
                     NOT NULL DEFAULT 'pending_payment',
  -- When an unpaid booking stops holding its slot (created_at + payment expiry).
  expires_at       DATETIME      NULL,
  confirmed_at     DATETIME      NULL,
  completed_at     DATETIME      NULL,
  cancelled_at     DATETIME      NULL,
  cancelled_reason VARCHAR(255)  NULL,
  -- Phase 11: when the H-1 reminder email was sent. It is what makes the daily
  -- reminder sweep idempotent across restarts and across processes — the sweep
  -- claims a booking with a guarded UPDATE (... WHERE reminder_sent_at IS NULL)
  -- and only a RowsAffected() == 1 authorises the send, exactly as
  -- RowsAffected() == 1 is what authorises a ReleaseSlot everywhere else here.
  reminder_sent_at DATETIME      NULL,

  -- MySQL and MariaDB have no partial indexes, so this column stands in for one.
  -- It is the slot id while the booking holds the slot and NULL once it releases
  -- it; unique indexes do not collide on NULLs, so uq_bookings_active_slot_user
  -- below enforces "at most one live booking per (slot, user)" and nothing else.
  -- It is GENERATED rather than application-maintained so that no future status
  -- transition can forget to clear it.
  active_slot_id   BIGINT GENERATED ALWAYS AS (
                     CASE WHEN status IN ('cancelled','expired') THEN NULL ELSE slot_id END
                   ) STORED,

  created_at       DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at       DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

  PRIMARY KEY (id),
  UNIQUE KEY uq_bookings_code (booking_code),
  -- Mitigation #2 from PLAN.md § Design note: slot concurrency. Enforced by the
  -- engine, so a double-submitted form cannot create two bookings even if the
  -- application-level lock logic regresses. Surfaces as error 1062.
  UNIQUE KEY uq_bookings_active_slot_user (active_slot_id, user_id),
  KEY idx_bookings_user_status (user_id, status, id),
  KEY idx_bookings_slot_status (slot_id, status),
  KEY idx_bookings_status_expires (status, expires_at),
  KEY idx_bookings_service (service_id),
  KEY idx_bookings_created (created_at),

  CONSTRAINT fk_bookings_user    FOREIGN KEY (user_id)    REFERENCES users (id)
    ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT fk_bookings_service FOREIGN KEY (service_id) REFERENCES services (id)
    ON DELETE RESTRICT ON UPDATE RESTRICT,
  -- Must stay RESTRICT/RESTRICT: slot_id is the base column of the stored
  -- generated column active_slot_id, and MySQL forbids CASCADE / SET NULL /
  -- SET DEFAULT on such a column.
  CONSTRAINT fk_bookings_slot    FOREIGN KEY (slot_id)    REFERENCES schedule_slots (id)
    ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT chk_bookings_price  CHECK (price_amount >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- payments — one row per Midtrans order attempt. A booking may have several.
--
-- There is no payments.status enum on purpose: transaction_status is a raw
-- mirror of whatever Midtrans sends, and an enum would turn an unrecognised
-- future status into a strict-mode INSERT failure on a webhook we are obliged to
-- accept. Internal state lives in bookings.status; the payment's own lifecycle
-- is expressed by paid_at / expired_at / cancelled_at.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payments (
  id                 BIGINT        NOT NULL AUTO_INCREMENT,
  booking_id         BIGINT        NOT NULL,
  -- "tpj-<uuid-v4>" — the tpj prefix is what separates our transactions from the
  -- other Appskep systems sharing this Midtrans merchant account.
  order_id           VARCHAR(64)   NOT NULL,
  gross_amount       DECIMAL(12,2) NOT NULL,
  payment_type       VARCHAR(50)   NULL,
  transaction_id     VARCHAR(100)  NULL,
  transaction_status VARCHAR(50)   NULL,
  -- Midtrans reports transaction_time in WIB. The session clock is pinned to
  -- +07:00, so the string parses and stores with no conversion.
  transaction_time   DATETIME      NULL,
  fraud_status       VARCHAR(30)   NULL,
  status_code        VARCHAR(10)   NULL,
  bank               VARCHAR(50)   NULL,
  va_number          VARCHAR(64)   NULL,
  snap_token         VARCHAR(255)  NULL,
  snap_redirect_url  VARCHAR(500)  NULL,
  paid_at            DATETIME      NULL,
  expired_at         DATETIME      NULL,
  cancelled_at       DATETIME      NULL,
  -- MariaDB implements JSON as LONGTEXT with an implicit json_valid() check:
  -- write NULL or valid JSON, never an empty string.
  raw_response       JSON          NULL,
  created_at         DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at         DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uq_payments_order_id (order_id),
  KEY idx_payments_booking (booking_id, id),
  KEY idx_payments_txn_status (transaction_status),
  KEY idx_payments_paid_at (paid_at),
  CONSTRAINT fk_payments_booking FOREIGN KEY (booking_id) REFERENCES bookings (id)
    ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- payment_notifications — raw audit log of every inbound Midtrans webhook.
--
-- Written BEFORE any validation or processing, for every payload, including
-- invalid signatures and order IDs belonging to other Appskep systems. There is
-- deliberately no foreign key on order_id: notifications legitimately arrive for
-- orders that do not exist in this database.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payment_notifications (
  id                 BIGINT       NOT NULL AUTO_INCREMENT,
  order_id           VARCHAR(64)  NOT NULL,
  transaction_status VARCHAR(50)  NULL,
  fraud_status       VARCHAR(30)  NULL,
  status_code        VARCHAR(10)  NULL,
  -- Stored as the exact string received. This value is an input to the SHA512
  -- signature, and round-tripping "150000.00" through DECIMAL would destroy it.
  gross_amount       VARCHAR(32)  NULL,
  signature_valid    TINYINT(1)   NOT NULL DEFAULT 0,
  processed          TINYINT(1)   NOT NULL DEFAULT 0,
  process_note       VARCHAR(255) NULL,
  payload            JSON         NOT NULL,
  remote_ip          VARCHAR(45)  NULL,
  received_at        DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_pn_order (order_id, received_at),
  KEY idx_pn_received (received_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- settings — key/value site configuration editable from /admin/pengaturan.
--
-- Columns are setting_key / setting_value rather than key / value: `key` is a
-- reserved word and would need backticks in every query.
--
-- Where a key also exists in the environment (payment_expiry_minutes vs
-- PAYMENT_EXPIRY_MINUTES), the DB row wins when present and parseable; the env
-- value is the fallback and the fail-fast default.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settings (
  setting_key   VARCHAR(100) NOT NULL,
  setting_value TEXT         NULL,
  updated_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (setting_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- activity_logs — audit trail for admin actions and booking state changes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS activity_logs (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  user_id    BIGINT       NULL,
  action     VARCHAR(80)  NOT NULL,
  entity     VARCHAR(50)  NULL,
  entity_id  BIGINT       NULL,
  meta       JSON         NULL,
  ip         VARCHAR(45)  NULL,
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_activity_user (user_id, created_at),
  KEY idx_activity_entity (entity, entity_id, created_at),
  -- The log outlives the user: an audit trail that vanishes with its subject is
  -- not an audit trail.
  CONSTRAINT fk_activity_user FOREIGN KEY (user_id) REFERENCES users (id)
    ON DELETE SET NULL ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
