# QA.md — the manual pass

Everything an automated test can check lives in `make test`. This file is the
remainder: **what only a browser can reveal.**

Phase 12 stated the reason plainly — *"a CSP violation is invisible to curl, and
so is a poller that never fires."* Phase 7 found the same class of thing: Turbo
silently discards a non-redirect 200 after a form POST, so the button appears to
do nothing, and **curl cannot see it** because curl is the no-JS path, which
always worked.

Run this before a release, and after any change to a layout, to `static/js/app.js`,
to the CSP in `internal/shared/middleware/secure.go`, or to a form's success path.

## Setup

```
make db-fresh          # a known starting state
make dev               # tailwind + air
```

Sign in at `/login` (Appskep SSO). Keep the browser console open for the whole
pass: **every check below assumes zero console errors**, and a CSP violation
appears there and nowhere else.

Widths to check: **390** (phone) and **1280** (desktop). Both layouts.

---

## 1. Every page renders

Tick each at both widths. For each: no console error, no horizontal scroll, and
no literal `ZgotmplZ` anywhere on the page (that string is html/template refusing
to escape something, and it renders visibly).

**Public**

- [ ] `/` — landing
- [ ] `/layanan` — the three layanan
- [ ] `/layanan/urut-therapeutic` — detail, with the jadwal-terdekat preview
- [ ] a coming-soon layanan — the CTA is **replaced**, not dimmed or disabled
- [ ] `/booking` — steps 1, 2 and 3
- [ ] `/booking` review panel (POST without `konfirmasi=1`)
- [ ] `/pembayaran` — with a live hold
- [ ] `/booking/{code}/konfirmasi` — in each status: pending, paid, cancelled, expired
- [ ] `/riwayat` — with and without bookings, and each status filter
- [ ] `/profil`
- [ ] `/nope` — the 404 keeps public chrome

**Admin**

- [ ] `/admin` — dashboard figures
- [ ] `/admin/layanan` — list, form, image upload
- [ ] `/admin/jadwal?view=kalender` and `?view=daftar`
- [ ] `/admin/jadwal/generate` — preview then commit
- [ ] `/admin/booking` — list, detail, each of the five actions
- [ ] `/admin/pembayaran` — list, detail, re-sync
- [ ] `/admin/users`
- [ ] `/admin/pengaturan`
- [ ] `/admin/nope` — the 404 keeps **admin** chrome

## 2. The browser-only checks

These are the ones this file exists for.

- [ ] **Zero CSP violations across every page above.** They appear only in the
      console. `script-src 'self'` admits no inline script at all, and
      `style-src` admits exactly one hash — Turbo's progress-bar stylesheet.
- [ ] **Turbo's progress bar appears** on a slow navigation. If it does not, the
      `style-src` hash has gone stale: re-derive it after `make js` bumps Turbo.
      The warning is in `scripts/build-js.sh`, where someone would do it.
- [ ] **The payment poller fires.** On `/pembayaran`, watch the network tab: the
      frame is re-fetched every few seconds while `data-poll="true"`.
- [ ] **And stops.** Navigate away. The requests must stop. A poller that keeps
      running is an interval on a page the user has left.
- [ ] **A POST that answers 200 actually does something.** Turbo requires a form
      submission to end in a redirect and silently discards a non-redirect 200.
      Check the two two-step previews: the jadwal generator and the bulk
      date-range delete. Both need `data-turbo="false"`.
- [ ] **Both confirm dialogs open** — the layanan delete and the jadwal bulk
      delete. They are `data-dialog` attributes read by one delegated listener;
      an inline `onclick` would be blocked by the CSP.
- [x] **The mobile drawers toggle** at 390 in both layouts — both slide in from
      the left, under their hamburger. They are CSS-only (`peer-checked:`), so the
      checkbox must be a *sibling* of everything it drives. Close each three ways:
      the `x`, the scrim, and `Esc` (the last is the one enhancement in `app.js`,
      keyed off `[data-drawer]`). Tab from the hamburger while **closed** must not
      land inside the public drawer — that is its `invisible peer-checked:visible`.
      Resize past the breakpoint with one open: drawer and scrim both vanish.
      Two things this found, both invisible outside a browser: the hide-past-the-
      breakpoint class needs Tailwind v4's **trailing** `!` (`md:hidden!` — the v3
      leading form compiles to nothing, silently, and `peer-checked:block` outranks
      a plain `md:hidden` on specificity); and the icon inside each toggle needs
      `pointer-events-none`, or a tap lands on the sprite's `<use>` and WebKit
      never activates the label.
- [ ] **Tap the hamburger in Safari and on a real iPhone**, not only in Chrome.
      The `<label>`-over-SVG hit target above behaves differently there, and that
      is the one difference a headless Chrome run cannot see.
- [ ] **Print** `/booking/{code}/konfirmasi`. Header and footer collapse
      (`print:hidden`); the booking code stays.
- [ ] **A Turbo Stream toggle updates in place** — the layanan active toggle. The
      row changes without a full page load.
- [ ] **A 422 re-render keeps every typed value.** Submit the booking form with a
      bad phone number: the message appears and nothing entered is lost.

## 3. The payment path

Sandbox only. `MIDTRANS_ENV` must not be `midtrans.Production`.

- [ ] Book → **press "Bayar sekarang"** → Snap's hosted page opens (a server-side
      303, not `snap.js`). The console must be clean: this button reached Midtrans by
      a Turbo `fetch()` once, which fails cross-origin on CORS and `connect-src`.
      **Repeat in Firefox** — `form-action` is checked on every redirect hop there
      and not in Chrome, so Chrome alone cannot clear this step.
- [ ] **The Snap page lists QRIS, GoPay and ShopeePay — and nothing else.**
      That is `MIDTRANS_ENABLED_PAYMENTS=other_qris,gopay,shopeepay`, sent per
      transaction. A **fourth** channel means the field never reached Midtrans and
      the page is showing the shared account's own list. A **missing** one means
      that channel is not active on the shared merchant account — confirm with
      `MIDTRANS_ENABLED_PAYMENTS=all`, then drop it from the list rather than
      shipping a picker entry that fails. An **empty** page is the `qris` /
      `other_qris` mistake: Snap drops a name it does not know without complaining.
      curl sees a 303 in every one of these cases and can clear none of them.
- [ ] **One channel still skips the picker.** Set
      `MIDTRANS_ENABLED_PAYMENTS=other_qris`, restart, and "Bayar sekarang" must
      land straight on the QR and its countdown with nothing to click. This is the
      supported route back to a QRIS-only checkout; put the three-channel value
      back afterwards.
- [ ] Complete the sandbox payment → the finish callback lands on
      `/booking/{code}/konfirmasi`.
- [ ] The status reaches `paid`. On a machine Midtrans cannot call back into, use
      the admin re-sync — it reaches the same state machine.
- [ ] The confirmation email arrives (or, with SMTP unconfigured, one INFO line
      at startup says email is disabled and a DEBUG line names each skipped mail).

## 4. Before finishing

- [ ] `make vet` clean.
- [ ] `make test` green.
- [ ] `make vulncheck` — expect zero once the toolchain is go1.25.12+
      (PLAN.md Phase 14).
- [ ] The log carries **zero** occurrences of `access_token`, the session key, the
      database password and the Midtrans server key.
- [ ] `SIGTERM` shuts down cleanly: the tickers stop, the email queue drains, and
      the process exits without an error.
- [ ] The `booked_count` invariant holds. `make test` asserts it after every
      DB-backed test; against a database you have been clicking around in:

      ```sql
      SELECT s.id, s.capacity, s.booked_count,
             (SELECT COUNT(*) FROM bookings b
               WHERE b.slot_id = s.id
                 AND b.status NOT IN ('cancelled','expired')) AS holding
      FROM schedule_slots s
      HAVING booked_count <> holding OR booked_count < 0 OR booked_count > capacity;
      ```

      Zero rows.
