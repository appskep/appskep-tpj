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
- [ ] `/layanan/urut-therapeutic` — detail, with the jadwal-terdekat preview and
      the "Ditangani oleh" strip
- [ ] a coming-soon layanan — the CTA is **replaced**, not dimmed or disabled
- [ ] `/terapis` — the grid, with layanan chips on each card
- [ ] `/terapis/budi-santoso` — profile, with "Menangani", "Sertifikasi" and
      "Terapis lain"
- [ ] a therapist with no photo, no bio and no pengalaman — placeholders, and
      **no "0 tahun"** where the field was left blank
- [ ] `/booking` — steps 1, 2 and 3
- [ ] `/booking` review panel (POST without `konfirmasi=1`) — opens as a modal
- [ ] `/pembayaran` — with a live hold
- [ ] `/booking/{code}/konfirmasi` — in each status: pending, paid, cancelled, expired
- [ ] `/riwayat` — with and without bookings, and each status filter
- [ ] `/profil`
- [ ] `/nope` — the 404 keeps public chrome

**Admin**

- [ ] `/admin` — dashboard figures
- [ ] `/admin/layanan` — list, form, image upload
- [ ] `/admin/terapis` — list, form, photo upload, the layanan checkbox group
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
- [ ] **The ringkasan dialog opens by itself** — see *The ringkasan modal* below.
      It is the only dialog with no button to click, so it is the only one the
      delegated listener cannot reach.
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

### The booking calendar (step 2)

Steps 2 and 3 share one frame, `booking_jadwal`. Everything below is a
consequence of that, and curl sees none of it: a fetched document is always
self-consistent, and only a partial swap in a live DOM can be wrong.

- [ ] **A date click swaps in place.** Pick a date on `/booking`: the grid and the
      slot panel change, the page does **not** scroll, and the address bar
      advances to `?tanggal=`. Back returns to the previous date.
- [ ] **A month arrow swaps in place and keeps the selection.** With a date
      chosen, press ›: the grid moves on, the panel still shows the chosen date's
      times, and the URL carries both `bulan` and `tanggal`. A date or slot link
      must **never** carry `bulan` — that would pin the calendar to a month the
      visitor left.
- [ ] **The stale step-3 form is gone.** Pick a slot so step 3 appears, then click
      a **different** date. Step 3 must disappear with it. Before the calendar
      landed, the frame swapped and the form stayed — hidden `slot` and all — so
      the calendar showed one date while the form was about to book another.
- [ ] **"Konfirmasi & lanjut ke pembayaran" still leaves the page.** It 303s out
      of the frame; without `data-turbo-frame="_top"` Turbo finds no
      `booking_jadwal` in `/pembayaran`, empties the frame, and the button appears
      to do nothing. Same for the **Riwayat** link in the already-booked notice,
      and the reason no empty state on this page carries an action link.
- [ ] **"Lihat ringkasan" still answers 200 inside the frame** — `data-turbo="false"`
      has to keep Turbo out of it now that the form sits inside `booking_jadwal`.
- [ ] **The arrows are absent, not dimmed, at the window edges.** First month: no
      ‹. Last bookable month: no ›. The month name must not shift when one goes.
- [ ] **Tab reaches only bookable days.** Past, full and out-of-window cells are
      `<div>`s and must be skipped entirely; the selected day announces
      `aria-current`, and a day link reads as "Kamis, 6 Agustus 2026 · 3 slot
      tersedia" rather than a bare number.
- [ ] **At 390px**: one column, calendar then panel; the seven columns never
      reflow; the "3 slot" chip stays legible.
- [ ] **With JavaScript off**, every date, month and slot link is an ordinary
      navigation that renders the same page.

### The ringkasan modal (step 3 → review)

The panel is a `<dialog>` the **server renders `open`** and `app.js` upgrades to a
real modal. There is no button to click it open — it arrives with the response —
so the delegated `data-dialog` listener cannot reach it, and `script-src 'self'`
leaves no inline script that could. `make test` proves the attribute is there and
nothing more: whether the upgrade happens is browser-only.

- [ ] **It opens by itself, centred, over a dimmed page.** Fill step 3 and press
      "Lihat ringkasan". If it appears as a panel *below the form* instead, the
      `showModal()` upgrade did not run — check the console for a CSP violation or
      an `InvalidStateError`.
- [ ] **Escape closes it. The X closes it. "Ubah data" closes it.** All three are
      the browser's own: `method="dialog"` and the modal's Escape handling. There
      is no JS close path to break.
- [ ] **The form behind is intact and editable after a close** — every field still
      filled, including the map pin. Press "Lihat ringkasan" again: the summary
      shows the *edited* values, because re-opening it costs a real POST that
      re-runs validation. There is deliberately no client-side re-open.
- [ ] **A long summary scrolls inside the dialog, not the page.** Book with a long
      alamat and a long catatan at 390px; the confirm button must stay reachable.
- [ ] **"Konfirmasi & lanjut ke pembayaran"** still lands on `/pembayaran` from
      inside the dialog — it is the `data-turbo-frame="_top"` path above, now one
      level deeper. **Repeat this one in Firefox**, which re-checks `form-action`
      on every redirect hop where Chrome does not.
- [ ] **A 422 shows no dialog.** Submit with the nama blank: the field errors must
      be visible on the form, with nothing covering them.
- [ ] **With JavaScript off**, the summary renders as a plain panel below the form
      and the confirm button still books. This is what the `open` attribute buys;
      without it the panel would be `display: none` and the flow would dead-end.

### The location picker (Phase 13.5)

Every one of these is invisible to `curl` and to `make test`: a permission
prompt, a blocked tile and a map that never initialises all look like a correct
page from the outside. Check on `/booking` step 3 **and** on `/profil` — one
widget, two hosts.

- [ ] **The tiles actually load.** A map with the streets on it, not a grey
      square. A grey square with a console `img-src` refusal means the CSP no
      longer matches `MAP_TILE_URL`; a grey square with 404s in the network tab
      means the tile template's `{s}/{z}/{x}/{y}` placeholders were escaped —
      check the rendered `data-map-tiles` attribute for `%7b`.
- [ ] **The map appears when step 3 arrives inside the frame.** Pick a layanan, a
      date, then a slot. The map must render at that moment, not only on a full
      page load — that path is `turbo:frame-load`, and `turbo:load` never fires
      for it.
- [ ] **Pick three different slots in a row.** Still exactly one map and one
      marker. Two stacked instances mean the `data-map-ready` guard is gone.
- [ ] **Navigate away and come back** (Turbo snapshot restore). The map still
      works, and dragging still updates the hidden inputs.
- [ ] **No permission prompt on load** for a visitor who has never granted it.
      The prompt belongs to the "Deteksi lokasi saya" button and nothing else.
- [ ] **Grant it, then reload.** The pin now appears with no prompt at all.
- [ ] **Deny it.** The status line says so, the map stays usable, and the booking
      still submits — the pin is optional.
- [ ] **Click the map, then drag the pin.** The review panel afterwards must name
      the dragged point, not the detected one.
- [ ] **"Hapus titik" on `/profil`** clears the pin, and saving leaves both
      columns NULL rather than 0,0.
- [ ] **With JavaScript off**, step 3 shows no map, **no dead buttons**, and the
      booking submits and commits normally.
- [ ] **On plain http** (not localhost): the detect button is absent rather than
      present and silent. `navigator.geolocation` never calls back outside a
      secure context.

### Terapis (Phase 13.7)

The list-page toggle and the 422 re-render are the two `curl` cannot see: a
Turbo Stream that silently fell back to a redirect still ends on a correct page,
and a checkbox group that loses its state on a rejected submit looks like the
admin never ticked anything.

- [ ] **The aktif toggle updates its row without a full page reload.** If the
      whole page flashes, the Turbo Stream reply is not being applied and the
      handler fell through to `FlashRedirect`.
- [ ] **Double-click the toggle.** It settles on the state of the last click, not
      alternating — it posts the value it wants, never a flip.
- [ ] **Submit `/admin/terapis/baru` with a blank name but several layanan
      ticked.** A 422 re-renders with the message beside the field **and every
      box still ticked**. The file input is cleared by the browser, which is
      expected; the ticks are not.
- [ ] **Edit a therapist, untick every layanan, save.** The profile page shows no
      "Menangani" section, and `/layanan/{slug}` no longer lists them.
- [ ] **Upload a photo over 2 MB.** A friendly message beside the file input, not
      a 413 error page.
- [ ] **Deactivate a therapist**, then open their `/terapis/{slug}` — a 404 with
      public chrome, and they are gone from the grid and from every layanan page.
- [ ] The nav entry appears in the desktop nav **and in the mobile drawer**, and
      is highlighted on `/terapis/{slug}` as well as `/terapis`.

**With every therapist deactivated** (Phase 13.9). The drawer is the one to
actually open: it is a second `{{range publicNav}}`, and a fix applied to one and
not the other is invisible at desktop width.

- [ ] **Open the mobile drawer at phone width.** No Terapis entry there either —
      and Layanan, Booking and Riwayat are all still present.
- [ ] The footer's "Jelajahi" list has no Terapis link.
- [ ] **Re-activate one from `/admin/terapis`, then reload the public site
      without restarting the server.** The entry is back everywhere. If it takes
      a restart, a `refresh` hook is missing from the write method you used —
      try the edit form with "Aktif" cleared as well as the list toggle, since
      `Update` is a separate path from `SetActive`.
- [ ] `/terapis` still loads (200, empty state) the whole time — hiding the menu
      must not take the route down.
- [ ] `/admin/terapis` is in the admin sidebar throughout. That is how the first
      therapist gets created, so it must never hide.

### The admin booking map (Phase 13.8)

Same class of check as the location picker, and for the same reason — a blocked
tile and a map that never initialises both look like a correct page from outside.

- [ ] **Open a booking that has a pin.** The map renders with the streets on it
      and one marker. Grey square + a console `img-src` refusal means the CSP no
      longer matches `MAP_TILE_URL`; grey square + 404s in the network tab means
      `data-map-tiles` was percent-escaped — check it for `%7b`.
- [ ] **The marker does not drag.** A preview that can be moved is a preview that
      lies about saving.
- [ ] **"Buka di Google Maps"** opens a new tab on the right point.
- [ ] **Open a booking with no pin.** No map, no button, and the rest of the page
      intact.
- [ ] **With JavaScript off**, the coordinates and the button are still there and
      still work. That is the half that matters for dispatch.
- [ ] **Navigate list → detail → back → detail** (Turbo). Still exactly one map.

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
