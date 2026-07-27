package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/config"
)

// refreshTimeout bounds the inline refresh call. It sits on the request path, so
// an unreachable auth service must fail fast rather than hold the user's
// connection open until the server's own write timeout fires.
const refreshTimeout = 5 * time.Second

// Refresher exchanges an expired Appskep token for a fresh one.
//
// An interface because it is an external call: per CLAUDE.md those sit behind
// one, and Phase 13's handler tests need to drive the refresh branch without a
// network.
type Refresher interface {
	Refresh(ctx context.Context, token string) (string, error)
}

// HTTPRefresher calls the real auth service.
type HTTPRefresher struct {
	cfg    config.AuthConfig
	client *http.Client
}

func NewHTTPRefresher(cfg config.AuthConfig) *HTTPRefresher {
	return &HTTPRefresher{
		cfg:    cfg,
		client: &http.Client{Timeout: refreshTimeout},
	}
}

// Refresh calls AUTH_URL/oauth/refresh-token?token=<current> and returns the new
// access token.
//
// GET with the token as a query parameter is the shape authentication.md
// documents; the method itself is not stated there, and GET is what the
// query-parameter form implies. Any failure — transport, non-200, missing field
// — is an error, and every caller responds to it the same way: clear the session
// and send the user back to Appskep to log in again.
func (h *HTTPRefresher) Refresh(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrNoToken
	}

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.RefreshURL(token), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: refresh request: %w", err)
	}
	defer resp.Body.Close()

	// Bounded read: an auth service returning an HTML error page should not be
	// buffered in full just to be discarded.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("auth: refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: refresh returned %d", resp.StatusCode)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("auth: decoding refresh response: %w", err)
	}
	if payload.AccessToken == "" {
		return "", errors.New("auth: refresh response carried no access_token")
	}
	return payload.AccessToken, nil
}
