package repository

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// The booking flow has to tell "this user already booked this slot" — which
// deserves a friendly Indonesian message — from "the database is broken", which
// is a 500. Both arrive as a bare error, so these helpers classify them.

// MariaDB / MySQL server error numbers this package branches on.
const (
	errDupEntry         = 1062 // ER_DUP_ENTRY
	errLockWaitTimeout  = 1205 // ER_LOCK_WAIT_TIMEOUT
	errLockDeadlock     = 1213 // ER_LOCK_DEADLOCK
	errRowIsReferenced  = 1451 // ER_ROW_IS_REFERENCED_2 — delete blocked by an FK
	errNoReferencedRow  = 1452 // ER_NO_REFERENCED_ROW_2 — insert naming a missing parent
	errCheckConstraint  = 3819 // MySQL 8 ER_CHECK_CONSTRAINT_VIOLATED
	errConstraintFailed = 4025 // MariaDB ER_CONSTRAINT_FAILED
)

func mysqlErrNo(err error) (uint16, bool) {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number, true
	}
	return 0, false
}

// IsDuplicateKey reports whether err is a unique-index violation.
func IsDuplicateKey(err error) bool {
	n, ok := mysqlErrNo(err)
	return ok && n == errDupEntry
}

// IsDuplicateKeyOn reports a unique-index violation naming a specific index.
//
// The booking flow uses this to map a violation of uq_bookings_active_slot_user
// to "Anda sudah memiliki booking untuk slot ini." rather than a generic error.
func IsDuplicateKeyOn(err error, index string) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == errDupEntry &&
		strings.Contains(me.Message, index)
}

// IsRetryable reports a deadlock or lock-wait timeout. The booking transaction
// may be retried once when this is true; anything else must surface.
func IsRetryable(err error) bool {
	n, ok := mysqlErrNo(err)
	return ok && (n == errLockDeadlock || n == errLockWaitTimeout)
}

// IsForeignKeyViolation reports a delete blocked by a child row, or an insert
// naming a parent that does not exist. Deleting a service that has bookings
// lands here.
func IsForeignKeyViolation(err error) bool {
	n, ok := mysqlErrNo(err)
	return ok && (n == errRowIsReferenced || n == errNoReferencedRow)
}

// IsCheckViolation reports a CHECK constraint failure — for example
// chk_slots_count when an admin tries to shrink a slot's capacity below its
// booked_count.
func IsCheckViolation(err error) bool {
	n, ok := mysqlErrNo(err)
	return ok && (n == errCheckConstraint || n == errConstraintFailed)
}
