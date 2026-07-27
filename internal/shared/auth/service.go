package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/model"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
)

// ErrUserNotFound is returned when a verified token names a user with no local
// mirror row — possible only if the row was deleted between logins.
var ErrUserNotFound = errors.New("auth: no local user for this identity")

// Service is the auth half of the application: token verification, token
// refresh, and the local users mirror.
type Service struct {
	cfg       *config.Config
	store     *repository.Store
	log       *slog.Logger
	refresher Refresher
}

func New(cfg *config.Config, store *repository.Store, log *slog.Logger) *Service {
	return &Service{
		cfg:       cfg,
		store:     store,
		log:       log,
		refresher: NewHTTPRefresher(cfg.Auth),
	}
}

// Verify checks a token against the configured secret.
func (s *Service) Verify(token string) (*Claims, error) {
	return Verify(s.cfg.Auth.Secret, token)
}

// Refresh exchanges an expired token for a fresh one and verifies the result.
//
// The new token is verified rather than trusted: it arrives over the network,
// and an auth service that returned something unsigned would otherwise hand this
// app an unverified identity for the rest of the session.
func (s *Service) Refresh(ctx context.Context, token string) (*Claims, error) {
	fresh, err := s.refresher.Refresh(ctx, token)
	if err != nil {
		return nil, err
	}
	return s.Verify(fresh)
}

// SyncUser mirrors a verified identity into the local users table and returns
// the resulting row. Called on the SSO callback only — it stamps last_login_at.
//
// Read-then-write, so it runs in one transaction per CLAUDE.md's rule: the
// bootstrap promotion decides from the row the upsert just wrote.
func (s *Service) SyncUser(ctx context.Context, c *Claims) (model.User, error) {
	var out model.User

	err := s.store.WithTx(ctx, func(q *sqlc.Queries) error {
		if _, err := q.UpsertUserFromSSO(ctx, sqlc.UpsertUserFromSSOParams{
			AppskepUserID: c.UserID,
			Email:         c.Email,
			Name:          c.Name,
		}); err != nil {
			return fmt.Errorf("upserting user: %w", err)
		}

		// Re-read rather than using the upsert's LastInsertId: on the
		// ON DUPLICATE KEY branch MySQL does not report the existing row's id,
		// so the insert-id is only meaningful for a first login.
		row, err := q.GetUserByAppskepID(ctx, c.UserID)
		if err != nil {
			return fmt.Errorf("reading user back: %w", err)
		}

		// Bootstrap promotion from ADMIN_USER_IDS. It only ever promotes: a
		// demotion made in the Phase 10 user panel must survive the next login,
		// which is also why UpsertUserFromSSO leaves role alone.
		if row.Role != sqlc.UsersRoleAdmin &&
			s.cfg.Auth.IsBootstrapAdmin(strconv.FormatUint(c.UserID, 10)) {
			if err := q.SetUserRole(ctx, sqlc.SetUserRoleParams{
				Role: sqlc.UsersRoleAdmin,
				ID:   row.ID,
			}); err != nil {
				return fmt.Errorf("promoting bootstrap admin: %w", err)
			}
			row.Role = sqlc.UsersRoleAdmin
			s.log.InfoContext(ctx, "auth: bootstrapped admin from ADMIN_USER_IDS",
				slog.Uint64("appskep_user_id", c.UserID))
		}

		out = model.UserFromSQLC(row)
		return nil
	})
	if err != nil {
		return model.User{}, err
	}
	return out, nil
}

// LoadUser reads the local mirror for an already-verified identity.
//
// Called on every authenticated request rather than caching the row in the
// session: role and is_active are the two things an admin changes expecting an
// immediate effect, and a cached copy would keep a demoted or deactivated user
// working until their cookie expired. It is one indexed lookup on a unique key.
func (s *Service) LoadUser(ctx context.Context, appskepUserID uint64) (model.User, error) {
	row, err := s.store.Queries.GetUserByAppskepID(ctx, appskepUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, ErrUserNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	return model.UserFromSQLC(row), nil
}
