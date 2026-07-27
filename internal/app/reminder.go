package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RunReminderTicker sends the H-1 reminder for tomorrow's bookings.
//
// It lives here, beside RunExpiryTicker, for the same reason: it is a background
// loop that needs Deps, and main.go runs it on the signal-derived context so a
// shutdown cancels it and waits for the pass in flight.
//
// The loop is coarse and the sweep is cheap, because the interval is not what
// decides anything. Service.SweepReminders does nothing at all before
// REMINDER_HOUR — a reminder is a wall-clock event, and one that arrives at
// 03:00 is worse than none — and after it, each booking is claimed exactly once
// by a guarded UPDATE on reminder_sent_at. Running every fifteen minutes for the
// rest of the day therefore costs one indexed query per tick and sends nothing
// twice.
//
// A sweep runs immediately on start, before the first tick, so a process that
// restarts at 09:05 does not wait until 09:15 to send the day's reminders.
func (d *Deps) RunReminderTicker(ctx context.Context) {
	interval := d.Cfg.App.ReminderSweepInterval

	d.Log.Info("reminder ticker started",
		slog.Duration("interval", interval),
		slog.Int("hour", d.Cfg.App.ReminderHour))

	d.sweepReminders(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.Log.Info("reminder ticker stopped")
			return
		case <-ticker.C:
			d.sweepReminders(ctx)
		}
	}
}

// sweepReminders runs one pass and reports it.
//
// An error is logged and swallowed, exactly as in sweepExpired: a database
// hiccup must not end the ticker for the life of the process. Retrying is safe
// and automatic — a booking the failed pass never claimed still has a NULL
// reminder_sent_at, so the next tick picks it up.
func (d *Deps) sweepReminders(ctx context.Context) {
	sent, err := d.Email.SweepReminders(ctx)
	if errors.Is(err, context.Canceled) {
		// Shutdown reached the sweep mid-query. Not a failure, and an error line
		// here would appear in the log of every clean restart.
		return
	}
	if err != nil {
		// sent is still meaningful — those claims are committed and queued.
		d.Log.ErrorContext(ctx, "reminder sweep failed",
			slog.Int("sent", sent), slog.Any("error", err))
		return
	}
	if sent > 0 {
		d.Log.Info("reminders queued", slog.Int("sent", sent))
	}
}
