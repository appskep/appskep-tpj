package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Users is the admin view of the local users mirror.
//
// Identity — name and email — belongs to Appskep and is refreshed from the JWT
// on every login (auth.Service.SyncUser). Nothing here writes it, and there is
// no local credential to reset. What an operator can change is exactly two
// locally-owned flags: role and is_active.
//
// Both of those can lock people out, so both go through the guards below rather
// than through Store.Queries at a handler. The guards are here, in the service,
// because they are rules about the system and not about one page.
type Users struct {
	store *repository.Store
}

func NewUsers(store *repository.Store) *Users {
	return &Users{store: store}
}

// UserListQuery is one page of the user list.
type UserListQuery struct {
	Search   string
	Page     int
	PageSize int
}

// UserRow is one listed user plus the count that tells an operator whether the
// row is a real customer before they deactivate it.
type UserRow struct {
	User     sqlc.User
	Bookings int64
}

// UserListResult is that page plus what the pagination partial needs.
type UserListResult struct {
	Items      []UserRow
	Page       int
	TotalPages int
	Total      int64
}

// List returns one page of mirrored users.
//
// The per-row booking count is N+1 queries against a page of twenty, which is
// the wrong shape at scale and the right one here: the alternative is a GROUP BY
// join on every list read to serve a number that only matters on the row an
// operator is about to act on. Revisit if the mirror ever grows past a few
// thousand rows.
func (u *Users) List(ctx context.Context, q UserListQuery) (UserListResult, error) {
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}

	// "%" for an empty box: name and email are NOT NULL, so no SQL branching is
	// needed. EscapeLike keeps a typed % or _ literal.
	search := "%"
	if term := strings.TrimSpace(q.Search); term != "" {
		search = "%" + util.EscapeLike(term) + "%"
	}

	total, err := u.store.Queries.CountUsers(ctx, sqlc.CountUsersParams{Search: search})
	if err != nil {
		return UserListResult{}, fmt.Errorf("counting users: %w", err)
	}

	totalPages := int((total + int64(q.PageSize) - 1) / int64(q.PageSize))
	if totalPages > 0 && q.Page > totalPages {
		q.Page = totalPages
	}

	rows, err := u.store.Queries.ListUsers(ctx, sqlc.ListUsersParams{
		Search: search,
		Limit:  int32(q.PageSize),
		Offset: int32((q.Page - 1) * q.PageSize),
	})
	if err != nil {
		return UserListResult{}, fmt.Errorf("listing users: %w", err)
	}

	items := make([]UserRow, 0, len(rows))
	for _, row := range rows {
		n, err := u.store.Queries.CountBookingsForUser(ctx, row.ID)
		if err != nil {
			return UserListResult{}, fmt.Errorf("counting bookings for user %d: %w", row.ID, err)
		}
		items = append(items, UserRow{User: row, Bookings: n})
	}

	return UserListResult{Items: items, Page: q.Page, TotalPages: totalPages, Total: total}, nil
}

// Get returns one mirrored user, or ErrNotFound.
func (u *Users) Get(ctx context.Context, id int64) (sqlc.User, error) {
	row, err := u.store.Queries.GetUser(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.User{}, ErrNotFound
	}
	if err != nil {
		return sqlc.User{}, fmt.Errorf("getting user %d: %w", id, err)
	}
	return row, nil
}

// SetRole promotes or demotes a user.
//
// A demotion is refused when the actor is demoting themselves (ErrSelfDemotion)
// or when it would empty the admin list (ErrLastAdmin). The count and the write
// are in one transaction: without it, two operators demoting each other
// simultaneously would each see two admins and both succeed, leaving none.
func (u *Users) SetRole(ctx context.Context, actorID, id int64, admin bool) (sqlc.User, error) {
	target, err := u.Get(ctx, id)
	if err != nil {
		return sqlc.User{}, err
	}

	role := sqlc.UsersRoleUser
	if admin {
		role = sqlc.UsersRoleAdmin
	}
	if target.Role == role {
		// Nothing to do. Reported as success so a resent request is idempotent —
		// the toggle posts the value it wants, not a flip.
		return target, nil
	}

	if !admin {
		if id == actorID {
			return sqlc.User{}, ErrSelfDemotion
		}
	}

	err = u.store.WithTx(ctx, func(q *sqlc.Queries) error {
		if !admin && target.IsActive {
			// Only an active admin counts toward the guard, so demoting an already
			// deactivated admin cannot be blocked by it.
			n, cerr := q.CountActiveAdmins(ctx)
			if cerr != nil {
				return fmt.Errorf("counting admins: %w", cerr)
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		if serr := q.SetUserRole(ctx, sqlc.SetUserRoleParams{Role: role, ID: id}); serr != nil {
			return fmt.Errorf("setting role on user %d: %w", id, serr)
		}
		return nil
	})
	if err != nil {
		return sqlc.User{}, err
	}

	return u.Get(ctx, id)
}

// SetActive enables or disables a user's access.
//
// LoadUser runs on every authenticated request, so a deactivation takes effect
// on the target's very next request rather than when their cookie expires. That
// is exactly why the same two guards apply here as to a demotion.
func (u *Users) SetActive(ctx context.Context, actorID, id int64, active bool) (sqlc.User, error) {
	target, err := u.Get(ctx, id)
	if err != nil {
		return sqlc.User{}, err
	}
	if target.IsActive == active {
		return target, nil
	}

	if !active {
		if id == actorID {
			return sqlc.User{}, ErrSelfDemotion
		}
	}

	err = u.store.WithTx(ctx, func(q *sqlc.Queries) error {
		if !active && target.Role == sqlc.UsersRoleAdmin {
			n, cerr := q.CountActiveAdmins(ctx)
			if cerr != nil {
				return fmt.Errorf("counting admins: %w", cerr)
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		if serr := q.SetUserActive(ctx, sqlc.SetUserActiveParams{IsActive: active, ID: id}); serr != nil {
			return fmt.Errorf("setting active on user %d: %w", id, serr)
		}
		return nil
	})
	if err != nil {
		return sqlc.User{}, err
	}

	return u.Get(ctx, id)
}
