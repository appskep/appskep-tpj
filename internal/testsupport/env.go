package testsupport

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/upload"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// TestAuthSecret signs the tokens the guard tests mint. It is a test constant, so
// it may live in the source; the real one comes from AUTH_SECRET and is never
// read here.
const TestAuthSecret = "test-auth-secret-not-a-real-one"

// TestSessionKey signs the session cookie. 32 bytes exactly, which is also
// NewSessionManager's minimum — so a change that loosened that check would break
// nothing here, but a change that tightened it would show up immediately.
const TestSessionKey = "test-session-key-0123456789abcde"

// TestServerKey is the Midtrans server key the webhook signatures are computed
// with. Sandbox keys are real credentials, if low-value ones, and a test does not
// need one: the SHA512 is over whatever string both sides use.
const TestServerKey = "SB-Mid-server-test-key"

// Env is the whole application object graph, wired the way main.go wires it, with
// the two external calls replaced by fakes and the logger pointed at a buffer.
type Env struct {
	T     *testing.T
	Cfg   *config.Config
	Store *repository.Store
	Deps  *app.Deps

	// Gateway is the Midtrans stand-in reachable from Deps.Payment.
	Gateway *FakeGateway
	// Mail is the mail.Sender behind Deps.Email.
	Mail *RecordingSender

	// AuthServer, when a test sets one up, is the httptest.Server standing in for
	// dev-auth.appskep.id. Stored so the refresh tests can adjust it.
	AuthURL string

	logMu  *sync.Mutex
	logBuf *bytes.Buffer
}

// New builds a fresh Env over the shared test database, resetting the data first
// and again on cleanup.
//
// The order below mirrors cmd/server/main.go's run() deliberately. If a future
// phase adds a dependency there and not here, a test that exercises it panics on
// a nil field rather than passing against a graph that no longer resembles the
// real one.
func New(t *testing.T) *Env {
	t.Helper()
	SkipIfShort(t)

	store := Store(t)
	Reset(t, store)

	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("testsupport: %v", err)
	}

	cfg := Config(t)

	// A buffer rather than os.Stdout, so a test can assert on what was logged.
	// Two of the checks every phase made — "access_token appears zero times in the
	// log" and "exactly one ERROR line" — only become permanent tests this way.
	logBuf := &bytes.Buffer{}
	logMu := &sync.Mutex{}
	log := slog.New(slog.NewTextHandler(&lockedWriter{mu: logMu, w: logBuf},
		&slog.HandlerOptions{Level: slog.LevelDebug}))

	sessions, err := auth.NewSessionManager(cfg.Session)
	if err != nil {
		t.Fatalf("testsupport: session manager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	settings, err := service.NewSettings(ctx, store)
	if err != nil {
		t.Fatalf("testsupport: settings: %v", err)
	}

	// t.TempDir, so an upload test writes into a directory the framework removes
	// and never into the repository's static/uploads.
	uploadRoot := t.TempDir()
	images, err := upload.New(uploadRoot, "uploads/services", 2<<20)
	if err != nil {
		t.Fatalf("testsupport: service image store: %v", err)
	}
	avatars, err := upload.New(uploadRoot, "uploads/avatars", 1<<20)
	if err != nil {
		t.Fatalf("testsupport: avatar store: %v", err)
	}

	// The real template tree, addressed from the repository root rather than
	// relatively: view.New requires both layouts and at least one page under each,
	// and both error.html files have to resolve for the 403 and 500 assertions.
	templates := os.DirFS(filepath.Join(root, "template"))

	renderer, err := view.New(templates, cfg, log, settings)
	if err != nil {
		t.Fatalf("testsupport: renderer: %v", err)
	}

	schedule := service.NewSchedule(store, settings, cfg.App.Location)
	gateway := NewFakeGateway()
	sender := NewRecordingSender()

	email, err := service.NewEmail(templates, store, sender, settings, log, service.EmailConfig{
		AppURL:       cfg.App.URL,
		Location:     cfg.App.Location,
		ReminderHour: cfg.App.ReminderHour,
		Development:  cfg.IsDevelopment(),
	})
	if err != nil {
		t.Fatalf("testsupport: email: %v", err)
	}

	deps := &app.Deps{
		Cfg:      cfg,
		Store:    store,
		Log:      log,
		View:     renderer,
		Settings: settings,
		Catalog:  service.NewCatalog(store, images),
		Schedule: schedule,
		Booking: service.NewBooking(store, settings, schedule, email,
			cfg.Midtrans.ExpiryMinutes, cfg.App.Location),
		Payment: service.NewPayment(store, gateway, log, email,
			cfg.Midtrans, cfg.App.URL, cfg.App.Location),
		Profile:   service.NewProfile(store, avatars),
		Users:     service.NewUsers(store),
		Dashboard: service.NewDashboard(store, log, cfg.App.Location),
		Audit:     service.NewAudit(store, log),
		Email:     email,
		Auth:      auth.New(cfg, store, log),
		Session:   sessions,
		Limits:    app.NewLimits(cfg),
	}

	return &Env{
		T:       t,
		Cfg:     cfg,
		Store:   store,
		Deps:    deps,
		Gateway: gateway,
		Mail:    sender,
		AuthURL: cfg.Auth.URL,
		logMu:   logMu,
		logBuf:  logBuf,
	}
}

// Config builds the configuration a test runs against.
//
// A struct literal, not config.Load(). Load reads .env relative to the working
// directory — the package directory under `go test` — and validate() then demands
// AUTH_SECRET and a 32-byte SESSION_KEY from the environment. Building the value
// directly also means a test can change one field without an env var, which is
// what keeps these tests runnable in parallel.
//
// Two fields exist only because Load computes them: DB.Loc and DB.TZOffset. A nil
// Loc makes the driver reject every DATETIME; an empty TZOffset sends an empty
// SET time_zone and the session clock stops agreeing with Go's.
func Config(t *testing.T) *config.Config {
	t.Helper()

	dbCfg, err := loadDBSettings()
	if err != nil {
		t.Fatalf("testsupport: %v", err)
	}

	return &config.Config{
		// Neither production nor development: templates parse once instead of on
		// every request, HSTS stays off, template errors are not echoed to the
		// browser, and robots.txt still serves Disallow: / — which is the correct
		// default and leaves the production branch to the one test that wants it.
		Env: "test",
		Server: config.ServerConfig{
			Host:            "127.0.0.1",
			Port:            "0",
			ReadTimeout:     15 * time.Second,
			WriteTimeout:    30 * time.Second,
			IdleTimeout:     60 * time.Second,
			ShutdownTimeout: 15 * time.Second,
			MaxBodyBytes:    4 << 20,
			TrustedProxies:  nil,

			RateLimitBooking: 20,
			RateLimitPayment: 20,
			RateLimitWebhook: 120,
		},
		App: config.AppConfig{
			Name:     "Terapi Pemuda Jompo",
			URL:      "http://localhost:8080",
			PageSize: 20,
			TZ:       dbCfg.Loc.String(),
			Location: dbCfg.Loc,

			ExpirySweepInterval: time.Minute,
			// Zero, so SweepReminders is past its hour at any time of day. The hour
			// gate is tested on its own by setting this above the current hour.
			ReminderHour:          0,
			ReminderSweepInterval: 15 * time.Minute,
		},
		DB: config.DBConfig{
			Host:            dbCfg.Host,
			Port:            dbCfg.Port,
			User:            dbCfg.User,
			Password:        dbCfg.Password,
			Name:            dbCfg.Name,
			MaxOpenConns:    40,
			MaxIdleConns:    10,
			ConnMaxLifetime: 5 * time.Minute,
			Loc:             dbCfg.Loc,
			TZOffset:        dbCfg.TZOffset,
		},
		Auth: config.AuthConfig{
			URL:      "https://dev-auth.example",
			Secret:   TestAuthSecret,
			ClientID: "19",
			// Empty on purpose. The bootstrap promotion is a Phase 3 path with its
			// own test; leaving it off here means a fixture user is never silently
			// made an admin.
			AdminUserIDs: nil,
		},
		Session: config.SessionConfig{
			Key:    TestSessionKey,
			Name:   "tpj_session",
			MaxAge: 24 * time.Hour,
			Secure: false,
		},
		Midtrans: config.MidtransConfig{
			Env:           "midtrans.Sandbox",
			ServerKey:     TestServerKey,
			ClientKey:     "SB-Mid-client-test-key",
			Prefix:        "tpj",
			ExpiryMinutes: 60,
			Timeout:       15 * time.Second,
		},
		SMTP: config.SMTPConfig{
			// Deliberately not Enabled(): the sender is injected directly, so nothing
			// here should be able to reach a real mail server.
			FromName:  "Terapi Pemuda Jompo",
			FromEmail: "",
			Timeout:   10 * time.Second,
		},
	}
}

// MustPrefixes parses a trusted-proxy list for a test that needs one.
func MustPrefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()

	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			addr, aerr := netip.ParseAddr(v)
			if aerr != nil {
				t.Fatalf("testsupport: %q is not an IP or CIDR: %v", v, err)
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		out = append(out, p)
	}
	return out
}

// Logs returns everything logged so far.
func (e *Env) Logs() string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.logBuf.String()
}

// ClearLogs forgets what has been logged, so an assertion about a single line
// does not have to account for the boot noise before it.
func (e *Env) ClearLogs() {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	e.logBuf.Reset()
}

// lockedWriter serialises writes to the log buffer. slog's handler is safe for
// concurrent use but a bytes.Buffer is not, and the concurrency tests log from
// twenty goroutines under -race.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
