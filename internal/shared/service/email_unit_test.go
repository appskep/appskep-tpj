package service

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/mail"
)

// In-package: emailJob and enqueue are unexported, so the queue-full case cannot
// be reached from outside. NewEmail's template check needs no database — it
// touches neither the store nor the settings before returning — so fstest.MapFS
// is enough for the whole of it.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// emailFS builds a template tree with every define each kind needs.
func emailFS(t *testing.T) fstest.MapFS {
	t.Helper()

	var b strings.Builder
	for _, kind := range allEmailKinds {
		b.WriteString(`{{define "` + string(kind) + `_subject"}}Subjek ` + string(kind) + `{{end}}`)
		b.WriteString(`{{define "` + string(kind) + `_html"}}<p>` + string(kind) + `</p>{{end}}`)
		b.WriteString(`{{define "` + string(kind) + `_text"}}` + string(kind) + `{{end}}`)
	}
	// The open/close pair every mail calls itself: there is no layout, because Go
	// templates cannot take a template name as a variable.
	b.WriteString(`{{define "email_open"}}<html><body>{{end}}`)
	b.WriteString(`{{define "email_close"}}</body></html>{{end}}`)

	return fstest.MapFS{
		"emails/all.html": &fstest.MapFile{Data: []byte(b.String())},
	}
}

func newTestEmail(t *testing.T, fsys fstest.MapFS, cfg EmailConfig) (*Email, error) {
	t.Helper()

	if cfg.Location == nil {
		cfg.Location = testLoc(t)
	}
	// A nil store and nil settings are safe here: NewEmail parses and checks names
	// and touches neither.
	return NewEmail(fsys, nil, mail.NewNoOp(discardLogger()), nil, discardLogger(), cfg)
}

func TestNewEmailAcceptsACompleteTree(t *testing.T) {
	if _, err := newTestEmail(t, emailFS(t), EmailConfig{}); err != nil {
		t.Fatalf("NewEmail rejected a complete template tree: %v", err)
	}
}

// TestNewEmailRequiresEveryName is the boot-time guarantee: a typo in a define is
// otherwise invisible until the first customer does not get their mail, and
// nobody is looking at a mail that failed to render.
func TestNewEmailRequiresEveryName(t *testing.T) {
	for _, kind := range allEmailKinds {
		for _, suffix := range []string{"_subject", "_html", "_text"} {
			name := string(kind) + suffix

			t.Run(name, func(t *testing.T) {
				fsys := emailFS(t)
				body := string(fsys["emails/all.html"].Data)

				// Rename the one define, so every other name still resolves.
				body = strings.Replace(body,
					`{{define "`+name+`"}}`,
					`{{define "`+name+`_typo"}}`, 1)
				fsys["emails/all.html"] = &fstest.MapFile{Data: []byte(body)}

				_, err := newTestEmail(t, fsys, EmailConfig{})
				if err == nil {
					t.Fatalf("NewEmail accepted a tree missing %q", name)
				}
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error = %q, want it to name the missing template", err)
				}
			})
		}
	}
}

func TestNewEmailRejectsAnEmptyTree(t *testing.T) {
	_, err := newTestEmail(t, fstest.MapFS{}, EmailConfig{})
	if err == nil {
		t.Fatal("NewEmail accepted a tree with no templates")
	}
	if !strings.Contains(err.Error(), "no templates found") {
		t.Errorf("error = %q", err)
	}
}

func TestNewEmailRejectsAMalformedTemplate(t *testing.T) {
	fsys := emailFS(t)
	fsys["emails/broken.html"] = &fstest.MapFile{Data: []byte(`{{define "x"}}{{.Unclosed`)}

	if _, err := newTestEmail(t, fsys, EmailConfig{}); err == nil {
		t.Fatal("NewEmail accepted a malformed template — a broken email must fail the boot")
	}
}

// TestEmailNamesMustBeUniqueAcrossFiles. Unlike pages, every email parses into
// ONE set and executes by name, so two files defining the same name is a silent
// override rather than an error — the second wins and the first mail is never
// sent. Nothing enforces it at boot, so this test enforces it over the real tree
// instead (see TestRealEmailTemplatesParse in the integration package for that);
// here it records the rule against the synthetic tree.
func TestEmailNamesAreExecutedFromOneSet(t *testing.T) {
	fsys := emailFS(t)
	// A second file redefining one of the six subjects.
	fsys["emails/override.html"] = &fstest.MapFile{
		Data: []byte(`{{define "booking_created_subject"}}Yang kedua{{end}}`),
	}

	e, err := newTestEmail(t, fsys, EmailConfig{})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}

	// It parses — which is exactly the hazard. The name resolves to one of the
	// two definitions and the other is unreachable, with no error anywhere.
	_, text, err := e.lookup()
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if text.Lookup("booking_created_subject") == nil {
		t.Fatal("the name did not resolve at all")
	}
	t.Log("two files may define one name and the loser is silent — " +
		"every define under emails/ must be uniquely named")
}

// TestEnqueueNeverBlocks is Audit.Record's rule applied to mail: a notification
// can never fail, delay or roll back the action it describes. A full queue is a
// dropped mail and a WARN, never a blocked request — or worse, a blocked
// transaction's caller.
func TestEnqueueNeverBlocks(t *testing.T) {
	e, err := newTestEmail(t, emailFS(t), EmailConfig{QueueSize: 2})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the queue holds, with no worker draining it.
		for i := range 100 {
			e.Notify(context.Background(), EmailBookingCreated, int64(i))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a full queue — a mail server must never be able to " +
			"hold up a booking")
	}

	if got := len(e.queue); got != 2 {
		t.Errorf("queue holds %d jobs, want the configured 2", got)
	}
}

// TestEnqueueDropsWithAWarning: the drop has to be visible, or a missing mail is
// indistinguishable from one that was never triggered.
func TestEnqueueDropsWithAWarning(t *testing.T) {
	var logged strings.Builder
	e, err := NewEmail(emailFS(t), nil, mail.NewNoOp(discardLogger()), nil,
		slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		EmailConfig{QueueSize: 1, Location: testLoc(t)})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}

	e.Notify(context.Background(), EmailBookingCreated, 1) // fills the queue
	e.Notify(context.Background(), EmailBookingCreated, 2) // dropped

	out := logged.String()
	if !strings.Contains(out, "queue full") {
		t.Errorf("a dropped notification produced no warning; log = %q", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Errorf("the drop was not logged at WARN; log = %q", out)
	}
}

func TestEmailQueueSizeDefaults(t *testing.T) {
	for _, size := range []int{0, -1} {
		e, err := newTestEmail(t, emailFS(t), EmailConfig{QueueSize: size})
		if err != nil {
			t.Fatalf("NewEmail: %v", err)
		}
		if got := cap(e.queue); got != defaultEmailQueueSize {
			t.Errorf("QueueSize=%d gave a queue of %d, want the default %d",
				size, got, defaultEmailQueueSize)
		}
	}
}

// TestDescribeSlot is the one fact a reschedule notice cannot re-read: once the
// move commits the booking points at the new slot and the old time is gone, so it
// travels with the job as an already-formatted string.
func TestDescribeSlot(t *testing.T) {
	e, err := newTestEmail(t, emailFS(t), EmailConfig{})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}

	got := e.describeSlot(scheduleSlot(t, 2026, time.July, 27, "08:00:00", "10:30:00"))
	want := "Senin, 27 Juli 2026 · 08:00 – 10:30 WIB"
	if got != want {
		t.Errorf("describeSlot = %q, want %q", got, want)
	}
}

// TestEmailButtonLabelsCoverEveryKind: the call to action is in Go rather than in
// six templates so the button and the mail around it cannot describe different
// actions — which only holds if every kind has one.
func TestEmailButtonLabelsCoverEveryKind(t *testing.T) {
	for _, kind := range allEmailKinds {
		if emailButtonLabels[kind] == "" {
			t.Errorf("no button label for %q — its mail would render a blank call to action", kind)
		}
	}
	if len(emailButtonLabels) != len(allEmailKinds) {
		t.Errorf("emailButtonLabels has %d entries for %d kinds",
			len(emailButtonLabels), len(allEmailKinds))
	}
}
