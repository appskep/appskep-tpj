# PLAN.md — Appskep TPJ (Terapi Pemuda Jompo)

Development plan derived from [specification.txt](specification.txt), [authentication.md](authentication.md), and [create-order.md](create-order.md).
Sistem booking & pembayaran online: **Go monolith + chi + sqlc + MariaDB + Go html/template + TailwindCSS + Lucide/Simple Icons + Hotwire Turbo**.

---

## How to use this plan

- Tasks are checkboxes. **Always tick (`- [x]`) a task immediately after it is executed and verified.**
- Phases are ordered by dependency, but each phase is self-contained enough to be executed in a separate session.
- When resuming: read the **Progress** table, jump to the first phase that is not `DONE`.
- If a task is intentionally skipped, mark it `- [~]` and add a short reason inline.
- Update the Progress table at the end of every working session.

**Status legend:** `TODO` · `WIP` · `DONE` · `BLOCKED`

### Progress

| # | Phase | Status | Notes |
|---|-------|--------|-------|
| 0 | Bootstrap & scaffolding | DONE | 2026-07-26. `make dev` + `/api/health` verified. `make sqlc` blocked until Phase 1 writes the schema. `AUTH_SECRET` in `.env` is unverified against dev-auth — see **Q2** |
| 1 | Database schema & sqlc | DONE | 2026-07-26. 8 tables, 7 query files, `repository.Store`. Generated columns accepted by sqlc natively — no fallback needed. See **Phase 1 notes** below |
| 2 | Frontend pipeline (Tailwind, Turbo, icons, layouts) | DONE | 2026-07-26. Tailwind v4 standalone (no Node), self-hosted fonts + icon sprite, `view` renderer, both layouts, error pages. Landing page pulled forward from Phase 6. See **Phase 2 notes** below |
| 3 | Authentication (Appskep SSO redirect flow) | DONE | 2026-07-26. Signed-cookie session (token only), `OptionalAuth`/`RequireAuth`/`RequireAdmin`, `/login` + `/logout`. The five other SSO redirect pages deferred — Appskep paths still undocumented. See **Phase 3 notes** below |
| 4 | Admin — Layanan (services) | DONE | 2026-07-26. Full CRUD, search, pagination, image upload, Turbo Stream toggles. Four codebase conventions established: validation errors, turbo partials, form parsing, uploads. See **Phase 4 notes** below |
| 5 | Admin — Penjadwalan (schedule slots + generator) | DONE | 2026-07-27. Calendar + list views, single-slot CRUD, Turbo Stream toggle, generator with preview, bulk range actions. Booked slots are edit-locked. See **Phase 5 notes** below. **M2 complete** |
| 6 | Public — Landing & Layanan | DONE | 2026-07-27. `/layanan` + `/layanan/{slug}` with a read-only jadwal-terdekat preview, ENV-aware `robots.txt`, generated `sitemap.xml`, canonical + OG pass. See **Phase 6 notes** below. **M3 complete** |
| 7 | Public — Booking flow | DONE | 2026-07-27. One-URL flow with a Turbo Frame slot picker, the locking booking transaction, a 1-minute expiry ticker, and a placeholder payment page. 20 goroutines on a capacity-1 slot → exactly 1 winner. Two real defects found by verification (REPEATABLE READ snapshot ordering; Turbo discarding 200-after-POST, which also affected Phase 5). See **Phase 7 notes** below. **M4 complete** |
| 8 | Payment — Midtrans sandbox | DONE | 2026-07-27. Real Snap orders against the sandbox, the webhook state machine, a manual re-sync, and a Turbo Frame status poller. Three defects found by verification (a NULL JSON column sqlc cannot scan; a self-referencing turbo-frame `src`; fragments served from the browser cache). See **Phase 8 notes** below. M5 needs Phase 9 too |
| 9 | Public — Konfirmasi, Riwayat, Profil | DONE | 2026-07-27. Konfirmasi is now the canonical booking page (pembayaran 303s to it, Midtrans `finish` repointed), riwayat with status filter + pagination, profil with avatar upload, and user cancellation that releases the slot **and** cancels the Midtrans order. Zero schema and zero sqlc change — Phase 1 had already written every query. See **Phase 9 notes** below. **M5 complete** |
| 10 | Admin — Booking, Pembayaran, Dashboard, Users | DONE | 2026-07-27. Dashboard with live figures, booking list + detail + five actions incl. the two-slot reschedule, payments list/detail/re-sync, users with the last-admin guard, settings form, CSV export. Two defects found by verification (sqlc silently mis-binding `BETWEEN`; a template shared between two pages). See **Phase 10 notes** below. M6 needs Phase 11 too |
| 11 | Notifikasi email | DONE | 2026-07-27. Six transactional emails, an async worker that drains on shutdown, and an H-1 reminder made idempotent by a new `bookings.reminder_sent_at` — the phase's one schema change. Split `mail` (transport) / `service.Email` (rules), mirroring Midtrans. One defect found while writing it (html/template escaping the plain-text part and the Subject header). See **Phase 11 notes** below. **M6 complete** |
| 12 | Hardening (CSRF, rate limit, validation, errors) | DONE | 2026-07-27. Session-bound CSRF with per-render masking, per-route token buckets over a trusted-proxy client IP, a strict CSP that cost the app its last four inline scripts, a global body cap, and `private, no-store` on authenticated pages. Two defects found by verification (a 403 where a 413 belonged; Turbo's progress-bar stylesheet blocked by `style-src`). **`govulncheck`: 30 stdlib findings, all fixed by a toolchain bump — Phase 14 must build on go1.25.12+.** See **Phase 12 notes** below |
| 13 | Testing | DONE | 2026-07-28. 33 test files, ~250 tests, in three tiers: pure unit beside the code, DB-backed in `internal/integration`, harness in `internal/testsupport`. Every earlier phase's deferred ask is now permanent, including the concurrency test PLAN.md calls the one that matters most. Stdlib `testing` only. Two real defects found (a 500 on an empty webhook body; a predicate named for a rule it did not implement), and three load-bearing tests proven to fail by breaking the code they guard. CI deferred to Phase 14. See **Phase 13 notes** below |
| 14 | Deployment & go-live | TODO | |

---

## Resolved decisions

| ID | Decision | Source |
|----|----------|--------|
| **R1** | **Auth is redirect-based SSO, not a local login form.** TPJ owns no credentials. Unauthenticated request → redirect to `AUTH_URL/v2/auth/login?client_base_url=<scheme>://<host><uri>`; Appskep redirects back with `?access_token=<jwt>&pass=<path>`; TPJ verifies the JWT with the shared `AUTH_SECRET` (HMAC) and stores it in a cookie session. Claims used: `user_id`, `email`, `name`, `expired_at`. | [authentication.md](authentication.md) |
| **R2** | **Register / forgot password / reset password / verify email / resend verification are owned by Appskep.** The pages listed in the spec become thin redirect routes in TPJ (see Phase 3) so links keep working, but no credential handling is implemented here. | [authentication.md](authentication.md) + spec |
| **R3** | **No Google OAuth.** Dropped from the spec — Appskep username/password only. | specification.txt:47 |
| **R4** | **Midtrans direct, Snap API, sandbox first.** Keys are Appskep's sandbox merchant keys, held in `.env`. `MIDTRANS_ENV=midtrans.Sandbox` (anything other than the literal `midtrans.Production` = sandbox), using the `midtrans-go` SDK. | specification.txt:50-55, [create-order.md](create-order.md) |
| **R5** | **Order ID format: `tpj-<uuid-v4>`** — the `tpj` prefix identifies this system inside the shared Appskep Midtrans account. | specification.txt:52, create-order.md:113-114 |
| **R6** | **Webhook signature:** `SHA512(order_id + status_code + gross_amount + MIDTRANS_SERVER_KEY)`, using the `gross_amount` string exactly as received (Midtrans sends `"150000.00"`). Public endpoint, authenticated by signature only. | create-order.md:199-205 |
| **R7** | **Payment is confirmed only by the webhook.** A successful Snap-token response is not a purchase — booking stays `pending_payment` until `settlement` (or `capture`+`accept`) arrives. | create-order.md:118, 216 |

## Open questions (resolve before the phase that needs them)

| ID | Question | Blocks | Assumption if unanswered |
|----|----------|--------|--------------------------|
| Q1 | ~~Admin panel access control: Appskep `_access` RBAC, or a local `users.role='admin'` flag?~~ **RESOLVED in Phase 1.** | Phase 3, 10 | **Local `users.role`**, bootstrapped from the `ADMIN_USER_IDS` allowlist (now `=1`). Schema shipped with the `role` enum; no dependency on the auth service DB. Structured so RBAC can be swapped in later. |
| Q2 | ~~What is TPJ's `AUTH_CLIENT_ID` at `dev-auth.appskep.id`?~~ **RESOLVED in Phase 3.** | Phase 3 | `AUTH_CLIENT_ID=19`, set in `.env` by the user. Admin identity is Appskep `user_id=1`, seeded and in `ADMIN_USER_IDS`. |
| Q3 | Slot capacity: 1 booking per slot, or multiple (multiple therapists)? | Phase 5, 7 | `capacity` column, default `1`. |
| Q4 | Are schedule slots global, or per-layanan? | Phase 5 | Global slots; booking picks layanan + slot. Column `service_id` nullable for future per-service slots. |
| Q5 | Is there an on-site/cash payment option, or online-only? | Phase 8 | Online-only via Midtrans for v1. |
| Q6 | ~~Cancellation/reschedule policy & refunds?~~ **RESOLVED in Phase 10.** | Phase 10 | **Both the customer and an admin may cancel**, with different reach: a customer only their own `pending_payment` booking (Phase 9), an admin any booking that still holds its slot — including a paid one, because otherwise freeing that slot means editing the database. **No automated refund in v1**: the cancel panel names the amount and states that the refund is manual, and the reason is recorded in `activity_logs`. Reschedule is admin-only and transactional. |
| Q7 | Which Appskep users may book — anyone with an Appskep account, or only an approved subset? | Phase 3, 7 | Any authenticated Appskep user is a customer. |

---

## Target architecture

```
appskep-tpj/
├── cmd/server/main.go              # config → routers → chi mount → http.Server
├── internal/
│   ├── app/
│   │   ├── public/                 # routes.go, handler/, middleware/
│   │   └── admin/                  # routes.go, handler/, middleware/
│   ├── database/
│   │   ├── migration/              # 0001_schema.sql, seed_dev.sql
│   │   ├── query/                  # *.sql for sqlc
│   │   └── sqlc/                   # GENERATED — never edit by hand
│   └── shared/
│       ├── config/                 # .env loader
│       ├── middleware/             # logger, csrf, ratelimit, recover
│       ├── model/                  # request/response structs, enums
│       ├── auth/                   # appskep SSO: jwt verify, refresh, session
│       ├── payment/                # PaymentGateway iface: midtrans.go
│       ├── service/                # booking, schedule, email — business logic
│       └── util/                   # jwt, response, upload, validate, format
├── template/
│   ├── layouts/{public,admin}/base.html
│   ├── pages/{public,admin}/*.html
│   └── partials/                   # turbo-frame fragments, components
├── static/{css,js,img,uploads/services}
├── sqlc.yaml · Makefile · .air.toml · .env.example · CLAUDE.md
```

**Module path:** `github.com/remorac/appskep-tpj` · **Go:** 1.25.1 · **Port:** 8080 (`SERVER_PORT`)

Route mounts: `/` → public, `/admin` → admin, `/api` → internal JSON (health, webhook).

---

## Phase 0 — Bootstrap & scaffolding

**Goal:** `make dev` serves a "hello" page with hot reload.

- [x] `git init`, add `.gitignore` (bin/, tmp/, .env, static/uploads/, node_modules/, *.DS_Store)
- [x] `go mod init github.com/remorac/appskep-tpj`
- [x] Add deps: `chi/v5`, `go-sql-driver/mysql`, `golang-jwt/jwt/v5`, `joho/godotenv`, `google/uuid`, `midtrans/midtrans-go`, `gorilla/sessions` (or a self-rolled signed cookie)
- [x] Create directory skeleton per target architecture
- [x] `internal/shared/config/config.go` — load `.env`; fields: Env, Server{Host,Port}, App{URL,PageSize,TZ}, DB{...,DSN()}, Auth{URL,Secret,ClientID,AdminUserIDs}, Session{Key}, Midtrans{Env,ServerKey,ClientKey,Prefix}, SMTP{...}
- [x] Fail-fast validation when `ENV=production` and any of `AUTH_SECRET`, `SESSION_KEY`, `DB_PASSWORD`, `MIDTRANS_SERVER_KEY` is empty
- [x] Write `.env.example` (placeholders only) + local `.env` with the real sandbox values:
      `AUTH_URL=https://dev-auth.appskep.id`, `MIDTRANS_ENV=midtrans.Sandbox`,
      `MIDTRANS_SERVER_KEY=SB-Mid-server-tWoZL9Wie7eL6Vg_72Qlf2de`, `MIDTRANS_CLIENT_KEY=SB-Mid-client-gHFFZLWjkJx6uhIY`, `MIDTRANS_PREFIX=tpj`
- [x] `.env` is gitignored; secrets never committed (note: `specification.txt` currently carries the sandbox keys — fine for sandbox, must not be repeated for production keys)
- [x] `SESSION_KEY` is a random 32-byte value from env — **never** a hardcoded literal (the reference app hardcodes `"secret"`; do not copy that)
- [x] `cmd/server/main.go` — config, DB pool (`SetMaxOpenConns`, `SetConnMaxLifetime`), chi root router, graceful shutdown on SIGINT/SIGTERM
- [x] `internal/shared/middleware/logger.go` + chi `Recoverer` + `RequestID`
- [x] `GET /api/health` returning `{"status":"ok","db":"ok"}`
- [x] `Makefile`: `build run dev test vet sqlc migrate seed clean tailwind tailwind-watch`
- [x] `.air.toml` for hot reload (watch go, html, js, sql)
- [x] `sqlc.yaml` (engine mysql, queries `internal/database/query`, schema `internal/database/migration`, out `internal/database/sqlc`, `emit_interface: true`, `emit_json_tags: true`)
- [x] Create MariaDB database `appskep_tpj` locally, confirm connection
- [x] Draft `CLAUDE.md` with commands + conventions (expand as the project grows)

**Acceptance:** `make dev` → `http://localhost:8080/api/health` returns ok, edits reload automatically.

---

## Phase 1 — Database schema & sqlc

**Goal:** Full schema applied, sqlc generates a compiling `sqlc` package.

### Schema — `internal/database/migration/0001_schema.sql`

- [x] `users` — **local mirror of the Appskep identity, no credentials stored.** id, appskep_user_id (unique, NOT NULL — from the JWT `user_id` claim), email, name, phone, address, avatar_path, role ENUM('user','admin') default 'user', is_active, last_login_at, created_at, updated_at
- [x] No `password_hash`, no `email_verifications`, no `password_resets` tables — credentials, verification, and resets are owned by the Appskep auth service (**R1**, **R2**)
- [x] `services` — id, slug (unique), name, description TEXT, price DECIMAL(12,2), duration_minutes INT, image_path, is_active BOOL default 1, is_coming_soon BOOL default 0, sort_order INT, created_at, updated_at
- [x] `schedule_slots` — id, service_id (nullable FK), slot_date DATE, start_time TIME, end_time TIME, capacity INT default 1, booked_count INT default 0, is_active BOOL default 1, note, created_at, updated_at; UNIQUE(service_id, slot_date, start_time); INDEX(slot_date, is_active)
- [x] `bookings` — id, booking_code (unique, e.g. `TPJ-20260726-A1B2`), user_id FK, service_id FK, slot_id FK, customer_name, customer_phone, customer_address, notes, price_amount DECIMAL(12,2) (snapshot), status ENUM('pending_payment','paid','confirmed','completed','cancelled','expired') default 'pending_payment', expires_at, cancelled_reason, created_at, updated_at; INDEX(user_id, status), INDEX(slot_id)
- [x] `payments` — id, booking_id FK, order_id (unique — `tpj-<uuid-v4>`, sent to Midtrans as `order_id`), gross_amount DECIMAL(12,2), payment_type, transaction_id, transaction_status, fraud_status, snap_token, snap_redirect_url, paid_at, expired_at, cancelled_at, raw_response JSON, created_at, updated_at
- [x] `payment_notifications` — id, order_id, signature_valid BOOL, payload JSON, received_at (raw Midtrans webhook audit log)
- [x] `settings` — key (PK), value TEXT, updated_at (site name, contact, WA number, booking lead time, slot default duration)
- [x] `activity_logs` (optional) — id, user_id, action, entity, entity_id, meta JSON, ip, created_at
- [x] Charset `utf8mb4_unicode_ci`, engine InnoDB, all FKs with sensible ON DELETE

### Seeds & queries

- [x] `seed_dev.sql` — 1 admin user (real Appskep `user_id`, `role='admin'`), the 3 layanan with the spec's copy, ±14 days of slots, default settings:
  - **Urut Therapeutic** — "Terapi fisik guna mengurangi nyeri otot, demam, dan insomnia."
  - **Massage Therapeutic** — "Terapi fisik untuk relaksasi, demam, cedera, dan insomnia. Dilengkapi dengan fasilitas VIP berupa kasur terapi, aromaterapi, dan bekam kering."
  - **Bekam Therapeutic** — "Terapi fisik dan bekam basah guna mendukung pemulihan tubuh, pengobatan, dan kesuburan. Dilengkapi fasilitas massage VIP, bekam kering, dan bekam luncur."
  - `is_coming_soon` left to the admin panel — no service is hardcoded as coming soon
- [x] `internal/database/query/users.sql`
- [x] `internal/database/query/services.sql`
- [x] `internal/database/query/schedules.sql` (incl. availability query joining booked_count < capacity)
- [x] `internal/database/query/bookings.sql` (incl. `SELECT ... FOR UPDATE` on slot for the booking transaction)
- [x] `internal/database/query/payments.sql`
- [x] `internal/database/query/settings.sql`
- [x] `make sqlc` generates without error; `go build ./...` passes
- [x] `internal/shared/repository` (or `store`) wrapper exposing `Queries` + `WithTx(ctx, fn)` helper

**Acceptance:** `make migrate && make seed && make sqlc && go build ./...` all succeed.

### Phase 1 notes — decisions taken during execution

Departures from the task list above, and the reasoning, so later phases build on the
right assumptions:

- **Slot-holding vs slot-releasing statuses defined.** `cancelled` and `expired` release
  the slot; `pending_payment`, `paid`, `confirmed`, `completed` hold it. PLAN.md said
  "non-terminal" without defining it — `completed` must hold, or the `booked_count`
  invariant breaks the moment an admin completes a booking.
- **`UNIQUE(service_id, slot_date, start_time)` replaced by `UNIQUE(slot_date, start_time)`.**
  Under Q4 every slot is global, so `service_id IS NULL` on every row — and MySQL unique
  indexes do not collide on NULLs. The original index would have permitted unlimited
  duplicate slots and silently broken Phase 5's "re-running creates zero duplicates".
  The migration path for when Q4 flips is commented in `0001_schema.sql`.
- **Partial unique index implemented as a STORED generated column.** `bookings.active_slot_id`
  is `slot_id` while the booking holds the slot and NULL once it releases, with
  `UNIQUE(active_slot_id, user_id)` on top. sqlc's parser accepted it natively, so the
  app-maintained fallback was **not** needed. Because `slot_id` is the base column of a
  stored generated column, `fk_bookings_slot` must stay `RESTRICT` — MySQL forbids
  CASCADE/SET NULL there.
- **`schedule_slots.starts_at` / `ends_at` added** as STORED generated DATETIMEs. They make
  the future-slot check a single indexable `starts_at >= ?`; sqlc cannot type a bind
  parameter sitting next to a `TIMESTAMP(...)` expression and would emit `interface{}`.
  MariaDB accepts no NULL/NOT NULL clause on a generated column, so `sqlc.yaml` overrides
  both back to a non-null `time.Time`.
- **Canonical lock order established: `schedule_slots` → `bookings` → `payments`.** Booking
  creation locks slot-then-booking while a naive expiry ticker would lock booking-then-slot;
  together they deadlock. This is why `ListExpiredPendingBookings` returns `slot_id`.
- **Timezone pinned to Jakarta wall clock.** `config.go` now sets the driver `loc` to
  `APP_TZ` and the MySQL session `time_zone` to a fixed `+07:00`, so `NOW()` and
  `time.Now().In(cfg.App.Location)` are the same clock. The fixed offset rather than the
  zone name because a server's `mysql.time_zone` tables are usually empty; `validate()`
  now fails fast if `APP_TZ` ever names a DST-observing zone.
- **sqlc override `db_type: time` → `string`.** `go-sql-driver` always returns TIME columns
  as a string, so sqlc's default `time.Time` mapping would fail at scan time.
- **`settings` columns are `setting_key` / `setting_value`** — `key` is a reserved word.
- **No `payments.status` enum.** `transaction_status` is a raw VARCHAR mirror of whatever
  Midtrans sends; an enum would turn an unrecognised future status into a strict-mode
  INSERT failure on a webhook we must accept. Payment lifecycle is `paid_at` / `expired_at`
  / `cancelled_at`.
- **`bookings.confirmed_at` / `completed_at` / `cancelled_at` added** for the Phase 10
  timeline and revenue reporting.
- **`0001_schema.sql` is authoritative until go-live.** `make migrate` re-pipes every
  `0*.sql` on each run, so it is re-runnable but blind to edits. Schema changes are made by
  editing that file and running the new `make db-fresh`. Numbered idempotent ALTERs start
  at Phase 14.
- **Phase 10 admin analytics queries deliberately deferred** — guessing filter shapes now
  buys nothing, and DECIMAL aggregates type poorly in sqlc's MySQL engine.
- **Verified, not assumed:** 20 goroutines racing one capacity-1 slot produced exactly one
  winner with `booked_count == COUNT(holding bookings)`; a duplicate booking by the same
  user raised 1062 on `uq_bookings_active_slot_user`; shrinking capacity below
  `booked_count` raised 4025 on `chk_slots_count`; applying a cancel twice moved the status
  once and decremented once. Phase 13 must turn this into a permanent test.

---

## Phase 2 — Frontend pipeline & layouts

**Goal:** Styled base layouts for public & admin, Turbo working, icons available offline.

- [x] Tailwind setup — **standalone Tailwind CLI v4.3.3, no Node dep**; input `static/css/input.css` → output `static/css/app.css`; `@source "../../template"` replaces content globs (v4 is CSS-first, no `tailwind.config.js`)
- [x] Define design tokens: brand palette, font (self-hosted, `static/fonts/`), radius/shadow scale
- [x] Vendor Hotwire Turbo (`static/js/turbo.js` — see notes on the name) — self-hosted, no CDN
- [x] Lucide icons — vendor as an SVG sprite (`static/img/icons.svg`) + Go template helper `{{icon "calendar" "w-5 h-5"}}`
- [x] Simple Icons for brand marks (WhatsApp, Instagram) into the same sprite as `brand-*`
- [x] `template/layouts/public/base.html` — header/nav (logo, Layanan, Booking, Riwayat, Login/Profil), footer, flash message region
- [x] `template/layouts/admin/base.html` — sidebar nav, topbar with user menu, content slot
- [x] Template renderer in **`internal/shared/view/`** (not `util/template.go` — see notes): parse-once at startup, reparse per request when `ENV=development`, FuncMap (`icon`, `rupiah`, `dateID`, `timeRange`, `statusBadge`, `csrfField`, `truncate`, + `asset`, `duration`, `nullstr`, `waLink`, `activeNav`)
- [x] Shared partials: alert/flash, empty-state, pagination, confirm-dialog (doubles as the modal), button, badge, service-card
- [x] Error pages: 404, 403, 405, 500 for both layouts, with a renderer-aware recoverer
- [x] Responsive check: mobile-first, ≥360px, no horizontal scroll — **measured**, not eyeballed (360/390/414/768 all `scrollWidth == clientWidth`)
- [x] `make tailwind` / `make tailwind-watch` wired into `make dev`
- [~] `<turbo-frame>` conventions — `RenderFragment` / `RenderStream` exist on the renderer, but no page needs a frame yet. The conventions get settled by the first real consumer in Phase 4.

**Acceptance:** ✅ Public landing + admin dashboard render styled, icons show, Turbo Drive navigations work without a full reload (verified: the JS context survived a `Turbo.visit`, so the document was swapped rather than reloaded).

### Phase 2 notes — decisions taken during execution

- **Renderer lives in `internal/shared/view/`, not `internal/shared/util/template.go`.** The
  render envelope carries a user view-model while the formatters it calls live in `util`.
  Putting the renderer in `util` would make `util` depend on the envelope and the envelope
  depend on `util` — an import cycle the moment Phase 3 adds the signed-in user. `util` stays
  a leaf package of pure helpers (`format.go`, `response.go`) and `view` composes them.
- **`app.Deps` replaces positional `Routes(cfg, store)` parameters.** Phases 3–11 each add a
  shared dependency (session store, payment gateway, email service, expiry ticker); threading
  those positionally means editing every `Routes` signature and handler constructor five more
  times. `Routes(d *app.Deps)` absorbs each addition for free. `internal/app` also now owns
  the error pages, the recoverer and the static handler, because all three need `Deps`.
- **Tailwind v4 standalone binary, downloaded on demand into `bin/`.** No `package.json`, no
  `node_modules`. v4 is CSS-first, so there is **no `tailwind.config.js`**: tokens live in an
  `@theme` block in `input.css` and `@source "../../template"` replaces the content globs. The
  version is pinned in the Makefile, and an unknown platform is a hard error rather than the
  old silent skip — a no-op CSS build serves an unstyled site that looks like a template bug.
- **`static/js/turbo.js`, not `turbo.min.js`.** `@hotwired/turbo` 8 publishes only unminified
  builds (`es2017-esm`, `es2017-umd`), so there is no minified artifact upstream to vendor and
  the `.min` suffix would be a lie. 212K, ~50K gzipped. The UMD build is used so the script tag
  needs no `type="module"`.
- **Vendored assets are committed and rebuilt by scripts, not by the build.** `make icons`,
  `make fonts`, `make js` (or `make assets`) re-fetch the sprite, the woff2 files and Turbo.
  A clean checkout needs no network and the app has no CDN dependency at runtime.
- **`.air.toml` no longer watches `template/` or `static/`.** Two bugs fixed at once: the
  Tailwind watcher writes `static/css/app.css`, so watching `static` or the `css` extension
  restarted the Go server on every keystroke in a template; and since the renderer reparses
  per request in development, `template/` never needed watching at all. `make dev` now runs the
  watcher backgrounded with a `trap`, so Ctrl-C does not leave an orphan.
- **A settings service was pulled forward from Phase 10.** The layouts need `site_name`,
  `site_tagline`, `whatsapp_number`, `instagram_url` and the contact block, all of which were
  already seeded rows with a `ListSettings` query waiting. `service.Settings` is an in-memory
  read-through cache with a `Reload` for Phase 10's settings form. It honours the "DB row wins
  when present and parseable, env is the fallback" rule without `config` losing its monopoly
  on `os.Getenv`.
- **The landing page was pulled forward from Phase 6** (`GET /`, ticked there). Phase 6 keeps
  `/layanan`, `/layanan/{slug}`, `sitemap.xml`, `robots.txt` and the full SEO pass.
- **`rupiah` takes a `string`.** sqlc maps `DECIMAL(12,2)` to `string` on MySQL, so
  `Service.Price` and `Booking.PriceAmount` arrive as `"150000.00"`. The formatter splits the
  fraction off textually and never parses a number, so there is no `float64` on a payment path
  by construction rather than by discipline.
- **`statusBadge` returns a struct, not HTML.** `{Label, Class}` rendered by the badge partial,
  so there is nothing for the Phase 12 `template.HTML` audit to look at. `icon` is the **only**
  `template.HTML` in the codebase; both its arguments are regex-validated and anything else
  renders nothing.
- **Both mobile menus are CSS-only** (a `sr-only` checkbox plus `peer-checked:`), because
  Turbo Drive replaces `<body>` on every navigation and would reset JS-held open state anyway.
  One trap, hit and fixed here: **Tailwind's `peer-*` variants compile to a subsequent-sibling
  selector** (`:is(:where(.peer):checked~*)`), so the checkbox has to be a *sibling* of every
  element it drives. The admin checkbox was first placed outside the flex wrapper, which made
  the sidebar a nephew rather than a sibling and left the hamburger doing nothing on mobile.
- **No dark mode.** A single committed public look. A theme axis would double the review
  surface of every screen from Phase 4 onward for no user benefit.
- **No separate `modal` partial.** `confirm.html` is a native `<dialog>`; a modal is the same
  markup with different copy, so a second near-identical partial would only be a thing to keep
  in sync. Focus trapping, Escape-to-close and the backdrop come from the browser.
- **No component gallery page.** The real landing page was built instead (the user's call), so
  the partials are exercised by production markup rather than by a showcase that can rot.
- **Design direction, since the spec carries no branding at all.** Warm clay + deep green,
  with deep green as the *structural* colour (public header, footer, admin sidebar) and clay
  reserved for actions — the inverse of the cream-page-with-a-terracotta-accent arrangement
  that every wellness site already uses. Display face Bricolage Grotesque, body face Plus
  Jakarta Sans (commissioned for Jakarta's city identity). The hero states a diagnosis —
  *"Umurmu 25. Punggungmu 65."* — over a list of the three aches, each linking to the layanan
  that treats it, beside a figure whose spine is the one clay line on the page.
- **The hero ache list is flow content, not hotspots on the illustration.** Absolute
  positioning was built first and thrown away: the labels collided with each other and with
  the figure, one dot sat on the head while claiming to mark the shoulders, and the staggered
  entrance briefly hid what is the hero's primary navigation.
- **Verified, not assumed:** all five routes return their intended status; the three seeded
  layanan render at `Rp75.000` / `Rp150.000` / `Rp300.000` and `2,5 jam`; zero `ZgotmplZ` in
  the output (no escaping failures); a template edit is picked up by the next request with no
  restart; a panic renders the styled 500, is logged with a stack, and leaves the server
  serving; `/admin/nope` keeps admin chrome while `/nope` keeps public chrome and `/api/nope`
  returns JSON; no horizontal overflow at 360/390/414/768; `Turbo.visit` preserved the JS
  context; both mobile toggles flip their targets' computed styles at a 390px viewport.
  Phase 13 should turn the formatter and status-machine cases into permanent tests.

---

## Phase 3 — Authentication (Appskep SSO)

> Per **R1**–**R3**: TPJ owns **no** credentials. No login form, no password check, no user lookup for authentication. Port the pattern from [authentication.md](authentication.md) — that document describes a Gin app, so adapt it to chi rather than copying code.

### Core flow

- [x] `internal/shared/auth/claims.go` (named `claims.go`, not `token.go`) — verify the Appskep JWT (HMAC, `AUTH_SECRET`, signing method pinned); parse `user_id` (JSON number → `float64`, narrowed defensively), `email`, `name`, `expired_at`
- [x] `internal/shared/auth/session.go` — signed cookie session holding **only the token**; HTTP-only, SameSite=Lax, Path=/, Secure from `SESSION_SECURE`, key from `SESSION_KEY`
- [x] `internal/app/auth.go` (not `shared/middleware/auth.go` — see notes) — the single gate:
  - [x] No session + no `access_token` query param → redirect to `AUTH_URL/v2/auth/login?client_base_url=<APP_URL><request URI>`
  - [x] Inbound `?access_token=<jwt>&pass=<path>` → verify, upsert user, store session, 303 to the sanitised `pass`
  - [x] Valid session → load user, put `*model.User` in the request context
  - [x] `expired_at` in the past → refresh inline via `AUTH_URL/oauth/refresh-token?token=<current>`; any failure clears the session and redirects to login
- [x] `auth.UserFrom(ctx)` helper + the `view.User` view-model (name, email, avatar, is-admin) injected into every template render by `renderer.fill`
- [x] User upsert on login: match on `appskep_user_id`, create the local `users` row on first login, refresh `email`/`name`, stamp `last_login_at`
- [x] `RequireAuth` (any Appskep user) and `RequireAdmin` (local `users.role='admin'`, bootstrapped from `ADMIN_USER_IDS`) — see **Q1**
- [x] `OptionalAuth` for public pages that render differently when logged in (landing, layanan) but must stay reachable anonymously
- [x] `GET /logout` — clears the local session, redirects to the **public home page** with a flash rather than to the Appskep login page (see notes). It does **not** invalidate the token upstream

### Spec pages under SSO (**R2**)

The spec lists login/register/forgot/reset/verify/resend pages. Under SSO these are Appskep's, so TPJ implements them as thin redirects that preserve the return path — links and bookmarks keep working, no credential code lives here.

- [x] `GET /login` → `AUTH_URL/v2/auth/login?client_base_url=...` (honours `?next=`)
- [~] `GET /register` → Appskep registration URL — deferred, path unknown
- [~] `GET /lupa-password` → Appskep forgot-password URL — deferred, path unknown
- [~] `GET /reset-password` → Appskep reset-password URL — deferred, path unknown
- [~] `GET /verifikasi-email` → Appskep verify-email URL — deferred, path unknown
- [~] `GET /kirim-ulang-verifikasi` → Appskep resend-verification URL — deferred, path unknown
- [~] Confirm the exact Appskep paths for register/forgot/reset/verify/resend before wiring (only the login path is documented in [authentication.md](authentication.md)) — **still unconfirmed**, which is why the five routes above are deferred rather than shipped against a guess. Ask Appskep, then add them; each is a three-line handler once the path is known.

### Do not repeat the reference app's weaknesses

[authentication.md](authentication.md) §Security notes lists real defects in the source app. Explicitly avoid each:

- [x] Session key from `SESSION_KEY` env (random 32 bytes) — **not** a hardcoded `[]byte("secret")`. `config.validate()` now requires it in *every* environment, and `NewSessionManager` refuses to build with a key under 32 bytes
- [x] Strip `access_token` from the URL immediately after consuming it (303 to a clean path, `Cache-Control: no-store`) so it stays out of history, `Referer`, and access logs. The request logger only ever records `r.URL.Path`, so the token never reaches the log either
- [x] Never render the raw token into HTML or JS — `view.User` has no token field, and the session carries no identity for a template to prefer over the claims
- [x] Deny paths `return` after writing the response — never fall through to the rest of the middleware
- [x] No unauthenticated cache-reload or debug endpoints
- [x] Hardcode nothing about the auth host in templates — the `loginURL` helper emits the local `/login?next=`, and only `config.AuthConfig` knows `AUTH_URL`

**Acceptance:** ✅ Verified 2026-07-26 (see notes for how). A protected route bounces to `dev-auth.appskep.id` with the right `client_base_url`; the callback lands back on the original page with a session; an expired token takes the refresh path and a failed refresh clears the session; `/logout` clears the session; a non-admin Appskep user gets the styled 403 on `/admin` with admin chrome.

### Phase 3 notes — decisions taken during execution

- **The middlewares live in `internal/app/auth.go`, not `internal/shared/middleware/auth.go`.**
  Denying a request means rendering the styled 403, which needs `Deps` — and
  `internal/app` already imports `shared/middleware`, so the direction PLAN.md sketched
  is an import cycle. This is the same reason `Deps.Recoverer` lives in `internal/app`.
  `OptionalAuth` / `RequireAuth` / `RequireAdmin` are thin wrappers over one shared
  resolver parameterised by what to do on denial.
- **The session cookie carries the token and nothing else.** PLAN.md line 327 listed
  `token / user_id / email / name`. Storing identity is what turns the reference app's
  hardcoded cookie key into an identity-spoofing bug rather than a cosmetic one: its
  middleware reads `email` and `name` from the session *in preference to the JWT claims*.
  Here name and email are re-derived from verified claims on every request, so the
  displayed identity is not forgeable even if `SESSION_KEY` leaked. Codec is
  `base64url(json) + "." + base64url(HMAC-SHA256)`, compared with `hmac.Equal`; a bad
  signature yields an anonymous visitor, never an error.
- **`client_base_url` is built from `APP_URL`, never from `r.Host` / `X-Forwarded-Proto`.**
  That value decides where Appskep delivers a valid token; taking it from a request header
  would let an attacker with a spoofed `Host` have the token posted to their own origin.
  `main.go` rejected chi's `RealIP` for the same class of reason.
- **`/logout` lands on `/`, not on the Appskep login page** as PLAN.md line 337 said.
  Redirecting to SSO re-enters the login flow immediately, and while the upstream Appskep
  session is alive that flow completes without prompting — so "Keluar" would clear the
  cookie and then silently restore it, appearing to do nothing. `/` is public, so the
  logout is observable, and a flash confirms it. The upstream session is untouched either
  way; this app cannot invalidate a token it did not issue.
- **A rejected callback token does not bounce back to Appskep.** A token that fails
  verification means the shared `AUTH_SECRET` is wrong, and redirecting to SSO would
  produce another bad token and another redirect — the redirect loop authentication.md's
  troubleshooting section describes. It lands on `/` with an error flash instead, which
  terminates the loop and is diagnosable.
- **The Turbo 8 prefetch trap.** Turbo 8 prefetches links on hover by default, so a plain
  `<a href="/logout">` ends the session on mouseover — the admin topbar already had one.
  Both logout links carry `data-turbo-prefetch="false"`.
- **`AUTH_SECRET` and `SESSION_KEY` are now required in every environment**, not just
  production. Without either, nothing verifies and nothing is signed; a development
  instance that quietly accepts forged sessions is worse than a failed boot. The
  production-only block in `validate()` keeps `DB_PASSWORD` and `MIDTRANS_SERVER_KEY`.
- **`LoadUser` runs on every authenticated request** rather than caching the row in the
  session. Role and `is_active` are precisely the two things an admin changes expecting an
  immediate effect; a cached copy would keep a demoted or deactivated user working until
  their cookie expired. It is one lookup on a unique index. `SyncUser` — the upsert that
  stamps `last_login_at` and applies the `ADMIN_USER_IDS` bootstrap — runs only on the
  callback, inside one transaction, and only ever *promotes*, so a Phase 10 demotion
  survives the next login.
- **`expired_at`'s wire format is still unconfirmed.** authentication.md never states it,
  and dev-auth was not reachable with a real session during this phase. `claimTime` accepts
  a unix number (seconds, or millis above 1e12) or an RFC3339/`2006-01-02 15:04:05` string,
  and falls back to the zero time — which disables the inline check without locking anyone
  out, since the signature is verified regardless and golang-jwt still enforces a standard
  `exp` if the token carries one. **Confirm against a real Appskep token at first live
  login** and tighten if needed.
- **The refresh call's HTTP method is likewise unconfirmed** — GET is what the
  query-parameter form implies and is what `HTTPRefresher` sends. The endpoint was reached
  (it answered), so the URL shape is right; the success path awaits a real token.
- **`view.Flash` is now an alias for `model.Flash`.** The session codec has to serialise a
  flash and cannot import `view`. An alias keeps every template and the alert partial
  unchanged while making one type serve both sides.
- **Verified, not assumed** — by minting tokens signed with the real `AUTH_SECRET` (a
  throwaway helper, since the callback is the only entry point and a live Appskep session
  was not available): anonymous `/admin` redirects with a correctly escaped
  `client_base_url`; the callback returns 303 + `Cache-Control: no-store` to a clean path
  and the cookie decodes to `{"t":"…"}` with no identity in it; the users row was created,
  had its placeholder email/name overwritten from the claims, and got `last_login_at`
  stamped; a non-allowlisted user got 403 with admin chrome on `/admin` while still
  rendering normally on `/`; a one-character cookie edit produced an anonymous request, not
  a 500; deactivating the user mid-session revoked access on the next request; the flash
  showed exactly once; an expired `expired_at` drove the refresh call to the real
  dev-auth endpoint and its failure cleared the session; `/api/health` and `/static` are
  untouched by the gates; zero `ZgotmplZ` and zero occurrences of `access_token` in the
  server log. Phase 13 turns the codec, the claim parser and the guards into permanent
  tests.

---

## Phase 4 — Admin: Layanan (services)

- [x] `GET /admin/layanan` — table (name, price, duration, active, coming soon, sort), search + pagination
- [x] `GET /admin/layanan/baru` + `POST /admin/layanan`
- [x] `GET /admin/layanan/{id}/edit` + `POST /admin/layanan/{id}`
- [x] `POST /admin/layanan/{id}/hapus` — blocks the delete when bookings exist and offers deactivation instead (no soft delete — see notes)
- [x] Inline toggles, via **Turbo Stream** rather than a Frame — `is_active`, `is_coming_soon`, no full page reload (see notes)
- [x] Image upload: MIME + size validation (≤2MB, jpg/png/webp), random filename → `static/uploads/services/`, delete old file on replace
- [x] Server-side validation: name required, unique slug (auto-generated from name), price ≥ 0, duration 15–480 min
- [x] Sort order number control (not drag — see notes)
- [x] Business rule: `is_coming_soon=1` services are shown but **not bookable** — already enforced by `ListBookableServices`; Phase 4 only makes the flag settable, and **Phase 7 must re-check it inside the booking transaction**

**Acceptance:** ✅ Verified 2026-07-26. All 3 seeded layanan manageable end-to-end; deactivating Bekam removed its card from the landing page and marking Massage coming-soon added the badge, both on the next public request.

---

### Phase 4 notes — decisions taken during execution

This phase invented four conventions that had no precedent in the codebase. Later phases
should copy them rather than re-decide.

- **Validation errors: `service.ValidationError` in `internal/shared/service/errors.go`.**
  A `map[field]message` keyed by the HTML input name, built up across every field so the
  user sees all the problems on one submit. A handler tells it apart with
  `errors.As(err, &ve)` and re-renders the form at **422** with the submitted strings
  intact; anything else is logged and becomes the 500 page. `ErrNotFound` and
  `ErrHasBookings` are sentinels so a handler never imports `database/sql` to tell a 404
  from a 500. Phases 5, 7, 9 and 10 all have forms — reuse this.
- **Validation lives in the service, not the handler.** CLAUDE.md's "parse → validate →
  call service" is honoured as: the handler collects raw form strings, the service owns the
  rules. Create and edit therefore cannot drift apart, which two copies of
  "durasi 15–480" eventually would.
- **Turbo Streams, not Turbo Frames, for inline toggles** — a deliberate departure from
  this plan's original wording. `RenderStream`'s own doc comment already said it existed
  for "the inline toggles in Phases 4 and 5"; a stream is Turbo's documented reply to a
  form POST that updates a fragment, and it avoids the frame's 200-vs-redirect subtlety.
  The handler falls back to an ordinary redirect when `Accept` carries no
  `text/vnd.turbo-stream.html`, so the toggles work with JS off — verified with curl.
  `RenderFragment` still has no caller.
- **The toggle posts the value it wants, not a flip.** A hidden `value=1|0` makes a
  double-clicked button or a resent request settle on the state of the last click.
  Verified: three identical `value=0` posts left `is_active=0`, not inverted.
- **`Deps.FlashRedirect` was a required addition, and the gap was real.**
  `SaveFlashAndRedirect` replaces the whole session with `auth.Session{Flash: …}`, dropping
  the token — correct for its two callers, which are *ending* a session, and an instant
  logout for "Layanan berhasil disimpan". The new helper keeps the token and **replaces**
  rather than appends the flash: `withSession` has already consumed what was pending and
  re-saved the cookie, but `Session.Load(r)` re-reads the *request* cookie, which still
  carries the consumed message, so appending would show it twice.
- **The slug is generated once, at creation, and never follows a rename.** It is the public
  URL of the service; regenerating it on every edit would break links, bookmarks and search
  results with nothing redirecting them. The form shows it read-only, marked "permanen".
  Collisions get a `-2`, `-3` suffix — but that loop only makes the slug *nice*;
  `uq_services_slug` is what makes it *unique*, since `SELECT EXISTS` takes no lock. A
  concurrent create is caught by `IsDuplicateKeyOn` and retried once.
- **Money parsing is string-only: `util.ParsePrice`.** No `float64` anywhere on the path,
  matching `util.Rupiah` on the render side. Separators follow Indonesian convention (comma
  decimal, dot thousands), and **thousands groups are validated**: the first draft accepted
  `150000.555` as 150,000,555 — a thousandfold error from one mistyped separator, silently
  charged to a customer. Every group after the first must now be exactly three digits.
- **`util.EscapeLike` was needed, not optional.** `ListServicesAdmin` is `name LIKE ?` and
  the caller wraps the term in `%…%`; without escaping, a typed `%` matched every row and
  `_` matched any character — a filter that quietly ignores the query.
- **Uploads live in their own package (`internal/shared/upload`), not `util`**, which is
  documented as a leaf of pure helpers and would otherwise drag disk IO into every importer
  of a formatter. Three things there are load-bearing:
  - **`http.MaxBytesReader` is what actually caps the request.**
    `ParseMultipartForm(n)` only bounds what is held in memory; everything past `n` spills
    to a temp file with no limit, so without the reader one request can fill the volume.
  - **The stored extension comes from the sniffed bytes, never the filename.** A `.txt`
    renamed `.jpg` is rejected. Trusting the filename would mean a second whitelist that
    can disagree with the first, which is the hole the whitelist exists to close.
  - **Ordering on replace: write the new file → update the row → delete the old file.**
    Interrupted anywhere, that leaves at most an orphaned file (invisible, costs disk); the
    other order leaves a row pointing at a missing file, which is a broken image on the
    public site. Text fields are validated *before* the file is touched, so a rejected name
    never strands an upload.
- **Delete is hard when unused and blocked otherwise** — this plan offered "soft delete or
  block". There is no `deleted_at` column and the schema is frozen outside
  `0001_schema.sql` until Phase 14, while `CountBookingsForService` plus the RESTRICT
  `fk_bookings_service` already implement blocking. The count and the delete are two
  round-trips, so `IsForeignKeyViolation` maps to the same error — the check is the
  friendly path, the constraint is the correct one. A blocked delete offers deactivation.
- **Sort order is a number input, not drag-and-drop.** There is no drag library and no
  `grip`/`arrow-up`/`arrow-down` icon in the sprite, and Turbo Drive replaces `<body>` on
  navigation, so JS-held order state would reset anyway.
- **The confirm partial gets a handler-built `[]confirmDialog` parallel to the row list.**
  There is no `dict` helper in the FuncMap, so a partial receives exactly one value — the
  established pattern is a handler-side struct, and a first draft using `dict` did not
  parse. Adding `dict` was rejected: it would let any template assemble arbitrary payloads
  and undo the reason every partial's shape is currently visible in Go.
- **Page templates are `layanan.html` and `layanan-form.html`.** The renderer globs
  `pages/admin/*.html` without recursing, so `layanan/form.html` would be invisible. Create
  and edit share one template driven by `IsNew`.
- **Verified, not assumed** (against a real MariaDB, with a minted admin token, then the
  test rows removed): `85.000` stored as `85000.00` and a duplicate name became
  `pijat-refleksi-2`; a submit with an empty name, `price=abc`, `duration=5` and
  `sort_order=-3` returned 422 with all four messages and kept every typed value; renaming
  a service left its slug untouched; a PNG uploaded, was replaced with the old file deleted,
  and was cleared by the remove checkbox; a `.txt` renamed `.jpg` and a 3 MB file were both
  rejected at 422 with no orphan left on disk; deleting an unused service worked and one
  with a booking was refused with the deactivation hint; three identical toggle posts were
  idempotent; a toggle without the turbo `Accept` header redirected 303 and still applied;
  the flash rendered exactly once and the admin was still signed in afterwards; `?q=%` and
  `?q=_` returned no matches instead of everything; with `APP_PAGE_SIZE=2` the pager
  appeared, carried `q=` through its links, and `?page=999` clamped to the last page rather
  than rendering an empty table; `/admin/layanan/abc/edit` and `/9999/edit` returned 404 and
  an anonymous request redirected to `dev-auth.appskep.id`; zero `ZgotmplZ` and zero
  `access_token` occurrences in the logs. Phase 13 should turn `ParsePrice`, `Slugify`,
  `EscapeLike` and the upload sniffing into permanent tests — they were checked here with a
  throwaway test file that was deleted, since Phase 13 owns testing.

---

## Phase 5 — Admin: Penjadwalan (schedule slots)

- [x] `GET /admin/jadwal` — calendar/month view **and** list view (`?view=kalender|daftar`), filter by date range & status, shows booked/capacity
- [x] `GET/POST /admin/jadwal/baru` — single slot (date, start time, end time, capacity, note)
- [x] `POST /admin/jadwal/{id}` — edit; `POST /admin/jadwal/{id}/hapus` — delete (rejected when booked)
- [x] Toggle `is_active` per slot — via **Turbo Stream**, following the Phase 4 convention rather than a Frame (see notes)
- [x] **Generator** `GET/POST /admin/jadwal/generate`:
  - [x] Inputs: start date, end date, daily window (e.g. 08:00–20:00), slot duration (**default 150 min = 2,5 jam**, read from `slot_default_duration_minutes`), break between slots, weekday selection, capacity
  - [~] skip-existing flag — dropped; skipping is the only safe behaviour, so the flag is always-on and invisible (see notes)
  - [x] Preview step listing every slot to be created before committing, marking the ones that already exist
  - [x] Idempotent: skips slots that already exist (`INSERT IGNORE` + `uq_slots_date_start`), reports created/skipped counts
  - [x] Executed in a single transaction with a guard on max generated rows (500/run, 180 days)
- [x] Bulk actions: activate/deactivate/delete a date range (delete blocked for booked slots, and confirmed against real counts first)
- [x] Timezone handling: store & render in `Asia/Jakarta` (`APP_TZ`), no UTC drift
- [x] Past slots automatically excluded from public availability — already true via `starts_at >= ?` in `ListAvailableSlotsByDate`; confirmed, not re-implemented

**Acceptance:** ✅ Verified 2026-07-27. Generating 2 weeks × 08:00–20:00 × 150 min produced exactly 56 slots
(14 days × 4 — the 18:00 slot correctly dropped because it would end at 20:30); an identical re-run reported
"0 slot dibuat. 56 dilewati karena sudah ada." and left the row count unchanged.

### Phase 5 notes — decisions taken during execution

- **The data layer needed almost nothing.** Phase 1 wrote `schedules.sql` with this phase in
  mind, down to `CreateSlotIfAbsent`'s comment. Three additions only: `is_active` joined the
  `UpdateSlot` SET list (the edit form owns the flag, and without it saving the form silently
  discarded a status change), plus `CountSlotsInRange` and `CountBookedSlotsInRange` for the
  bulk-delete confirmation. **No schema change**, so `0001_schema.sql` stayed frozen and no
  `make db-fresh` was needed.
- **A booked slot cannot be moved.** Editing a slot with `booked_count > 0` keeps its date and
  times whatever the request says; only capacity and note change. Moving it would reschedule a
  paying customer silently — no notification, and the booking's own record would show the new
  time as if it had always been so. Per-booking reschedule is Phase 10's, with a flow of its
  own. The form renders those inputs `disabled`, but **the service enforces it independently**:
  a disabled input is an affordance, not a guarantee. Verified with a hand-crafted POST that
  tried to move a booked slot to another date and changed nothing but the capacity.
  A disabled input also submits *nothing*, which is why `validate` skips the date/time rules
  entirely on a locked slot instead of rejecting the save with "Tanggal wajib diisi."
- **The generator has no skip-existing flag,** which this plan listed. Skipping is the only
  safe behaviour — the alternative is overwriting a slot that may already hold bookings — so
  the flag would be a switch whose second setting must never be chosen. It is always on, and
  the preview says so by striking through the slots it will pass over.
- **Preview and commit share one expansion.** `Plan` and `Apply` both call the same unexported
  `expand`, so the preview cannot describe a different set from the one that gets written, and
  the confirm form re-posts every input verbatim as hidden fields rather than trusting the
  live form above it. A preview writes nothing at all — verified by counting rows across it.
- **`t + duration <= windowEnd` is the whole "no spilling past closing time" rule.** With
  08:00–20:00 and 150 minutes that yields four slots, not five: the 18:00 one would end at
  20:30. Getting this bound wrong is the defect that quietly books customers after hours.
- **Bulk delete is confirmed against real numbers, not a generic dialog.** It posts once to get
  "11 slot akan dihapus, 1 dilewati karena sudah punya booking", then again with `konfirmasi=1`
  — the same two-step shape as the generator. The `confirm` partial could not serve here: its
  form carries only the CSRF field, so it cannot carry a date range. Activate and deactivate
  apply immediately instead, being reversible and never touching a booking.
- **Clock arithmetic is in minutes, not `time.Time`.** `util.ParseClock` / `util.FormatClock`
  work on "HH:MM" alone. `start_time` and `end_time` are `TIME` columns that sqlc maps to
  `string`, and a slot has no date until it is paired with one — building a `time.Time` to add
  150 minutes would invent a date and a timezone the column does not carry.
- **The status filter runs in Go, not SQL.** "penuh" and "lewat" describe a state rather than a
  column, and sqlc cannot express a predicate that changes shape per request without one query
  per combination. The window is bounded to 92 days, so filtering after the read is cheap and
  one query serves every filter.
- **A bulk validation error forces the list view.** The bulk panel only exists there, so a 422
  rendered into the default calendar answered with a status the admin could not see. Caught by
  the verification pass, not by reading the code.
- **The month grid is built in Go, not the template.** `Month` returns `[][]DayCell` with
  whole Monday-first weeks and the neighbouring month's days flagged `Outside`; `time.Weekday`
  starts on Sunday, and that offset is handled in exactly one function.
- **Verified, not assumed** (against a real MariaDB, with a minted admin token, then every test
  row removed and the seed state restored): the calendar showed today highlighted at `0/4` with
  32 slots for July, matching the seed; the list grouped 56 slots across 14 days and the four
  status filters each narrowed correctly (`lewat` = the one slot that had already started);
  a create at a free time landed and the same date+time again returned 422 with the duplicate
  message and every typed value intact; one submit with a blank date, an end before the start,
  `capacity=0` and a 300-character note returned 422 with **all four** messages; a booked slot
  rendered all three time inputs `disabled` and refused a forced move while still accepting the
  capacity change; `capacity` below the booking count returned 422 with the count named, not a
  500 from `chk_slots_count`, and the input's `min` tracked it; deleting a booked slot was
  refused with `booked_count` untouched while an unbooked one deleted; three identical
  `value=0` toggle posts left `is_active=0` and a request without the turbo `Accept` header
  redirected 303 and still applied; the 500-slot guard rejected a 152-day range at 422 with
  nothing written; bulk deactivate flipped all 56 slots in range and none outside it; bulk
  delete removed 11 and left the booked one; `/admin/jadwal/abc/edit`, `/0/edit` and
  `/999999/edit` all returned 404 and an anonymous request redirected to `dev-auth.appskep.id`;
  zero `ZgotmplZ` and zero `access_token` occurrences in the log; **the `booked_count` ==
  holding-bookings invariant held after every one of the above**; and no page scrolls
  horizontally at 360/390/414/768 (measured by attempting a window scroll — `documentElement.scrollWidth`
  reads the table's own scroll container and reports a false positive on the Phase 4 page too).
  Phase 13 should turn `ParseClock`/`FormatClock`, the generator expansion boundaries and the
  booked-slot lock into permanent tests.

---

## Phase 6 — Public: Landing & Layanan

- [x] `GET /` — hero, ringkasan layanan (active only), cara kerja/alur booking, FAQ, CTA booking, kontak/WA — **landed in Phase 2** so the layouts had a real page to prove themselves against. Everything else in this phase is still open.
- [x] `GET /layanan` — grid of active services; `coming soon` badge on `is_coming_soon`; price + duration + image
- [x] `GET /layanan/{slug}` — detail page, "Booking Sekarang" CTA (**replaced, not disabled**, when coming soon — see notes)
- [x] Inactive services return 404 on detail and are hidden from lists — and from `sitemap.xml`
- [x] SEO: title/description per page, Open Graph tags, `sitemap.xml`, `robots.txt`, favicon
- [x] Image lazy-loading + explicit dimensions; graceful placeholder when `image_path` is empty
- [x] Mobile layout pass
- [x] Read-only "jadwal terdekat" preview on the detail page — pulled forward from Phase 7's availability queries at the user's request; it writes nothing

**Acceptance:** ✅ Verified 2026-07-27. Deactivating Urut Therapeutic took `/layanan/urut-therapeutic` to 404 and removed it from `/layanan`, `/` and `sitemap.xml` on the next request; Massage Therapeutic renders the coming-soon badge with **zero** `booking?layanan=` links on its page.

### Phase 6 notes — decisions taken during execution

- **The crawler endpoints are mounted on the root router, not in `public.Routes`.**
  Everything in that router is wrapped in `d.OptionalAuth`, which does a session
  round-trip and will consume an inbound `?access_token=` — neither of which makes
  sense for `robots.txt`, `sitemap.xml` or `favicon.ico`. They live in
  `internal/app/seo.go` as methods on `*Deps`, the same family as `StaticHandler`,
  and chi matches the literal paths ahead of the `/` mount. Verified: the sitemap
  response carries no `Set-Cookie`.
- **`robots.txt` is ENV-aware, and that is the load-bearing part.** Outside
  production it serves `Disallow: /`. A staging deployment serving the same content
  as production is the ordinary way to get the wrong host indexed, and it cannot be
  fixed from the app afterwards. The production branch also emits the `Sitemap:`
  line and disallows `/admin`, `/api`, `/booking`, `/riwayat`, `/profil`, `/login`,
  `/logout`.
- **The sitemap is built with `encoding/xml`, not a template.** `view.Render`
  hardcodes `Content-Type: text/html` and only globs `pages/{public,admin}/*.html`,
  so a template was never an option — and the encoder is what guarantees a slug or
  a site URL containing `&` produces a document a parser accepts. It is built from
  the active services rather than from the route table, so a future authenticated
  route cannot accidentally appear in it.
- **`Catalog` gained the two public read methods** (`ListActive`,
  `GetActiveBySlug`) rather than the handler reaching into `Store.Queries` as the
  landing page did. The detail page has to tell "no such slug" from a real failure,
  and `service/errors.go` exists precisely so a handler need not import
  `database/sql` to do it. The landing handler was moved onto `ListActive` too, so
  there is one public read path. `GetActiveServiceBySlug` carries the `is_active`
  predicate in SQL, where it cannot be forgotten at a call site.
- **A coming-soon CTA is replaced, not dimmed.** PLAN.md said "disabled with
  tooltip"; a tooltip is invisible on touch and a disabled-looking button still
  reads as clickable. The space instead states that the layanan cannot be booked
  yet and offers a prefilled WhatsApp link. The `service_card` partial had already
  settled this rule in Phase 4 — this page follows it rather than inventing a
  second treatment.
- **The service card now links to the detail page.** Before this phase a bookable
  card linked *only* to `/booking?layanan=…`, which left `/layanan/{slug}` reachable
  only for coming-soon services and from the landing hero. The image link is
  `aria-hidden` and out of the tab order, so a keyboard user gets one link to the
  detail page, not two.
- **`Page.OGType`, `Page.Canonical` and an absolute `Page.OGImage` are resolved in
  `Renderer.fill`,** not by each handler. `data.go` already promised "empty falls
  back to the site default" for `OGImage` and nothing implemented it. A handler now
  sets the site-relative path it already has and the base is applied once, from
  `APP_URL` — never from `r.Host`, for the same reason `client_base_url` is not.
- **`site_og_image` is seeded empty and ships no asset.** No default social image
  exists in the repo, and inventing one was out of scope; the row exists so Phase
  10's settings form has something to edit, and the tag is simply omitted until it
  is filled. `twitter:card` degrades to `summary` when there is no image rather than
  claiming `summary_large_image` with nothing to show.
- **`/favicon.ico` is a 302, not a 301.** Browsers request it whether or not the
  document declares an icon, and without the route the public catch-all rendered the
  full styled 404 page for every first page view. A permanent redirect is cached
  indefinitely and is effectively impossible to walk back if the icon moves.
- **`Schedule.Upcoming` reads the same two settings the booking flow will**
  (`booking_lead_time_minutes`, `booking_max_days_ahead`), so the preview cannot
  advertise a date the Phase 7 form would then refuse. Bounded at four queries and
  four times per day. A failure is logged and drops the section: the page's job is
  to describe the layanan, and it must not 500 because a schedule read hiccupped.
- **`util.Paragraphs` returns `[]string`, not markup.** A helper that emitted `<p>`
  tags would have to return `template.HTML`, and `icon` stays the only one in the
  codebase; the template writes its own tags, so every paragraph is still escaped.
- **Two layout bugs found by looking at the rendered page, not by reading it:** the
  detail page's image column stretched to the height of the text beside it, leaving
  the 4:3 frame floating in a tall empty box (`self-start`), and the service cards'
  price rows sat at different heights because the footer was not pinned (`mt-auto`,
  with an `mb-5` above it so the gap survives when there is no slack).
- **Verified, not assumed** (against a real MariaDB, with test rows added and then
  removed and the seed state restored): all nine new routes return their intended
  status and `/layanan/tidak-ada`, `/layanan/` both 404 with public chrome; the three
  seeded layanan render at `Rp75.000` / `Rp150.000` / `Rp300.000` and `2,5 jam`;
  deactivating a service 404'd its detail URL and removed it from `/layanan`, `/`
  **and** the sitemap; the coming-soon page had zero booking links and no jadwal
  section; the jadwal preview correctly dropped today's 08:00 and 10:30 slots against
  the 120-minute lead time; a fifth free slot on one date produced "+1 lagi"; an
  `image_path` produced an absolute `og:image` and flipped `twitter:card` to
  `summary_large_image`; the sitemap parsed under `xmllint` and carried no
  `Set-Cookie`; the production `robots.txt` branch was exercised through a throwaway
  test (deleted — Phase 13 owns testing) since `ENV=production` will not boot against
  a passwordless dev database; `/favicon.ico` 302s to the SVG; zero `ZgotmplZ` on
  every page and zero `access_token` occurrences in the log; and no page scrolls
  horizontally at 360/390/414/768, measured by attempting a window scroll. Phase 13
  should turn `Paragraphs`, `DateISO` and the sitemap builder into permanent tests.

---

## Phase 7 — Public: Booking flow

- [x] `GET /booking` — step 1: pick layanan (skips step when `?layanan=slug`)
- [x] Step 2: pick date → **Turbo Frame** loads available slots for that date — the frame is fed by `GET /booking` itself, not a separate `/booking/slot` route (see notes)
- [x] Step 3: customer data form (prefilled from profile: name, phone, address, notes)
- [~] `GET /booking/review` — replaced by a POST to `/booking` without `konfirmasi=1`; a GET would carry the customer's name, phone and address in the query string (see notes)
- [x] `POST /booking` — create booking in **one transaction** (see [Design note: slot concurrency](#design-note-slot-concurrency)):
  - [x] `SELECT ... FOR UPDATE` the slot row — the lock must be taken *before* the capacity check, not after
  - [x] Re-check `booked_count < capacity`, `is_active=1`, and slot still in the future **inside** the lock
  - [x] Increment `booked_count`, insert booking with `status='pending_payment'`, `expires_at = now + PAYMENT_EXPIRY_MINUTES` (default 60)
  - [x] Generate unique `booking_code`
  - [x] Snapshot `price_amount` from the service (price changes later must not alter past bookings)
  - [x] Commit, then redirect to `/booking/{code}/pembayaran`
- [x] Second line of defence: unique index on `(slot_id, user_id)` for non-terminal bookings, so a double-submitted form cannot create two bookings even if the lock logic regresses
- [x] Never do the availability check in the handler and the write in another function without the transaction spanning both
- [x] Guards: auth required, service active & not coming soon, slot in the future, no duplicate active booking for the same slot by the same user
- [x] Friendly race-condition error ("slot baru saja terisi") that re-renders available slots instead of an error page
- [x] Background job (ticker, **every 1 min** — see notes): expire `pending_payment` bookings past `expires_at` — in a single transaction per booking: mark `status='expired'` **and** decrement `booked_count`
- [x] Decrement is guarded (`booked_count = GREATEST(booked_count - 1, 0)`) and only runs on a real status transition, never on an already-expired row
- [x] `GET /booking/{code}/pembayaran` — added here, ahead of Phase 8, so the flow is walkable and verifiable end to end. Ownership-guarded, states the amount and the deadline; Phase 8 replaces its panel with the Snap button

**Acceptance:** ✅ Verified 2026-07-27. 20 goroutines against one capacity-1 slot produced **exactly 1** winner and 19 friendly
`ErrSlotTaken`, with `booked_count == 1`; the same test at capacity 3 produced exactly 3. An expired booking released its slot
exactly once and a second sweep changed nothing. The invariant `booked_count == COUNT(bookings NOT IN ('cancelled','expired'))`
held after every check.

### Phase 7 notes — decisions taken during execution

**Two real defects were found by verification, neither visible by reading the code.** Both are
recorded first because they are the reason this phase took the shape it did.

- **`GetSlotForUpdate` must be the transaction's *first statement*, and "first" is not about
  lock ordering alone.** The first draft read the layanan before taking the slot lock, on the
  reasoning that a plain `SELECT` takes no lock and so could not affect the ordering. It takes
  no lock, but it *establishes the REPEATABLE READ snapshot* — and a locking read that then
  finds a row newer than that snapshot fails outright with MariaDB **ER_CHECKREAD (1020,
  "Record has changed since last read")**. The counter stayed correct, so nothing looked wrong
  in the data; but **19 of 20 racers got a 500 instead of "slot baru saja terisi"**. Moving the
  lock to the top fixed it completely. `schedules.sql:14` already said "must be the first
  statement"; this is why. A sequential test would never have shown it.
- **Turbo silently discards a 200 HTML response to a form POST**, so the review step's button
  did nothing at all in a real browser. Turbo requires a form submission to end in a redirect
  ("Form responses must redirect to another location"); the review cannot redirect, because its
  state is the customer's name, phone and address. The fix is `data-turbo="false"` on that one
  form. **The same latent defect was already in Phase 5** — the jadwal generator's preview and
  the bulk-delete panel have the identical POST-then-200 shape and were equally dead in a
  browser. Both were fixed here. Phase 5 verified them with curl, which is the no-JS path and
  always worked; that is the gap. **Any new form that answers a POST with 200 needs this
  attribute** — it is now in CLAUDE.md.

Departures from the task list, and the reasoning:

- **The whole flow is one URL.** `GET /booking/review` and `GET /booking/slot?date=&service=`
  were both dropped. The review is a POST to `/booking` without `konfirmasi=1`, exactly the
  two-step shape Phase 5 settled twice; a GET review would put the customer's name, phone and
  address into a query string, and from there into access logs and the `Referer` of every asset
  the page loads. The slot frame is fed by `/booking` itself: date links carry
  `data-turbo-frame="booking_slots"`, Turbo fetches the full page and extracts the matching
  frame, and the *same href* is an ordinary navigation with JS off. A second route rendering the
  same slot list is precisely the drift the codebase avoids elsewhere (`slot_toggle` /
  `slot_toggle_stream` exist for that reason). Cost: `Renderer.RenderFragment` still has no
  caller.
- **This is the codebase's first `<turbo-frame>`.** Two things about it are load-bearing:
  the frame carries `data-turbo-action="advance"` so a date swap updates the address bar and
  the back button stays honest; and links *inside* a frame navigate that frame by default, so
  the slot links need `data-turbo-frame="_top"` — choosing a slot has to reveal step 3, which
  lives outside the frame. Verified in a real browser: the frame content changed, the URL
  advanced, and the surrounding page was not replaced.
- **The ticker runs every minute, not every five.** The interval is the upper bound on how long
  a dead booking's slot stays invisibly held; a visitor watching the picker should see it come
  back while they are still on the page. `EXPIRY_SWEEP_INTERVAL`, and `validate()` refuses a
  non-positive value — there is deliberately no "off", and a zero would panic `time.NewTicker`
  at boot.
- **The ticker's two defers are ordered deliberately.** `defer wg.Wait()` is registered *before*
  `defer stop()`, so on every exit path — including a listener error, not just a clean shutdown
  — the ticker is cancelled first, then waited for, and both happen before the `db.Close()`
  registered further up. `context.Canceled` from a sweep interrupted by shutdown is swallowed
  rather than logged, or every clean restart would end with a red line.
- **No schema change and no new query.** Phase 1 wrote this phase's entire data layer, down to
  `ListExpiredPendingBookings` returning `slot_id` so the ticker can lock the slot first.
  `make sqlc` produces no diff; `0001_schema.sql` stayed frozen.
- **`Schedule.Window()` was extracted** so the layanan page's availability preview and the
  booking transaction cannot disagree about the lead time or the max-days-ahead bound —
  the promise `schedule.go` already made in a comment, now enforced by there being one
  function. `slotMessage` plays the same role for the per-slot rules: it is called once in
  validation, to put a message beside the picker, and once inside the lock, against the row
  that is actually true.
- **`layanan` and `slot` validation errors are promoted to the page-level notice.** They are
  pickers, not inputs, so a message keyed to them has no field to sit beside, and choosing one
  drops the page back a step where the message would render off-screen. Without this, a slot
  deactivated mid-flow returned a 422 that said *nothing at all* — found by verification.
- **A booking code carries the slot's date, not today's:** `TPJ-20260730-7WNZ`. Read over the
  phone, it says when the customer is coming. The four random characters come from
  `crypto/rand` over a 31-character alphabet with `0 1 O I L` removed, because a code that gets
  read aloud and typed back in should not contain the pairs people confuse. `BookingCodeTaken`
  is the friendly check; `uq_bookings_code` is the guarantee, and a collision retries the
  INSERT *inside* the same transaction — InnoDB rolls back the statement, not the transaction,
  so the slot hold taken above survives.
- **Ownership failure is a 404, not a 403**, and it lives in `Booking.DetailForUser` rather than
  the handler. A 403 confirms the code exists, which is the one fact someone guessing codes
  wants. Verified: a stranger's booking and a nonexistent code are indistinguishable.
- **The payment page decides "expired" from the clock, not the status.** The ticker runs every
  minute, so there is a window where a booking is dead but still reads `pending_payment`, and
  inviting a payment during it would be a lie.
- **Alamat is optional.** The clinic receives customers at its own address — the field is
  useful context, not a delivery requirement, and making it mandatory would cost bookings.
- **Verified, not assumed** (against a real MariaDB, with minted tokens and a headless Chrome,
  then every test row removed and the seed state restored): 20 goroutines on one capacity-1
  slot gave exactly 1 winner / 19 `ErrSlotTaken` / `booked_count == 1`, and 3 winners at
  capacity 3 — with 20 **distinct** users, so `uq_bookings_active_slot_user` could not mask the
  race; the same user booking a capacity-2 slot twice was refused by that index with the hold
  rolled back and `booked_count` unmoved; an anonymous `/booking` redirected to
  `dev-auth.appskep.id` with a correctly escaped `client_base_url`; the full path
  layanan → tanggal → slot → data → ringkasan → konfirmasi landed on the payment page with the
  flash shown exactly once and the user still signed in; `price_amount` stayed `75000.00` after
  the service price was raised to `99000.00`; a submit with a blank nama, a 3-character telepon
  and a 600-character catatan returned 422 with all three messages and every typed value
  intact; deactivating the slot between review and confirm returned 422 with the reason named,
  nothing written, and a freshly loaded picker; coming-soon and inactive layanan vanished from
  step 1 and a hand-crafted POST naming either was refused; a slot inside the 120-minute lead
  time and one 60 days out were both absent from the picker and refused on POST; pushing
  `expires_at` into the past saw the live ticker flip the booking to `expired` and drop
  `booked_count` within ~7s, with a second sweep releasing nothing; another user's payment page
  and an unknown code both returned 404; the Turbo frame swapped the slot list and advanced the
  URL without replacing the page, and the same links worked as full navigations without JS;
  zero `ZgotmplZ` on every page state, zero `access_token` occurrences in the log, zero ERROR
  lines, a clean `SIGTERM` shutdown with the ticker stopping first; no horizontal scroll at
  360/390/414/768 (measured by attempting a window scroll); and **the `booked_count` ==
  holding-bookings invariant held across every slot after all of the above**, with no slot ever
  outside `0 <= booked_count <= capacity`. Phase 13 should turn the concurrency harness, the
  booking-code generator, the window bounds and the expiry idempotency into permanent tests —
  they were checked here with throwaway commands (`cmd/racecheck`, `cmd/minttoken`) that have
  been deleted.

---

## Phase 8 — Payment: Midtrans (sandbox)

> Per **R4**–**R7**, following the conventions in [create-order.md](create-order.md). Uses Appskep's shared Midtrans account, so the `tpj` order-id prefix is what separates our transactions from ukom's and every other Appskep system's — get it right.

### Client

- [x] `internal/shared/payment/gateway.go` — the interface, named `payment.Gateway` (the package already says "payment"): `CreateTransaction(ctx, Order) (*Charge, error)` and `GetStatus(ctx, orderID) (*Notification, error)`. Signature verification is a package function, `payment.VerifySignature(n, serverKey)`, not a method — it needs no client and must be callable on a payload that never reached one
- [x] `internal/shared/payment/midtrans.go` — Snap via the `midtrans-go` SDK; env selection follows Appskep convention: `MIDTRANS_ENV` equal to the literal `midtrans.Production` → Production, **anything else → Sandbox**
- [x] `internal/shared/payment/httpclient.go` — a context-aware replacement for the SDK's transport, because the SDK's own discards the context (see notes)
- [x] Customer details on the Snap request come from the session/JWT (`email`, `name`) plus the booking's phone
- [x] Item details: one line item = the layanan (name, price, qty 1) so the Snap page shows what is being bought
- [x] `Expiry` on the Snap request is derived from the booking's remaining hold, so Midtrans stops accepting payment at the moment the ticker releases the slot — **not in the original task list, and the defect it prevents is taking money for a slot given to someone else**

### Order & payment page

- [x] `order_id` = `tpj-<uuid-v4>` (**R5**) — generated per payment attempt, stored on the `payments` row, unique
- [x] `GET /booking/{code}/pembayaran` — halaman pembayaran: order summary, deadline, Snap button (**redirect, not embedded snap.js** — see notes)
- [x] `POST /booking/{code}/pembayaran` — create the Snap transaction, persist `payments`, 303 to the Snap URL. PLAN.md named this `/pembayaran/token`; the same URL as the GET follows the Phase 7 one-URL shape, and a redirect reply keeps Turbo happy
- [x] **Reuse an existing valid payment URL** instead of minting a new order on every POST — reuse when the booking's newest payment is not cancelled/expired, has a `snap_redirect_url`, is still within the booking's `expires_at`, and its `gross_amount` matches the **booking's snapshot** (not the live price — see notes). Repeated taps then return the same Snap URL, which is expected behaviour, not a bug
- [x] Guard: only the booking owner may create a payment; only for `status='pending_payment'`, and only with more than 5 minutes left on the hold

### Webhook — the only source of truth (**R7**)

- [x] `POST /api/webhook/midtrans` — public route, no auth middleware, **exempt from CSRF** (Phase 12 must keep it so), authenticated by signature alone
- [x] Verify `signature_key` = `SHA512(order_id + status_code + gross_amount + MIDTRANS_SERVER_KEY)` using the `gross_amount` string exactly as received; mismatch → reject and change nothing
- [x] Ignore notifications whose `order_id` does not carry the `tpj-` prefix or has no matching payment row (the account is shared across Appskep systems)
- [x] Log the raw payload to `payment_notifications` **before** processing, including whether the signature validated
- [x] Lock the payment/booking row (`FOR UPDATE`) and apply the transition idempotently — Midtrans retries, and a retry can arrive while the first is still in flight:

  | `transaction_status` | Effect |
  |---|---|
  | `settlement` | payment `paid_at=now`; booking → `paid` |
  | `capture` + `payment_type=credit_card` + `fraud_status=accept` | same as settlement |
  | `capture` + `fraud_status=challenge` | leave pending, flag for admin review |
  | `pending` | record only, no state change |
  | `deny`, `expire` | payment `expired_at=now`; booking → `expired`; **release the slot** |
  | `cancel` | payment `cancelled_at=now`; booking → `cancelled`; **release the slot** |

- [x] Slot release on a terminal status uses the same guarded decrement as Phase 7 — and must not double-decrement when the expiry ticker already released it
- [x] Always return 200 for a valid, already-applied duplicate
- [x] **Money arriving after the slot was released** is recorded, logged at ERROR and left for a human — not in the original task list, and the one case where the state machine cannot decide anything safely (see notes)

### Round-trip & operations

- [x] `GET /booking/{code}/status` — Turbo Frame poller so the payment page updates without a manual refresh
- [x] Midtrans finish redirect → `/booking/{code}/pembayaran` for now; Phase 9 repoints it to `/booking/{code}/konfirmasi`. `unfinish` and `error` are account-wide dashboard settings on a **shared** account and were deliberately not touched — Snap's API only accepts `callbacks.finish` per transaction
- [~] Manual re-sync `POST /admin/pembayaran/{id}/sync` — shipped as the customer-facing `POST /booking/{code}/periksa` instead; the admin route lands in Phase 10 over the same `Payment.Sync`, when there is a payments page to click it from
- [x] Never mark a booking paid from a client-side callback or the Snap JS success handler
- [~] Sandbox end-to-end test per channel (VA, QRIS, card test numbers) — Midtrans cannot reach `localhost`, so this was verified as real outbound Snap orders plus signed simulated notifications for every status. A real inbound callback needs a tunnel and is deferred (see notes)
- [~] Register the webhook URL in the Midtrans sandbox dashboard — **deliberately not done**: that setting is account-wide and belongs to another Appskep system. Our URL is attached per transaction with `X-Append-Notification` instead
- [x] Going live = swapping `MIDTRANS_ENV` + keys in `.env`, no code change

**Acceptance:** ✅ Verified 2026-07-27. Real Snap orders were created against the sandbox and repeated taps returned the same one; a signed `settlement` flipped the booking to `paid` with the slot still held, and replaying it twice more changed zero rows; `expire`, `deny` and `cancel` each released the slot exactly once; a foreign prefix and an unknown `tpj-` order were both ignored with 200; a tampered signature returned 401 and changed nothing. 10 concurrent identical settlements produced exactly 1 transition.

### Phase 8 notes — decisions taken during execution

**Three real defects were found by verification, and none of them was visible by reading the
code.** They are recorded first because two of them shaped the design.

- **A nullable JSON column is unreadable as sqlc generates it.** `payments.raw_response` is
  `JSON NULL`, which sqlc maps to `json.RawMessage`, and `database/sql` cannot scan NULL into
  it: `convertAssign` special-cases `*[]byte` and `*sql.RawBytes` but not a named slice type.
  Every `SELECT *` on a payment row therefore failed with *"unsupported Scan, storing
  driver.Value type &lt;nil&gt; into type *json.RawMessage"* — and since `raw_response` is NULL
  on every payment until Midtrans answers, that is the ordinary case, not an edge one. It
  broke the very first payment attempt. The fix is a `sqlc.yaml` override to
  `sql.NullString`; `[]byte` is rejected by sqlc's validator as "not a Go basic type". This
  was a **latent Phase 1 defect** — nothing had ever inserted a `payments` row, so nothing
  had ever read one back. `payment_notifications.payload` is NOT NULL and is unaffected.
  So much for "no `make sqlc` diff": there was one, and it was necessary.
- **A `<turbo-frame>` in a frame response must not carry a `src` pointing at itself.** Turbo
  throws *"Matching &lt;turbo-frame&gt; element has a source URL which references itself"* and
  the catch path leaves the frame **empty**. The symptom was a payment panel that appeared
  and then silently vanished a moment later, and — because the poller reads its continue-flag
  out of the frame — polling that stopped on its first tick: two requests in eighteen seconds
  instead of four. Nothing in the server log, nothing in the markup, and curl cannot see it
  because curl never runs Turbo. The fix is `paymentData.FrameSrc`, set only when rendering
  the full page. The frame element itself is emitted by the *fragment* template, because
  Turbo matches on the id and a bare fragment carries none — that half is also required, and
  is what Phase 2's `RenderFragment` doc comment must have meant by "the frame's own markup
  lives in a partial".
- **A polled fragment is served out of the browser cache.** `RenderFragment` set no
  `Cache-Control`, so the second and every later fetch of the same URL never reached the
  server. This produces exactly the same symptom as a dead poller — no requests — from an
  entirely different cause, which is what made the frame bug above take two rounds to
  isolate. `RenderFragment` now sends `Cache-Control: no-store`; it is the right default for
  a fragment, which is live state fetched from one URL over and over by definition.

Departures from the task list, and the reasoning:

- **The SDK is used for its types, not its transport.** `midtrans-go` v1.3.8's
  `HttpClientImplementation.Call` contains `req.WithContext(options.Ctx)` and **discards the
  result** — `WithContext` returns a copy. Its context option is therefore a no-op, and
  CLAUDE.md's "external calls always take a context timeout" would have been decorative. Its
  `HttpClient` is a one-method interface, so `payment/httpclient.go` replaces it with sixty
  lines built on `http.NewRequestWithContext`, and every typed Snap request/response and the
  environment→URL mapping still come from the SDK. A second benefit: the SDK's transport logs
  every request header at Info level, and one of those headers is the Basic-auth server key.
- **The webhook URL is attached per transaction with `X-Append-Notification`,** not registered
  in the sandbox dashboard as this plan said. That setting is account-wide on a merchant
  account shared with ukom; pointing it here would stop *their* payments being confirmed.
  The same reasoning applies to the finish/unfinish/error redirects, which is why only
  `callbacks.finish` — a per-transaction field — is set.
- **Redirect to Snap's hosted page, not an embedded `snap.js` popup.** `snap.js` can only be
  loaded from Midtrans' own domain, so embedding it would make the payment path the
  codebase's first CDN dependency, against the rule every asset has followed since Phase 2.
  A redirect also removes the temptation to believe the JS success callback.
- **The Snap `expiry` block is set from the booking's remaining hold.** Not in the task list,
  and its absence is a money bug: Midtrans defaults to 24 hours, so a customer could pay
  perfectly happily for a slot our ticker released an hour earlier. For the same reason
  `Start` refuses to open a payment with less than five minutes left rather than sending
  someone to a page that expires while they read it.
- **Reuse matches the booking's `price_amount`, not the layanan's current price.** PLAN.md
  said "matches the current price"; the current price is exactly what must *not* be used —
  Phase 7 snapshots the price onto the booking precisely so a later edit cannot change what a
  customer agreed to pay. Verified: raising the layanan price mid-flow returned the same
  order.
- **The state machine's authorisation runs in series: payment, then booking, then slot.** The
  payment transition must report `RowsAffected() == 1` before the booking transition is
  attempted, and the booking transition must report 1 before `ReleaseSlot` is called. Two
  guards rather than one, and the second is Phase 7's rule unchanged. It is also what stops a
  stray `cancel` after a settlement from unbooking a paid slot: `MarkPaymentCancelled` is
  guarded by `paid_at IS NULL`, reports zero, and the booking is never touched.
- **Money arriving after the slot was released is a case with no safe automatic answer.** The
  ticker expires a booking; `settlement` then arrives. `MarkPaymentPaid` succeeds — the money
  is real and the row must say so — while `MarkBookingPaid` reports zero rows. Re-taking the
  slot would evict whoever booked it since, and v1 has no refund path (**Q6**). It is logged
  at ERROR with the booking code, written into `process_note`, and left for Phase 10's
  payments list. Verified end to end against the live ticker.
- **`GetSlotForUpdate` is the webhook transaction's first statement too**, which means the
  payment and booking rows are resolved *outside* the transaction purely to learn `slot_id` —
  the same shape `ListExpiredPendingBookings` gives the expiry ticker. The lock order inside
  is slot → booking → payment, and a re-read guard catches a booking that changed slots
  between the two (Phase 10's reschedule), retrying once.
- **`Sync` exists now and is customer-facing.** `POST /booking/{code}/periksa` — "Saya sudah
  bayar". It calls the Midtrans status API and feeds the identical `apply`, so a manual check
  cannot reach a conclusion the webhook would not have. It is also the only way to complete a
  payment from a machine Midtrans cannot call back into, which is every development machine.
  Phase 10 adds the admin button over the same method.
- **HTTP status codes on the webhook are chosen for what Midtrans does with them.** Anything
  we will never accept — a foreign prefix, an unknown order, a body that is not JSON —
  answers 200, because a non-2xx is retried for hours. Only a genuine failure on our side
  returns 500, and only a signature that fails on an order claiming to be ours returns 401,
  so a wrong server key is visible at both ends instead of silently swallowing every payment.
- **The payment page's poller is a `setInterval` in the page, not a `setTimeout` in the
  fragment**, because Turbo does not execute scripts inserted into a frame. Whether to
  continue is still the server's decision, carried as `data-poll` on a wrapper inside the
  frame; a booking that reaches a final state stops the loop on the next tick.
- **No schema change.** `0001_schema.sql` stayed frozen and no query file was edited — Phase 1
  wrote every query this phase needed, guarded and commented for it. The only generated-code
  change is the `raw_response` override above.
- **Verified, not assumed** (against a real MariaDB and the real Midtrans sandbox, with
  minted tokens and headless Chrome; every test row then removed and the seed state
  restored): a real Snap order was created and its redirect URL returned, with `tpj-<uuid>`,
  the token, and `gross_amount` matching the booking; four further taps returned the *same*
  order and left one payment row, including after the layanan price was raised; a booking with
  two minutes left was refused at 422 with nothing written; `settlement` moved the booking to
  `paid` with `booked_count` untouched, and two replays changed **zero** rows while still
  being logged; `expire`, `deny` and `cancel` each ended the booking and decremented the slot
  exactly once, with replays inert; `pending` and a challenged `capture` recorded the VA
  number and left the booking holding its slot; a one-character signature edit returned 401
  with the notification still logged at `signature_valid = 0`; `ukom-abc123` and an unknown
  `tpj-` order returned 200 and changed nothing; a `gross_amount` of 999000.00 was refused
  with an ERROR line naming both amounts; a body that was not JSON was stored as
  `{"raw": "…"}` and a 1 MB body was rejected at 413; the live ticker expired a booking and
  the `settlement` that followed set `paid_at`, left the booking `expired`, did **not**
  re-increment the slot, and logged the manual-action ERROR; 10 concurrent identical
  settlements produced exactly one transition and nine "already final"; another user's
  `/pembayaran`, `/status`, `POST /pembayaran` and `POST /periksa` all returned 404 while an
  anonymous request redirected to `dev-auth.appskep.id`; the real status API answered 404 for
  an unpaid Snap order and the page said so in Indonesian; in a browser the frame polled four
  times in eighteen seconds and stopped after a settlement landed mid-poll; every booking
  state rendered its own copy with zero `ZgotmplZ`; the log carried zero occurrences of
  `access_token`, of the Midtrans server key, and of any Snap URL; no page scrolled
  horizontally at 360/390/414/768 (measured by attempting a window scroll); `SIGTERM` stopped
  the ticker first and exited cleanly; and **the `booked_count` == holding-bookings invariant
  held across every slot after all of the above**. Phase 13 should turn `PriceToRupiah`, the
  signature check, `Outcome()` and the notification state machine into permanent tests — they
  were checked here with a throwaway `cmd/tpjcheck` that has been deleted.

**Left for later, deliberately:** a real inbound webhook from Midtrans (needs a tunnel;
`localhost` is unreachable), a genuine paid sandbox transaction per channel, the settled
branch of `GetStatus` (its 404 branch is proven against the live API), the admin re-sync
route (Phase 10) and refunds (**Q6**).

---

## Phase 9 — Public: Konfirmasi, Riwayat, Profil

- [x] `GET /booking/{code}/konfirmasi` — halaman konfirmasi: status badge, booking code, layanan, jadwal, total, payment method, next steps, WA contact, print/save view
- [x] Ownership guard: only the booking's user (or admin) can view — reuses `Booking.DetailForUser`, so it is a **404**, never a 403
- [x] `GET /riwayat` — halaman riwayat booking: paginated list, filter by status, sort by newest; each row links to its konfirmasi page
- [x] "Bayar sekarang" action on `pending_payment` rows still within `expires_at`
- [x] User-initiated cancellation of `pending_payment` bookings (releases the slot) — and **cancels the Midtrans order too**, which was not in this list (see notes)
- [x] `GET/POST /profil` — halaman profil: phone, address, avatar are editable locally; **name and email are read-only** (owned by Appskep, refreshed from the JWT on each login)
- [x] Change-password / edit-account links point to Appskep (`AUTH_URL`), never a local form
- [x] Empty states for every list — two on riwayat, because "no bookings yet" and "none with this status" want different offers

**Acceptance:** ✅ Verified 2026-07-27. A user booked, opened a real Snap order, saw the booking in riwayat with the right
status, and reopened its konfirmasi page. Cancelling released the slot exactly once and moved the live sandbox transaction
to `cancel`; 20 concurrent cancels of one booking produced exactly one transition with `booked_count` 1 → 0.

### Phase 9 notes — decisions taken during execution

**No defect survived to be found by verification this time** — the three earlier phases' traps (Turbo discarding a
200-after-POST, a self-referencing frame `src`, a fragment served from cache) were all avoided by construction, and the
browser pass confirmed it rather than discovering it. What the verification *did* earn is the last item below, which is a
property rather than a bug.

- **The data layer needed nothing at all.** `CancelPendingBookingByOwner`, `ListBookingsByUser`, `CountBookingsByUser`,
  `UpdateUserProfile` and `UpdateUserAvatar` were written in Phase 1 *for this phase*, each with a comment saying so, and
  had zero Go callers until now. `make sqlc` produced a byte-identical tree and `0001_schema.sql` stayed frozen — the
  first phase since 7 where that claim was checked by diffing the generated directory rather than asserted.
- **Konfirmasi is the canonical page for a booking; pembayaran is only the payment step.** `GET /pembayaran` now 303s to
  konfirmasi for any status other than `pending_payment`, and `payment.Order.FinishURL` points at konfirmasi (PLAN.md:924).
  The alternative — letting both pages render every status, which pembayaran already did — is two places for the same
  paid/cancelled/expired copy to drift. The redirect condition is the **status alone**, deliberately not the clock: a
  lapsed hold still has a payment panel to explain and a "Saya sudah bayar" button to offer until the ticker moves it,
  which happens within the minute.
- **Cancellation cancels the Midtrans order, best-effort, after the transaction commits.** Not in the task list. Without
  it, the Snap page a customer still has open keeps accepting payment until the order's own expiry — for a slot that has
  already been given back. `payment.Gateway` gains `Cancel`; `Payment.CancelOrder` is called only when the booking
  actually transitioned, on `context.WithoutCancel(r.Context())` so the redirect the customer is already following cannot
  abort it, and a failure is logged at WARN while the page still reports success — because the slot *is* released.
- **The two Midtrans refusals are sentinels, not statuses to inspect.** `404` is the **ordinary** case: a Snap token the
  customer never used means there is no transaction to cancel, and equally nothing that could take their money later.
  `412` means the order already reached a final state. Both are swallowed. Verified against the live sandbox: an unused
  Snap order answers `404 "Transaction doesn't exist."`, and cancelling an already-cancelled one answers
  `412 "Transaction status cannot be updated."`.
- **`ErrNotPayable` was reused rather than adding `ErrNotCancellable`.** Its doc already reads "already paid, or it was
  cancelled or expired", which is exactly the cancel precondition. A second sentinel for the same predicate is a second
  thing to keep in agreement.
- **`CancelForUser` takes no `isAdmin`, unlike `DetailForUser`.** An admin cancelling someone else's booking is Phase 10's,
  and wants a reason, the wider `CancelBooking` guard and an audit entry. Passing `userID` straight into the query's WHERE
  is what makes ownership the database's guarantee here rather than the handler's.
- **The cancel reason is fixed copy.** The action runs from the `confirm` partial, whose form carries only the CSRF field —
  there is nowhere to type one, the same constraint that made Phase 5's bulk delete re-post its inputs as hidden fields.
- **The status filter goes through a whitelist, and that is about behaviour, not injection.** The value is a bind
  parameter either way. Mapping it through `bookingStatusFilters` means `?status=bogus` falls back to "everything";
  passing it through would return an empty list indistinguishable from "you have no bookings". Verified.
- **`statusBadge` was exported as `view.StatusBadge`** so the riwayat filter chips draw their labels from the same table
  the badges do. A handler that retyped them would eventually call one status two different things on one page.
- **Riwayat is cards, not a table.** A table is right for the admin lists, where columns are compared against each other.
  Here each row is one booking a person is trying to recognise, and six columns at 360px is a horizontal scroll to read a
  date.
- **The profile shows name and email as text, not as disabled inputs.** A disabled input still reads as a field somebody
  can fix; saying where the value lives and linking to `{{authURL}}` is the only honest treatment. Same reasoning as
  Phase 6's replaced coming-soon CTA.
- **Phone and address are optional on the profile although they are required on the booking form.** A profile is prefill
  convenience; refusing to save an address because the phone box is empty would be a worse page. The bounds and the
  shape helpers are the booking form's (`validPhoneShape`, `countDigits`, `maxCustomerPhoneLen`), so a number accepted on
  one page cannot be rejected on the other.
- **Avatars are a second `upload.ImageStore` at 1 MB**, not a reuse of the services store: the caps differ and
  `upload.New` creates its directory at boot, so an unwritable path is a failed boot rather than a failed upload. The
  replace ordering is Phase 4's unchanged — write new → update row → delete old — and the text fields are validated
  before the file is touched.
- **Printing is `print:hidden` on the layout chrome**, not a stylesheet. Tailwind v4's variant, no config file, and it
  improves every public page rather than only the one that needed it.
- **The property the verification earned: writing `cancelled_at` locally is safe *because* it is conditional on the
  gateway succeeding.** `CancelOrder` records `MarkPaymentCancelled` only after Midtrans returns 200. If the customer had
  already paid, Midtrans answers **412** instead, nothing is written, and a later `settlement` webhook still sets
  `paid_at`, leaves the booking cancelled, does not re-take the slot, and logs the manual-action ERROR — Phase 8's rule
  intact. Verified end to end: a settlement replayed against a user-cancelled booking produced exactly that.
- **Verified, not assumed** (against a real MariaDB and the real Midtrans sandbox, with minted tokens and headless
  Chrome; every test row then removed and the seed state restored): all five new routes answer, and anonymous requests to
  `/riwayat`, `/profil` and `/konfirmasi` redirect to `dev-auth.appskep.id`; another user's konfirmasi, another user's
  `POST /batal`, and an unknown code are **all 404 and indistinguishable**, with the refused cancel writing nothing;
  cancelling moved the booking, set the reason, decremented the slot once and re-offered it, and a second cancel returned
  303 with an info flash and changed nothing; **20 concurrent cancels produced exactly one transition** and never drove
  the counter below zero; the live ticker expired a booking and the cancel that followed released nothing a second time;
  a real sandbox transaction went from `pending` to `cancel` through the app, with `cancelled_at` recorded; a settlement
  replayed after a cancellation set `paid_at`, left the booking `cancelled`, left `booked_count` alone and logged the
  manual-action ERROR; the six status filters each narrowed correctly, `?status=bogus` showed everything with "Semua"
  selected, and with `APP_PAGE_SIZE=1` the pager carried `status=` through every link and `?page=999` clamped to the last
  page; "Bayar sekarang" appeared only on the pending row; a profile submit with a 3-character phone and a 600-character
  address returned **422 with both messages and all 600 characters retained**, writing nothing; an avatar uploaded,
  replaced with the old file deleted, and cleared, with the header chip following it; a `.txt` renamed `.jpg` (422) and a
  2.4 MB PNG (413) were both refused **with no orphan on disk**; a hand-crafted POST carrying `name` and `email` changed
  neither; in a browser the profile form saved through Turbo and the cancel dialog opened, confirmed and redirected, each
  showing its flash exactly once with the user still signed in and **zero console errors**; the payment frame still polled
  **four times in eighteen seconds** and stayed filled (the Phase 8 regression check); print collapsed header and footer
  while keeping the booking code; zero `ZgotmplZ` on every page state; the log carried zero occurrences of
  `access_token`, of the Midtrans server key and of any Snap URL, and exactly one ERROR — the deliberate one above;
  `SIGTERM` stopped the ticker first and exited cleanly; and **the `booked_count` == holding-bookings invariant held
  across every slot after all of the above**, with no slot outside `0 <= booked_count <= capacity`. Phase 13 should turn
  the cancellation idempotency, the status-filter whitelist and the profile validation into permanent tests — they were
  checked here with a throwaway `cmd/tpjcheck9` that has been deleted.

**Left for later, deliberately:** the admin-side cancellation with a reason and the reschedule flow (Phase 10), an
`ORDER BY created_at DESC` that `idx_bookings_user_status (user_id, status, id)` does not cover — a filesort over one
user's rows, correct and cheap at this cardinality, and **not** worth changing the SQL for before Phase 14 measures it.

---

## Phase 10 — Admin: Booking, Pembayaran, Dashboard, Users

- [x] `GET /admin` — dashboard: today's bookings, upcoming schedule, revenue (this month), counts by status, recent payments. Every figure links to the booking list filtered to exactly the rows it counted
- [x] `GET /admin/booking` — list with filters (status, date range, layanan, search by code/name/phone), pagination, sort. The date range applies to the slot date or to `created_at`, chosen by `?tanggal=jadwal|dibuat`
- [x] `GET /admin/booking/{id}` — detail: customer, jadwal, payment history, timeline from `activity_logs`
- [x] Admin actions: confirm, mark completed, cancel (with reason → releases slot), edit notes
- [x] Reschedule to another slot (decrement old, increment new, transactional) — **both slot locks taken in ascending id order**, see notes
- [x] `GET /admin/pembayaran` — list of payments with state, method, amount, order_id; detail view with the raw response and the full webhook trail; re-sync action over the Phase 8 `Payment.Sync`
- [x] `GET /admin/users` — list of local mirrored users, search, toggle active, promote/demote `role` (identity itself is read-only, owned by Appskep). Self-demotion and last-admin removal are refused
- [x] `GET/POST /admin/pengaturan` — settings: site name, contact, WA, payment expiry minutes, default slot duration, booking lead time. Driven by a whitelist, reloaded in-process on save
- [x] CSV export for bookings & payments (filter-aware, not only date-range)
- [~] All destructive actions behind a confirm dialog — the two destructive actions here (cancel, reschedule) each need an input the `confirm` partial cannot carry, so both are explicit forms with their own copy instead. See notes

**Acceptance:** ✅ Verified 2026-07-27. An admin ran the full daily loop from the panel — day sheet, confirm, notes, reschedule, cancel with a reason,
payment re-sync against the live sandbox, user role/active toggles, settings, CSV — without a single SQL statement. 20 concurrent admin cancels of one
booking produced **exactly 1** transition with `booked_count` 1 → 0; 25 rounds of cross-direction reschedules over the same slot pair surfaced **zero**
deadlocks; the `booked_count == COUNT(holding bookings)` invariant held after every check.

### Phase 10 notes — decisions taken during execution

**Two defects were found by verification, and neither was visible by reading the code.** Both are
recorded first because the first one would have failed on the very first request.

- **sqlc's MySQL engine silently mis-binds `BETWEEN`, in two different directions.** The first draft
  wrote the date filter as `(CASE WHEN ? = 1 THEN DATE(b.created_at) ELSE sl.slot_date END) BETWEEN ? AND ?`.
  sqlc generated the SQL with all ten placeholders and a call passing **seven arguments** — an
  expression on the left of `BETWEEN` is not bound at all, and both its parameters vanish from the
  params struct without a warning. Rewriting it as two bare columns fixed that half and exposed the
  other: `b.created_at BETWEEN ? AND ?` across four joined tables that each have a `created_at`
  column emitted each parameter **four times**, 20 arguments for 14 placeholders, the table
  qualifier ignored. `col >= ? AND col <= ?` binds correctly in every case and is what all six admin
  queries now use. Neither form fails at generate time; both fail on the first request with an
  argument-count error. A generated-code check comparing each query's `?` count to its call's
  argument count caught it, and is worth keeping in mind for Phase 13.
- **A template shared by two pages must live in `partials/`.** `view.Renderer` builds one set per
  page — layout + `partials/*.html` + **that one page file** — so a `{{define}}` in
  `pembayaran.html` is invisible from `pembayaran-detail.html`, which 500s with
  `no such template "payment_state"`. Phase 4's note that "the partial namespace is global" is about
  `partials/`, not about page files. The fix was better than a new partial: the label/colour table
  became `view.PaymentBadge`, beside `StatusBadge`, so the filter chips and the badges they filter to
  now draw from one map in Go rather than two lists in two languages. Caught by loading the page;
  `go build` and `go vet` are both blind to it.

Departures from the task list, and the reasoning:

- **The date filter applies both ranges always, and the caller widens the unused one.** That is not
  only a workaround for the `BETWEEN` defect: two bare columns are index-friendly where the `CASE`
  was not, and the mode switch is then a Go-side decision rather than a SQL branch. `created_at` is a
  DATETIME, so the upper bound is an **end-of-day** instant — a bare date there silently hides
  everything booked after midnight on the last day of the range, which is most of it.
- **Reschedule is the first transaction in the system that locks two rows of the same table**, and
  the ascending-id order is the entire reason it is safe. Both `GetSlotForUpdate` calls are the
  transaction's first two statements; the booking is then re-read **under** those locks and the
  attempt abandoned if it changed slots in between (`errSlotMoved`, the sentinel Phase 8 already
  defined for this exact case). 25 rounds of two operators moving two bookings between the same pair
  of slots in opposite directions produced zero deadlocks. `HoldSlot` on the target must report 1
  before `RescheduleBooking` is attempted, and that must report 1 before `ReleaseSlot` on the old
  slot — Phase 8's chain of authorisations, unchanged. A `1062` on `uq_bookings_active_slot_user`
  becomes a field message, and the rollback undoes the hold: verified, the target's `booked_count`
  did not move.
- **`price_amount` and `booking_code` are never touched by a reschedule.** The snapshot is the whole
  point of `bookings.price_amount`, and a code that changed when a session moved would be unreadable
  over the phone — even though it carries the old date, which is the lesser of the two evils.
- **Cancelling a paid booking is allowed, and the page says what that means.** `CancelBooking`'s
  guard already covered `pending_payment`, `paid` and `confirmed`; the acceptance criterion is that
  an operator never has to touch the database, and a paid booking whose slot must be freed is
  precisely when they would. The dialog names the amount and states that the refund is manual (Q6:
  no refund path in v1), the reason is required, and an `activity_logs` entry records who did it.
  Verified: the slot came back, `paid_at` was left alone and `cancelled_at` was **not** written to
  the payment.
- **The gateway order is cancelled only for a booking that was `pending_payment`.** That is the one
  case where a Snap page the customer still has open could take money for a slot just given back.
  A paid booking's order is settled — Midtrans answers 412 and there is nothing to close. The call
  runs after the commit on `context.WithoutCancel`, best-effort, exactly as Phase 9's customer
  cancellation. Verified: the 404 branch (an unused Snap token, the ordinary case) was silent and
  wrote nothing.
- **`ErrBookingFinal` is one sentinel, not two.** Cancel and reschedule are guarded on the identical
  status set, so a second sentinel would have been a second thing to keep in agreement.
- **The confirm partial could not serve the two destructive actions.** Its form carries only the CSRF
  field, so it cannot carry a cancellation reason or a chosen slot — the same constraint that made
  Phase 5's bulk delete re-post its inputs as hidden fields. Both are explicit forms in their own
  panel instead, the cancel one behind a warning that states the amount.
- **The users module's two guards are in the service, and the transaction matters.** Self-demotion
  and self-deactivation are refused outright; the last **active** admin cannot be demoted or
  deactivated. `CountActiveAdmins` and the write are in one transaction, because two operators
  demoting each other simultaneously would otherwise each count two admins and both succeed, leaving
  none — and TPJ owns no credentials to recover with. The signed-in operator's own row renders a
  label rather than a dead button.
- **The settings form is generated from a whitelist, `service.EditableSettings`.** A key/value table
  plus "write whatever was posted" is how an unknown key gets invented or a key the code parses gets
  a value it cannot read. The whitelist is iterated, never the request; a key that is absent from the
  submission is left alone rather than blanked; and `Settings.Reload` runs after the commit, so an
  edit reaches the public site on the next request with no restart. Verified: a hand-posted
  `hack_key` wrote nothing, and a new tagline appeared on `/` immediately. The generator defaults
  (`slot_day_start`, `slot_weekdays`, `slot_break_minutes`) are deliberately **not** in the list —
  they are per-run inputs on the generator form, not site configuration.
- **CSV is written straight to the ResponseWriter, never through `view.Renderer`** — which forces
  `text/html` and globs only `pages/*.html`, the same constraint that made `sitemap.xml` an
  `encoding/xml` document. Three things there are load-bearing: a **UTF-8 BOM**, or Excel reads the
  file as the local codepage and mangles every accented name; **`csvSafe`**, which prefixes a cell
  beginning `= + - @` with an apostrophe, because these files carry names and notes typed by whoever
  made the booking and a spreadsheet evaluates them as formulas; and **`data-turbo="false"` on the
  download links**, so Turbo does not try to swap a CSV response into the document. Verified with a
  booking named `=cmd|' /C calc'!A0`.
- **The export is capped at 5 000 rows and says so in a header.** `X-Export-Truncated` rather than an
  extra CSV row, which would corrupt the column layout of the file it is warning about.
- **`activity_logs.meta` needed the Phase 8 `sqlc.yaml` override too.** It is `JSON NULL`, which sqlc
  maps to `json.RawMessage`, and `database/sql` cannot scan NULL into that — the identical latent
  defect as `payments.raw_response`, in a table that had never been read. Overridden to
  `sql.NullString` before it could produce its first 500 rather than after.
- **The audit trail can never fail the action it records.** `Audit.Record` returns nothing and logs
  its own failures at WARN. It also runs outside the transaction it describes: the booking
  transactions hold slot locks and are kept short, and a rolled-back action leaves nothing to
  explain.
- **`clientIP` reads `RemoteAddr` only.** `X-Forwarded-For` is attacker-controlled until Phase 12
  supplies a trusted-proxy resolver — `main.go` already refuses chi's `RealIP` for the same reason.
  An audit row naming the proxy is honest; one naming whatever the client typed is not.
- **A no-op transition is information, not an error.** Confirming an already-confirmed booking, or
  cancelling one the ticker just expired, reports `RowsAffected() == 0` and answers with an info
  flash. Verified: a second confirm changed nothing and wrote no second audit entry.
- **The user list's per-row booking count is N+1 queries**, deliberately: the alternative is a GROUP
  BY on every list read for a number that matters only on the row about to be acted on. Revisit if
  the mirror grows past a few thousand rows.
- **The one untyped parameter in the codebase is `oldest_first`.** sqlc cannot infer a Go type for a
  parameter compared against a literal, and `CAST(... AS UNSIGNED)` does type it — as **two**
  `int64` fields a caller could set to disagreeing values, silently producing a wrong order. One
  `interface{}` bound twice cannot disagree with itself, and has exactly one call site.
- **Verified, not assumed** (against a real MariaDB and the real Midtrans sandbox, with minted
  tokens and headless Chrome; every test row then removed and the seed state restored): all fourteen
  new routes answer, `/admin/booking/ekspor` resolves ahead of `/{id}`, and anonymous requests to
  every one of them redirect to `dev-auth.appskep.id` while a non-admin gets the styled 403 with
  admin chrome; `abc`, `0` and `999999` return 404 on every id route, GET on a POST-only route
  returns 405; **20 concurrent admin cancels produced exactly 1 transition** with `booked_count`
  1 → 0 and never below; **10 concurrent reschedules onto one target moved both counters exactly
  once** and left the booking on the target; **25 rounds of cross-direction reschedules over the
  same slot pair surfaced zero deadlocks**; a reschedule onto a full slot, a past slot, a missing
  slot, the current slot, and a slot the same customer already holds were each refused with their
  own message and **the target's `booked_count` did not move** (the rollback undoing the hold);
  rescheduling a cancelled booking was refused; cancelling a **paid** booking released the slot,
  recorded the reason, wrote the audit row and left `paid_at` untouched with no `cancelled_at` on
  the payment; cancelling a **pending** booking took the gateway's 404 branch silently; an empty
  cancellation reason returned 422 and wrote nothing; a 600-character note returned 422 with the
  message and all 600 characters retained; every one of the six status chips narrowed correctly and
  `?status=bogus` showed everything; both date modes, the layanan filter and search by code, name
  and phone each narrowed correctly while `?q=%` matched nothing; with `APP_PAGE_SIZE=2` the pager
  clamped `?page=999` to the last page and carried the active filter through every link; the sort
  toggle reversed the order; a live Snap order was created, settled by a signed webhook, and its
  payment rendered on the list, the detail page and the booking timeline; the admin re-sync reached
  the live sandbox; three identical user toggles were idempotent and a turbo-stream reply swapped
  the cell without replacing the page; **self-demotion, self-deactivation, last-admin demotion and
  last-admin deactivation were all refused** while demoting a second admin succeeded; a settings
  submit with three bad numerics returned 422 with all three messages, kept every typed value and
  wrote nothing, a valid save reached `/` with no restart, and a hand-posted unknown key wrote
  nothing; the CSV carried a BOM, respected the active filter, and neutralised a `=cmd|…` name and a
  `+1+1` note; the dashboard's revenue, day sheet and pending count each matched the equivalent SQL
  by hand; in a browser the note form saved through Turbo with its flash shown exactly once, a 422
  re-rendered normally, and there were **zero JavaScript errors**; zero `ZgotmplZ` on every page
  state; the log carried zero occurrences of `access_token`, of the Midtrans server key, of any Snap
  URL, of a raw JWT, and **zero ERROR lines**; `SIGTERM` stopped the ticker first and exited
  cleanly; and **the `booked_count` == holding-bookings invariant held across all 68 slots after
  every one of the above**, with no slot outside `0 <= booked_count <= capacity`. Phase 13 should
  turn the reschedule concurrency harness, the cross-direction deadlock probe, the last-admin guard,
  the settings whitelist and `csvSafe` into permanent tests — they were checked here with a
  throwaway `cmd/tpjcheck10` that has been deleted.

**Left for later, deliberately:** a bulk action on the booking list (each action here wants its own
reason or target, so a bulk version would need a different flow), an admin-side reschedule
notification to the customer (Phase 11 owns email), and numbered pagination links — prev/next still
serves a filtered, sorted list, and the `pagination` partial's own note already says Phase 10 could
add them if a list needed them. None did.

---

## Phase 11 — Notifikasi email

- [x] `internal/shared/mail/` — `Sender` interface + `SMTPSender` + `NoOp` (used when SMTP is unconfigured), and `internal/shared/service/email.go` for the rules. **Split into two packages** rather than one, mirroring `payment.Gateway` / `service.Payment` — see notes
- [x] HTML email templates (`template/emails/`): booking dibuat (pending payment), pembayaran berhasil, booking dibatalkan, booking kedaluwarsa, **jadwal dipindahkan** (Phase 10 deferred it here), reminder H-1. **No verification or password-reset emails** — those are sent by Appskep (**R2**)
- [x] Send asynchronously (goroutine + buffered queue), never block the HTTP request; log failures
- [x] Reminder job: daily ticker sends H-1 reminders for **paid and confirmed** bookings — not `confirmed` alone, see notes
- [x] Optional/stretch: WhatsApp deep link (`wa.me`) in every email footer, alongside email. `util.WhatsAppLink` is now shared with the `waLink` template helper

**Acceptance:** ✅ Verified 2026-07-27. One booking driven through all six triggers produced exactly six emails, each once, in order,
against a local SMTP catcher. The reminder survived a process restart without resending. With the mail server down, every booking action
still succeeded with one WARN and no ERROR. `SIGTERM` with three mails still queued drained all three before the process exited.

### Phase 11 notes — decisions taken during execution

**One defect, found by reading the delivered bytes rather than the code.** `html/template`
escapes everything it writes, so the plain-text part of a `multipart/alternative` message and
the `Subject:` header both came out HTML-escaped: a customer named `Ari & Co` reached the text
body as `Ari &amp; Co`. The HTML body needs that escaping and the other two must not have it,
so the templates are parsed **twice** — once with `html/template` for the `_html` bodies, once
with `text/template` for `_subject` and `_text`. Both sets read the same files and each executes
only the names it owns. `go build`, `go vet` and a glance at the rendered HTML are all blind to
this; it was visible only in the raw `.eml`.

Departures from the task list, and the reasoning:

- **Two packages, not one.** PLAN.md named a single `service/email.go`. Transport went to
  `internal/shared/mail` instead, because that is the shape CLAUDE.md already requires of an
  external call and `payment.Gateway` / `service.Payment` is the precedent: an interface, a real
  implementation with a context timeout, and nothing in it that knows what a booking is.
  `service.Email` — templates, queue, rules — is still where the plan asked for it.
- **`net/smtp`, not a library.** What this needs is one connection, optional STARTTLS, optional
  PLAIN auth and one DATA command. The single thing the standard library does not offer is a
  context-aware dial, so the connection is opened here and handed to `smtp.NewClient`; the
  context deadline is pushed onto the socket so it bounds the whole conversation, not just the
  dial. **Credentials are refused over an unencrypted connection to a non-loopback host** —
  verified against a sink on the LAN IP: the password was not sent, the mail was dropped with a
  WARN, and the booking was unaffected.
- **Dispatch lives in the service layer, not the handlers.** "Cancelled" is reached from three
  places (customer, admin, gateway) and "expired" from two (ticker, gateway); only the service
  knows which of them actually moved a row. **`RowsAffected() == 1` authorises the mail** exactly
  as it authorises a `ReleaseSlot`, which is what makes a Midtrans retry, a double-clicked button
  and the ticker racing a cancellation produce exactly one notice between them.
- **Nothing is sent from inside a transaction.** `Payment.transition` runs under a slot lock, so
  it fills in a `pendingEmail` and `applyOnce` enqueues it after the commit — the same rule as
  Phase 9's "the gateway order is cancelled after the transaction commits, never inside it".
- **Only a booking id and a kind cross the channel.** The worker re-reads through
  `GetBookingAdminDetail`, which Phase 10 had already written joining the layanan, the slot **and**
  `u.email` — so the six transactional mails needed no new query at all, and a queued mail
  describes the booking as it is when it goes out. The two facts that cannot be re-read travel
  with the job: the slot a reschedule left, and the status a cancellation left (which is the only
  way to know whether the mail has to mention a refund).
- **`bookings.reminder_sent_at` is the phase's one schema change**, applied by editing
  `0001_schema.sql` and running `make db-fresh`. The sweep claims a booking with a guarded
  `UPDATE … WHERE reminder_sent_at IS NULL` and only `RowsAffected() == 1` sends. That is what
  makes it idempotent across a restart, a second process, and a ticker that runs every fifteen
  minutes for the rest of the day. **Claim-then-send is deliberate and the trade is stated**: a
  send that fails afterwards is not retried, because a duplicate reminder is worse than a missing
  one. An in-memory alternative was considered and rejected — it resends everything after any
  restart between the reminder hour and midnight.
- **The reminder covers `paid` as well as `confirmed`.** PLAN.md said `confirmed`, but confirming
  is an operator action that may never happen while the customer has paid and is coming; tying the
  reminder to admin housekeeping is not a rule, it is an accident.
- **`REMINDER_HOUR` is a wall-clock hour, not an interval.** The sweep does nothing before it. A
  reminder that arrives at 03:00 is worse than none, and a bare interval ticker sends at whatever
  time the process happened to start.
- **Emails are table-based with inline styles and no images.** A mail client does not fetch
  `static/css/app.css`, most strip `<style>`, and many block remote images — so no sprite, no logo
  and no Tailwind, only the hex values from the `@theme` block written out by hand.
- **All `emails/*.html` parse into one set and execute by name**, the opposite of the pages, so
  every `{{define}}` in the directory must be uniquely named. There is no layout: Go templates
  cannot take a template name as a variable, so a wrapper around arbitrary content is not
  expressible without the `dict` helper this codebase has now declined three times. Each mail
  calls `email_open` / `email_close` itself. `NewEmail` asserts all three templates exist for every
  kind, so a typo is a failed boot rather than a mail nobody notices missing.
- **`util.WhatsAppLink` was moved out of `view`.** An email cannot import `view` — `view` imports
  `service` — and duplicating eight lines is exactly what the "one helper does all formatting" rule
  exists to prevent. `view.waLink` is now that function.
- **A full queue drops the mail with a WARN.** The alternative is blocking a request, or a
  transaction's caller, behind a mail server. `Audit.Record`'s rule: a notification can never fail
  the action it describes.
- **Shutdown drains under a bounded grace period.** A confirmation queued a moment before a
  restart is worth a few seconds; it is not worth holding the process open behind an unreachable
  mail server. `Email.Run` joins the existing `sync.WaitGroup`, so the `defer wg.Wait()` before
  `defer stop()` already covers it.
- **Verified, not assumed** (against a real MariaDB and a throwaway SMTP sink that stored the raw
  `.eml`, with a minted admin token; every test row then removed and the seed state restored):
  all six kinds fired, each exactly once and in order, for one booking driven through create →
  pay (a signed webhook against a seeded payment row, so the production state machine ran without
  a network call) → reschedule → admin-cancel, plus a customer cancellation and an expiry; the
  paid cancellation named the amount and said the refund is manual while the unpaid one did not;
  the reschedule mail carried the slot the booking had left (13:00–15:30) beside the one it moved
  to (15:30–18:00); the em-dash subjects Q-encoded as `=?utf-8?q?…?=` and the ASCII ones did not;
  `Ari & Co Tester` appeared unescaped in the text part and as `&amp;` in the HTML part; the
  reminder went out once and **five later sweeps plus a full process restart sent nothing more**,
  with `reminder_sent_at` stamped only on the paid booking and never on a cancelled or
  pending one; with the mail server killed, a booking still returned 303 and produced exactly one
  WARN and no ERROR; SMTP credentials against a non-loopback host without TLS were refused and
  nothing was delivered; `SIGTERM` with three mails queued against a deliberately slow server
  drained all three before the process exited; the password, the Midtrans server key and
  `access_token` each appear zero times in every log; and **the `booked_count` ==
  holding-bookings invariant held across every slot**, with none outside `0 <= booked_count <=
  capacity`. Phase 13 should turn the MIME builder (header injection, Q-encoding, the two-part
  ordering), the reminder claim and the queue-full drop into permanent tests — they were checked
  here with a throwaway `cmd/tpjcheck11` that has been deleted.

**Left for later, deliberately:** a per-user notification preference (there is no column for it
and no request for one), a retry queue for a failed send (it needs durable state, and Phase 12's
hardening pass is a better place to decide whether that is worth a table), and an admin-facing
log of what was emailed — `activity_logs` records the action, and the mail is a consequence of it.

---

## Phase 12 — Hardening

- [x] CSRF middleware on all state-changing routes + `csrfField` template helper; exempt the Midtrans webhook (signature-verified instead). **Not a double-submit cookie** — the secret lives in the signed session, which is strictly stronger. See notes
- [x] Rate limits: booking creation, payment-token creation, webhook endpoint (login/register are Appskep's problem)
- [x] Centralized input validation + user-friendly Indonesian error messages — `service/validate.go`, messages unchanged
- [x] Security headers: HSTS (prod), X-Content-Type-Options, X-Frame-Options/CSP frame-ancestors, Referrer-Policy, Permissions-Policy. ~~CSP allowing the Midtrans Snap script~~ — **no Snap script is loaded**, checkout is a server-side 303 to Midtrans' hosted page, so no third-party origin is needed anywhere in the policy
- [x] All output escaped by `html/template`; audited every use of `template.HTML` — two, both guarded, unchanged
- [x] Every SQL access goes through sqlc — no string-concatenated queries (audited, unchanged)
- [x] Secrets never logged; tokens redacted in payment logs (audited, unchanged — `payment/httpclient.go` has no logger by design)
- [x] Structured request logging with request ID; error logging with stack on panic — plus the client IP now resolves through `ClientIP`
- [x] Uploads: MIME sniffing, extension whitelist, size cap, non-executable directory (audited, unchanged; a `sandbox` CSP added on the uploads path)
- [x] Session cookie flags verified in production build
- [x] `go vet ./...` clean; `make vulncheck` added and run — see notes, **the toolchain needs a bump**
- [x] Timezone/currency formatting consistent (`Asia/Jakarta`, `Rp1.234.567`) — verified, unchanged
- [x] Global request-body cap (not in the original list, found necessary by the CSRF work)
- [x] `Cache-Control: private, no-store` on authenticated pages (not in the original list)
- [x] `GET /logout` → `POST /logout` (not in the original list)

**Acceptance:** ✅ Verified 2026-07-27. Every one of the 24 POST routes refuses a request without a
valid token and works with one, including both multipart uploads; a full booking ran end to end
through the new middleware and released its slot on cancellation with the `booked_count` invariant
intact. Zero CSP violations across all 14 pages in a real browser. Two defects found by verification
(a 403 where a 413 belonged; Turbo's progress-bar stylesheet blocked by `style-src`).

### Phase 12 notes — decisions taken during execution

- **The CSRF secret lives in the session, not in a second cookie.** PLAN.md said "double-submit
  cookie", and that is the weaker design: double-submit only checks that a cookie and a form field
  agree, so anyone who can write a cookie on the site's domain — a compromised sibling subdomain,
  and a shared Appskep deployment has several — sets both halves to a value they know and walks
  through. The session cookie is already HMAC-signed and HttpOnly, so a secret carried inside it
  cannot be read by script or forged without `SESSION_KEY`, and an attacker's own cookie carries
  their secret rather than the victim's. It also rotates on login and dies on logout for free,
  because `consumeCallback` writes a fresh session and logout clears it.
- **The token rendered into a form is a fresh one-time-pad mask of that secret**,
  `base64(mask ‖ mask XOR secret)`. Not decoration: the booking form reflects the customer's
  submitted input back into the same 422 response that carries the token, which is exactly the
  shape a BREACH-style compression oracle needs. A constant token in that response would be
  extractable; a per-render mask is not.
- **The secret is minted only for a signed-in visitor,** in `withSession`. Every state-changing
  route in the app is behind `RequireAuth` or `RequireAdmin`, so an anonymous token would protect
  nothing, and minting one would set a cookie on every crawler hit and every anonymous landing-page
  view. `csrfField` already renders nothing for an empty token, so no public page changed.
- **`withSession` writes the cookie at most once per request**, whatever combination of minting and
  flash-consumption applies. Two saves would be harmless; a second `Session.Load` between them would
  not — it re-reads the *request* cookie, which still holds the flash just consumed. That is the
  trap `FlashRedirect`'s comment has described since Phase 4, now load-bearing in a second place.
- **A `{{define}}` cannot reach `.CSRF` on the page envelope**, because `{{template "x" .Row}}`
  rebinds `$` to its own argument. The three inline-toggle fragments (layanan, users, jadwal) and
  the confirm dialog therefore take a handler-built wrapper — `serviceToggle`, `userToggle`,
  `slotToggle` — with the sqlc row **embedded**, so every field the markup already used still
  resolves by promotion and only `{{csrfField .CSRF}}` was added. This is Phase 4's "a partial
  receives exactly one value, built in the handler package" rule reaching its natural conclusion.
- **The CSRF middleware parses the body itself rather than calling `PostFormValue`,** which discards
  the parse error. Without that, a body over the `MaxBody` cap arrives at the token check as an empty
  form and is reported as a CSRF failure — telling someone who uploaded a large file that they lack
  permission. **Found by verification, not by reading the code:** the oversized-body case returned
  403 until it was fixed to distinguish `*http.MaxBytesError` (413) from an unreadable body (400).
- **Reading the body in middleware moves where an oversized upload is caught, and that is the
  deliberate trade.** A multipart request is now parsed before the handler's own
  `http.MaxBytesReader` narrows it, so the effective transport cap is the global
  `SERVER_MAX_BODY_BYTES` (4 MB) rather than the per-store 2 MB/1 MB. The friendly message is
  unaffected: `upload.ImageStore.Save` enforces its own cap on `fh.Size` and through an
  `io.LimitReader`, so a 3 MB image is still a 422 naming the limit — verified.
- **`X-Forwarded-For` is believed only from a configured proxy.** `TRUSTED_PROXIES` is empty by
  default and a malformed value fails the boot, because the difference between "empty" and
  "malformed but ignored" is whether the rate limiter can be bypassed by anyone who sends a header
  — and that must not be discoverable only in production. This is the resolver `main.go` promised
  when it rejected chi's `RealIP`. Walking the header right-to-left is what makes it unspoofable:
  a client can prepend anything, but cannot remove what the trusted hops appended after it.
- **A token bucket, not a fixed window,** because a window resets on a clock edge and a caller can
  spend a full allowance on either side of it. The bucket map is capped and swept: a map keyed by
  client IP is itself a memory-exhaustion vector.
- **The CSP is strict, and the app was changed to fit it rather than the reverse.** `script-src
  'self'` with no `'unsafe-inline'` and no nonce, because there is no inline script left: three
  `onclick` handlers became `data-dialog` attributes read by one delegated listener, `window.print()`
  became `data-action="print"`, and the payment poller moved out of `pembayaran.html` into the
  project's first app-level JS file, `static/js/app.js`. A nonce was the easier route and the worse
  one — it does not cover attribute handlers, so those had to be rewritten regardless, and once they
  are gone the nonce buys nothing.
- **The poller re-arms on `turbo:load` and stops on `turbo:before-cache`.** It used to re-run simply
  by being an inline script in a replaced body; living in a file loaded once, it needs the events.
  Verified in a browser: 3 fetches in 16 seconds while `data-poll="true"`, and **zero after
  navigating away** — the failure mode of getting this wrong is a stacked interval that keeps
  polling a page the user has left.
- **Turbo injects one inline stylesheet, and `style-src 'self'` blocked it.** The three-pixel Drive
  progress bar arrives as a `<style>` element inserted into `<head>` (`ProgressBar.defaultCSS`), so
  the bar silently never appeared. **Found only in a browser — curl cannot see a CSP violation.**
  Admitted by SHA-256 hash rather than by `'unsafe-inline'`, which would have permitted every future
  inline style on the site to buy back one. The cost is that re-vendoring Turbo can stale the hash;
  the warning sits in `scripts/build-js.sh`, where someone would do it.
- **`govulncheck` reports 30 findings, all in the Go standard library, none in a dependency.**
  Every one is fixed by a toolchain upgrade — this tree builds with go1.25.1 and the fixes land
  across 1.25.2 to 1.25.12. Not silently bumped here: the toolchain is the build environment's, and
  **Phase 14 must build against go1.25.12 or later.** The reachable ones matter: `net/mail` and
  `net/textproto` are on the email path, `crypto/x509` and `crypto/tls` on the SMTP and DB paths.
- **Validation was de-duplicated, not rewritten.** `service/validate.go` collects the four idioms six
  validators were each writing by hand (trim, required, rune-length, the phone shape/digit pair) and
  every user-facing message is byte-identical to the one it replaces. Rune length, never bytes: a
  byte count rejects a shorter Indonesian name than the column holds and than the message promises.
- **Verified, not assumed** (against a real MariaDB with a minted admin token, driving a real
  headless Chrome, with every test row removed afterwards and the seed state restored):
  a POST with no token, a tampered token, a token from another session, and a `https://evil.example`
  Origin were each refused **403** with the row unchanged, while the same request with a valid token
  succeeded — checked on the layanan/users/jadwal toggles, the layanan multipart edit, the booking
  commit, the payment action, the user cancellation and logout; a request with no session at all
  redirected to SSO rather than 403; the Turbo Stream reply to a toggle carries fresh tokens;
  a 5 MB body returned **413** and a 3 MB image still returned **422** with its own message and no
  orphan on disk; the webhook still applied with no cookie and no token, and still answered 200 to a
  foreign `ukom-` prefix and to garbage; 25 rapid booking POSTs gave exactly 20×422 then 5×429 with
  `Retry-After`; every one of 12 pages rendered exactly as many `_csrf` inputs as `method="post"`
  forms, with zero `ZgotmplZ`; an authenticated page carried `Cache-Control: private, no-store` and
  an anonymous one did not; HSTS appeared only under `ENV=production` and `robots.txt` still served
  `Disallow: /` outside it; **zero CSP violations across all 14 pages**, both confirm dialogs opened,
  the print button and Turbo Drive's body swap both survived; no horizontal overflow at 390 or 1280
  and the logout control renders correctly in both layouts and the mobile drawer; `access_token`,
  `_csrf`, the session key, the DB password and the Midtrans server key each appear **zero** times in
  the run's log; and **the `booked_count` == holding-bookings invariant held**, with the database
  returned to exactly its seeded state. Phase 13 should turn the mask/verify round trip, the origin
  check, the token bucket and the XFF walk into permanent tests — they were checked here with a
  throwaway `cmd/tpjcheck12` and a puppeteer script, both deleted.

**Left for later, deliberately:** a `__Host-` cookie prefix (it needs `Secure`, which is off in
development, so the name would differ per environment for no gain while `SESSION_SECURE` already
tracks `ENV`); CSP reporting (`report-to` needs an endpoint to collect it, which is Phase 14's
monitoring decision); and a distributed rate limiter (one process, one map — a second instance would
each get their own budget, which is looser, not broken).

---

## Phase 13 — Testing

- [x] **Concurrency: N goroutines booking one capacity-1 slot against a real test DB → exactly 1 success, `booked_count=1`.** This is the test that matters most; a sequential test proves nothing here
- [x] Concurrency: capacity-N slot with 2N concurrent bookers → exactly N succeed
- [x] Invariant check after any booking test: `booked_count == COUNT(non-terminal bookings for that slot)` — `testsupport.AssertInvariant`, which every DB-backed test ends with
- [x] Unit: schedule generator (boundaries, TZ, duplicate skip, re-run is a no-op)
- [x] Unit: Midtrans signature verification (valid, tampered amount, wrong key) + notification state machine, table-driven over every `transaction_status`
- [x] Idempotency: same `settlement` webhook applied twice → one paid booking, no double side effects
- [x] Foreign-prefix webhook (`ukom-...`) is ignored without error
- [x] Unit: price snapshot & expiry logic (expiry releases the slot exactly once)
- [x] Handler tests with `httptest` for auth guards (no session → redirect to `AUTH_URL`, non-admin → 403 on `/admin`, expired token → refresh path)
- [x] Integration: seed → book → pay (mocked gateway) → webhook → assert booking `paid`
- [x] Manual QA checklist per page (both layouts, mobile + desktop) — [QA.md](QA.md)
- [~] `make test` green in CI — **deferred to Phase 14**, which owns the deployment target and therefore the CI one. There is no `.github/` and no remote yet. `make test` is self-contained instead: the harness creates, migrates and seeds its own database, and a missing MariaDB is a hard failure rather than a skip, so a green run always means the concurrency test ran.

Plus every "Phase 13 should turn X into a permanent test" left by an earlier phase — the eleven at PLAN.md:216, 316, 434, 547, 640, 739, 872, 1055, 1163, 1333, 1447 and 1576. See the notes below.

**Acceptance:** ✅ Verified 2026-07-28. 33 test files, ~250 tests, `make vet` and `make test` green across five consecutive runs.
Three load-bearing tests were proven to fail by breaking the code they guard and watching them go red (see notes).
**Two real defects found by writing them:** an empty webhook body answered 500 instead of 200, and `holdsSlot` was
named for a rule it does not implement.

### Phase 13 notes — decisions taken during execution

- **Three tiers, and the split was forced rather than chosen.** There is no interface over the database —
  `repository.Store` is a concrete struct and `WithTx` hands out a concrete `*sqlc.Queries` — and the invariants this
  system is built on are properties of InnoDB, not of Go. So the DB-backed tier runs against a real MariaDB.
  Meanwhile most pure logic is unexported *and* its types cannot be constructed from outside without a store
  (`Schedule.expand`, `Booking.slotMessage`, `validate.go`'s primitives, `Limiter`'s injectable clock,
  `app.safePath`, `handler.csvSafe`), so that tier is in-package, using struct literals like
  `&Schedule{loc: jkt}` and `&Settings{values: …}` instead of constructors.
- **All DB-backed tests live in one package, `internal/integration`.** `testsupport` imports `service`, so an
  in-package `service` test importing the harness would be an import cycle. One package also means one test
  database, one schema apply per run, and no cross-package clobbering when `go test ./...` runs packages in
  parallel. Nothing they need is unexported — testing through `Booking.Create`, `Payment.ApplyWebhook` and
  `Email.SweepReminders` is the better seam anyway.
- **`-short` skips the DB tier; `make test` does not pass it.** A missing database is therefore a hard failure in
  `make test` and a clean skip in `make test-unit`. A green `make test` can never mean the concurrency test
  quietly did not run.
- **The harness owns `appskep_tpj_test` and refuses any name not ending `_test`.** It DROPs the database on every
  run, so that guard is what stops a mis-set `TEST_DB_NAME` from reaching `appskep_tpj`. Verified: the development
  database ended the phase with exactly its own rows and zero test-shaped ones.
- **`Reset` deletes the settings rows rather than trusting the seed to restore them.** `seed_dev.sql` ends its
  settings INSERT with `ON DUPLICATE KEY UPDATE setting_key = setting_key` — a deliberate no-op, because those rows
  are edited in `/admin/pengaturan`. That makes the seed useless as a restore: a test lowering
  `booking_max_days_ahead` left it lowered, and the failure surfaced three tests later in a booking fixture that
  had nothing to do with settings.
- **No new dependency.** Stdlib `testing` only; `go.mod` is still at six direct requires.
- **No production seam had to be added.** `payment.Gateway` and `mail.Sender` are already interfaces, and the
  refresh branch is driven by pointing `cfg.Auth.URL` at an `httptest.Server` — which also exercises the real
  `HTTPRefresher`, where a fake `auth.Refresher` would have skipped it. `auth.Refresher`'s doc comment promises a
  seam "for Phase 13's handler tests"; the promise is kept without one.

**Two real defects, both found by writing the test rather than by reading the code:**

- **An empty request body to the public webhook answered 500.** `validJSON(nil)` returned nil and
  `payment_notifications.payload` is NOT NULL, so the audit insert failed — and 500 is precisely the status that
  tells Midtrans to retry. A zero-length POST is something anything on the internet can send to an
  unauthenticated endpoint, so it would have been retried forever and logged at ERROR each time. `validJSON` now
  wraps an empty body like any other unreadable one; `nullJSON` grew its own emptiness check, because
  `payments.raw_response` is nullable and wants the opposite.
- **`holdsSlot` was named for a rule it does not implement.** Its doc comment claimed to be "the rule the
  `booked_count` invariant is defined by", but it returns false for `completed` — while
  `bookings.active_slot_id` counts a completed booking as still holding its slot. The behaviour at all four call
  sites was right (an operator may not cancel or reschedule a visit that already happened); the name was the trap,
  and a future caller reaching for it to decide whether to `ReleaseSlot` would have freed the slot of every
  completed booking. Renamed `canCancelOrReschedule`, with the distinction recorded in two tests.

**Proving the tests can fail.** A suite never seen red is not evidence, so each of the three load-bearing ones was
verified by breaking the code it guards:

- Moving `GetSlotForUpdate` below a plain read in `Booking.create` reproduced Phase 7's defect exactly — 19 of 20
  racers got MariaDB **ER_CHECKREAD (1020)** instead of a friendly message, with `booked_count` still correct so
  nothing looked wrong in the data. The test names the fix in its own failure message.
- Removing the `RowsAffected() == 1` guard before `ReleaseSlot` in `expireOne` **did not fail the capacity-1
  tests** — `GREATEST(booked_count - 1, 0)` clamps a second release at zero, so mitigation #3 covers for the
  missing guard. It took a capacity-2 slot with a second live booking to expose it, where a double release takes
  the counter from 2 to 0 while someone is still coming. That test
  (`TestDoubleReleaseWouldStealAnotherBookingsSlot`) exists because of this experiment, and would not have been
  written without it.
- Rewriting one admin query's `col >= ? AND col <= ?` back to `BETWEEN ? AND ?` and running `make sqlc` produced
  Phase 10's measured numbers — `expected 14 arguments, got 20` — while `go build` stayed green, which is what
  makes the defect ship.

**Left for later, deliberately:** a CI workflow (Phase 14); browser-driven tests of the checks in
[QA.md](QA.md), because a headless-Chrome dependency is a large thing to maintain for assertions a person makes in
two minutes; a fake `sqlc.Querier`, which would mean changing service signatures to buy tests that cannot see the
locking; and property-based testing of the money parsers, where the table already covers every documented rule.

---

## Phase 14 — Deployment & go-live

- [ ] **Build on go1.25.12 or later.** Phase 12's `make vulncheck` found 30 standard-library
      vulnerabilities against go1.25.1, several reachable (`net/mail` and `net/textproto` on the
      email path, `crypto/x509` and `crypto/tls` on the SMTP and DB paths). All are fixed by the
      toolchain; none are in a dependency. Re-run `make vulncheck` after the bump and expect zero.
- [ ] **CI, handed over by Phase 13.** `make test` is self-contained — the harness creates,
      migrates and seeds its own database — so a workflow needs only a MariaDB service
      container, go1.25.12+, and `make vet && make test`. It was not written in Phase 13
      because there is no `.github/`, no commits and no remote to run it against, so it
      could not have been verified. Whoever picks the deploy target picks this.
- [ ] Set `TRUSTED_PROXIES` to the reverse proxy's address. Left empty it is safe but the rate
      limits and the request log see the proxy as every client, so one visitor can exhaust the
      budget for all of them.
- [ ] `Dockerfile` (multi-stage, distroless/alpine, `-ldflags "-s -w"`) or systemd unit — match existing Appskep deployment practice
- [ ] Embed templates & static assets (`embed.go`) so the binary ships standalone
- [ ] Production `.env` checklist (all secrets set, `ENV=production`)
- [ ] Migration runbook + DB backup/restore procedure
- [ ] Reverse proxy (nginx/caddy) config: TLS, gzip, static caching headers
- [ ] Staging deploy with Midtrans **sandbox**; register webhook URL in the Midtrans dashboard
- [ ] UAT with the client on staging
- [ ] Switch to Midtrans **production** keys, re-register the production webhook URL
- [ ] Monitoring: health endpoint check, log rotation, error alerting
- [ ] Post-launch checklist: verify a real small payment, verify emails deliver, verify backups run

---

## Milestones

| Milestone | Phases | Definition of done |
|-----------|--------|--------------------|
| **M1 — Skeleton** | 0–2 | App runs, DB migrated, layouts styled |
| **M2 — Admin ready** | 3–5 | Admin can log in and manage layanan + jadwal |
| **M3 — Public browse** | 6 | Public site shows real services from the DB |
| **M4 — Booking works** | 7 | Bookings created safely, slots reserved |
| **M5 — Payments work** | 8–9 | Sandbox payment end-to-end, user sees history |
| **M6 — Operable** | 10–11 | ✅ Admin runs daily ops; emails sent |
| **M7 — Production** | 12–14 | Hardened, tested, deployed |

---

## Conventions (keep in sync with CLAUDE.md)

- Generated `internal/database/sqlc/` is never edited by hand — change the `.sql` and run `make sqlc`.
- Templates live in `template/` (singular). Layouts per subsystem, pages under `pages/<subsystem>/`.
- Handlers stay thin: parse → validate → call service → render. Business logic in `internal/shared/service/`.
- Money as `DECIMAL(12,2)` in DB, `int64` minor units or `decimal` in Go — never `float64` in payment paths.
- All times stored UTC-naive in `Asia/Jakarta` semantics; one helper does all formatting.
- User-facing copy in Bahasa Indonesia; code, comments, and commits in English.
- External calls (Appskep auth, Midtrans) always have a context timeout and are behind an interface.
- Any read-then-write on a shared row goes through a transaction with `FOR UPDATE` — see the design note below.

---

## Design note: slot concurrency

The single highest-risk defect in this system is **double-booking one slot**, because it takes money from a customer for a service that cannot be delivered, and it does so silently.

### The failure

Checking availability and recording the booking are separate DB round-trips. Two requests can interleave between them:

| time | request A | request B | `booked_count` |
|---|---|---|---|
| t1 | read slot → 0 | | 0 |
| t2 | | read slot → 0 | 0 |
| t3 | `0 < 1` → free | | 0 |
| t4 | | `0 < 1` → free | 0 |
| t5 | insert booking A | | 0 |
| t6 | | insert booking B | 0 |
| t7 | write `0+1` | | 1 |
| t8 | | write `0+1` | 1 |

Two bookings exist, both users pay, and the counter reads `1` — a **lost update** that also erases its own evidence, so the admin panel shows nothing wrong.

The window is only milliseconds, but booking traffic concentrates precisely where it hurts: one popular slot, one promo broadcast, many simultaneous taps. It is also invisible in manual testing and in any test that runs requests sequentially — it will not show up until production.

### The mitigations this plan uses

1. **`SELECT ... FOR UPDATE`** inside the booking transaction — the lock is taken *before* the capacity check, so request B blocks until A commits and then correctly sees the slot as full. Works for any `capacity`.
2. **Unique index on `(slot_id, user_id)`** for non-terminal bookings — the database itself rejects a duplicate from a double-submitted form, independent of application logic.
3. **Guarded decrement** on release (`GREATEST(booked_count - 1, 0)`), applied only on a real status transition, so the expiry ticker and the Midtrans webhook cannot both release the same booking and drive the counter below zero.
4. **A concurrency test** (Phase 13) that fires N goroutines at one slot and asserts exactly one winner. Without this test, a future refactor that moves the check outside the transaction reintroduces the bug undetected.

### The same shape appears twice more

- **Expiry must release.** Flip `status` and decrement `booked_count` in one transaction, or slots leak until the schedule looks fully booked with no real bookings behind it.
- **Webhook retries are concurrent.** Midtrans re-sends notifications; a retry can arrive while the first is mid-flight. Lock the payment row, check the current status, make every transition idempotent — otherwise one payment applies twice.

**Rule of thumb for this codebase:** if a handler reads a row, decides something from it, and then writes based on that decision, the read and the write belong in the same transaction and the read needs `FOR UPDATE`.
