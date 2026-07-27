package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/mail"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Email is what the system tells a customer, and when.
//
// One rule governs this whole type, and it is Audit.Record's rule applied to
// mail: **a notification can never fail, delay or roll back the action it
// describes.** Notify pushes an id onto a buffered channel and returns; a full
// queue is a dropped mail and a WARN line, never a failed booking. Nothing here
// is ever called from inside a transaction — every call site sits after the
// commit that made the transition real, which is also the only place that knows
// a transition happened at all.
//
// It does not use view.Renderer. That type forces text/html, globs only
// pages/*.html and hands templates a FuncMap that emits Tailwind classes no mail
// client reads — the same three reasons sitemap.xml and the CSV export bypass
// it. It also cannot import view, which imports this package; the formatters
// come straight from util, which is where view gets them too.
type Email struct {
	store    *repository.Store
	sender   mail.Sender
	settings *Settings
	log      *slog.Logger

	fsys fs.FS
	// appURL is where a link in an email points. From APP_URL, because there is
	// no request to take a host from — the same reason Payment builds its
	// callback URLs from config.
	appURL       string
	loc          *time.Location
	reminderHour int
	dev          bool

	// mu guards the two template sets, which are replaced wholesale on a
	// development reparse.
	mu   sync.RWMutex
	html *htmltemplate.Template
	text *texttemplate.Template

	queue chan emailJob
}

// EmailConfig is the handful of settings NewEmail needs, as a struct rather than
// six more positional parameters.
type EmailConfig struct {
	AppURL       string
	Location     *time.Location
	ReminderHour int
	// QueueSize bounds the send buffer. Zero uses defaultEmailQueueSize.
	QueueSize int
	// Development reparses the templates before every send, matching
	// view.Renderer, so editing an email needs no restart.
	Development bool
}

const (
	// defaultEmailQueueSize is far more than a clinic's traffic will ever queue.
	// The bound exists so an SMTP host that has stopped answering cannot grow the
	// backlog without limit while every send sits in its timeout.
	defaultEmailQueueSize = 256

	// emailSendTimeout bounds rendering plus delivery for one message. It is
	// larger than SMTP_TIMEOUT so the transport's own deadline is what normally
	// fires, and this is only the backstop.
	emailSendTimeout = 30 * time.Second

	// emailDrainGrace is how long shutdown will spend flushing the queue. A
	// booking confirmation is worth a few seconds of a restart; it is not worth
	// holding the process open behind an unreachable mail server.
	emailDrainGrace = 10 * time.Second

	// reminderBatch bounds one reminder sweep. A day's bookings are far below it.
	reminderBatch = 200
)

// EmailKind selects which mail to send. The value is also the template prefix:
// "<kind>_subject", "<kind>_html" and "<kind>_text" must all be defined
// somewhere under template/emails/.
type EmailKind string

const (
	EmailBookingCreated     EmailKind = "booking_created"
	EmailPaymentReceived    EmailKind = "payment_received"
	EmailBookingCancelled   EmailKind = "booking_cancelled"
	EmailBookingExpired     EmailKind = "booking_expired"
	EmailBookingRescheduled EmailKind = "booking_rescheduled"
	EmailBookingReminder    EmailKind = "booking_reminder"
)

// allEmailKinds is what NewEmail checks the parsed templates against, so a
// missing or misspelled define is a failed boot rather than a mail that silently
// never goes out.
var allEmailKinds = []EmailKind{
	EmailBookingCreated,
	EmailPaymentReceived,
	EmailBookingCancelled,
	EmailBookingExpired,
	EmailBookingRescheduled,
	EmailBookingReminder,
}

// emailButtonLabels is the call to action each mail carries. Indonesian copy,
// in Go rather than in six templates, so the button and the mail around it
// cannot describe different actions.
var emailButtonLabels = map[EmailKind]string{
	EmailBookingCreated:     "Bayar sekarang",
	EmailPaymentReceived:    "Lihat detail booking",
	EmailBookingCancelled:   "Lihat detail booking",
	EmailBookingExpired:     "Pesan jadwal lagi",
	EmailBookingRescheduled: "Lihat jadwal baru",
	EmailBookingReminder:    "Lihat detail booking",
}

// emailJob is one queued mail. Only an id and a kind cross the channel: the
// worker re-reads the booking at send time, so a mail describes the booking as
// it is when it goes out rather than as it was when it was queued.
type emailJob struct {
	kind      EmailKind
	bookingID int64
	// previousSlot is set for EmailBookingRescheduled alone — it is the one fact
	// no longer readable from the database once the move has committed.
	previousSlot string
	// wasPaid is set for EmailBookingCancelled: the status the booking held
	// before it was cancelled decides whether the mail has to mention a refund.
	wasPaid bool
}

// EmailSite is the sender's identity as the reader sees it, from the settings
// table so an edit in /admin/pengaturan reaches the next mail.
type EmailSite struct {
	Name         string
	URL          string
	WhatsApp     string
	Address      string
	ContactPhone string
}

// EmailBooking is the booking as an email describes it: flat, formatted-ready,
// and carrying nothing operational. booked_count and the slot's capacity are
// deliberately absent — a customer has no use for them.
type EmailBooking struct {
	Code      string
	Customer  string
	Service   string
	Date      time.Time
	StartTime string
	EndTime   string
	Duration  int32
	Amount    string
	URL       string
	// Address is where the therapist goes. Empty for bookings taken before the
	// field became required, so every template renders it conditionally.
	Address string
	// ExpiresAt is the payment deadline, zero when the booking has none.
	ExpiresAt time.Time
}

// EmailData is the single value every email template receives — the same rule as
// every partial in this codebase, and the reason there is still no dict helper.
type EmailData struct {
	Site        EmailSite
	Booking     EmailBooking
	ButtonLabel string
	// Reason is the recorded cancellation reason, when there is one.
	Reason string
	// Refundable marks a cancellation of a booking that had already been paid
	// for. There is no automated refund in v1 (PLAN.md Q6), so the mail says the
	// refund is manual rather than pretending one is on its way.
	Refundable bool
	// PreviousSlot describes where a rescheduled booking came from.
	PreviousSlot string
}

// NewEmail parses the templates and returns a ready worker.
//
// Parsing here rather than lazily makes a malformed email template a failed boot
// — the same guarantee view.New gives the pages, and worth more here because
// nobody is looking at a mail that failed to render.
func NewEmail(
	fsys fs.FS,
	store *repository.Store,
	sender mail.Sender,
	settings *Settings,
	log *slog.Logger,
	cfg EmailConfig,
) (*Email, error) {
	size := cfg.QueueSize
	if size <= 0 {
		size = defaultEmailQueueSize
	}

	e := &Email{
		store:        store,
		sender:       sender,
		settings:     settings,
		log:          log,
		fsys:         fsys,
		appURL:       strings.TrimRight(cfg.AppURL, "/"),
		loc:          cfg.Location,
		reminderHour: cfg.ReminderHour,
		dev:          cfg.Development,
		queue:        make(chan emailJob, size),
	}

	html, text, err := e.parse()
	if err != nil {
		return nil, err
	}
	e.html, e.text = html, text

	// Every kind must resolve to three templates. A typo in a define is
	// otherwise invisible until the first customer does not get their mail.
	for _, kind := range allEmailKinds {
		for _, suffix := range []string{"_subject", "_html", "_text"} {
			name := string(kind) + suffix
			if suffix == "_html" {
				if html.Lookup(name) == nil {
					return nil, fmt.Errorf("email: template %q is not defined under emails/", name)
				}
				continue
			}
			if text.Lookup(name) == nil {
				return nil, fmt.Errorf("email: template %q is not defined under emails/", name)
			}
		}
	}
	return e, nil
}

// parse builds the two template sets over the same files.
//
// Two sets, not one, and the reason is the plain-text part. html/template escapes
// everything it writes, so a customer named "Ari & Co" would reach the text body
// as "Ari &amp; Co" and the Subject header with it. The HTML bodies need
// html/template's escaping; the subjects and text bodies must not have it. Each
// set parses every file and executes only the names it owns.
//
// Unlike pages, ALL emails go into ONE set and are executed by name — so every
// define across emails/*.html must be uniquely named. The one-set-per-page rule
// exists because two pages both define "content"; emails never do.
func (e *Email) parse() (*htmltemplate.Template, *texttemplate.Template, error) {
	files, err := fs.Glob(e.fsys, "emails/*.html")
	if err != nil {
		return nil, nil, fmt.Errorf("email: globbing templates: %w", err)
	}
	if len(files) == 0 {
		return nil, nil, errors.New("email: no templates found under emails/")
	}

	html, err := htmltemplate.New("emails").Funcs(e.funcMap()).ParseFS(e.fsys, files...)
	if err != nil {
		return nil, nil, fmt.Errorf("email: parsing HTML templates: %w", err)
	}

	text, err := texttemplate.New("emails").Funcs(e.funcMap()).ParseFS(e.fsys, files...)
	if err != nil {
		return nil, nil, fmt.Errorf("email: parsing text templates: %w", err)
	}
	return html, text, nil
}

// lookup returns the current sets, reparsing first in development so an edit to
// an email is visible on the next send with no restart — matching view.lookup.
func (e *Email) lookup() (*htmltemplate.Template, *texttemplate.Template, error) {
	if e.dev {
		html, text, err := e.parse()
		if err != nil {
			return nil, nil, err
		}
		e.mu.Lock()
		e.html, e.text = html, text
		e.mu.Unlock()
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.html, e.text, nil
}

// funcMap is the email formatter set. Every entry delegates to util, which is
// also where view.funcMap gets its formatters — so a date in an email and the
// same date on the konfirmasi page cannot be written two different ways.
func (e *Email) funcMap() map[string]any {
	return map[string]any{
		"rupiah":     util.Rupiah,
		"dateID":     func(t time.Time) string { return util.DateID(e.inTZ(t)) },
		"dateTimeID": func(t time.Time) string { return util.DateTimeID(e.inTZ(t)) },
		"clock":      util.HourMinute,
		"timeRange":  util.TimeRange,
		"duration":   util.Duration,
	}
}

func (e *Email) inTZ(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.In(e.loc)
}

// ---------------------------------------------------------------------------
// Enqueueing
// ---------------------------------------------------------------------------

// Notify queues one mail about a booking. It never blocks and never fails — see
// the type comment.
func (e *Email) Notify(ctx context.Context, kind EmailKind, bookingID int64) {
	e.enqueue(ctx, emailJob{kind: kind, bookingID: bookingID})
}

// NotifyCancelled queues a cancellation notice. wasPaid is the status the
// booking held before it was cancelled, which is the only way to know whether
// there is money to talk about — the booking itself now reads "cancelled".
func (e *Email) NotifyCancelled(ctx context.Context, bookingID int64, wasPaid bool) {
	e.enqueue(ctx, emailJob{
		kind:      EmailBookingCancelled,
		bookingID: bookingID,
		wasPaid:   wasPaid,
	})
}

// NotifyRescheduled queues a reschedule notice, carrying the slot the booking
// has just left. That is the one fact the worker cannot re-read: once the move
// commits, the booking points at the new slot and the old time is gone.
func (e *Email) NotifyRescheduled(ctx context.Context, bookingID int64, previous sqlc.ScheduleSlot) {
	e.enqueue(ctx, emailJob{
		kind:         EmailBookingRescheduled,
		bookingID:    bookingID,
		previousSlot: e.describeSlot(previous),
	})
}

func (e *Email) describeSlot(s sqlc.ScheduleSlot) string {
	return util.DateID(e.inTZ(s.SlotDate)) + " · " + util.TimeRange(s.StartTime, s.EndTime) + " WIB"
}

// enqueue is the non-blocking push. A full queue drops the mail with a WARN: the
// alternative is blocking a request — or worse, a transaction's caller — behind
// a mail server, and no notification is worth that.
func (e *Email) enqueue(ctx context.Context, j emailJob) {
	select {
	case e.queue <- j:
	default:
		e.log.WarnContext(ctx, "email: queue full, notification dropped",
			slog.String("kind", string(j.kind)),
			slog.Int64("booking_id", j.bookingID))
	}
}

// ---------------------------------------------------------------------------
// The worker
// ---------------------------------------------------------------------------

// Run delivers queued mail until ctx is cancelled, then drains what is left.
//
// Started in main.go on the signal context and registered in the same WaitGroup
// as the expiry ticker, so a shutdown cancels it and then waits for the send in
// flight.
func (e *Email) Run(ctx context.Context) {
	e.log.Info("email worker started", slog.Int("queue", cap(e.queue)))

	for {
		select {
		case <-ctx.Done():
			e.drain()
			e.log.Info("email worker stopped")
			return
		case job := <-e.queue:
			e.process(ctx, job)
		}
	}
}

// drain flushes the buffer on shutdown, under a bounded grace period. A booking
// confirmation is worth a few seconds of a restart; it is not worth holding the
// process open behind a mail server that has stopped answering.
func (e *Email) drain() {
	deadline := time.Now().Add(emailDrainGrace)

	for {
		select {
		case job := <-e.queue:
			if time.Now().After(deadline) {
				e.log.Warn("email: shutdown grace expired, notification dropped",
					slog.String("kind", string(job.kind)),
					slog.Int64("booking_id", job.bookingID),
					slog.Int("remaining", len(e.queue)))
				continue
			}
			e.process(context.Background(), job)
		default:
			return
		}
	}
}

// process renders and sends one job. Every failure is a WARN and nothing else —
// there is no caller left to report to, and the action this describes committed
// long ago.
func (e *Email) process(ctx context.Context, j emailJob) {
	// WithoutCancel because the shutdown drain runs on a context that is already
	// cancelled, and a mail half-written to the socket should still finish.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), emailSendTimeout)
	defer cancel()

	row, err := e.store.Queries.GetBookingAdminDetail(ctx, j.bookingID)
	if errors.Is(err, sql.ErrNoRows) {
		e.log.WarnContext(ctx, "email: booking gone before its notification was sent",
			slog.String("kind", string(j.kind)), slog.Int64("booking_id", j.bookingID))
		return
	}
	if err != nil {
		e.log.WarnContext(ctx, "email: loading booking",
			slog.String("kind", string(j.kind)),
			slog.Int64("booking_id", j.bookingID), slog.Any("error", err))
		return
	}

	if strings.TrimSpace(row.UserEmail) == "" {
		// The address comes from the Appskep account, and this app never writes
		// it. A blank one is upstream's, and there is nothing to retry.
		e.log.WarnContext(ctx, "email: no address for this customer",
			slog.String("kind", string(j.kind)),
			slog.String("booking_code", row.BookingCode))
		return
	}

	msg, err := e.render(j, row)
	if err != nil {
		e.log.WarnContext(ctx, "email: rendering",
			slog.String("kind", string(j.kind)),
			slog.String("booking_code", row.BookingCode), slog.Any("error", err))
		return
	}

	if err := e.sender.Send(ctx, msg); err != nil {
		e.log.WarnContext(ctx, "email: sending",
			slog.String("kind", string(j.kind)),
			slog.String("booking_code", row.BookingCode),
			slog.Any("error", err))
		return
	}

	e.log.InfoContext(ctx, "email sent",
		slog.String("kind", string(j.kind)),
		slog.String("booking_code", row.BookingCode))
}

// render turns one job into a message. The subject and the text body come from
// the text/template set, the HTML body from the html/template one — see parse.
func (e *Email) render(j emailJob, row sqlc.GetBookingAdminDetailRow) (mail.Message, error) {
	html, text, err := e.lookup()
	if err != nil {
		return mail.Message{}, err
	}

	data := e.data(j, row)
	name := string(j.kind)

	var out strings.Builder
	if err := text.ExecuteTemplate(&out, name+"_subject", data); err != nil {
		return mail.Message{}, fmt.Errorf("email: rendering %s_subject: %w", name, err)
	}
	// The subject is a header: a template that ends in a newline would otherwise
	// produce one, and a header cannot carry it.
	subject := strings.Join(strings.Fields(out.String()), " ")

	out.Reset()
	if err := text.ExecuteTemplate(&out, name+"_text", data); err != nil {
		return mail.Message{}, fmt.Errorf("email: rendering %s_text: %w", name, err)
	}
	body := strings.TrimSpace(out.String()) + "\n"

	out.Reset()
	if err := html.ExecuteTemplate(&out, name+"_html", data); err != nil {
		return mail.Message{}, fmt.Errorf("email: rendering %s_html: %w", name, err)
	}

	return mail.Message{
		To:      row.UserEmail,
		ToName:  row.UserName,
		Subject: subject,
		HTML:    out.String(),
		Text:    body,
	}, nil
}

// data assembles the envelope every template receives.
func (e *Email) data(j emailJob, row sqlc.GetBookingAdminDetailRow) EmailData {
	d := EmailData{
		Site: EmailSite{
			Name:         e.settings.String(KeySiteName, "Terapi Pemuda Jompo"),
			URL:          e.appURL,
			WhatsApp:     util.WhatsAppLink(e.settings.String(KeyWhatsAppNumber, ""), ""),
			Address:      e.settings.String(KeyContactAddress, ""),
			ContactPhone: e.settings.String(KeyContactPhone, ""),
		},
		Booking: EmailBooking{
			Code:      row.BookingCode,
			Customer:  row.CustomerName,
			Service:   row.ServiceName,
			Date:      row.SlotDate,
			StartTime: row.SlotStartTime,
			EndTime:   row.SlotEndTime,
			Duration:  row.ServiceDurationMinutes,
			Amount:    row.PriceAmount,
			Address:   nullText(row.CustomerAddress),
			// Konfirmasi, not pembayaran: it is the canonical page for a booking in
			// any status, and pembayaran 303s here for anything but pending_payment
			// (Phase 9). A mail read a week later must still land somewhere useful.
			URL: e.appURL + "/booking/" + row.BookingCode + "/konfirmasi",
		},
		ButtonLabel:  emailButtonLabels[j.kind],
		PreviousSlot: j.previousSlot,
	}

	if row.ExpiresAt.Valid {
		d.Booking.ExpiresAt = row.ExpiresAt.Time
	}
	if j.kind == EmailBookingCancelled {
		d.Reason = nullText(row.CancelledReason)
		d.Refundable = j.wasPaid
	}
	return d
}

// nullText unwraps a nullable column for a template, which would otherwise print
// "{text true}". The inverse of nullString.
func nullText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// ---------------------------------------------------------------------------
// The H-1 reminder sweep
// ---------------------------------------------------------------------------

// SweepReminders queues tomorrow's reminders and reports how many it claimed.
//
// The claim is the whole design. MarkBookingReminderSent is guarded on
// reminder_sent_at IS NULL, and only RowsAffected() == 1 authorises the mail —
// the same rule that makes ReleaseSlot safe, applied to a mailbox. It is what
// makes the sweep idempotent across a restart, a second process, and a ticker
// that runs every fifteen minutes for the rest of the day.
//
// Claim-then-send is deliberate, and the trade is stated rather than hidden: a
// send that fails after the claim is not retried. A duplicate reminder is worse
// than a missing one, and the failure is a WARN in process().
//
// It does nothing at all before ReminderHour. A reminder is a wall-clock event —
// one that arrives at 03:00 is worse than none.
func (e *Email) SweepReminders(ctx context.Context) (int, error) {
	now := time.Now().In(e.loc)
	if now.Hour() < e.reminderHour {
		return 0, nil
	}

	// time.Date normalises, so this is still correct on the last day of a month.
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, e.loc)

	ids, err := e.store.Queries.ListBookingsForReminder(ctx, sqlc.ListBookingsForReminderParams{
		SlotDate: tomorrow,
		Limit:    reminderBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("listing bookings to remind for %s: %w", tomorrow.Format(dateLayout), err)
	}

	claimed := 0
	for _, id := range ids {
		res, err := e.store.Queries.MarkBookingReminderSent(ctx, id)
		if err != nil {
			return claimed, fmt.Errorf("claiming reminder for booking %d: %w", id, err)
		}
		did, err := changed(res)
		if err != nil {
			return claimed, fmt.Errorf("reading reminder claim for booking %d: %w", id, err)
		}
		if !did {
			// Another process got there first. Not an error, and not ours to send.
			continue
		}

		e.Notify(ctx, EmailBookingReminder, id)
		claimed++
	}
	return claimed, nil
}
