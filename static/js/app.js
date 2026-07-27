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
// the click handler is delegated from document, and the poller re-arms on
// turbo:load.
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

  // turbo:load fires after the initial page load as well as after every Turbo
  // navigation, so this covers both. turbo:before-cache stops the interval before
  // Turbo snapshots the page, so a cached preview never leaves a timer running.
  document.addEventListener("turbo:load", start);
  document.addEventListener("turbo:before-cache", stop);
})();
