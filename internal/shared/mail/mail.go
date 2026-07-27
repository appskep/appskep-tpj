// Package mail delivers already-rendered email.
//
// It is the transport half of the split CLAUDE.md requires of every external
// call: an interface, a real implementation that takes a context timeout, and
// nothing that knows what a booking is. The business half — what to send, when,
// and in what words — lives in service.Email, exactly as payment.Gateway is the
// transport under service.Payment.
//
// Nothing here reads the database, renders a template or logs a secret.
package mail

import (
	"context"
	"log/slog"
)

// Message is one rendered email, ready to encode.
//
// Both bodies are required. A multipart/alternative message with an empty text
// part is worse than no text part at all: a text-only client renders a blank
// mail rather than falling back to the HTML.
type Message struct {
	// To is the recipient's address. It is the Appskep account email — the
	// booking form collects a name, a phone and an address, never an email.
	To     string
	ToName string

	Subject string
	HTML    string
	Text    string
}

// Sender delivers a message, or explains why it could not.
//
// Implementations must respect ctx: a mail server that accepts a connection and
// then stops answering would otherwise hold the sending goroutine for as long as
// the OS keeps the socket open.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// NoOp is the Sender used when SMTP is unconfigured (config.SMTPConfig.Enabled
// is false). It is deliberately not an error path: a development instance, and a
// staging one before its mail credentials exist, must run the whole booking flow
// with email switched off.
//
// It logs at DEBUG rather than INFO so it does not fill a production log if a
// deploy ever ships without SMTP — the one INFO line naming that at startup is
// enough, and it is written by main.
type NoOp struct{ log *slog.Logger }

func NewNoOp(log *slog.Logger) NoOp { return NoOp{log: log} }

func (n NoOp) Send(ctx context.Context, m Message) error {
	if n.log != nil {
		n.log.DebugContext(ctx, "mail: not sent, SMTP is not configured",
			slog.String("to", m.To), slog.String("subject", m.Subject))
	}
	return nil
}
