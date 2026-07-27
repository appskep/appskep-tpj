package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"

	"github.com/remorac/appskep-tpj/internal/shared/config"
)

// SMTPSender delivers over SMTP.
//
// net/smtp rather than a library: what this needs is one connection, optional
// STARTTLS, optional PLAIN auth and one DATA command, and every dependency added
// to this project has to earn itself. The one thing net/smtp does not offer is a
// context-aware dial, which is why the connection is opened here and handed to
// smtp.NewClient rather than going through smtp.SendMail.
type SMTPSender struct {
	cfg config.SMTPConfig
}

func NewSMTP(cfg config.SMTPConfig) *SMTPSender { return &SMTPSender{cfg: cfg} }

// implicitTLSPort is the submission port that expects TLS from the first byte
// (SMTPS). Every other port negotiates it with STARTTLS or runs in the clear.
const implicitTLSPort = 465

// Send encodes and delivers one message.
func (s *SMTPSender) Send(ctx context.Context, m Message) error {
	raw, err := Build(Envelope{FromName: s.cfg.FromName, FromEmail: s.cfg.FromEmail}, m)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	return s.deliver(ctx, m.To, raw)
}

func (s *SMTPSender) deliver(ctx context.Context, to string, raw []byte) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dialling %s: %w", addr, err)
	}
	defer conn.Close()

	// The context deadline is pushed down to the socket, so it bounds the whole
	// SMTP conversation and not just the dial. Without this a server that accepts
	// the connection and then never answers EHLO would hold this goroutine open
	// past the timeout the caller asked for.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("mail: setting deadline: %w", err)
		}
	}

	encrypted := false
	if s.cfg.Port == implicitTLSPort {
		tlsConn := tls.Client(conn, s.tlsConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("mail: TLS handshake with %s: %w", addr, err)
		}
		conn, encrypted = tlsConn, true
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("mail: SMTP greeting from %s: %w", addr, err)
	}
	// Close rather than Quit on the error paths below: Quit is attempted once, at
	// the end, and a second one on a broken connection only masks the real error.
	defer c.Close()

	if !encrypted {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.tlsConfig()); err != nil {
				return fmt.Errorf("mail: STARTTLS with %s: %w", addr, err)
			}
			encrypted = true
		}
	}

	if s.cfg.Username != "" {
		// net/smtp refuses PLAIN over a connection that is neither encrypted nor
		// to localhost, and it is right to. The check is repeated here only so the
		// failure names the cause: a production mail host that has quietly stopped
		// advertising STARTTLS must not be handed the password, and the log line
		// should say so rather than reporting an "unencrypted connection" from
		// somewhere inside the standard library.
		if !encrypted && !isLoopback(s.cfg.Host) {
			return errors.New("mail: refusing to send SMTP credentials over an unencrypted connection")
		}
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := c.Auth(auth); err != nil {
			// The error from net/smtp carries the server's reply, never the
			// password — but wrap it without the config so nothing here can grow a
			// credential into a log line later.
			return fmt.Errorf("mail: authenticating to %s: %w", s.cfg.Host, err)
		}
	}

	if err := c.Mail(s.cfg.FromEmail); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("mail: writing message: %w", err)
	}
	// The close is what commits the message; its error is the server's verdict on
	// the mail and must never be dropped.
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: completing message: %w", err)
	}

	if err := c.Quit(); err != nil {
		// The server already accepted the message at w.Close above. A failed QUIT
		// is a rude disconnect, not a lost mail, and reporting it as a failure
		// would make the caller believe nothing was sent.
		return nil
	}
	return nil
}

func (s *SMTPSender) tlsConfig() *tls.Config {
	// ServerName is the configured host, so a certificate for someone else fails
	// verification. There is deliberately no InsecureSkipVerify escape hatch.
	return &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
}

// isLoopback reports whether the host is this machine — the one case where
// sending credentials without TLS is not a mistake, and the one a local Mailpit
// or MailHog runs on.
func isLoopback(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
