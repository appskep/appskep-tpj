package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Envelope is who the message is from. It comes from SMTP_FROM_NAME and
// SMTP_FROM_EMAIL, never from anything a user typed.
type Envelope struct {
	FromName  string
	FromEmail string
}

// ErrHeaderInjection reports a value that would break out of its header.
//
// The recipient address and the subject are the two fields that reach a header
// carrying data from the database — a customer's own name ends up in a subject
// line. A bare CR or LF there would let that name append headers of its own, so
// it is refused rather than stripped: a mail nobody sent is a bug worth seeing,
// and a silently rewritten one is not.
var ErrHeaderInjection = errors.New("mail: header value contains a line break")

// Build encodes one message as RFC 5322 bytes: a multipart/alternative document
// with a text/plain and a text/html part, both quoted-printable so the
// Indonesian copy survives a 7-bit relay intact.
func Build(env Envelope, m Message) ([]byte, error) {
	if strings.TrimSpace(env.FromEmail) == "" {
		return nil, errors.New("mail: no from address configured")
	}
	if strings.TrimSpace(m.To) == "" {
		return nil, errors.New("mail: no recipient")
	}
	if m.HTML == "" || m.Text == "" {
		return nil, errors.New("mail: both an HTML and a text body are required")
	}
	for _, v := range []string{m.To, m.ToName, m.Subject, env.FromEmail, env.FromName} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, ErrHeaderInjection
		}
	}
	// Parsing rather than pattern-matching: a value that net/mail cannot read is
	// one the receiving server will not read either.
	if _, err := mail.ParseAddress(m.To); err != nil {
		return nil, fmt.Errorf("mail: invalid recipient %q: %w", m.To, err)
	}

	boundary, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	msgID, err := messageID(env.FromEmail)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	if err := mp.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("mail: setting boundary: %w", err)
	}

	// Order matters: a client picks the LAST part it can display, so the HTML
	// alternative has to come second or every graphical client shows plain text.
	if err := writePart(mp, "text/plain; charset=utf-8", m.Text); err != nil {
		return nil, err
	}
	if err := writePart(mp, "text/html; charset=utf-8", m.HTML); err != nil {
		return nil, err
	}
	if err := mp.Close(); err != nil {
		return nil, fmt.Errorf("mail: closing multipart: %w", err)
	}

	// net/mail.Address.String applies RFC 2047 to the display name itself, so a
	// site name with an accent in it needs no special handling here.
	from := (&mail.Address{Name: env.FromName, Address: env.FromEmail}).String()
	to := (&mail.Address{Name: m.ToName, Address: m.To}).String()

	var out bytes.Buffer
	header(&out, "From", from)
	header(&out, "To", to)
	// QEncoding leaves a pure-ASCII subject alone and encodes anything else, so a
	// booking code reads as itself in a mail client's list view.
	header(&out, "Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header(&out, "Date", time.Now().Format(time.RFC1123Z))
	header(&out, "Message-ID", msgID)
	header(&out, "MIME-Version", "1.0")
	header(&out, "Content-Type", "multipart/alternative; boundary="+boundary)
	// Transactional mail: a bounce is for us, and an auto-reply to it is not.
	header(&out, "Auto-Submitted", "auto-generated")
	out.WriteString("\r\n")
	out.Write(body.Bytes())

	return out.Bytes(), nil
}

func header(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// writePart adds one alternative, quoted-printable encoded.
//
// quotedprintable.Writer normalises a lone \n to \r\n and soft-wraps at 76
// characters, which is what keeps a long Indonesian sentence from being
// truncated by a relay that enforces the line limit itself.
func writePart(mp *multipart.Writer, contentType, content string) error {
	w, err := mp.CreatePart(map[string][]string{
		"Content-Type":              {contentType},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return fmt.Errorf("mail: creating %s part: %w", contentType, err)
	}

	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(content)); err != nil {
		return fmt.Errorf("mail: writing %s part: %w", contentType, err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("mail: closing %s part: %w", contentType, err)
	}
	return nil
}

// messageID mints a unique id in the sender's own domain. Some receivers treat a
// missing Message-ID as a spam signal, and a duplicate one as a resend of a mail
// already delivered — which would hide a second, genuinely different mail.
func messageID(fromEmail string) (string, error) {
	domain := "localhost"
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 && at < len(fromEmail)-1 {
		domain = fromEmail[at+1:]
	}
	local, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return "<" + local + "@" + domain + ">", nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mail: reading random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
