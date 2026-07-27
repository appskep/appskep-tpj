package mail_test

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/mail"
)

func envelope() mail.Envelope {
	return mail.Envelope{FromName: "Terapi Pemuda Jompo", FromEmail: "halo@tpj.example"}
}

func message() mail.Message {
	return mail.Message{
		To:      "budi@example.test",
		ToName:  "Budi Santoso",
		Subject: "Booking TPJ-20260726-A1B2 diterima",
		HTML:    "<p>Halo Budi</p>",
		Text:    "Halo Budi",
	}
}

// parsed is a Build output taken apart again, so the assertions are about what a
// mail client would see rather than about substrings in a blob.
type parsed struct {
	header netmail.Header
	parts  []part
}

type part struct {
	contentType string
	encoding    string
	body        string
}

func build(t *testing.T, env mail.Envelope, m mail.Message) parsed {
	t.Helper()

	raw, err := mail.Build(env, m)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	msg, err := netmail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("the built message is not readable as RFC 5322: %v", err)
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediaType)
	}

	out := parsed{header: msg.Header}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		// NextRawPart, not NextPart: NextPart transparently decodes a
		// quoted-printable part AND deletes its Content-Transfer-Encoding header, so
		// the encoding this test is asserting on would be invisible and the body
		// would then be decoded twice.
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading part: %v", err)
		}

		body, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatalf("decoding part: %v", err)
		}
		out.parts = append(out.parts, part{
			contentType: p.Header.Get("Content-Type"),
			encoding:    p.Header.Get("Content-Transfer-Encoding"),
			body:        string(body),
		})
	}
	return out
}

// TestBuildPartOrder is the rule that decides what a customer actually sees.
//
// A mail client picks the LAST alternative it can display, so the HTML part has
// to come second. Reversed, every graphical client shows plain text.
func TestBuildPartOrder(t *testing.T) {
	got := build(t, envelope(), message())

	if len(got.parts) != 2 {
		t.Fatalf("got %d parts, want 2 (text then HTML)", len(got.parts))
	}
	if !strings.HasPrefix(got.parts[0].contentType, "text/plain") {
		t.Errorf("part 0 is %q, want text/plain — a client picks the last part it can "+
			"display, so HTML last is what makes the HTML win", got.parts[0].contentType)
	}
	if !strings.HasPrefix(got.parts[1].contentType, "text/html") {
		t.Errorf("part 1 is %q, want text/html", got.parts[1].contentType)
	}

	for i, p := range got.parts {
		if !strings.Contains(p.contentType, "charset=utf-8") {
			t.Errorf("part %d Content-Type %q carries no charset", i, p.contentType)
		}
		// Quoted-printable so the Indonesian copy survives a 7-bit relay, and so a
		// long sentence is soft-wrapped rather than truncated at 76 characters.
		if p.encoding != "quoted-printable" {
			t.Errorf("part %d encoding = %q, want quoted-printable", i, p.encoding)
		}
	}

	if got.parts[0].body != "Halo Budi" {
		t.Errorf("text body = %q", got.parts[0].body)
	}
	if got.parts[1].body != "<p>Halo Budi</p>" {
		t.Errorf("html body = %q", got.parts[1].body)
	}
}

func TestBuildHeaders(t *testing.T) {
	got := build(t, envelope(), message())

	from, err := netmail.ParseAddress(got.header.Get("From"))
	if err != nil {
		t.Fatalf("From is unparseable: %v", err)
	}
	if from.Address != "halo@tpj.example" || from.Name != "Terapi Pemuda Jompo" {
		t.Errorf("From = %+v", from)
	}

	to, err := netmail.ParseAddress(got.header.Get("To"))
	if err != nil {
		t.Fatalf("To is unparseable: %v", err)
	}
	if to.Address != "budi@example.test" || to.Name != "Budi Santoso" {
		t.Errorf("To = %+v", to)
	}

	// Some receivers treat a missing Message-ID as a spam signal and a duplicate
	// one as a resend of a mail already delivered — which would hide a second,
	// genuinely different mail.
	id := got.header.Get("Message-ID")
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") {
		t.Errorf("Message-ID = %q, want it angle-bracketed", id)
	}
	if !strings.HasSuffix(id, "@tpj.example>") {
		t.Errorf("Message-ID = %q, want the sender's own domain", id)
	}

	if got.header.Get("MIME-Version") != "1.0" {
		t.Errorf("MIME-Version = %q", got.header.Get("MIME-Version"))
	}
	// Transactional mail: a bounce is for us, and an auto-reply to it is not.
	if got.header.Get("Auto-Submitted") != "auto-generated" {
		t.Errorf("Auto-Submitted = %q", got.header.Get("Auto-Submitted"))
	}
	if _, err := got.header.Date(); err != nil {
		t.Errorf("Date is unparseable: %v", err)
	}
}

// TestBuildMintsAUniqueMessageID: a duplicate id makes a receiver treat the
// second mail as a resend of the first and drop it.
func TestBuildMintsAUniqueMessageID(t *testing.T) {
	seen := make(map[string]bool, 50)
	for range 50 {
		id := build(t, envelope(), message()).header.Get("Message-ID")
		if seen[id] {
			t.Fatalf("Message-ID %q was minted twice", id)
		}
		seen[id] = true
	}
}

// TestBuildSubjectEncoding: QEncoding leaves a pure-ASCII subject alone, so a
// booking code reads as itself in a client's list view, and encodes anything else
// rather than sending raw UTF-8 in a header.
func TestBuildSubjectEncoding(t *testing.T) {
	t.Run("ascii is left alone", func(t *testing.T) {
		got := build(t, envelope(), message())
		raw := got.header.Get("Subject")

		if strings.Contains(raw, "=?") {
			t.Errorf("Subject = %q, want an ASCII subject sent as-is", raw)
		}
		if raw != "Booking TPJ-20260726-A1B2 diterima" {
			t.Errorf("Subject = %q", raw)
		}
	})

	t.Run("non-ascii is Q-encoded and decodes back", func(t *testing.T) {
		m := message()
		m.Subject = "Booking diterima — terima kasih, Ari & Co"

		got := build(t, envelope(), m)
		raw := got.header.Get("Subject")

		if !strings.Contains(raw, "=?") {
			t.Errorf("Subject = %q, want RFC 2047 encoding for a non-ASCII subject", raw)
		}

		decoded, err := (&mime.WordDecoder{}).DecodeHeader(raw)
		if err != nil {
			t.Fatalf("decoding the subject: %v", err)
		}
		if decoded != m.Subject {
			t.Errorf("Subject round-tripped to %q, want %q", decoded, m.Subject)
		}
	})
}

// TestBuildDoesNotEscapeTheTextPart is Phase 11's defect, made permanent.
//
// html/template escapes everything it writes, so a customer named "Ari & Co"
// reached the plain-text body as "Ari &amp; Co" and the Subject header with it.
// The fix was two template sets over the same files; this is what proves the
// plain side of that split is still plain.
func TestBuildDoesNotEscapeTheTextPart(t *testing.T) {
	m := message()
	m.ToName = "Ari & Co"
	m.Subject = "Booking Ari & Co diterima"
	m.Text = "Halo Ari & Co, booking <Anda> diterima."
	m.HTML = "<p>Halo Ari &amp; Co</p>"

	got := build(t, envelope(), m)

	if want := "Halo Ari & Co, booking <Anda> diterima."; got.parts[0].body != want {
		t.Errorf("text part = %q, want %q — the plain-text body must not be HTML-escaped",
			got.parts[0].body, want)
	}
	if strings.Contains(got.header.Get("Subject"), "&amp;") {
		t.Errorf("Subject = %q, want a literal ampersand", got.header.Get("Subject"))
	}

	to, err := netmail.ParseAddress(got.header.Get("To"))
	if err != nil {
		t.Fatalf("To is unparseable with an ampersand in the display name: %v", err)
	}
	if to.Name != "Ari & Co" {
		t.Errorf("To display name = %q, want %q", to.Name, "Ari & Co")
	}
}

// TestBuildRefusesHeaderInjection. The recipient address, the display name and
// the subject all carry data out of the database — a customer's own name ends up
// in a subject line. A bare CR or LF there would let that name append headers of
// its own, so it is refused rather than stripped: a mail nobody sent is a bug
// worth seeing, and a silently rewritten one is not.
func TestBuildRefusesHeaderInjection(t *testing.T) {
	injections := []string{
		"Budi\r\nBcc: attacker@example.test",
		"Budi\nBcc: attacker@example.test",
		"Budi\rBcc: attacker@example.test",
		"Budi\n\nnot a header any more",
	}

	for _, bad := range injections {
		t.Run(strings.ReplaceAll(bad[:12], "\r", "CR"), func(t *testing.T) {
			// Every field that reaches a header, one at a time.
			t.Run("ToName", func(t *testing.T) {
				m := message()
				m.ToName = bad
				assertInjectionRefused(t, envelope(), m)
			})
			t.Run("Subject", func(t *testing.T) {
				m := message()
				m.Subject = bad
				assertInjectionRefused(t, envelope(), m)
			})
			t.Run("To", func(t *testing.T) {
				m := message()
				m.To = "budi@example.test" + bad
				assertInjectionRefused(t, envelope(), m)
			})
			t.Run("FromName", func(t *testing.T) {
				env := envelope()
				env.FromName = bad
				assertInjectionRefused(t, env, message())
			})
		})
	}
}

func assertInjectionRefused(t *testing.T, env mail.Envelope, m mail.Message) {
	t.Helper()

	raw, err := mail.Build(env, m)
	if !errors.Is(err, mail.ErrHeaderInjection) {
		t.Fatalf("Build accepted a line break in a header (err = %v)", err)
	}
	if raw != nil {
		t.Error("Build returned bytes alongside a header-injection error")
	}
}

func TestBuildRequiresItsInputs(t *testing.T) {
	tests := []struct {
		name    string
		env     mail.Envelope
		mutate  func(*mail.Message)
		wantErr string
	}{
		{
			name:    "no from address",
			env:     mail.Envelope{FromName: "TPJ"},
			mutate:  func(*mail.Message) {},
			wantErr: "no from address",
		},
		{
			name:    "no recipient",
			env:     envelope(),
			mutate:  func(m *mail.Message) { m.To = "" },
			wantErr: "no recipient",
		},
		{
			// A multipart/alternative with an empty text part is worse than no text
			// part at all: a text-only client renders a blank mail rather than
			// falling back to the HTML.
			name:    "no text body",
			env:     envelope(),
			mutate:  func(m *mail.Message) { m.Text = "" },
			wantErr: "both an HTML and a text body",
		},
		{
			name:    "no html body",
			env:     envelope(),
			mutate:  func(m *mail.Message) { m.HTML = "" },
			wantErr: "both an HTML and a text body",
		},
		{
			// Parsed rather than pattern-matched: a value net/mail cannot read is
			// one the receiving server will not read either.
			name:    "unparseable recipient",
			env:     envelope(),
			mutate:  func(m *mail.Message) { m.To = "not-an-address" },
			wantErr: "invalid recipient",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := message()
			tc.mutate(&m)

			_, err := mail.Build(tc.env, m)
			if err == nil {
				t.Fatalf("Build accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildSoftWrapsLongLines: a relay that enforces the 998-character line limit
// would otherwise truncate a long Indonesian sentence mid-word.
func TestBuildSoftWrapsLongLines(t *testing.T) {
	m := message()
	m.Text = strings.Repeat("Terapi fisik untuk relaksasi dan pemulihan tubuh. ", 40)

	raw, err := mail.Build(envelope(), m)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for line := range strings.SplitSeq(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a %d-character line would be truncated by a conforming relay", len(line))
		}
	}

	// And it still decodes back to exactly what went in.
	got := build(t, envelope(), m)
	if got.parts[0].body != m.Text {
		t.Error("the soft-wrapped text part does not decode back to the original")
	}
}
