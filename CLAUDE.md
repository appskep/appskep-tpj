# CLAUDE.md — Appskep TPJ

Booking & online payment system for Terapi Pemuda Jompo.
Go monolith · chi · sqlc · MariaDB · html/template · TailwindCSS · Hotwire Turbo.

The authoritative development plan is [PLAN.md](PLAN.md). Read its **Progress** table
before starting work, and tick a task the moment it is executed and verified.

## Commands

| Command | What it does |
|---|---|
| `make dev` | Hot-reload server via air (builds Tailwind first) |
| `make run` | Build + run once |
| `make build` | Compile to `bin/server` |
| `make test` | Whole suite, `-race -count=1`. **Needs MariaDB** — a missing one fails rather than skips |
| `make test-unit` | `-short`: only the tests that need no database |
| `make db-test-drop` / `make db-test-shell` | Drop / inspect the test database (`make test` recreates it) |
| `make vet` | `go vet ./...` |
| `make vulncheck` | `govulncheck ./...` — run before a release |
| `make sqlc` | Regenerate `internal/database/sqlc/` |
| `make db-create` / `make db-drop` | Create / drop the database |
| `make migrate` | Apply `internal/database/migration/0*.sql` |
| `make seed` | Load `seed_dev.sql` |
| `make db-reset` | Drop + create + migrate (destructive, dev only) |
| `make db-fresh` | `db-reset` + `seed` — **the loop to run after any schema edit** |
| `make tailwind` / `make tailwind-watch` | Build / watch `static/css/app.css` (downloads the pinned CLI into `bin/` on first run) |
| `make assets` | Re-vendor the icon sprite, fonts and Turbo — only when a version or the icon list changes |

Server listens on `SERVER_PORT` (default 8080). Health check: `GET /api/health`.

## Layout

```
cmd/server/main.go          config → DB pool → settings → renderer → Deps → chi router
internal/app/deps.go               app.Deps — the shared dependency bundle
internal/app/{auth,errorpage,middleware}.go  auth gates, error pages, recoverer, static
internal/app/{public,admin,api}/   routes.go + handler/ + middleware/ per subsystem
internal/database/migration/       0001_schema.sql, seed_dev.sql
internal/database/query/           *.sql — sqlc input
internal/database/sqlc/            GENERATED — never edit by hand
internal/shared/config/            the only place that reads the environment
internal/shared/view/              template renderer, View envelope, FuncMap
internal/shared/{auth,payment,service,middleware,model,repository,util}/
template/{layouts,pages,partials,emails}/
static/{css,js,img,fonts,uploads}/
scripts/build-{icons,fonts,js}.sh  re-vendor committed assets (make assets)
```

Route mounts: `/` → public, `/admin` → admin, `/api` → internal JSON.

## Rendering

Every subsystem takes a single `*app.Deps` (config, store, logger, renderer, settings).
Add a field there rather than growing `Routes` signatures.

`view.Renderer` holds one template set per page, keyed `"<layout>/<page>"` —
`view.Render(w, r, status, "public/landing", &view.View{…})`. The layout is part of the key,
so a page cannot render into the wrong chrome. Templates are parsed at startup (a malformed
template fails the boot) and reparsed per request when `ENV=development`, so an HTML edit
needs no restart — which is why `.air.toml` deliberately does not watch `template/` or
`static/`.

The renderer fills `Site`, `Now`, `User`, `CSRF`, `Page.Path`, `Page.OGType`,
`Page.Canonical` and the absolute form of `Page.OGImage` itself; handlers set only `Page`
and `Data`. Formatting lives in `internal/shared/util/format.go` and is reached from
templates through the FuncMap — never format inline. `icon` is the only `template.HTML` in
the codebase; helpers that would otherwise emit markup return data instead (`statusBadge`
returns a struct, `paragraphs` returns `[]string`).

Frontend: Tailwind v4 via the pinned standalone CLI (no Node, no `tailwind.config.js` —
tokens are an `@theme` block in `static/css/input.css`). Turbo, the Lucide/Simple-Icons
sprite and the woff2 fonts are committed under `static/`; `make assets` re-fetches them.

## Conventions

- **Generated code is never hand-edited.** Change the `.sql` and run `make sqlc`.
- **`0001_schema.sql` is the single source of truth until go-live.** `make migrate` re-pipes
  every `0*.sql` on each run, so it is re-runnable but blind to edits — change a column by
  editing that file and running `make db-fresh`. No `0002_*.sql` before Phase 14; after
  go-live the schema file freezes and changes become additive, idempotent numbered ALTERs.
- **Database access goes through `*repository.Store`**, not `*sql.DB`. Only `main.go` and
  `internal/shared/repository` may import `database/sql` for the pool. Use
  `store.WithTx(ctx, fn)` for anything that reads-then-writes.
- **Handlers stay thin**: parse → validate → call service → render. Business logic
  lives in `internal/shared/service/`.
- **All config flows from `config.Load()`.** Nothing else calls `os.Getenv`.
- **Money** is `DECIMAL(12,2)` in the DB and `int64` minor units or `decimal` in Go —
  never `float64` on a payment path. `util.Rupiah` renders and `util.ParsePrice` parses,
  both on the string alone.
- **Times** are `Asia/Jakarta` (`APP_TZ`) end to end — nothing is stored as UTC. The driver
  `loc` and the MySQL session `time_zone` (`+07:00`) are both pinned in `DBConfig.DSN`, so
  `NOW()` and `time.Now().In(cfg.App.Location)` are the same wall clock and SQL may use
  `NOW()` directly. `DATETIME` everywhere, never `TIMESTAMP`. One helper does all formatting.
  Changing `APP_TZ` to a DST-observing zone fails at startup by design.
- **User-facing copy in Bahasa Indonesia**; code, comments, and commits in English.
- **External calls** (Appskep auth, Midtrans) always take a context timeout and sit
  behind an interface.
- **Every SQL access goes through sqlc** — no string-concatenated queries.
- **Background loops live in `internal/app/*.go`** (`expiry.go`, `reminder.go`) because
  they need `Deps`, and run in `main.go`'s single `sync.WaitGroup` on the signal context.
  Three of them now: the expiry ticker, the email worker and the reminder ticker.

## The rule that matters most

> If a handler reads a row, decides something from it, and then writes based on that
> decision, the read and the write belong in the **same transaction** and the read
> needs `SELECT ... FOR UPDATE`.

This is what prevents double-booking a slot — the highest-risk defect in the system.
See [PLAN.md § Design note: slot concurrency](PLAN.md#design-note-slot-concurrency).
Slot releases use a guarded decrement (`GREATEST(booked_count - 1, 0)`) applied only
on a real status transition, so the expiry ticker and the Midtrans webhook cannot
both release the same booking.

Three rules follow from it, all load-bearing:

- **Canonical lock order: `schedule_slots` → `bookings` → `payments`.** Any transaction
  that will change a slot's `booked_count` takes that slot's `FOR UPDATE` *first*, before
  locking any booking or payment row. Never acquire a slot lock after a booking lock —
  booking creation and the expiry ticker would otherwise deadlock.
  **`GetSlotForUpdate` is the literal first statement, ahead of every plain `SELECT` too.**
  A non-locking read looks harmless but establishes the REPEATABLE READ snapshot, and a
  locking read that then meets a newer row fails with MariaDB **ER_CHECKREAD (1020)**
  instead of blocking. Measured: reading `services` one line early turned 19 of 20 racers
  from a friendly "slot baru saja terisi" into a 500, with the counter still correct so
  nothing looked wrong in the data.
- **Slot-holding vs slot-releasing statuses.** `cancelled` and `expired` release the slot;
  `pending_payment`, `paid`, `confirmed`, `completed` hold it. The invariant is
  `booked_count == COUNT(bookings for that slot NOT IN ('cancelled','expired'))`.
  This rule is encoded once, in the `bookings.active_slot_id` generated column.
- **`RowsAffected() == 1` is the definition of a real status transition,** and is the only
  thing that authorises a `ReleaseSlot` call. Every booking and payment transition query is
  guarded by its current status so a Midtrans retry or a double-clicked button affects
  0 rows and changes nothing else.

### Settings vs environment

Where a key exists in both the `settings` table and the environment
(`payment_expiry_minutes` ↔ `PAYMENT_EXPIRY_MINUTES`), the **DB row wins when present and
parseable**; the env value is the fallback and the fail-fast default. `config` remains the
only reader of `os.Getenv`.

## Forms, uploads and Turbo (settled in Phase 4)

The layanan module is the reference implementation for every admin form that follows.

- **Validation errors are `*service.ValidationError`** — a `map[field]message` keyed by the
  HTML input name, with Indonesian copy. Build it across every field so one submit reports
  every problem. A handler does `errors.As(err, &ve)` → re-render the form at **422** with
  the submitted strings; `service.ErrNotFound` → 404; anything else → log + 500. Never
  redirect a rejected form, that loses the input.
- **The rules live in the service, not the handler.** The handler collects raw strings;
  `service` parses and validates them, so create and edit cannot drift apart.
- **`Deps.FlashRedirect` after a successful action**, never `SaveFlashAndRedirect` — the
  latter drops the session token on purpose and is only for requests that end a session.
- **Turbo partial updates use Streams, not Frames.** `RenderStream(w, r, page, fragment, v)`
  with a `<turbo-stream action="replace" target="…">`. The fragment template is defined in
  the page file (the partial namespace is global) and is the *same* template the full page
  renders, so the two cannot drift. Always fall back to a redirect when `Accept` carries no
  `text/vnd.turbo-stream.html`, so the control works without JS.
- **A toggle posts the value it wants**, not a flip, so a resent request is idempotent.
- **Uploads go through `internal/shared/upload`.** Set `http.MaxBytesReader` before
  `ParseMultipartForm` — the latter's argument only bounds memory, the rest spills to an
  unbounded temp file. The stored extension comes from `http.DetectContentType`, never the
  filename. Replacing an image is always **write new → update row → delete old**, so an
  interruption leaves an orphan file rather than a row pointing at nothing.
- **A partial receives exactly one value.** There is no `dict` helper and adding one is
  declined: build the payload as a struct in the handler package.
- **Page templates are flat.** `pages/<layout>/*.html` is globbed without recursion, so a
  second page is `layanan-form.html`, not `layanan/form.html`.

Two more, added by the jadwal module in Phase 5:

- **A `disabled` input is an affordance, not a guarantee.** Where the UI locks a field, the
  service must enforce the same rule on the way in — a disabled input submits nothing, so the
  server takes the stored value rather than validating a field the form never sent. The
  booked-slot lock in `service.Schedule.Update` is the reference.
- **A destructive bulk action confirms against real numbers, in two POSTs to one route.**
  Step one returns the counts ("11 dihapus, 1 dilewati") and writes nothing; step two carries
  `konfirmasi=1` plus every input re-posted as hidden fields. The `confirm` partial cannot do
  this — its form carries only the CSRF field, so it cannot carry a date range. Reversible
  actions (activate/deactivate) skip the step entirely.

One more, found the hard way in Phase 7 and retrofitted to Phase 5:

- **A form whose POST answers with 200 HTML needs `data-turbo="false"`.** Turbo requires a
  form submission to end in a redirect and **silently discards** a non-redirect 200 — the
  button appears to do nothing. This bites exactly the two-step preview/review shape above,
  where the reply cannot redirect because its state is form data that must not go in a URL.
  4xx/5xx responses are rendered normally, so the 422 re-render path is unaffected, as is any
  form that redirects on success. **curl cannot catch this**: curl is the no-JS path, which
  always worked. Check a POST-then-200 form in a real browser.

## Public pages & SEO (settled in Phase 6)

- **A public read goes through the service, not `Store.Queries`.** `Catalog.ListActive`
  and `Catalog.GetActiveBySlug` carry the `is_active` predicate in SQL and map a missing
  row to `service.ErrNotFound`, so a handler renders a 404 without importing
  `database/sql`. Never use `GetServiceBySlug` on a public path — it does not filter.
- **Crawler endpoints live in `internal/app/seo.go` and mount on the root router**
  (`/robots.txt`, `/sitemap.xml`, `/favicon.ico`), beside `StaticHandler` and *before*
  `r.Mount("/", …)`. They must skip `OptionalAuth`: it costs a session round-trip and
  would consume an inbound `?access_token=`. Neither can go through `view.Renderer`,
  which forces `text/html` and only globs `pages/*.html`.
- **`robots.txt` serves `Disallow: /` unless `ENV=production`.** Staging must never be
  indexable, and that cannot be fixed after the fact.
- **The sitemap is generated from the active services**, with `encoding/xml`, so a new
  authenticated route cannot appear in it and an `&` in a slug cannot break the document.
- **`Page.OGImage` is set site-relative by the handler**; the renderer absolutises it
  against `APP_URL`. Never build it from `r.Host`.
- **A coming-soon CTA is replaced, never dimmed or disabled** — a tooltip is invisible on
  touch and a disabled-looking button still reads as clickable. Say what to do instead.

## Booking flow (settled in Phase 7)

- **The whole flow is one URL.** `/booking` renders steps 1–3 from `?layanan=&tanggal=&slot=`;
  the review is a POST to the same route without `konfirmasi=1`, and `konfirmasi=1` commits.
  Nothing that identifies a customer ever travels in a query string.
- **`Schedule.Window()` is the single definition of what may be booked** (lead time,
  max days ahead). The public availability preview and the booking transaction both call it,
  so the preview cannot advertise a date the form refuses. `Booking.slotMessage` plays the
  same role for the per-slot rules: once in validation for the message, once inside the lock
  for the truth.
- **Validation errors on `layanan` and `slot` are promoted to the page-level notice.** They
  are pickers, not inputs — there is no field for a message to sit beside, and selecting one
  moves the page back a step. Without this a rejected submit renders a 422 that says nothing.
- **A booking code is `TPJ-<slot date>-<4 chars>`** from `crypto/rand`, over an alphabet with
  `0 1 O I L` removed — it gets read over the phone. A collision retries the INSERT inside the
  same transaction; InnoDB rolls back the statement, not the transaction, so the slot hold
  survives.
- **Someone else's booking is a 404, never a 403,** and the rule lives in
  `Booking.DetailForUser` so the next page that loads a booking inherits it. A 403 confirms
  the code exists.
- **The expiry ticker is `Deps.RunExpiryTicker`**, started in `main.go` on the signal context.
  `defer wg.Wait()` is registered *before* `defer stop()` so every exit path cancels then waits,
  both ahead of `db.Close()`. `EXPIRY_SWEEP_INTERVAL` defaults to 1m and may not be zero.

## Authentication

TPJ owns **no credentials**. Identity comes from the Appskep SSO service (`AUTH_URL`)
as a JWT verified with the shared HMAC `AUTH_SECRET`. There is no login form, no
password check. `users` is a local mirror keyed on `appskep_user_id` — never add a
password column. Login/register/reset pages are thin redirects to Appskep.
See [authentication.md](authentication.md) (describes a Gin app — adapt, don't copy)
and PLAN.md Phase 3, including the list of the reference app's defects not to repeat.

Shipped in Phase 3, and load-bearing:

- **The gate is three `*app.Deps` methods in `internal/app/auth.go`** — `OptionalAuth`,
  `RequireAuth`, `RequireAdmin` — applied with `r.Use` per subsystem, never at the root
  (`/static` and the Midtrans webhook must stay ungated). A new admin route is admin-only
  by construction. They live in `internal/app` rather than `shared/middleware` because
  denying needs `Deps.ErrorPage`; the reverse import is a cycle.
- **The session cookie holds the token and nothing else.** Name, email and user ID are
  re-derived from the verified claims on every request. Never store identity in the
  session — that is what makes the reference app's cookie key an impersonation bug.
- **`client_base_url` comes from `APP_URL`**, never `r.Host` or `X-Forwarded-Proto`: it
  decides where a valid token gets delivered.
- **Anything that redirects a user must go through `safePath`.** `pass` and `next` are
  attacker-supplied; a bare leading `/` is not enough (`//host` is protocol-relative).
- **An action is a POST with a CSRF token, never a link.** Logout was a `GET` guarded by
  `data-turbo-prefetch="false"` (Turbo 8 prefetches on hover, so the link ended the session on
  mouseover); Phase 12 made it `POST /logout`, which fixes both that and the cross-site
  `<img src="/logout">`. If a future `<a>` must change state, it needs the prefetch guard — but
  prefer the form.
- `AUTH_SECRET` and `SESSION_KEY` (32+ bytes) are required to boot in **every**
  environment, not only production.

## Payments (settled in Phase 8)

Midtrans Snap on Appskep's **shared** merchant account, so `order_id` must carry the
`tpj-` prefix (`tpj-<uuid-v4>`, minted by `config.MidtransConfig.OrderID`) and inbound
webhooks for other prefixes are ignored. Payment is confirmed **only by the webhook** —
never by a Snap token response or a client-side callback. See
[create-order.md](create-order.md) and PLAN.md Phase 8.

- **Nothing account-wide may be changed, because the account is shared.** Our webhook URL
  is attached per transaction via `X-Append-Notification`; the dashboard's notification URL
  and its finish/unfinish/error redirects belong to another Appskep system. Only
  `callbacks.finish`, a per-transaction field, is set.
- **`POST /api/webhook/midtrans` is public, ungated and must stay exempt from CSRF.** The
  SHA512 signature over `order_id + status_code + gross_amount + MIDTRANS_SERVER_KEY` — with
  `gross_amount` used *exactly* as received — is the whole authentication. Its status codes
  are chosen for what Midtrans does with them: 200 for anything we will never accept (foreign
  prefix, unknown order, unparseable body) so it stops retrying, 401 only for a bad signature
  on one of our orders, 500 only for our own failure.
- **Authorisation runs in series: payment → booking → slot.** The payment transition must
  report `RowsAffected() == 1` before the booking transition is attempted, and the booking
  transition must report 1 before `ReleaseSlot`. This is what makes a Midtrans retry, the
  expiry ticker and a user cancellation all reach the same booking harmlessly.
- **The Snap request carries an `expiry` derived from the booking's remaining hold**, so
  Midtrans stops accepting payment at the moment the ticker releases the slot. Without it
  Midtrans defaults to 24 hours and will take money for a slot someone else now has.
- **Money after the slot was released is logged at ERROR and left for a human.** `paid_at` is
  set because the money is real; the booking stays `expired`/`cancelled`; the slot is never
  re-taken. There is no automated refund in v1.
- **Reuse matches the booking's `price_amount`, never the layanan's current price** — the
  snapshot is the whole point of `bookings.price_amount`.
- **The gateway is `payment.Gateway` with a hand-written HTTP transport.** `midtrans-go`
  v1.3.8 discards the context handed to it (`req.WithContext(...)` with the result thrown
  away) and logs request headers — including the Basic-auth server key — at Info level.
  `payment/httpclient.go` replaces its transport; everything else comes from the SDK.

### Turbo Frames that poll

Two rules, both found only in a browser and both cheap to re-break:

- **A frame response must contain a matching `<turbo-frame>` element, and that element must
  NOT carry a `src` equal to the URL it was fetched from.** Turbo throws "source URL which
  references itself" and empties the frame. Render the frame from the fragment template and
  pass the `src` in from the page only (`paymentData.FrameSrc`).
- **`RenderFragment` sends `Cache-Control: no-store`.** Without it the browser answers every
  repeat fetch of a polled URL from its own cache and the requests never reach the server —
  indistinguishable, from the outside, from a poller that died.
- **Scripts inside a frame's replaced content never execute.** A poller lives in the page and
  reads a server-rendered `data-poll` flag out of the frame to decide whether to continue.

## Konfirmasi, riwayat & profil (settled in Phase 9)

- **`/booking/{code}/konfirmasi` is the canonical page for a booking in any status.** Riwayat
  links there, Midtrans' `finish` callback returns there, and `GET /pembayaran` **303s there
  for any status other than `pending_payment`** — pembayaran is only the payment step. The
  redirect condition is the status alone, never the clock: a lapsed hold still has a panel to
  explain until the ticker moves it.
- **User cancellation copies `Booking.expireOne` exactly** — `store.WithTx` →
  `GetSlotForUpdate` as the literal first statement → the guarded
  `CancelPendingBookingByOwner` → `RowsAffected() == 1` is the only thing that authorises
  `ReleaseSlot`. `moved == false` is a *success*: the ticker or the webhook got there first
  and released the slot already. `CancelForUser` takes no `isAdmin` — admin cancellation is
  Phase 10's, and `user_id` in the WHERE is what makes ownership the database's guarantee.
- **The gateway order is cancelled after the transaction commits, never inside it.** A Snap
  page left open would otherwise keep taking money for a released slot.
  `Payment.CancelOrder` runs on `context.WithoutCancel`, and `payment.ErrOrderNotFound` (404 —
  the ordinary case, an unused Snap token) and `payment.ErrNotCancelable` (412) are both
  swallowed. `MarkPaymentCancelled` is written **only** when Midtrans returns 200, which is
  what keeps Phase 8's money-after-release path intact: if the customer already paid, Midtrans
  answers 412, nothing is written, and the late `settlement` still records `paid_at`.
- **A list filter resolves through a whitelist, and that is about behaviour, not injection.**
  The value is a bind parameter either way; the whitelist is what makes an unrecognised
  `?status=` mean "everything" instead of an empty list that reads as "you have none".
- **Labels come from `view.StatusBadge`,** never retyped in a handler, so a filter chip and
  the badge it filters to cannot call one status two different things.
- **Read-only identity is text with a link, not a disabled input.** Name and email belong to
  Appskep; `{{authURL}}` is the only way to reach them. Same rule as the coming-soon CTA.
- **Uploads with different caps get their own `upload.ImageStore`** (avatars are 1 MB, service
  images 2 MB), built in `main.go` so an unwritable directory is a failed boot.
- **Print is `print:hidden` on the layout chrome**, not a stylesheet — Tailwind v4's variant,
  consistent with there being no config file.

## Admin operations (settled in Phase 10)

- **A reschedule locks two slots, in ascending id order, as the transaction's first two
  statements.** Then the booking is re-read *under* those locks and the attempt abandoned
  if it moved slots in between (`errSlotMoved`). Without the ordering, two operators
  moving bookings between the same pair of slots in opposite directions deadlock. The
  chain after the locks is Phase 8's unchanged: `HoldSlot` on the target must report 1
  before the booking moves, and that must report 1 before `ReleaseSlot` on the old slot.
  `price_amount` and `booking_code` are never touched.
- **Admin cancellation covers `pending_payment`, `paid` and `confirmed`** — an operator
  must never need the database to free a slot. Cancelling a paid booking is allowed, the
  UI names the amount and says the refund is manual (Q6), the reason is required, and an
  `activity_logs` entry records who did it. The Midtrans order is cancelled afterwards
  **only when the booking was `pending_payment`**: that is the only state where an open
  Snap page can still take money.
- **`ErrBookingFinal` is the single sentinel** for "this booking no longer holds its
  slot", because cancel and reschedule are guarded on the identical status set.
- **A no-op transition is an info flash, never an error page.** `RowsAffected() == 0`
  means someone already did it.
- **The audit trail can never fail the action it records.** `Audit.Record` returns
  nothing, logs its own failures at WARN, and runs *outside* the transaction it describes.
  `clientIP` reads `RemoteAddr` only — `X-Forwarded-For` is untrusted until Phase 12.
- **The settings form is generated from `service.EditableSettings`,** and the whitelist is
  iterated, never the request. An absent key is left alone rather than blanked, and
  `Settings.Reload` runs after the commit so an edit reaches the public site on the next
  request.
- **Removing admin access has two guards, both in the service and both transactional:**
  no self-demotion, and never the last *active* admin. TPJ owns no credentials to recover
  an empty admin list with.
- **Labels for a derived state live in `view` beside `StatusBadge`** — `view.PaymentBadge`
  for the four payment lifecycle states — so a filter chip and the badge it filters to
  cannot call one state two different things.

### CSV export

- **Never through `view.Renderer`** (it forces `text/html` and globs only `pages/*.html`)
  — write `encoding/csv` straight to the ResponseWriter, same constraint as `sitemap.xml`.
- **A UTF-8 BOM first**, or Excel reads the file as the local codepage.
- **`csvSafe` on every cell**: a value beginning `= + - @` is prefixed with `'`. These
  files carry names and notes typed into a public form, and a spreadsheet evaluates them.
- **`data-turbo="false"` on the download link**, so Turbo does not try to swap a CSV
  response into the document. The export is capped and reports truncation in a header,
  never as an extra row.

### sqlc traps worth knowing before writing a query

- **`BETWEEN` is mis-bound by the MySQL engine, in two directions, silently.** An
  *expression* on its left (`DATE(x) BETWEEN ? AND ?`, `(CASE …) BETWEEN ? AND ?`) drops
  both parameters from the generated call; a *column name shared by several joined tables*
  duplicates each parameter once per table, ignoring the qualifier. Both produce SQL whose
  placeholder count does not match its arguments and fail on the first request.
  **Use `col >= ? AND col <= ?`.**
- **A nullable `JSON` column must be overridden to `sql.NullString` in `sqlc.yaml`**
  (`payments.raw_response`, `activity_logs.meta`). `database/sql` cannot scan NULL into
  `json.RawMessage`, and NULL is the ordinary case for both.
- **A `DECIMAL` aggregate needs an explicit `CAST(… AS DECIMAL(p,s))`** to type as a Go
  string; bare `SUM()` and `CAST(… AS CHAR)` both yield `interface{}`. The string is also
  what keeps money off `float64` all the way to `util.Rupiah`.
- **A template shared by two pages belongs in `template/partials/`.** The renderer builds
  one set per page — layout + partials + *that one page file* — so a `{{define}}` in
  another page file is invisible and 500s at render time. `go build` cannot see this.

## Notifikasi email (settled in Phase 11)

Six transactional mails, split the same way Midtrans is: `internal/shared/mail` is
transport only (`Sender`, `SMTPSender`, `NoOp`, MIME assembly) and
`service.Email` owns the templates, the queue and the rules.

- **A notification can never fail, delay or roll back the action it describes** —
  `Audit.Record`'s rule applied to mail. `Notify` pushes an id onto a buffered channel
  and returns; a full queue is a WARN and a dropped mail, never a failed booking.
- **Every dispatch sits after the commit, never inside a transaction.** The booking
  transactions hold slot locks, and nothing that talks to another machine belongs under
  one. `Payment.transition` runs inside the tx, so it fills in a `pendingEmail` that
  `applyOnce` enqueues after the commit — the same shape as "the gateway order is
  cancelled after the transaction commits, never inside it".
- **The trigger is a real transition, not a request.** `RowsAffected() == 1` is what
  authorises the mail, so a Midtrans retry, a double-clicked button and the expiry
  ticker racing a cancellation all produce exactly one notice. Dispatch therefore lives
  in the **service layer**: "cancelled" is reached from three places and "expired" from
  two, and only the service knows which one actually moved the row.
- **Only an id and a kind cross the channel.** The worker re-reads through
  `GetBookingAdminDetail` — which already joins the layanan, the slot and `u.email` — so
  a mail describes the booking as it is when sent. The two facts that cannot be re-read
  travel with the job: the slot a reschedule left, and the status a cancellation left.
- **Email is off, not broken, when SMTP is unconfigured.** `SMTPConfig.Enabled()` picks
  `NoOp`, and one INFO line at startup says which. Credentials are refused over an
  unencrypted connection to a non-loopback host.
- **Two template sets over the same files.** `html/template` renders the `_html` bodies;
  `text/template` renders `_subject` and `_text`, because html/template would turn a
  customer named "Ari & Co" into "Ari &amp; Co" in the plain-text part and the Subject
  header. Every mail is multipart/alternative with the HTML part **last** — a client
  picks the last part it can display.
- **All `emails/*.html` parse into ONE set and execute by name**, so every `{{define}}`
  across the directory must be uniquely named. The one-set-per-page rule exists because
  two pages both define `content`; emails never do. There is no layout — Go templates
  cannot take a template name as a variable — so each mail calls `email_open` /
  `email_close` itself. `NewEmail` checks all three names exist for every kind, so a
  typo is a failed boot rather than a mail nobody notices missing.
- **Inline styles and tables only.** A mail client does not fetch `app.css`, most strip
  `<style>`, and many block remote images — so no sprite, no logo, no Tailwind, and the
  hex values from the `@theme` block written out by hand.
- **The H-1 reminder is claimed before it is sent.** `MarkBookingReminderSent` is guarded
  on `reminder_sent_at IS NULL` and only `RowsAffected() == 1` authorises the mail, which
  is what makes the sweep idempotent across a restart, a second process, and a ticker
  running every 15 minutes all day. Claim-then-send is deliberate: a send that then fails
  is not retried, because a duplicate reminder is worse than a missing one. It covers
  `paid` **and** `confirmed` — confirming is an operator action that may never happen to
  a customer who has already paid.
- **`util.WhatsAppLink` is shared with the `waLink` FuncMap entry.** An email cannot
  import `view` (view imports service), so the rule lives in `util` like every other
  formatter.

## Hardening (settled in Phase 12)

- **The CSRF secret lives in the session, not in a cookie of its own.** The session cookie is
  already HMAC-signed and HttpOnly, so a secret inside it cannot be read by script or forged
  without `SESSION_KEY` — which closes the hole a plain double-submit cookie leaves open, where
  anyone able to write a cookie on the domain sets both halves to a value they know. What reaches
  a form is always a **fresh one-time-pad mask**, `base64(mask ‖ mask XOR secret)`: the booking
  form reflects submitted input into the same 422 that carries the token, which is exactly what a
  compression oracle needs. Minted in `withSession` **only for a signed-in user** — every
  state-changing route is gated, so an anonymous token would protect nothing and would put a
  cookie on every crawler hit.
- **`withSession` writes the cookie at most once per request.** Never re-`Session.Load` to save:
  it reads the *request* cookie, which still carries the flash the call just consumed.
- **`r.Use(d.CSRF)` goes after the auth middleware, per subsystem.** The `/api` router takes
  neither — the Midtrans webhook *is* a cross-site POST and its SHA512 signature is its
  authentication.
- **A `{{define}}` cannot reach `.CSRF`**, because `{{template "x" .Row}}` rebinds `$`. The
  toggles and the confirm dialog take a handler-built wrapper with the sqlc row **embedded**
  (`serviceToggle`, `userToggle`, `slotToggle`), so field access is unchanged by promotion.
- **The CSRF middleware parses the body itself**, never `PostFormValue`, which discards the parse
  error and would report an oversized upload as a permission failure. `*http.MaxBytesError` → 413,
  anything else unreadable → 400. It reads the body before the handler's own `MaxBytesReader`, so
  the transport cap is the global `SERVER_MAX_BODY_BYTES`; the friendly per-upload 422 still comes
  from `upload.ImageStore.Save`, which enforces its own cap.
- **Nothing believes `X-Forwarded-For` unless it came from `TRUSTED_PROXIES`,** which is empty by
  default and fails the boot when malformed. `middleware.ClientIP` walks the header right-to-left,
  which is what makes it unspoofable, and both the rate limiter and the request logger use it so
  they always name the same caller. chi's `RealIP` is rejected for the same reason `client_base_url`
  never comes from `r.Host`.
- **The CSP is strict and there is no inline script anywhere.** `script-src 'self'` with no
  `'unsafe-inline'` and no nonce; dialog triggers are `data-dialog` attributes read by one
  delegated listener in `static/js/app.js`, the only app-level JS in the project. A nonce does not
  cover attribute handlers, so it would not have saved the rewrite. `style-src` admits exactly one
  hash: Turbo's progress-bar stylesheet — **re-derive it after `make js` bumps Turbo**, or the
  loading bar silently disappears. No third-party origin appears as a *source*, because none is
  loaded: Snap checkout is a server-side 303 to Midtrans' hosted page, not their `snap.js`.
  `form-action` is the lone exception — see the next rule.
- **A form whose POST redirects to another origin needs `data-turbo="false"` too, and its
  target named in `form-action`.** Turbo submits by `fetch()` and follows the redirect, so a
  cross-origin hop dies on CORS *and* on `connect-src 'self'` — `TypeError: Failed to fetch`,
  and the button does nothing. Phase 7's rule ("a form that redirects on success is
  unaffected") only ever held for a same-origin redirect Turbo can swap into the document.
  Relaxing the CSP alone would not have fixed it: CORS blocks the fetch regardless, so Turbo
  has to come off the form. Then `form-action 'self'` becomes the next blocker, because
  Firefox and Safari check it on **every redirect hop** and Chrome does not — which is why
  "Bayar sekarang" must be re-checked in Firefox, not only in the browser that reported it.
- **A poller in `app.js` re-arms on `turbo:load` and stops on `turbo:before-cache`.** It used to
  re-run by being an inline script in a replaced body; loaded once, it needs the events, and
  getting it wrong leaves an interval polling a page the user has left.
- **`view.Render` sets `Cache-Control: private, no-store` when there is a user.** A booking page
  names a customer and carries a token; anonymous pages stay cacheable.
- **Validation primitives live in `service/validate.go`** — `required`, `maxLen`, `requiredMaxLen`,
  `phone`. Rune length, never bytes. Adding a rule there rather than in a sixth validator is what
  keeps a number accepted on the booking form from being rejected on the profile form.
- **Anything user-facing that a browser can only reveal must be checked in a browser.** Phase 12
  found two defects that way, and curl saw neither: a CSP violation is invisible to it, and so is
  a poller that never fires.

## Testing (settled in Phase 13)

Three tiers. Which one a test belongs in is decided by the code, not by taste.

- **Pure unit tests sit beside the code, usually `package <pkg>` rather than `<pkg>_test`.**
  Most of the logic worth testing is unexported *and* its type cannot be built from
  outside without a store — `Schedule.expand`, `Booking.slotMessage`, `validate.go`'s
  primitives, `Limiter`'s injectable clock, `app.safePath`, `handler.csvSafe`. In-package
  they are reachable with a struct literal (`&Schedule{loc: jkt}`,
  `&Settings{values: …}`), which is what keeps them free of MariaDB.
- **Every DB-backed test lives in `internal/integration`,** one package. `testsupport`
  imports `service`, so an in-package `service` test importing the harness is an import
  cycle; one package also means one test database and no clobbering when `go test ./...`
  runs packages in parallel. Test through the exported seam (`Booking.Create`,
  `Payment.ApplyWebhook`) rather than reaching for `applyOnce`.
- **`internal/testsupport` is the harness**: `New(t)` mirrors `main.go`'s boot with a
  `FakeGateway`, a `RecordingSender` and a logger writing to a buffer (`env.Logs()` is how
  "`access_token` appears zero times in the log" became a test). It owns
  `appskep_tpj_test`, drops and recreates it per run, and **refuses any `TEST_DB_NAME` not
  ending `_test`** — it DROPs what it is given.

Rules that are load-bearing:

- **`AssertInvariant` ends every DB-backed test.** `booked_count == COUNT(bookings NOT IN
  ('cancelled','expired'))` and `0 <= booked_count <= capacity`, across every slot. It is
  the check each phase from 1 to 12 made by hand.
- **DB tests skip under `-short`, and `make test` does not pass it,** so a green `make test`
  can never mean the concurrency test quietly did not run.
- **Create fixture data through the application, never by INSERT** — `seed_dev.sql:84`
  says why: a hand-written booking either violates the invariant or needs hand-maintaining.
- **`Reset` deletes settings rows rather than trusting the seed to restore them.** The
  seed's settings INSERT is `ON DUPLICATE KEY UPDATE setting_key = setting_key`, a
  deliberate no-op, so a test that changes a setting would otherwise poison every test
  after it — and the failure lands somewhere unrelated.
- **A losing racer may fail in two legitimate places**: at the row lock (`ErrSlotTaken`) or
  before it, in the non-locking `validate` (a `ValidationError` on `slot`). Assert that it
  is one of those and never anything else — a raw error there is the ER_CHECKREAD
  regression, and it is invisible in the data because the counter stays correct.
- **Never assert "no collisions" on a random value.** 2000 booking codes over 31⁴ expect
  ~2 collisions by the birthday bound; a first draft allowing 3 failed one run in three.
  Assert the scale of the keyspace instead.
- **Never flip the last character of a base64 value to tamper with it.** The final
  character of a `RawURLEncoding` value carries padding bits that decoding ignores, so the
  "tampered" token can decode identically. Decode, flip a byte, re-encode.
- **A test that reads the wall clock must fix it, not sample it.** The reminder-hour test
  chooses a `time.FixedZone` where it is 01:00 rather than picking an hour relative to
  `time.Now()` — the relative version skipped itself on the run that wrote it.
- **`config` tests must clear the whole environment first.** `make` does `include .env` and
  `export`, so the developer's real `DB_PASSWORD` leaks in and the production-secret tests
  passed under `go test` while failing under `make test`.
- **A suite never seen red is not evidence.** Break the code a load-bearing test guards,
  watch it fail, revert. Doing that is what revealed that removing the `RowsAffected()`
  guard before `ReleaseSlot` does *not* fail a capacity-1 test — `GREATEST(count - 1, 0)`
  covers for it — and that exposing it needs a capacity-2 slot with a second live booking.

Anything a browser alone can reveal is in [QA.md](QA.md), not here: a CSP violation is
invisible to curl, and so is a poller that never fires.

## Secrets

`.env` is gitignored and must stay that way. `SESSION_KEY` is a random 32+ byte value
from the environment — never a hardcoded literal. `ENV=production` fails fast when
`AUTH_SECRET`, `SESSION_KEY`, `DB_PASSWORD`, or `MIDTRANS_SERVER_KEY` is empty.
The Midtrans keys currently in `.env` are **sandbox**; production keys must never be
committed.
