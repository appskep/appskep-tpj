package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/config"
)

// accountTimeout bounds a call to the Appskep account API. Like the refresh
// call it sits on the request path — a save waits for it — so an unreachable
// auth service must fail fast rather than hold the user's connection open.
const accountTimeout = 5 * time.Second

// maxAccountResponseBytes caps one account-API response. The envelope is a few
// hundred bytes; a provider answering with an unbounded stream must not be able
// to exhaust memory here.
const maxAccountResponseBytes = 1 << 20

// ErrAccountRejected is a 4xx from the auth service — the submission was
// understood and refused (email already used, password too weak). It carries
// the provider's own message so the service can show it beside the form field,
// exactly as a local ValidationError would. Every other failure (transport,
// 5xx, a body we cannot read) is wrapped and treated as our own fault: a 500.
type ErrAccountRejected struct {
	// Message is the auth service's `message` field, already in Bahasa
	// Indonesia. Empty when the body carried none, so callers supply a fallback.
	Message string
}

func (e *ErrAccountRejected) Error() string {
	if e.Message == "" {
		return "auth: account update rejected"
	}
	return "auth: account update rejected: " + e.Message
}

// Account edits the user's Appskep account through the auth service, authorised
// by that user's own bearer token.
//
// An interface for the same reason Refresher is one: it is an external call, so
// CLAUDE.md keeps it behind a seam, and the handler and service tests drive it
// without a network. TPJ still owns no credentials — it never verifies a
// password or stores one; it only forwards the signed-in user's intent to the
// service that does.
type Account interface {
	UpdateProfile(ctx context.Context, token string, in AccountProfile) error
	SetPassword(ctx context.Context, token, newPassword, confirm string) error
}

// AccountProfile is one profile submission bound for /v2/user/update. Raw
// strings, already validated by the service — this type only shapes the wire
// request, keeping the provider's field names out of the caller.
type AccountProfile struct {
	Name      string
	Email     string
	Phone     string
	Birthdate string // yyyy-mm-dd, empty when not provided
	Sex       string // "1" or "2", empty when not provided
}

// HTTPAccount calls the real auth service.
type HTTPAccount struct {
	cfg    config.AuthConfig
	client *http.Client
}

func NewHTTPAccount(cfg config.AuthConfig) *HTTPAccount {
	return &HTTPAccount{
		cfg:    cfg,
		client: &http.Client{Timeout: accountTimeout},
	}
}

// UpdateProfile sends name/email/phone/birthdate/sex to /v2/user/update.
//
// Empty optional fields (birthdate, sex) are omitted from the JSON body rather
// than sent blank: the endpoint's partial-vs-full-replace behaviour is not
// documented, and omitting a field is the only encoding that cannot wipe a
// value TPJ never learned (education, address) — TPJ does not share Appskep's
// user table and only knows what the JWT carries.
func (a *HTTPAccount) UpdateProfile(ctx context.Context, token string, in AccountProfile) error {
	body := map[string]string{
		"name":  in.Name,
		"email": in.Email,
		"phone": in.Phone,
	}
	if in.Birthdate != "" {
		body["birthdate"] = in.Birthdate
	}
	if in.Sex != "" {
		body["sex"] = in.Sex
	}
	return a.call(ctx, a.cfg.UpdateURL(), token, body)
}

// SetPassword sends the new password to /v2/user/set-password. The bearer token
// is the whole authorisation: the account is the one it identifies, so there is
// no current-password field, mirroring the auth service's own contract.
func (a *HTTPAccount) SetPassword(ctx context.Context, token, newPassword, confirm string) error {
	body := map[string]string{
		"newPassword":        newPassword,
		"newPasswordConfirm": confirm,
	}
	return a.call(ctx, a.cfg.SetPasswordURL(), token, body)
}

// call issues one PUT with the bearer token and maps the reply: a 4xx becomes
// ErrAccountRejected carrying the decoded message; anything else unhealthy is a
// wrapped error the caller logs and turns into a 500.
func (a *HTTPAccount) call(ctx context.Context, url, token string, body map[string]string) error {
	if token == "" {
		return ErrNoToken
	}

	ctx, cancel := context.WithTimeout(ctx, accountTimeout)
	defer cancel()

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("auth: encoding account request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("auth: building account request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: account request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAccountResponseBytes))
	if err != nil {
		return fmt.Errorf("auth: reading account response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// A 4xx is the user's problem to fix and carries a message to show them; a
	// 5xx is the service's, and unreadable to the user.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &ErrAccountRejected{Message: decodeAccountMessage(raw)}
	}
	return fmt.Errorf("auth: account update returned %d", resp.StatusCode)
}

// decodeAccountMessage pulls the `message` out of the {status, message, data}
// envelope the auth service uses. Best-effort: a body that does not parse or
// carries no message yields "", and the caller supplies a generic fallback.
func decodeAccountMessage(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return env.Message
}

// compile-time assertion that HTTPAccount satisfies the interface.
var _ Account = (*HTTPAccount)(nil)
