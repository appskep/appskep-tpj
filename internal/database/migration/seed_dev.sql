-- seed_dev.sql — development seed data.
--
-- Safe to re-run: every statement is idempotent, and rows a human may have
-- edited in the admin panel are never overwritten. `make db-fresh` is the normal
-- path after a schema change.
--
-- Not loaded by sqlc: sqlc.yaml points at 0001_schema.sql specifically, and this
-- file is excluded from `make migrate` by the 0*.sql glob.

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;
-- Match the session clock the application uses (see DBConfig.DSN), so CURDATE()
-- below resolves to the same day the app thinks it is.
SET time_zone = '+07:00';

-- ---------------------------------------------------------------------------
-- Admin user.
--
-- appskep_user_id 1 is the developer's own account at dev-auth.appskep.id. The
-- email and name here are placeholders: the Phase 3 login upsert overwrites both
-- from the JWT on the first real login. Keep ADMIN_USER_IDS=1 in .env so the
-- bootstrap promotion path also works against a database seeded from scratch.
--
-- To make a different account admin, either add its Appskep user_id to
-- ADMIN_USER_IDS and log in once, or:
--   UPDATE users SET role='admin' WHERE appskep_user_id = <id>;
-- ---------------------------------------------------------------------------
INSERT INTO users (appskep_user_id, email, name, role, is_active)
VALUES (1, 'admin@tpj.local', 'Admin TPJ', 'admin', 1)
ON DUPLICATE KEY UPDATE role = 'admin', is_active = 1;

-- ---------------------------------------------------------------------------
-- Layanan. Copy is verbatim from specification.txt; prices confirmed with the
-- client. duration_minutes is the spec's "2,5 jam untuk 1 jadwal".
--
-- Re-seeding refreshes the copy and ordering but never touches price,
-- is_active, is_coming_soon or image_path — those belong to /admin/layanan.
-- ---------------------------------------------------------------------------
INSERT INTO services (slug, name, description, price, duration_minutes, is_active, is_coming_soon, sort_order)
VALUES
  ('urut-therapeutic', 'Urut Therapeutic',
   'Terapi fisik guna mengurangi nyeri otot, demam, dan insomnia.',
   75000.00, 150, 1, 0, 1),
  ('massage-therapeutic', 'Massage Therapeutic',
   'Terapi fisik untuk relaksasi, demam, cedera, dan insomnia. Terapis membawa kasur terapi, aromaterapi, dan perlengkapan bekam kering ke tempat Anda.',
   150000.00, 150, 1, 0, 2),
  ('bekam-therapeutic', 'Bekam Therapeutic',
   'Terapi fisik dan bekam basah guna mendukung pemulihan tubuh, pengobatan, dan kesuburan. Terapis membawa perlengkapan massage VIP, bekam kering, dan bekam luncur ke tempat Anda.',
   300000.00, 150, 1, 0, 3)
ON DUPLICATE KEY UPDATE
  name             = VALUES(name),
  description      = VALUES(description),
  duration_minutes = VALUES(duration_minutes),
  sort_order       = VALUES(sort_order);

-- ---------------------------------------------------------------------------
-- Schedule slots: CURDATE()-2 .. CURDATE()+14, four 150-minute slots per day
-- (08:00-10:30, 10:30-13:00, 13:00-15:30, 15:30-18:00). A fifth would end at
-- 20:30, outside the 08:00-20:00 operating window. 17 days x 4 = 68 rows.
--
-- The two past days are deliberate: they make "past slots are excluded from
-- public availability" testable today rather than tomorrow.
--
-- INSERT IGNORE against uq_slots_date_start makes a re-run a no-op. The
-- generated columns starts_at/ends_at are correctly omitted from the column
-- list. A flat UNION ALL rather than WITH RECURSIVE, whose placement inside
-- INSERT varies between MariaDB versions.
-- ---------------------------------------------------------------------------
INSERT IGNORE INTO schedule_slots (service_id, slot_date, start_time, end_time, capacity, is_active)
SELECT NULL, DATE_ADD(CURDATE(), INTERVAL d.n DAY), t.st, t.et, 1, 1
FROM (
            SELECT -2 AS n
  UNION ALL SELECT -1 UNION ALL SELECT  0 UNION ALL SELECT  1 UNION ALL SELECT  2
  UNION ALL SELECT  3 UNION ALL SELECT  4 UNION ALL SELECT  5 UNION ALL SELECT  6
  UNION ALL SELECT  7 UNION ALL SELECT  8 UNION ALL SELECT  9 UNION ALL SELECT 10
  UNION ALL SELECT 11 UNION ALL SELECT 12 UNION ALL SELECT 13 UNION ALL SELECT 14
) AS d
CROSS JOIN (
            SELECT '08:00:00' AS st, '10:30:00' AS et
  UNION ALL SELECT '10:30:00', '13:00:00'
  UNION ALL SELECT '13:00:00', '15:30:00'
  UNION ALL SELECT '15:30:00', '18:00:00'
) AS t;

-- No bookings and no payments are seeded. A hand-written booking would either
-- violate the booked_count invariant or need hand-maintaining, and it would
-- pollute the starting state of the Phase 13 concurrency test. Create bookings
-- through the application.

-- ---------------------------------------------------------------------------
-- Settings. Never overwritten on re-seed: these are edited in
-- /admin/pengaturan (Phase 10). Where a key also exists in the environment
-- (payment_expiry_minutes vs PAYMENT_EXPIRY_MINUTES), the DB row wins when
-- present and parseable; the env value is the fallback.
-- ---------------------------------------------------------------------------
INSERT INTO settings (setting_key, setting_value) VALUES
  ('site_name',                     'Terapi Pemuda Jompo'),
  ('site_tagline',                  'Terapi fisik profesional, terapis datang ke tempatmu.'),
  ('site_description',              'Layanan urut, massage, dan bekam therapeutic ke rumah atau kos. Booking dan pembayaran online.'),
  -- Fallback og:image for pages with no image of their own. Seeded empty on
  -- purpose: no default social asset ships with the repo, so the row exists to
  -- give the Phase 10 settings form something to edit, and the tag is simply
  -- omitted until someone fills it in.
  ('site_og_image',                 ''),
  ('contact_email',                 'halo@terapipemudajompo.id'),
  ('contact_phone',                 '0812-3456-7890'),
  ('whatsapp_number',               '6281234567890'),
  -- The area served, not a venue: the therapist travels to the customer.
  ('contact_address',               'Yogyakarta dan sekitarnya'),
  ('instagram_url',                 'https://instagram.com/terapipemudajompo'),
  ('booking_lead_time_minutes',     '120'),
  ('booking_max_days_ahead',        '30'),
  ('booking_terms',                 'Terapis datang ke alamat yang Anda isi — mohon siapkan ruang yang cukup untuk berbaring. Pembatalan kurang dari 2 jam sebelum jadwal tidak dapat direfund.'),
  ('payment_expiry_minutes',        '60'),
  ('slot_default_duration_minutes', '150'),
  ('slot_default_capacity',         '1'),
  ('slot_break_minutes',            '0'),
  ('slot_day_start',                '08:00'),
  ('slot_day_end',                  '20:00'),
  ('slot_weekdays',                 '1,2,3,4,5,6,7'),
  ('admin_notification_email',      'admin@tpj.local')
ON DUPLICATE KEY UPDATE setting_key = setting_key;
