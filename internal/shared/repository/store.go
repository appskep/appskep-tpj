// Package repository wraps the generated sqlc queries with the transaction
// helper the booking and payment flows require.
//
// It is the only package besides main that holds a *sql.DB: handlers and
// services take a *Store. That confines raw database access to one place, which
// is what makes "every SQL access goes through sqlc" checkable by reading
// imports.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
)

// Store is the single entry point to the database.
//
// Use Queries directly for a self-contained read or write. Use WithTx whenever a
// flow reads a row, decides something from it, and then writes based on that
// decision — the read and the write belong in one transaction, and the read
// needs SELECT ... FOR UPDATE. See PLAN.md § Design note: slot concurrency.
type Store struct {
	db *sql.DB
	// Queries is a named field rather than embedded: embedding would place the
	// generated Queries.WithTx(*sql.Tx) and this package's Store.WithTx(ctx, fn)
	// at different depths under the same name. That is legal Go, but it silently
	// shadows a real method, and the two do very different things.
	Queries *sqlc.Queries
}

// New wraps an already-open, already-pinged pool. The caller retains ownership
// of the pool and is responsible for closing it.
func New(db *sql.DB) *Store {
	return &Store{db: db, Queries: sqlc.New(db)}
}

// DB exposes the pool for tests and for the rare caller that genuinely needs raw
// access. Application code should not reach for this.
func (s *Store) DB() *sql.DB { return s.db }

// Ping reports database liveness for the health endpoint.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// WithTx runs fn inside one transaction, committing when fn returns nil and
// rolling back otherwise.
func (s *Store) WithTx(ctx context.Context, fn func(q *sqlc.Queries) error) error {
	return s.WithTxOpts(ctx, nil, fn)
}

// WithTxOpts is WithTx with explicit isolation and read-only options.
//
// The booking transaction runs at the default isolation level: SELECT ... FOR
// UPDATE, not the isolation level, is what serialises it.
//
// A panic inside fn rolls the transaction back and is then re-raised, so chi's
// Recoverer still sees it and no connection returns to the pool holding an open
// transaction.
func (s *Store) WithTxOpts(ctx context.Context, opts *sql.TxOptions, fn func(q *sqlc.Queries) error) (err error) {
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("repository: begin tx: %w", err)
	}

	// The named return is what lets this see fn's result: `return fn(...)` below
	// assigns err before the deferred function runs.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				err = fmt.Errorf("%w (rollback failed: %v)", err, rbErr)
			}
			return
		}
		if cErr := tx.Commit(); cErr != nil {
			err = fmt.Errorf("repository: commit tx: %w", cErr)
		}
	}()

	return fn(s.Queries.WithTx(tx))
}
