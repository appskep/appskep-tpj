// Package testsupport is the Phase 13 test harness: a real MariaDB behind a
// *repository.Store, the whole application object graph with the two external
// calls faked, and the fixtures and assertions the tests share.
//
// It exists because there is no interface over the database. repository.Store is
// a concrete struct and WithTx hands out a concrete *sqlc.Queries, so the
// invariants this system is built around — the canonical lock order, the
// REPEATABLE READ snapshot rule that makes GetSlotForUpdate the literal first
// statement, the guarded decrement on release — cannot be reproduced by a mock
// at all. They are properties of InnoDB, and testing them needs InnoDB.
//
// Nothing here is imported by application code. It lives under internal/ and is
// reached only from _test.go files.
package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"

	"github.com/remorac/appskep-tpj/internal/shared/repository"
)

// defaultTestDBName is the database the harness owns. It is dropped and
// recreated once per test binary, so it must never be the development database.
const defaultTestDBName = "appskep_tpj_test"

// testDBSuffix is the guard that makes the drop safe. Store refuses any name
// that does not end in it, because TEST_DB_NAME is an environment variable and a
// mis-set one must not be able to reach appskep_tpj.
const testDBSuffix = "_test"

var (
	once   sync.Once
	shared *repository.Store
	// seedDB is a second pool with multiStatements on, kept for the lifetime of
	// the test binary so Reset can re-pipe seed_dev.sql in one Exec.
	//
	// Splitting the file in Go was tried and thrown away: "Copy is verbatim from
	// specification.txt; prices confirmed with the client" puts a semicolon inside
	// a comment, and a splitter that handles that correctly is the beginning of a
	// SQL parser. The server already has one.
	seedDB   *sql.DB
	setupErr error
)

// Store returns the process-wide test store, creating the database and applying
// the schema and the development seed on first use.
//
// One database per test binary rather than one per test: every DB-backed test in
// this project lives in internal/integration, so there is exactly one binary
// that needs it, and Reset is what isolates the tests inside it.
//
// A database that cannot be reached is a skip under -short and a failure
// otherwise. That asymmetry is the point: `make test` does not pass -short, so a
// green `make test` can never mean the concurrency test quietly did not run.
func Store(t *testing.T) *repository.Store {
	t.Helper()

	once.Do(func() { shared, setupErr = openTestDB() })

	if setupErr != nil {
		if testing.Short() {
			t.Skipf("testsupport: skipping DB test in -short mode: %v", setupErr)
		}
		t.Fatalf("testsupport: test database unavailable: %v\n\n"+
			"Start MariaDB and check DB_HOST/DB_PORT/DB_USER/DB_PASSWORD in .env, "+
			"or run `make test-unit` to skip every DB-backed test.", setupErr)
	}
	return shared
}

// SkipIfShort skips a DB-backed test under -short without touching the database.
// Tests call this first so -short never pays for a connection attempt.
func SkipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("testsupport: DB-backed test skipped in -short mode")
	}
}

// dbSettings is the connection information, read from the repository root .env.
//
// This is the one place in the test tree that reads the environment, and it
// reads it for credentials only. CLAUDE.md's rule that config.Load is the only
// reader of os.Getenv is about application settings; the harness builds a
// config.Config literal rather than calling Load, because Load resolves .env
// relative to the working directory — which under `go test` is the package
// directory, not the repository root.
type dbSettings struct {
	Host, Port, User, Password, Name string
	Loc                              *time.Location
	TZOffset                         string
}

func loadDBSettings() (dbSettings, error) {
	root, err := RepoRoot()
	if err != nil {
		return dbSettings{}, err
	}

	// Explicit path, and errors ignored: a missing .env is not fatal, the same
	// contract config.Load has. Real values then come from the environment.
	_ = godotenv.Load(filepath.Join(root, ".env"))

	loc, err := time.LoadLocation(env("APP_TZ", "Asia/Jakarta"))
	if err != nil {
		return dbSettings{}, fmt.Errorf("APP_TZ: %w", err)
	}

	name := env("TEST_DB_NAME", defaultTestDBName)
	if !strings.HasSuffix(name, testDBSuffix) {
		return dbSettings{}, fmt.Errorf(
			"TEST_DB_NAME=%q must end in %q — this harness DROPs the database it is given",
			name, testDBSuffix)
	}

	return dbSettings{
		Host:     env("DB_HOST", "127.0.0.1"),
		Port:     env("DB_PORT", "3306"),
		User:     env("DB_USER", "root"),
		Password: env("DB_PASSWORD", ""),
		Name:     name,
		Loc:      loc,
		TZOffset: utcOffset(loc),
	}, nil
}

// dsn builds a connection string. Two things differ from config.DBConfig.DSN:
// dbName may be empty (to CREATE DATABASE), and multiStatements may be on.
//
// The app pins MultiStatements = false, which is correct for it and means it
// cannot pipe 0001_schema.sql in one Exec. The harness needs to, so it builds
// its own. Everything else — parseTime, the collation, the pinned session
// time_zone — matches the application exactly, because a test running on a
// different clock than the app would prove nothing about the expiry ticker.
func (s dbSettings) dsn(dbName string, multiStatements bool) string {
	c := mysql.NewConfig()
	c.User = s.User
	c.Passwd = s.Password
	c.Net = "tcp"
	c.Addr = s.Host + ":" + s.Port
	c.DBName = dbName
	c.ParseTime = true
	c.Loc = s.Loc
	c.Collation = "utf8mb4_unicode_ci"
	c.MultiStatements = multiStatements
	c.Params = map[string]string{"time_zone": "'" + s.TZOffset + "'"}
	return c.FormatDSN()
}

func openTestDB() (*repository.Store, error) {
	cfg, err := loadDBSettings()
	if err != nil {
		return nil, err
	}

	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Step one: a connection with no database selected, to drop and recreate.
	admin, err := sql.Open("mysql", cfg.dsn("", true))
	if err != nil {
		return nil, err
	}
	defer admin.Close()

	if err := admin.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connecting to MariaDB at %s:%s as %q: %w",
			cfg.Host, cfg.Port, cfg.User, err)
	}

	// Dropped, not truncated: 0001_schema.sql is the single source of truth until
	// go-live and `make migrate` is deliberately blind to edits, so a stale test
	// database would silently test yesterday's schema.
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+cfg.Name+"`"); err != nil {
		return nil, fmt.Errorf("dropping %s: %w", cfg.Name, err)
	}
	if _, err := admin.ExecContext(ctx,
		"CREATE DATABASE `"+cfg.Name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return nil, fmt.Errorf("creating %s: %w", cfg.Name, err)
	}

	// Step two: the schema, in one Exec. Safe because 0001_schema.sql is pure DDL
	// — every statement is CREATE TABLE IF NOT EXISTS, with no routine, trigger or
	// DELIMITER block to confuse a multi-statement send.
	migrate, err := sql.Open("mysql", cfg.dsn(cfg.Name, true))
	if err != nil {
		return nil, err
	}
	migrate.SetMaxOpenConns(2)

	for _, file := range []string{"0001_schema.sql", "seed_dev.sql"} {
		body, err := readMigration(root, file)
		if err != nil {
			migrate.Close()
			return nil, err
		}
		if _, err := migrate.ExecContext(ctx, body); err != nil {
			migrate.Close()
			return nil, fmt.Errorf("applying %s: %w", file, err)
		}
	}
	// Kept, not closed: Reset re-pipes the seed through it before and after every
	// test in the binary.
	seedDB = migrate

	// Step three: the pool the tests actually use, with the application's own
	// single-statement DSN so nothing a test does could work only because
	// multi-statement was on.
	db, err := sql.Open("mysql", cfg.dsn(cfg.Name, false))
	if err != nil {
		return nil, err
	}
	// Well above the widest concurrency test (20 goroutines racing one slot);
	// too few and the racers would queue on the pool instead of on the row lock,
	// which is the thing under test.
	db.SetMaxOpenConns(40)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return repository.New(db), nil
}

// Reset returns the database to its seeded state and registers itself to run
// again when the test finishes.
//
// Running both before and after is deliberate: a test that fails partway leaves
// rows behind, and the next test must not inherit them however the previous one
// ended.
func Reset(t *testing.T, store *repository.Store) {
	t.Helper()
	reset(t, store)
	t.Cleanup(func() { reset(t, store) })
}

func reset(t *testing.T, store *repository.Store) {
	t.Helper()

	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("testsupport: %v", err)
	}
	seed, err := readMigration(root, "seed_dev.sql")
	if err != nil {
		t.Fatalf("testsupport: %v", err)
	}

	ctx := context.Background()
	db := store.DB()

	// FK checks off so the truncation order does not matter. Every one of these
	// tables is written by tests; users, services, schedule_slots and settings are
	// restored by re-running the seed below instead, because they carry the rows
	// the fixtures resolve by slug and by appskep_user_id.
	stmts := []string{
		"SET FOREIGN_KEY_CHECKS = 0",
		"TRUNCATE TABLE activity_logs",
		"TRUNCATE TABLE payment_notifications",
		"TRUNCATE TABLE payments",
		"TRUNCATE TABLE bookings",
		// Slots created by a test are removed; the seeded ones come back below.
		"DELETE FROM schedule_slots",
		// Users created by a test are removed. The seeded admin (appskep_user_id=1)
		// comes back with the seed.
		"DELETE FROM users",
		"DELETE FROM services",
		// Explicitly, not by relying on ON DELETE CASCADE: FOREIGN_KEY_CHECKS = 0
		// above disables cascades as well as checks, so deleting the parents alone
		// would leave orphan join rows pointing at ids the re-seed then recycles.
		"TRUNCATE TABLE therapist_services",
		"DELETE FROM therapists",
		// Settings must be deleted, not left to the seed.
		//
		// seed_dev.sql ends its settings INSERT with `ON DUPLICATE KEY UPDATE
		// setting_key = setting_key` — a deliberate no-op, because those rows are
		// edited in /admin/pengaturan and a re-seed must not undo an operator's
		// work. That makes the seed useless as a restore here: a test that lowers
		// booking_max_days_ahead would leave it lowered for every test after it,
		// and the failure lands somewhere else entirely. (It did: a booking fixture
		// 20 days out started failing only when the settings test ran first.)
		"DELETE FROM settings",
		"SET FOREIGN_KEY_CHECKS = 1",
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("testsupport: reset %q: %v", s, err)
		}
	}

	// The seed is idempotent by construction (INSERT IGNORE / ON DUPLICATE KEY),
	// which is what makes it usable as a restore. In one Exec through the
	// multi-statement pool, for the reason seedDB documents.
	if _, err := seedDB.ExecContext(ctx, seed); err != nil {
		t.Fatalf("testsupport: reseeding: %v", err)
	}
}

// readMigration reads one file from internal/database/migration.
func readMigration(root, file string) (string, error) {
	path := filepath.Join(root, "internal", "database", "migration", file)
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(body), nil
}

// RepoRoot walks up from the working directory to the module root.
//
// Under `go test` the working directory is the package directory, so a relative
// path to template/ or internal/database/migration/ differs per package. Every
// path in this harness is built from here instead.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("testsupport: no go.mod above %q", dir)
		}
		dir = parent
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return fallback
}

// utcOffset mirrors config.utcOffset, which is unexported. The value pins the
// MySQL session clock, and the test pool must pin it the same way the app does.
func utcOffset(loc *time.Location) string {
	_, secs := time.Now().In(loc).Zone()
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	return fmt.Sprintf("%s%02d:%02d", sign, secs/3600, (secs%3600)/60)
}
