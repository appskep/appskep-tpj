package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/midtrans/midtrans-go"
)

// httpClient replaces midtrans-go's own transport.
//
// The SDK ships one, and it is used everywhere else in the SDK, but it cannot
// honour a context: midtrans.HttpClientImplementation.Call does
//
//	req.WithContext(options.Ctx)
//
// and throws the result away — WithContext returns a copy, it does not mutate.
// So a context.WithTimeout handed to the SDK bounds nothing at all, and a
// Midtrans that stops answering would hold a request goroutine until the
// operating system gave up on the socket. CLAUDE.md requires every external call
// to take a context timeout; this is what makes that true rather than decorative.
//
// The SDK's HttpClient is a single-method interface, so replacing it costs one
// small type and leaves every typed request and response struct, and the
// environment-to-URL mapping, exactly as the SDK defines them.
type httpClient struct {
	// ctx is the caller's context. The interface method has nowhere to accept
	// one, so a client is built per call — they are trivially cheap, and sharing
	// one across requests would mean sharing a deadline.
	ctx context.Context
	do  *http.Client
}

// Call issues one Midtrans API request.
//
// The response body is decoded into result whatever the status code is: Midtrans
// reports application-level failures in the body (status_code, error_messages)
// while still using 4xx, and the caller needs to see both.
func (c *httpClient) Call(
	method string,
	url string,
	apiKey *string,
	options *midtrans.ConfigOptions,
	body io.Reader,
	result any,
) *midtrans.Error {
	req, err := http.NewRequestWithContext(c.ctx, method, url, body)
	if err != nil {
		return &midtrans.Error{
			Message:  fmt.Sprintf("payment: building request: %s", err),
			RawError: err,
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// The one SDK option this codebase uses. Our notification URL is attached per
	// transaction so the account-wide setting, which belongs to another Appskep
	// system on this shared merchant account, is never touched.
	if options != nil && options.PaymentAppendNotification != nil {
		req.Header.Set("X-Append-Notification", *options.PaymentAppendNotification)
	}

	// Basic auth: server key as the username, empty password. The key never
	// reaches a log — nothing here logs headers, which is exactly why the SDK's
	// own transport (whose logger prints every request header) is not used.
	if apiKey != nil {
		req.SetBasicAuth(*apiKey, "")
	}

	res, err := c.do.Do(req)
	if err != nil {
		return &midtrans.Error{
			Message:  fmt.Sprintf("payment: calling midtrans: %s", err),
			RawError: err,
		}
	}
	defer res.Body.Close()

	// Bounded: a provider that answers with an unbounded stream must not be able
	// to exhaust memory on our side.
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return &midtrans.Error{
			Message:    fmt.Sprintf("payment: reading midtrans response: %s", err),
			StatusCode: res.StatusCode,
			RawError:   err,
		}
	}

	if result != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			return &midtrans.Error{
				Message:    fmt.Sprintf("payment: decoding midtrans response: %s", err),
				StatusCode: res.StatusCode,
				RawError:   err,
			}
		}
	}

	if res.StatusCode >= http.StatusBadRequest {
		// The body is already decoded into result, so the caller can read the
		// provider's own error_messages. This carries the transport-level fact.
		return &midtrans.Error{
			Message:    fmt.Sprintf("payment: midtrans returned %d", res.StatusCode),
			StatusCode: res.StatusCode,
			RawApiResponse: &midtrans.ApiResponse{
				Status:     res.Status,
				StatusCode: res.StatusCode,
				Header:     res.Header,
				RawBody:    raw,
			},
		}
	}
	return nil
}

// maxResponseBytes caps one Midtrans response. Snap answers are a few hundred
// bytes; a status response with refund history is still tiny.
const maxResponseBytes = 1 << 20
