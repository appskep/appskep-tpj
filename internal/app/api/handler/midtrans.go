package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Midtrans serves the payment notification webhook.
//
// This endpoint is public and has to be: Midtrans holds no credential of ours,
// so the SHA512 signature over (order_id + status_code + gross_amount +
// server_key) is the entire authentication (PLAN.md R6). It carries no session,
// no auth middleware, and — when Phase 12 adds CSRF — must be exempt from it,
// because a cross-site request is precisely what a webhook is.
//
// It is also the only path in the system that may mark a booking paid (R7).
//
// The status codes below are chosen for what Midtrans does with them, not for
// what they say about us. Midtrans retries a non-2xx for hours, so anything we
// will never be able to accept — a foreign order ID, a body that is not JSON —
// answers 200 and is dealt with in the audit log. Only a genuine failure on our
// side asks to be retried.
type Midtrans struct {
	deps *app.Deps
}

func NewMidtrans(deps *app.Deps) *Midtrans {
	return &Midtrans{deps: deps}
}

// maxNotificationBytes caps the request body. A Midtrans notification is a few
// hundred bytes; the limit is what stops an unauthenticated public endpoint from
// being handed a stream.
const maxNotificationBytes = 64 << 10

func (h *Midtrans) Notify(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNotificationBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			util.JSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
			return
		}
		util.JSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}

	res, err := h.deps.Payment.ApplyWebhook(r.Context(), body, r.RemoteAddr)
	if err != nil {
		// Our side failed — a locked row, a dropped connection. 500 is the right
		// answer because it is the one that makes Midtrans send this again.
		h.deps.Log.ErrorContext(r.Context(), "midtrans webhook failed", slog.Any("error", err))
		util.JSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if res.SignatureRejected {
		// Recorded, and nothing else. 401 rather than 200 so a misconfigured
		// server key shows up as a failure at both ends instead of silently
		// swallowing every payment.
		util.JSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	h.deps.Log.InfoContext(r.Context(), "midtrans notification applied", slog.String("note", res.Note))
	util.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
