package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RunExpiryTicker releases the slots of bookings that were never paid for.
//
// Without it, every abandoned checkout holds its slot forever: booked_count
// keeps climbing, the schedule fills up with reservations nobody intends to
// honour, and the public availability query stops offering times that are in
// fact free. The booking's own expires_at is only a timestamp — something has to
// act on it.
//
// It lives in internal/app for the same reason auth.go and errorpage.go do: it
// needs Deps. main.go runs it on the signal-derived context, so a shutdown
// cancels it and waits for the pass in flight to finish.
//
// A sweep runs immediately on start, before the first tick. Bookings that
// expired while the process was down are exactly the ones nobody is watching,
// and waiting a full interval to notice them serves no purpose.
func (d *Deps) RunExpiryTicker(ctx context.Context) {
	interval := d.Cfg.App.ExpirySweepInterval

	d.Log.Info("expiry ticker started", slog.Duration("interval", interval))

	d.sweepExpired(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.Log.Info("expiry ticker stopped")
			return
		case <-ticker.C:
			d.sweepExpired(ctx)
		}
	}
}

// sweepExpired runs one pass and reports it.
//
// An error is logged and swallowed: a database hiccup must not end the ticker
// for the life of the process, and the next tick retries the same rows anyway
// because their status has not moved.
func (d *Deps) sweepExpired(ctx context.Context) {
	released, err := d.Booking.Expire(ctx)
	if errors.Is(err, context.Canceled) {
		// Shutdown reached the sweep mid-query. Not a failure, and logging it at
		// error level would put a red line in the log of every clean restart.
		return
	}
	if err != nil {
		// released is still meaningful — Expire returns what it managed before
		// failing, and those bookings are committed.
		d.Log.ErrorContext(ctx, "expiry sweep failed",
			slog.Int("released", released), slog.Any("error", err))
		return
	}
	if released > 0 {
		d.Log.Info("expired bookings released", slog.Int("released", released))
	}
}
