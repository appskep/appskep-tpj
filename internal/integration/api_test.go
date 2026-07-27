package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/app/api"
	"github.com/remorac/appskep-tpj/internal/shared/payment"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// The webhook's status codes are chosen for what Midtrans does with them, which
// makes them a contract rather than a detail:
//
//	200 — anything we will never accept, so it stops retrying
//	401 — only a bad signature on one of OUR orders
//	500 — only our own failure, because that is what makes it send again
//
// These go through api.Routes rather than the handler, so the absence of CSRF on
// that router is part of what is checked.

func webhook(t *testing.T, env *testsupport.Env, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/webhook/midtrans", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	api.Routes(env.Deps).ServeHTTP(rec, r)
	return rec
}

func TestWebhookStatusCodes(t *testing.T) {
	t.Run("a valid settlement is 200", func(t *testing.T) {
		env := testsupport.New(t)
		p := openPayment(t, env, 8600)

		body := testsupport.Notification(p.orderID, p.amount, testsupport.TestServerKey,
			payment.StatusSettlement, nil)

		rec := webhook(t, env, body, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body)
		}
		if got := env.BookingStatus(p.bookingID); got != "paid" {
			t.Errorf("booking status = %q, want paid", got)
		}
	})

	t.Run("a bad signature is 401", func(t *testing.T) {
		env := testsupport.New(t)
		p := openPayment(t, env, 8601)

		body := testsupport.Notification(p.orderID, p.amount, "SB-Mid-server-attacker",
			payment.StatusSettlement, nil)

		rec := webhook(t, env, body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 — a misconfigured server key has to show "+
				"up as a failure at both ends", rec.Code)
		}
	})

	t.Run("a foreign prefix is 200", func(t *testing.T) {
		env := testsupport.New(t)

		body := testsupport.Notification("ukom-abc", "150000.00",
			testsupport.TestServerKey, payment.StatusSettlement, nil)

		rec := webhook(t, env, body, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 — another Appskep system's notification "+
				"must not be retried at us for hours", rec.Code)
		}
	})

	t.Run("garbage is 200", func(t *testing.T) {
		env := testsupport.New(t)

		rec := webhook(t, env, []byte("not json at all"), nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("an empty body is 200", func(t *testing.T) {
		env := testsupport.New(t)

		// Anything on the internet can send a zero-length POST to an
		// unauthenticated endpoint. Before Phase 13 this was a 500, because the
		// audit insert put NULL into payload NOT NULL — so it would have been
		// retried forever and logged at ERROR each time.
		rec := webhook(t, env, nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("an oversized body is 413", func(t *testing.T) {
		env := testsupport.New(t)

		rec := webhook(t, env, []byte(strings.Repeat("x", (64<<10)+1024)), nil)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})
}

// TestWebhookNeedsNoCSRFToken. The webhook IS a cross-site POST, and its SHA512
// signature is its whole authentication — so the /api router takes neither the
// auth middleware nor d.CSRF.
func TestWebhookNeedsNoCSRFToken(t *testing.T) {
	env := testsupport.New(t)
	p := openPayment(t, env, 8610)

	body := testsupport.Notification(p.orderID, p.amount, testsupport.TestServerKey,
		payment.StatusSettlement, nil)

	// No cookie, no token, and an Origin that would be refused anywhere else.
	rec := webhook(t, env, body, map[string]string{"Origin": "https://api.midtrans.com"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a cookie-less cross-site POST is exactly "+
			"what Midtrans sends", rec.Code)
	}
	if got := env.BookingStatus(p.bookingID); got != "paid" {
		t.Errorf("booking status = %q, want paid", got)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("the webhook set a cookie")
	}
}

// TestWebhookAnswersJSON: the /api router's failures are JSON, not the styled
// HTML error page.
func TestWebhookAnswersJSON(t *testing.T) {
	env := testsupport.New(t)

	rec := webhook(t, env, []byte("garbage"), nil)

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v (%s)", err, rec.Body)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestAPINotFoundAndMethodNotAllowed(t *testing.T) {
	env := testsupport.New(t)

	t.Run("unknown path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		api.Routes(env.Deps).ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/nope", nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want JSON — /api never renders the HTML "+
				"error page", ct)
		}
	})

	t.Run("wrong method on the webhook", func(t *testing.T) {
		rec := httptest.NewRecorder()
		api.Routes(env.Deps).ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/webhook/midtrans", nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})
}

func TestHealth(t *testing.T) {
	env := testsupport.New(t)

	rec := httptest.NewRecorder()
	api.Routes(env.Deps).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if body["status"] != "ok" || body["db"] != "ok" {
		t.Errorf("body = %v, want status and db both ok", body)
	}

	// The health endpoint is ungated: it is checked by a load balancer that has no
	// session.
	if len(rec.Result().Cookies()) != 0 {
		t.Error("the health endpoint set a cookie")
	}
}

// TestWebhookRateLimitIsGenerous. Midtrans retries legitimately, and a dropped
// notification means a customer who paid and is still shown as unpaid — so the
// webhook's budget is far above the booking route's.
func TestWebhookRateLimitIsGenerous(t *testing.T) {
	env := testsupport.New(t)

	if env.Cfg.Server.RateLimitWebhook <= env.Cfg.Server.RateLimitBooking {
		t.Errorf("the webhook limit (%d) is not above the booking limit (%d)",
			env.Cfg.Server.RateLimitWebhook, env.Cfg.Server.RateLimitBooking)
	}

	// Well inside the budget, and every one is answered.
	for i := range 30 {
		rec := webhook(t, env, []byte("garbage"), nil)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate-limited inside the webhook budget of %d",
				i+1, env.Cfg.Server.RateLimitWebhook)
		}
	}
}
