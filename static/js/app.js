// The application's own JavaScript, and all of it.
//
// It exists because Phase 12's Content-Security-Policy has no 'unsafe-inline' in
// script-src: an inline <script> and an onclick= attribute are both refused by
// the browser, so the four that used to live in templates moved here. Everything
// below is progressive enhancement — every control it touches still works with
// this file blocked, because the dialogs degrade to their form and the payment
// panel is rendered server-side before any of this runs.
//
// Loaded once, deferred, after turbo.js. Turbo Drive replaces <body> on every
// navigation, so nothing here may hold a reference to an element across visits:
// the click handler is delegated from document, the poller re-arms on
// turbo:load, and the maps are torn down on turbo:before-cache and rebuilt on
// turbo:load and turbo:frame-load.
(function () {
  "use strict";

  // Dialog triggers. A button carrying data-dialog="<id>" opens that <dialog>.
  //
  // Delegated from document rather than bound per button, because a per-element
  // listener would be lost with the body Turbo swapped it out of, and rebinding
  // on every visit is a leak waiting to happen.
  document.addEventListener("click", function (event) {
    var trigger = event.target.closest("[data-dialog]");
    if (trigger) {
      var dialog = document.getElementById(trigger.dataset.dialog);
      if (dialog && typeof dialog.showModal === "function") {
        dialog.showModal();
      }
      return;
    }

    if (event.target.closest("[data-action='print']")) {
      window.print();
    }
  });

  // Dialogs the server renders already open.
  //
  // The booking ringkasan has no trigger to click: it arrives with the response
  // to "Lihat ringkasan", so the delegated handler above can never reach it and
  // there is no inline script to call showModal() — script-src is 'self' with no
  // nonce. It is therefore rendered with the `open` attribute, which is what the
  // no-JS path sees (a plain panel in flow, styled by the not-modal: variants),
  // and upgraded here to a real modal so that the backdrop, the focus trap and
  // Escape come from the browser.
  function openDialogs() {
    var pending = document.querySelectorAll("dialog[data-dialog-open]");
    for (var i = 0; i < pending.length; i++) {
      var dialog = pending[i];
      if (typeof dialog.showModal !== "function" || dialog.matches(":modal")) {
        // Already upgraded — turbo:load and turbo:frame-load can both fire for
        // one arrival, and showModal() on a modal dialog throws.
        continue;
      }
      // showModal() also throws InvalidStateError on a dialog that is open but
      // NOT modal, which is exactly how the server rendered this one.
      if (dialog.open) {
        dialog.close();
      }
      dialog.showModal();
    }
  }

  // showModal() sets the open attribute, so without this Turbo would snapshot a
  // dialog mid-modal and restore it as a stray panel. Same discipline as stop()
  // and destroyMaps(): leave nothing live in a cached page.
  function closeDialogs() {
    var open = document.querySelectorAll("dialog[data-dialog-open][open]");
    for (var i = 0; i < open.length; i++) {
      open[i].close();
    }
  }

  // Escape closes an open drawer — the public mobile menu and the admin sidebar,
  // both of which are checkbox-driven so that they work with this file blocked.
  //
  // This is the one thing the checkbox pattern cannot express in CSS, and it is
  // pure enhancement: the scrim and the close button still shut the drawer
  // without it. Generic over [data-drawer] rather than naming the two ids, so a
  // third drawer needs the attribute and nothing here.
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape") {
      return;
    }
    var open = document.querySelectorAll("input[data-drawer]:checked");
    for (var i = 0; i < open.length; i++) {
      open[i].checked = false;
    }
  });

  // The payment status poller.
  //
  // It lives in the page rather than inside the frame it reloads: Turbo replaces
  // a frame's contents with parsed markup, and a <script> inserted that way never
  // executes, so a poller shipped inside the fragment would run once and vanish
  // with the content it arrived in.
  //
  // Whether to keep going stays the server's decision. Each rendered fragment
  // carries data-poll, so the loop asks the page it was just given rather than
  // holding its own idea of the booking's state, and a booking that reaches a
  // final state stops the polling on the very next tick.
  var timer = null;

  function stop() {
    if (timer) {
      clearInterval(timer);
      timer = null;
    }
  }

  function start() {
    // Always clear first: turbo:load fires on every visit, and without this a
    // second visit to the payment page would stack a second interval on the
    // first.
    stop();

    var frame = document.getElementById("payment_status");
    if (!frame) {
      return;
    }

    timer = setInterval(function () {
      var live = document.getElementById("payment_status");
      if (!live || !live.querySelector('[data-poll="true"]')) {
        stop();
        return;
      }
      live.reload();
    }, 5000);
  }

  // The location picker.
  //
  // A [data-map] element becomes a Leaflet map with one draggable marker, whose
  // position is mirrored into two hidden inputs the form submits. Everything
  // about it is enhancement: the pin is optional server-side, the detect button
  // ships hidden and is only revealed below, and with this file blocked the
  // booking form is exactly what it was before — a required address textarea.
  //
  // Leaflet is loaded on demand rather than by a <script> tag in the layout. The
  // landing page does not need 145K of map library, and a body script would be
  // re-evaluated by Turbo on every visit with no ordering guarantee against this
  // file's turbo:load handler — which would see `L` undefined some of the time
  // and work the rest, the worst kind of bug to be handed.

  var LEAFLET_SRC = "/static/js/leaflet.js";
  var maps = [];
  var leafletPending = null;

  // ensureLeaflet runs cb once the library is available, loading it at most once
  // however many widgets ask for it.
  function ensureLeaflet(cb) {
    if (window.L) {
      cb();
      return;
    }
    if (leafletPending) {
      leafletPending.push(cb);
      return;
    }
    leafletPending = [cb];

    var script = document.createElement("script");
    script.src = LEAFLET_SRC;
    script.addEventListener("load", function () {
      var waiting = leafletPending;
      leafletPending = null;
      for (var i = 0; i < waiting.length; i++) {
        waiting[i]();
      }
    });
    script.addEventListener("error", function () {
      // The map is the only thing lost. Leave the form as the server rendered it
      // rather than showing a control that cannot work.
      leafletPending = null;
    });
    // Not inline, so script-src 'self' admits it — the CSP needs no exception.
    document.head.appendChild(script);
  }

  function setStatus(entry, message) {
    if (entry.status) {
      entry.status.textContent = message;
    }
  }

  // write mirrors the marker's position into the hidden inputs, rounded to the
  // seven decimal places the DECIMAL(9,7)/DECIMAL(10,7) columns hold. The server
  // normalises to the same precision, so what is submitted is what is stored.
  function write(entry, latlng) {
    entry.latInput.value = latlng.lat.toFixed(7);
    entry.lngInput.value = latlng.lng.toFixed(7);
    if (entry.clear) {
      entry.clear.hidden = false;
    }
  }

  function place(entry, lat, lng, zoom) {
    var latlng = window.L.latLng(lat, lng);
    if (entry.marker) {
      entry.marker.setLatLng(latlng);
    } else {
      entry.marker = window.L.marker(latlng, {
        draggable: true,
        icon: entry.icon,
        keyboard: true,
        alt: "Titik lokasi terapi"
      }).addTo(entry.map);
      entry.marker.on("dragend", function () {
        write(entry, entry.marker.getLatLng());
        setStatus(entry, "Pin dipindahkan. Titik ini yang dikirim ke terapis.");
      });
    }
    entry.map.setView(latlng, zoom || entry.map.getZoom());
    write(entry, latlng);
  }

  function clearPin(entry) {
    if (entry.marker) {
      entry.map.removeLayer(entry.marker);
      entry.marker = null;
    }
    entry.latInput.value = "";
    entry.lngInput.value = "";
    if (entry.clear) {
      entry.clear.hidden = true;
    }
    setStatus(entry, "Titik lokasi dihapus. Klik peta untuk menandai lagi.");
  }

  // locate asks the browser where the user is.
  //
  // `silent` is the auto-detect path, taken only when permission is ALREADY
  // granted: it must never be the thing that raises the permission dialog, since
  // that would land on step 1 of the booking form before the customer has any
  // idea why they are being asked.
  function locate(entry, silent) {
    if (!navigator.geolocation || !window.isSecureContext) {
      return;
    }
    if (!silent) {
      setStatus(entry, "Mencari lokasi Anda…");
    }

    navigator.geolocation.getCurrentPosition(
      function (position) {
        place(entry, position.coords.latitude, position.coords.longitude, 16);
        setStatus(entry, "Lokasi terdeteksi. Geser pin kalau belum tepat.");
      },
      function (error) {
        if (silent) {
          return;
        }
        if (error && error.code === 1) {
          setStatus(entry, "Izin lokasi ditolak. Klik peta untuk menandai sendiri.");
          return;
        }
        setStatus(entry, "Lokasi tidak terdeteksi. Klik peta untuk menandai sendiri.");
      },
      { enableHighAccuracy: true, timeout: 10000, maximumAge: silent ? 300000 : 0 }
    );
  }

  // autoDetect locates without prompting, and only when the browser says the
  // permission is already granted. A browser with no Permissions API does
  // nothing here and leaves the button to ask — the conservative half of the
  // choice, since the alternative is prompting someone who never agreed to it.
  function autoDetect(entry) {
    if (!navigator.permissions || !navigator.permissions.query) {
      return;
    }
    try {
      navigator.permissions.query({ name: "geolocation" }).then(function (result) {
        if (result.state === "granted") {
          locate(entry, true);
        }
      }, function () {});
    } catch (e) {
      // Firefox used to throw on an unsupported descriptor name. Nothing to do.
    }
  }

  function build(el) {
    var latInput = el.querySelector("[data-map-lat-input]");
    var lngInput = el.querySelector("[data-map-lng-input]");
    var canvas = el.querySelector("[data-map-canvas]");
    if (!latInput || !lngInput || !canvas) {
      return;
    }

    var zoom = parseInt(el.dataset.mapZoom, 10) || 13;
    var entry = {
      el: el,
      map: null,
      marker: null,
      latInput: latInput,
      lngInput: lngInput,
      status: el.querySelector("[data-map-status]"),
      clear: el.querySelector("[data-map-clear]"),
      // The icon paths are given explicitly rather than left to Leaflet's
      // default, which guesses them by looking for its own stylesheet in the
      // document — and there is no leaflet.css to find, because it is compiled
      // into app.css.
      icon: window.L.icon({
        iconUrl: "/static/css/images/marker-icon.png",
        iconRetinaUrl: "/static/css/images/marker-icon-2x.png",
        shadowUrl: "/static/css/images/marker-shadow.png",
        iconSize: [25, 41],
        iconAnchor: [12, 41],
        shadowSize: [41, 41]
      })
    };

    entry.map = window.L.map(canvas, {
      // The form scrolls; a wheel that zooms the map instead of the page is the
      // single most complained-about behaviour of an embedded map. Dragging and
      // the +/- control still zoom.
      scrollWheelZoom: false
    });

    // dataset.mapTiles, from data-map-tiles. The attribute is deliberately not
    // called data-map-tile-url: html/template URL-normalises anything ending in
    // "url" and would percent-escape the {s}/{z}/{x}/{y} placeholders.
    window.L.tileLayer(el.dataset.mapTiles, {
      attribution: el.dataset.mapTileAttribution,
      maxZoom: 19
    }).addTo(entry.map);

    // Clicking the map is the other half of "choose a location": dragging only
    // works once a pin exists, and a first-time user has none.
    entry.map.on("click", function (event) {
      place(entry, event.latlng.lat, event.latlng.lng);
      setStatus(entry, "Titik dipilih. Geser pin kalau belum tepat.");
    });

    var hasPin = latInput.value !== "" && lngInput.value !== "";
    if (hasPin) {
      place(entry, parseFloat(latInput.value), parseFloat(lngInput.value), 16);
      setStatus(entry, "Titik lokasi tersimpan. Geser pin kalau perlu.");
    } else {
      // No pin: open on the configured service area rather than at 0°,0°, and
      // drop no marker — an unset location must not look like a set one.
      entry.map.setView(
        [parseFloat(el.dataset.mapDefaultLat), parseFloat(el.dataset.mapDefaultLng)],
        zoom
      );
      setStatus(entry, "Klik peta untuk menandai lokasi, atau tekan Deteksi lokasi saya.");
    }

    // Leaflet measures the container when it is created. Step 3 arrives inside a
    // Turbo frame, and the browser has not always finished laying it out by then,
    // which leaves the map rendered into the wrong size with grey bands at the
    // edges. One re-measure on the next frame costs nothing and fixes it.
    requestAnimationFrame(function () {
      entry.map.invalidateSize();
    });

    var detect = el.querySelector("[data-map-detect]");
    if (detect) {
      // Revealed only now: with this file blocked the button would be a control
      // that does nothing, which the same rule as the coming-soon CTA forbids.
      // Hidden entirely where geolocation cannot work at all — an http:// staging
      // host, where the API simply never calls back.
      if (navigator.geolocation && window.isSecureContext) {
        detect.hidden = false;
        detect.addEventListener("click", function () {
          locate(entry, false);
        });
      } else if (!hasPin) {
        setStatus(entry, "Klik peta untuk menandai lokasi terapi.");
      }
    }

    if (entry.clear) {
      entry.clear.hidden = !hasPin;
      entry.clear.addEventListener("click", function () {
        clearPin(entry);
      });
    }

    maps.push(entry);
    el.dataset.mapReady = "1";

    if (!hasPin) {
      autoDetect(entry);
    }
  }

  function initMaps() {
    var widgets = document.querySelectorAll("[data-map]:not([data-map-ready])");
    if (!widgets.length) {
      return;
    }
    ensureLeaflet(function () {
      // Re-queried rather than closed over: the library load is asynchronous, and
      // a frame may have been replaced in the meantime, which would leave this
      // building a map into an element no longer in the document.
      var live = document.querySelectorAll("[data-map]:not([data-map-ready])");
      for (var i = 0; i < live.length; i++) {
        build(live[i]);
      }
    });
  }

  function destroyMaps() {
    for (var i = 0; i < maps.length; i++) {
      maps[i].map.remove();
      delete maps[i].el.dataset.mapReady;
    }
    maps = [];
  }

  // turbo:load fires after the initial page load as well as after every Turbo
  // navigation, so this covers both. turbo:before-cache stops the interval before
  // Turbo snapshots the page, so a cached preview never leaves a timer running.
  //
  // turbo:frame-load is what step 3 of the booking form depends on. Choosing a
  // slot replaces the contents of the booking_jadwal frame, and a FRAME
  // navigation fires no turbo:load at all — so without this listener the map
  // would never appear, since step 3 is only ever reached through that frame.
  // The [data-map-ready] guard is what keeps a second slot pick from stacking a
  // second map on the same element, and destroyMaps clears the flag so a
  // restored Turbo snapshot rebuilds rather than showing dead markup.
  //
  // openDialogs rides on turbo:load for the same reason: the review panel is a
  // native full-page load (the step-3 form is data-turbo="false"), and Turbo
  // dispatches turbo:load on the initial load as well as on its own navigations.
  // turbo:frame-load is there for symmetry only — a frame navigation on /booking
  // is always a GET, which renders Review=false and no dialog at all.
  document.addEventListener("turbo:load", start);
  document.addEventListener("turbo:load", initMaps);
  document.addEventListener("turbo:load", openDialogs);
  document.addEventListener("turbo:frame-load", initMaps);
  document.addEventListener("turbo:frame-load", openDialogs);
  document.addEventListener("turbo:before-cache", stop);
  document.addEventListener("turbo:before-cache", destroyMaps);
  document.addEventListener("turbo:before-cache", closeDialogs);
})();
