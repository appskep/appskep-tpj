package integration

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// errNoMailServer stands in for an SMTP host that has stopped answering.
var errNoMailServer = errors.New("dial tcp: connection refused")

// templatesFrom is the real template tree, for the one test that builds a second
// Email with a different reminder hour.
func templatesFrom(root string) fs.FS {
	return os.DirFS(filepath.Join(root, "template"))
}

// A notification can never fail, delay or roll back the action it describes, and
// its trigger is a real transition rather than a request. These tests check both
// halves against the real template tree and a recording sender.

// drain runs the worker until it has nothing left, then stops it. Run's own
// drain() on cancellation is what makes this work: a booking confirmed a moment
// before a restart still goes out.
func drain(t *testing.T, env *testsupport.Env) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.Deps.Email.Run(ctx)
	}()

	// Cancel immediately: Run answers ctx.Done() by draining what is queued under
	// its own grace period rather than dropping it.
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the email worker did not finish draining")
	}
}

// TestBookingCreatedIsNotified is the base case, and it also proves the real
// templates under template/emails/ render against a real booking row.
func TestBookingCreatedIsNotified(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9970)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	drain(t, env)

	msgs := env.Mail.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d mails sent for one booking, want 1", len(msgs))
	}
	m := msgs[0]

	// The worker re-reads the booking at send time, so the mail describes it as it
	// is when it goes out.
	if !strings.Contains(m.Subject, booking.BookingCode) {
		t.Errorf("subject %q does not name the booking code", m.Subject)
	}
	if m.To == "" {
		t.Error("no recipient — u.email comes from the joined admin detail row")
	}
	if m.HTML == "" || m.Text == "" {
		t.Error("a multipart/alternative mail needs both bodies")
	}
	// The plain-text part must not be HTML-escaped: that is what the two template
	// sets exist for.
	if strings.Contains(m.Text, "&amp;") || strings.Contains(m.Text, "&#") {
		t.Errorf("the text body is HTML-escaped: %q", m.Text)
	}
	if !strings.Contains(m.HTML, booking.BookingCode) {
		t.Error("the HTML body does not name the booking code")
	}
}

// TestOnlyARealTransitionNotifies. "cancelled" is reached from three places and
// "expired" from two, and only the service knows which one actually moved the
// row — which is why dispatch lives in the service layer and not in a handler.
func TestOnlyARealTransitionNotifies(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9971)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	drain(t, env)
	env.Mail.Reset()

	// Cancel once, then twice more. Only the first moved anything.
	if _, moved, err := env.Deps.Booking.CancelForUser(context.Background(),
		booking.BookingCode, user.ID); err != nil || !moved {
		t.Fatalf("CancelForUser: moved=%v err=%v", moved, err)
	}
	for range 2 {
		_, _, _ = env.Deps.Booking.CancelForUser(context.Background(), booking.BookingCode, user.ID)
	}

	drain(t, env)

	if got := env.Mail.Count(); got != 1 {
		t.Errorf("%d cancellation mails for three cancel attempts, want 1 — "+
			"RowsAffected() == 1 is what authorises the notice", got)
	}
}

// TestExpirySendsOneNotice: the ticker sweeps every minute forever, so a notice
// per sweep would be a mail every minute for the rest of the day.
func TestExpirySendsOneNotice(t *testing.T) {
	env := testsupport.New(t)

	user := env.User(9972)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})
	env.ExpireHold(booking.ID)

	drain(t, env)
	env.Mail.Reset()

	for range 5 {
		if _, err := env.Deps.Booking.Expire(context.Background()); err != nil {
			t.Fatalf("Expire: %v", err)
		}
	}
	drain(t, env)

	if got := env.Mail.Count(); got != 1 {
		t.Errorf("%d expiry mails after five sweeps, want 1", got)
	}
}

// TestReminderIsClaimedBeforeItIsSent is Phase 11's design, and the claim is what
// makes the sweep idempotent across a restart, a second process, and a ticker
// running every fifteen minutes all day.
func TestReminderIsClaimedBeforeItIsSent(t *testing.T) {
	env := testsupport.New(t)

	// Tomorrow, so the sweep's window matches.
	slot := env.FutureSlot(1, 4)

	paid := env.Booking(testsupport.BookingInput{UserID: env.User(9980).ID, SlotID: slot.ID})
	confirmed := env.Booking(testsupport.BookingInput{UserID: env.User(9981).ID, SlotID: slot.ID})
	pending := env.Booking(testsupport.BookingInput{UserID: env.User(9982).ID, SlotID: slot.ID})
	cancelled := env.Booking(testsupport.BookingInput{UserID: env.User(9983).ID, SlotID: slot.ID})

	env.Exec("UPDATE bookings SET status = 'paid' WHERE id = ?", paid.ID)
	// Confirming is an operator action that may never happen to a customer who has
	// already paid, so the sweep has to cover both statuses.
	env.Exec("UPDATE bookings SET status = 'confirmed' WHERE id = ?", confirmed.ID)
	env.Exec("UPDATE bookings SET status = 'cancelled' WHERE id = ?", cancelled.ID)

	drain(t, env)
	env.Mail.Reset()

	claimed, err := env.Deps.Email.SweepReminders(context.Background())
	if err != nil {
		t.Fatalf("SweepReminders: %v", err)
	}
	if claimed != 2 {
		t.Errorf("claimed %d reminders, want 2 (the paid one and the confirmed one)", claimed)
	}

	// Five more sweeps plus, in effect, a restart: the claim is in the database,
	// so nothing further goes out.
	for i := range 5 {
		again, err := env.Deps.Email.SweepReminders(context.Background())
		if err != nil {
			t.Fatalf("sweep %d: %v", i+2, err)
		}
		if again != 0 {
			t.Errorf("sweep %d claimed %d reminders, want 0 — a duplicate reminder is "+
				"worse than a missing one", i+2, again)
		}
	}

	drain(t, env)
	if got := env.Mail.Count(); got != 2 {
		t.Errorf("%d reminder mails after six sweeps, want 2", got)
	}

	// The stamp lands only on the two that were reminded.
	for _, tc := range []struct {
		name    string
		id      int64
		stamped bool
	}{
		{name: "paid", id: paid.ID, stamped: true},
		{name: "confirmed", id: confirmed.ID, stamped: true},
		{name: "pending_payment", id: pending.ID},
		{name: "cancelled", id: cancelled.ID},
	} {
		got := env.CountRows("bookings", "id = ? AND reminder_sent_at IS NOT NULL", tc.id)
		if (got == 1) != tc.stamped {
			t.Errorf("%s booking: reminder_sent_at stamped = %v, want %v",
				tc.name, got == 1, tc.stamped)
		}
	}
}

// TestReminderDoesNothingBeforeItsHour. A reminder is a wall-clock event: one
// that arrives at 03:00 is worse than none, so the sweep does nothing at all
// before ReminderHour however often the ticker runs.
//
// The hour is fixed by choosing the LOCATION rather than by picking a number
// relative to time.Now(). SweepReminders reads time.Now().In(e.loc), so a zone
// whose current wall clock is 01:00 makes the gate closed at any real-world time
// — a test that only runs 23 hours out of 24 would have been skipped on the run
// that wrote it, which is exactly what happened at 23:19 WIB.
func TestReminderDoesNothingBeforeItsHour(t *testing.T) {
	env := testsupport.New(t)

	slot := env.FutureSlot(1, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9990).ID, SlotID: slot.ID})
	env.Exec("UPDATE bookings SET status = 'paid' WHERE id = ?", booking.ID)

	root, err := testsupport.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	early, err := service.NewEmail(templatesFrom(root), env.Store, env.Mail, nil,
		env.Deps.Log, service.EmailConfig{
			AppURL:   env.Cfg.App.URL,
			Location: zoneWhereItIs(t, 1),
			// The seeded REMINDER_HOUR default. It is 01:00 in that zone, so the
			// gate is shut.
			ReminderHour: 9,
		})
	if err != nil {
		t.Fatalf("NewEmail: %v", err)
	}

	claimed, err := early.SweepReminders(context.Background())
	if err != nil {
		t.Fatalf("SweepReminders: %v", err)
	}
	if claimed != 0 {
		t.Errorf("claimed %d reminders at 01:00 against a reminder hour of 9, want 0", claimed)
	}
	if got := env.CountRows("bookings", "id = ? AND reminder_sent_at IS NOT NULL", booking.ID); got != 0 {
		t.Error("reminder_sent_at was stamped before the reminder hour — the claim " +
			"is permanent, so an early sweep would suppress the real one")
	}

	drain(t, env)
	if got := env.Mail.Count(); got != 1 {
		// Only the booking-created mail from the fixture.
		t.Errorf("%d mails, want only the booking confirmation", got)
	}
}

// zoneWhereItIs returns a fixed zone in which the current wall-clock hour is
// exactly hour. Fixed rather than named, so it cannot observe DST and cannot
// drift between the two calls that bracket it.
//
// It verifies its own arithmetic: an offset that landed on some other hour would
// still be below the reminder hour most of the time, so the test it feeds would
// pass without testing anything.
func zoneWhereItIs(t *testing.T, hour int) *time.Location {
	t.Helper()

	utc := time.Now().UTC()
	// Shift so the hour lands where we want it, zeroing out the minutes so the
	// result cannot roll over while the test runs.
	offset := (hour-utc.Hour())*3600 - utc.Minute()*60 - utc.Second()
	for offset <= -12*3600 {
		offset += 24 * 3600
	}
	for offset > 14*3600 {
		offset -= 24 * 3600
	}

	loc := time.FixedZone("TEST", offset)
	if got := time.Now().In(loc).Hour(); got != hour {
		t.Fatalf("zoneWhereItIs(%d) built a zone reading %02d:00", hour, got)
	}
	return loc
}

// TestASendFailureNeverFailsTheAction. Every failure in the worker is a WARN and
// nothing else: no booking, no cancellation and no payment may be rolled back by
// a mail server.
func TestASendFailureNeverFailsTheAction(t *testing.T) {
	env := testsupport.New(t)

	env.Mail.Err = errNoMailServer

	user := env.User(9995)
	slot := env.FutureSlot(3, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: user.ID, SlotID: slot.ID})

	// The booking itself succeeded, which is the assertion.
	if got := env.BookedCount(slot.ID); got != 1 {
		t.Errorf("booked_count = %d with the mail server down, want 1", got)
	}
	if got := env.BookingStatus(booking.ID); got != "pending_payment" {
		t.Errorf("status = %q, want pending_payment", got)
	}

	env.ClearLogs()
	drain(t, env)

	if got := env.Mail.Count(); got != 0 {
		t.Errorf("%d mails recorded against a failing sender", got)
	}
	logs := env.Logs()
	if !strings.Contains(logs, "WARN") {
		t.Error("a failed send produced no warning")
	}
	if strings.Contains(logs, "ERROR") {
		t.Errorf("a failed send was logged at ERROR; it can never fail the action "+
			"it describes. log = %q", logs)
	}

	env.AssertInvariant()
}

// TestRescheduleNoticeCarriesTheOldSlot: the one fact the worker cannot re-read,
// because once the move commits the booking points at the new slot.
func TestRescheduleNoticeCarriesTheOldSlot(t *testing.T) {
	env := testsupport.New(t)

	from := env.SlotAt(3, 20*60, 1)
	to := env.SlotAt(4, 20*60, 1)
	booking := env.Booking(testsupport.BookingInput{UserID: env.User(9996).ID, SlotID: from.ID})

	drain(t, env)
	env.Mail.Reset()

	if err := env.Deps.Booking.Reschedule(context.Background(), booking.ID, to.ID); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	drain(t, env)

	msgs := env.Mail.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d mails for one reschedule, want 1", len(msgs))
	}

	// Both dates appear: where it was, and where it is now.
	oldDay := from.SlotDate.Format("2")
	newDay := to.SlotDate.Format("2")
	if !strings.Contains(msgs[0].Text, oldDay) {
		t.Errorf("the notice does not mention the slot the booking left (%s)", oldDay)
	}
	if !strings.Contains(msgs[0].Text, newDay) {
		t.Errorf("the notice does not mention the new slot (%s)", newDay)
	}
}
