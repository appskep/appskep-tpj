package payment

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/midtrans/midtrans-go"

	"github.com/remorac/appskep-tpj/internal/shared/config"
)

// In-package: httpClient, itemName, expiryMinutes and wrap are all unexported,
// and the first of them is the reason this package replaces the SDK's transport
// at all.

func TestNewMidtransFailsSafe(t *testing.T) {
	// PLAN.md R4: the literal "midtrans.Production" selects production and
	// anything else — including a typo — is sandbox. Failing safe matters more than
	// failing loudly here, because the error case is charging real cards from a
	// staging box.
	tests := []struct {
		env  string
		want midtrans.EnvironmentType
	}{
		{env: "midtrans.Production", want: midtrans.Production},
		{env: "midtrans.Sandbox", want: midtrans.Sandbox},
		{env: "", want: midtrans.Sandbox},
		{env: "production", want: midtrans.Sandbox},
		{env: "Midtrans.Production", want: midtrans.Sandbox},
		{env: "midtrans.production", want: midtrans.Sandbox},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			m := NewMidtrans(config.MidtransConfig{Env: tc.env, ServerKey: "k", Timeout: time.Second})
			if m.env != tc.want {
				t.Errorf("MIDTRANS_ENV=%q selected %v, want %v", tc.env, m.env, tc.want)
			}
		})
	}
}

// TestSnapRequestEnabledPayments asserts the wire body, not the struct: the
// field is omitempty, so "the slice is empty" and "the key is absent" are the
// same thing to Midtrans and only the JSON says which happened.
//
// Sending the channels per transaction is the only lever available — the
// merchant account is shared, and its account-wide channel list belongs to
// another Appskep system.
func TestSnapRequestEnabledPayments(t *testing.T) {
	order := Order{
		ID:            "tpj-11111111-2222-3333-4444-555555555555",
		GrossAmount:   75000,
		ItemID:        "1",
		ItemName:      "Terapi Lutut",
		CustomerName:  "Ari",
		CustomerEmail: "ari@example.com",
	}

	t.Run("other_qris alone", func(t *testing.T) {
		// One channel is what makes Snap skip its method picker and open the QR
		// page directly. Two would restore the picker, which is why the count
		// matters as much as the value — and why this is an opt-in rather than the
		// default, which carries the e-wallet deeplinks and so shows a picker.
		//
		// The value matters just as much: Snap's generic QRIS channel is
		// "other_qris". Plain "qris" is a Core API payment_type, and Snap drops an
		// unrecognised name instead of rejecting the transaction — the token and
		// the redirect still come back, and the customer lands on "Metode
		// pembayaran tidak tersedia". config rejects "qris" at boot for this
		// reason; the assertion here is the wire half of the same guard.
		m := NewMidtrans(config.MidtransConfig{
			ServerKey:       "k",
			Timeout:         time.Second,
			EnabledPayments: []string{"other_qris"},
		})

		body, err := json.Marshal(m.snapRequest(order))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got, want := string(body), `"enabled_payments":["other_qris"]`; !strings.Contains(got, want) {
			t.Errorf("snap request = %s\nwant it to contain %s", got, want)
		}
	})

	t.Run("several channels keep their order", func(t *testing.T) {
		// This list is the shipped default. Order is the order Snap lists them in.
		m := NewMidtrans(config.MidtransConfig{
			ServerKey:       "k",
			Timeout:         time.Second,
			EnabledPayments: []string{"other_qris", "gopay", "shopeepay"},
		})

		body, err := json.Marshal(m.snapRequest(order))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `"enabled_payments":["other_qris","gopay","shopeepay"]`
		if got := string(body); !strings.Contains(got, want) {
			t.Errorf("snap request = %s\nwant it to contain %s", got, want)
		}
	})

	t.Run("none omits the field", func(t *testing.T) {
		// MIDTRANS_ENABLED_PAYMENTS=all. An empty array would mean "no channel at
		// all" to Midtrans; the key has to be absent for the account's own list to
		// apply.
		m := NewMidtrans(config.MidtransConfig{ServerKey: "k", Timeout: time.Second})

		body, err := json.Marshal(m.snapRequest(order))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(body), "enabled_payments") {
			t.Errorf("snap request = %s\nwant no enabled_payments key at all", body)
		}
	})
}

// TestSnapRequestKeepsItsOtherFields guards the extraction of snapRequest out of
// CreateTransaction: everything the Snap page shows is decided here, and the
// only other place to read it is Midtrans' hosted page.
func TestSnapRequestKeepsItsOtherFields(t *testing.T) {
	m := NewMidtrans(config.MidtransConfig{ServerKey: "k", Timeout: time.Second})

	req := m.snapRequest(Order{
		ID:          "tpj-abc",
		GrossAmount: 75000,
		ItemID:      "1",
		ItemName:    "Terapi Lutut",
		FinishURL:   "https://tpj.test/booking/TPJ-1/konfirmasi",
		Expiry:      90 * time.Second,
	})

	if req.TransactionDetails.OrderID != "tpj-abc" || req.TransactionDetails.GrossAmt != 75000 {
		t.Errorf("transaction_details = %+v", req.TransactionDetails)
	}
	// Exactly one item priced at the gross amount: Midtrans rejects the request
	// when the item prices do not sum to gross_amount.
	if req.Items == nil || len(*req.Items) != 1 || (*req.Items)[0].Price != 75000 {
		t.Errorf("item_details = %+v", req.Items)
	}
	if req.Callbacks == nil || req.Callbacks.Finish == "" {
		t.Error("callbacks.finish is not set; Snap would use the shared account's redirect")
	}
	// Truncated, never rounded up: Midtrans must stop accepting payment before
	// the ticker releases the slot.
	if req.Expiry == nil || req.Expiry.Duration != 1 || req.Expiry.Unit != "minute" {
		t.Errorf("expiry = %+v, want 1 minute", req.Expiry)
	}
}

func TestItemName(t *testing.T) {
	// Midtrans rejects an item_details name over 50 characters outright, so a
	// layanan renamed to something long would break checkout rather than merely
	// look untidy.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "short name untouched", in: "Urut Therapeutic", want: "Urut Therapeutic"},
		{name: "exactly at the limit", in: strings.Repeat("a", 50), want: strings.Repeat("a", 50)},
		{
			name: "one over is truncated with an ellipsis",
			in:   strings.Repeat("a", 51),
			want: strings.Repeat("a", 49) + "…",
		},
		{
			name: "trailing space is trimmed before the ellipsis",
			in:   strings.Repeat("a", 48) + " bbbb",
			want: strings.Repeat("a", 48) + "…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := itemName(tc.in)
			if got != tc.want {
				t.Errorf("itemName(%d chars) = %q, want %q", len([]rune(tc.in)), got, tc.want)
			}
			if len([]rune(got)) > maxItemNameLen {
				t.Errorf("itemName produced %d runes, over Midtrans' limit of %d",
					len([]rune(got)), maxItemNameLen)
			}
		})
	}
}

// TestItemNameCountsRunes: a byte-based truncation would split a multi-byte
// character and send Midtrans invalid UTF-8.
func TestItemNameCountsRunes(t *testing.T) {
	in := strings.Repeat("é", 60)
	got := itemName(in)

	if len([]rune(got)) > maxItemNameLen {
		t.Errorf("itemName produced %d runes, want at most %d", len([]rune(got)), maxItemNameLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("itemName(%q...) = %q, want a truncation marker", in[:10], got)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("itemName split a multi-byte character")
	}
}

func TestExpiryMinutes(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want int64
	}{
		// Zero disables the expiry block, which the caller only wants when it
		// genuinely has no deadline to impose. Anything non-positive means the hold
		// has already run out.
		{name: "zero", in: 0, want: 0},
		{name: "negative", in: -time.Hour, want: 0},
		{name: "under a minute rounds down to nothing", in: 30 * time.Second, want: 0},

		{name: "one minute", in: time.Minute, want: 1},
		{name: "the default hold", in: 60 * time.Minute, want: 60},
		{name: "truncates rather than rounds", in: 90*time.Second + 59*time.Second, want: 2},

		// The largest value a time.Duration can hold — an int64 of nanoseconds,
		// about 292 years. See TestExpiryMinutesSaturationIsUnreachable below.
		{name: "the largest possible duration", in: time.Duration(math.MaxInt64), want: 153722867},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := expiryMinutes(tc.in); got != tc.want {
				t.Errorf("expiryMinutes(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpiryMinutesSaturationIsUnreachable records that the MaxInt32 clamp in
// expiryMinutes cannot fire.
//
// A time.Duration is an int64 of nanoseconds, so its largest value is about 292
// years — 153,722,867 minutes, an order of magnitude below MaxInt32. The clamp is
// therefore dead code rather than a live guard. It is harmless and worth keeping
// (the field it feeds is an int64 and the arithmetic is not obviously bounded on
// sight), but nothing should be built on the belief that it is reachable, and a
// future change that made minutes come from somewhere other than a Duration would
// want to re-check it. This test fails if that ever becomes possible.
func TestExpiryMinutesSaturationIsUnreachable(t *testing.T) {
	const maxDurationMinutes = int64(math.MaxInt64) / int64(time.Minute)

	if maxDurationMinutes > math.MaxInt32 {
		t.Fatalf("a time.Duration can now reach %d minutes, past MaxInt32 — "+
			"the clamp in expiryMinutes is live and needs a real test", maxDurationMinutes)
	}
	if got := expiryMinutes(time.Duration(math.MaxInt64)); got != maxDurationMinutes {
		t.Errorf("expiryMinutes(max) = %d, want %d", got, maxDurationMinutes)
	}
}

// TestWrapAvoidsTheTypedNilTrap. The SDK returns a concrete *midtrans.Error, so
// assigning one to an error-typed variable makes `err != nil` true even when the
// pointer is nil. Every call site checks the concrete pointer and then calls
// wrap, and wrap must return a genuinely nil error for a nil input.
func TestWrapAvoidsTheTypedNilTrap(t *testing.T) {
	if err := wrap("charging", nil); err != nil {
		t.Errorf("wrap(nil) = %v, want a nil error", err)
	}

	err := wrap("charging", &midtrans.Error{Message: "boom", StatusCode: 500})
	if err == nil {
		t.Fatal("wrap of a real error returned nil")
	}
	for _, want := range []string{"charging", "boom", "500"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("wrap message %q does not mention %q", err.Error(), want)
		}
	}
}

// TestHTTPClientHonoursContext is the whole reason this package replaces the
// SDK's transport.
//
// midtrans.HttpClientImplementation.Call writes `req.WithContext(options.Ctx)`
// and discards the result — WithContext returns a copy, it does not mutate — so
// a context deadline handed to the SDK bounds nothing, and a Midtrans that
// accepts the connection then stops answering would hold a request goroutine
// until the OS gave up on the socket.
func TestHTTPClientHonoursContext(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // never answers until the test lets it
	}))
	defer func() {
		close(blocked)
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	c := &httpClient{ctx: ctx, do: &http.Client{}}

	done := make(chan *midtrans.Error, 1)
	go func() {
		done <- c.Call(http.MethodGet, srv.URL, nil, nil, nil, nil)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call returned success against a server that never answered")
		}
		if !errors.Is(err.RawError, context.DeadlineExceeded) {
			t.Errorf("Call error = %v, want a deadline", err.RawError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Call ignored its context deadline — the SDK transport is back")
	}
}

func TestHTTPClientRequestShape(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status_code":"201","token":"tok"}`))
	}))
	defer srv.Close()

	notify := "https://tpj.example/api/webhook/midtrans"
	key := "SB-Mid-server-secret"

	var out struct {
		StatusCode string `json:"status_code"`
		Token      string `json:"token"`
	}

	c := &httpClient{ctx: context.Background(), do: &http.Client{}}
	if err := c.Call(http.MethodPost, srv.URL, &key,
		&midtrans.ConfigOptions{PaymentAppendNotification: &notify},
		strings.NewReader(`{}`), &out); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if out.Token != "tok" {
		t.Errorf("decoded token = %q, want %q", out.Token, "tok")
	}
	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}

	// Append, never override: the account-wide notification URL belongs to another
	// Appskep system on this shared merchant account and must not be touched.
	if h := got.Header.Get("X-Append-Notification"); h != notify {
		t.Errorf("X-Append-Notification = %q, want %q", h, notify)
	}

	user, pass, ok := got.BasicAuth()
	if !ok || user != key || pass != "" {
		t.Errorf("basic auth = %q/%q ok=%v, want the server key as the username with an empty password",
			user, pass, ok)
	}
}

// TestHTTPClientDecodesErrorBodies: Midtrans reports application-level failures
// in the body while still using a 4xx, and the caller needs both.
func TestHTTPClientDecodesErrorBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status_code":"404","status_message":"Transaction doesn't exist."}`))
	}))
	defer srv.Close()

	var out struct {
		StatusCode    string `json:"status_code"`
		StatusMessage string `json:"status_message"`
	}

	c := &httpClient{ctx: context.Background(), do: &http.Client{}}
	err := c.Call(http.MethodGet, srv.URL, nil, nil, nil, &out)

	if err == nil {
		t.Fatal("Call returned nil for a 404")
	}
	if err.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", err.StatusCode)
	}
	// The body is decoded regardless of status, so GetStatus can tell an
	// order-not-found from a transport failure.
	if out.StatusCode != "404" {
		t.Errorf("body was not decoded on the error path: %+v", out)
	}
	if err.RawApiResponse == nil || len(err.RawApiResponse.RawBody) == 0 {
		t.Error("RawApiResponse does not carry the body")
	}
}

// TestHTTPClientCapsTheResponse: a provider answering with an unbounded stream
// must not be able to exhaust memory here.
func TestHTTPClientCapsTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Well past maxResponseBytes, and deliberately not valid JSON once cut, so
		// the truncation is observable.
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 64<<10)
		_, _ = w.Write([]byte(`{"padding":"`))
		for range 32 { // 2 MB
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()

	var out map[string]any
	c := &httpClient{ctx: context.Background(), do: &http.Client{}}
	err := c.Call(http.MethodGet, srv.URL, nil, nil, nil, &out)

	// The read stops at the cap, so the JSON is truncated and fails to decode. The
	// assertion that matters is that it returned at all rather than buffering 2 MB.
	if err == nil {
		t.Error("a 2 MB response decoded cleanly — the read cap is not being applied")
	}
}
