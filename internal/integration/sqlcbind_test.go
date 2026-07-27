package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/testsupport"
)

// TestEveryReadQueryBinds is Phase 10's ask (PLAN.md:1206).
//
// sqlc's MySQL engine silently mis-binds BETWEEN, in two directions. An
// EXPRESSION on its left — `DATE(x) BETWEEN ? AND ?` — drops both parameters from
// the generated call. A COLUMN NAME SHARED BY SEVERAL JOINED TABLES duplicates
// each parameter once per table, ignoring the qualifier: one draft emitted 20
// arguments for 14 placeholders. Neither form fails at generate time. Both fail
// on the FIRST REQUEST, with an argument-count error — which means the defect
// ships, and is found by a customer.
//
// The check is to call every read query once and look only for that specific
// failure. A query whose zero-value parameters simply match nothing is fine; a
// query whose placeholder count disagrees with its own call is not.
//
// An AST-based counter over the generated files was considered and rejected: more
// code, more ways to be wrong, and it cannot see the params struct that the
// mis-binding actually corrupts. Executing the query is what the engine does.
func TestEveryReadQueryBinds(t *testing.T) {
	env := testsupport.New(t)

	q := sqlc.New(env.Store.DB())
	v := reflect.ValueOf(q)
	typ := v.Type()

	ctx := context.Background()
	ctxValue := reflect.ValueOf(ctx)

	var checked, skipped int
	for i := range typ.NumMethod() {
		m := typ.Method(i)

		if !isReadQuery(m.Name) {
			skipped++
			continue
		}
		// (receiver, ctx) or (receiver, ctx, params).
		if m.Type.NumIn() < 2 || m.Type.NumIn() > 3 {
			skipped++
			continue
		}
		if m.Type.In(1) != reflect.TypeOf((*context.Context)(nil)).Elem() {
			skipped++
			continue
		}

		t.Run(m.Name, func(t *testing.T) {
			args := []reflect.Value{ctxValue}
			if m.Type.NumIn() == 3 {
				// The zero value of whatever the query takes: a scalar, or the
				// generated params struct whose field count is exactly what a
				// mis-binding gets wrong.
				args = append(args, reflect.Zero(m.Type.In(2)))
			}

			out := v.Method(i).Call(args)
			err, _ := out[len(out)-1].Interface().(error)
			if err == nil {
				return
			}
			if isArgumentCountError(err) {
				t.Fatalf("%s: %v\n\n"+
					"This is sqlc's silent BETWEEN mis-binding, or something like it: the "+
					"generated SQL's placeholder count disagrees with the arguments its own "+
					"call passes. Rewrite any BETWEEN as `col >= ? AND col <= ?` and re-run "+
					"`make sqlc`.", m.Name, err)
			}
			// Everything else is expected: ErrNoRows for a Get with a zero id, a
			// range that matches nothing, a NULL that will not scan into a
			// zero-value target. None of those is what this test is for.
			t.Logf("tolerated: %v", err)
		})
		checked++
	}

	if checked < 50 {
		t.Errorf("only %d read queries were exercised (%d skipped) — the reflection "+
			"filter has stopped matching the generated names", checked, skipped)
	}
	t.Logf("exercised %d read queries, skipped %d writes and helpers", checked, skipped)
}

// isReadQuery keeps this to queries that cannot change data.
//
// A prefix whitelist rather than a blacklist: a future Delete* or Truncate* must
// be excluded by default, because this calls every match with zero-value
// parameters and a WHERE that matched everything would be a very bad afternoon.
func isReadQuery(name string) bool {
	// GetSlotForUpdate takes a row lock, and outside a transaction that is a
	// lock held for the length of one statement. Harmless, but there is no reason
	// to reach for it here.
	if strings.HasSuffix(name, "ForUpdate") {
		return false
	}

	for _, prefix := range []string{"Get", "List", "Count", "Exists"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	// BookingCodeTaken and friends: a read whose name reads as a question.
	return strings.HasSuffix(name, "Taken")
}

// isArgumentCountError matches database/sql's own complaint, which is the only
// symptom a mis-bound query has.
func isArgumentCountError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "expected") && strings.Contains(msg, "arguments")
}

// TestQuerierIsComplete guards the reflection above: if sqlc ever stopped
// emitting the interface, the test would still pass while checking nothing.
func TestQuerierIsComplete(t *testing.T) {
	var q sqlc.Querier = (*sqlc.Queries)(nil)
	if q == nil {
		t.Fatal("sqlc.Queries no longer implements Querier")
	}

	n := reflect.TypeOf((*sqlc.Querier)(nil)).Elem().NumMethod()
	if n < 100 {
		t.Errorf("Querier has %d methods; the generated set has shrunk unexpectedly", n)
	}
}
