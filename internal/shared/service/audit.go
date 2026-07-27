package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
)

// Audit writes the activity_logs trail behind every admin action.
//
// The one rule that governs this type: **an audit write can never fail the
// action it records.** A cancelled booking that reports failure because its log
// line did not insert would leave the operator retrying an action that already
// succeeded — and a retry of a cancellation is harmless only because the status
// guards make it so. Every method here logs its own failure at WARN and returns
// nothing.
//
// It is deliberately not part of the transaction it describes either. The
// booking transactions in this codebase take slot locks and are kept as short as
// possible; an INSERT into a table nothing reads under lock would widen them for
// no benefit, and a rolled-back action leaves no log entry to explain, because
// nothing happened.
type Audit struct {
	store *repository.Store
	log   *slog.Logger
}

func NewAudit(store *repository.Store, log *slog.Logger) *Audit {
	return &Audit{store: store, log: log}
}

// Entities an activity_logs row can point at. Named constants so the timeline
// query and the writers cannot disagree about the spelling.
const (
	EntityBooking = "booking"
	EntityPayment = "payment"
	EntityUser    = "user"
	EntitySetting = "setting"
)

// Actions, in the same spirit. They are rendered to the operator by the
// timeline, so each maps to a label in the admin templates.
const (
	ActionBookingConfirm    = "booking.confirm"
	ActionBookingComplete   = "booking.complete"
	ActionBookingCancel     = "booking.cancel"
	ActionBookingReschedule = "booking.reschedule"
	ActionBookingNotes      = "booking.notes"
	ActionPaymentSync       = "payment.sync"
	ActionUserRole          = "user.role"
	ActionUserActive        = "user.active"
	ActionSettingsUpdate    = "settings.update"
)

// Entry is one audit record. Meta is arbitrary JSON-serialisable detail — the
// old and new value of whatever changed.
type Entry struct {
	// ActorID is the local users.id of the admin who acted. Zero is written as
	// NULL, which is what a system action (the expiry ticker, a webhook) would
	// use if one ever logged here.
	ActorID  int64
	Action   string
	Entity   string
	EntityID int64
	Meta     any
	IP       string
}

// Record writes one entry. It never returns an error — see the type comment.
//
// The context is the caller's, so a request that is already finishing will
// abandon the write; that is the correct trade for a log line, and the WARN
// records it either way.
func (a *Audit) Record(ctx context.Context, e Entry) {
	var meta sql.NullString
	if e.Meta != nil {
		// A JSON column in MariaDB is LONGTEXT with an implicit json_valid()
		// check, so an unserialisable value must become NULL rather than an
		// empty string that the constraint would reject.
		if raw, err := json.Marshal(e.Meta); err == nil {
			meta = sql.NullString{String: string(raw), Valid: true}
		} else {
			a.log.WarnContext(ctx, "audit: meta not serialisable",
				slog.String("action", e.Action), slog.Any("error", err))
		}
	}

	var actor sql.NullInt64
	if e.ActorID > 0 {
		actor = sql.NullInt64{Int64: e.ActorID, Valid: true}
	}

	var entity sql.NullString
	if e.Entity != "" {
		entity = sql.NullString{String: e.Entity, Valid: true}
	}

	var entityID sql.NullInt64
	if e.EntityID > 0 {
		entityID = sql.NullInt64{Int64: e.EntityID, Valid: true}
	}

	var ip sql.NullString
	if e.IP != "" {
		ip = sql.NullString{String: e.IP, Valid: true}
	}

	if err := a.store.Queries.CreateActivityLog(ctx, sqlc.CreateActivityLogParams{
		UserID:   actor,
		Action:   e.Action,
		Entity:   entity,
		EntityID: entityID,
		Meta:     meta,
		Ip:       ip,
	}); err != nil {
		a.log.WarnContext(ctx, "audit: writing activity log",
			slog.String("action", e.Action),
			slog.String("entity", e.Entity),
			slog.Int64("entity_id", e.EntityID),
			slog.Any("error", err))
	}
}
