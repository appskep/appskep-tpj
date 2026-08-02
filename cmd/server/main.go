// Command server is the Appskep TPJ monolith: public website, admin panel, and
// internal JSON API served from one process.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	_ "github.com/go-sql-driver/mysql"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/app/admin"
	"github.com/remorac/appskep-tpj/internal/app/api"
	"github.com/remorac/appskep-tpj/internal/app/public"
	"github.com/remorac/appskep-tpj/internal/shared/auth"
	"github.com/remorac/appskep-tpj/internal/shared/config"
	"github.com/remorac/appskep-tpj/internal/shared/mail"
	appmw "github.com/remorac/appskep-tpj/internal/shared/middleware"
	"github.com/remorac/appskep-tpj/internal/shared/payment"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/upload"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// templateDir is where the renderer reads templates from.
//
// os.DirFS rather than an embed.FS so a template edit is picked up without a
// rebuild in development. Phase 14 swaps this for //go:embed to ship a standalone
// binary; nothing outside this line changes when it does.
const templateDir = "template"

// Where uploaded images live. staticDir is what the static file server serves,
// so a stored path of "uploads/services/x.jpg" is reachable at
// /static/uploads/services/x.jpg — the shape the templates already render.
const (
	staticDir       = "static"
	serviceImageDir = "uploads/services"
	// maxImageBytes is PLAN.md Phase 4's 2 MB cap on a service image.
	maxImageBytes = 2 << 20

	avatarImageDir = "uploads/avatars"
	// maxAvatarBytes is half the service cap: an avatar renders at 28px in the
	// header chip, so 1 MB is already far more than it can ever show.
	maxAvatarBytes = 1 << 20
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	store := repository.New(db)

	// The session codec is built before anything can serve: a SESSION_KEY too
	// short to sign with is a failed boot, not a warning on the first login.
	sessions, err := auth.NewSessionManager(cfg.Session)
	if err != nil {
		return err
	}

	// Settings are read once into memory here: every page render needs the site
	// name and contact block, and a failure to load them should stop the boot
	// rather than quietly render the whole site with fallback copy.
	settingsCtx, cancelSettings := context.WithTimeout(context.Background(), 5*time.Second)
	settings, err := service.NewSettings(settingsCtx, store)
	cancelSettings()
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	// The uploads directory is created here, so a path that cannot be written is
	// a failed boot with a clear message rather than a failed upload for the
	// first admin who tries one.
	images, err := upload.New(staticDir, serviceImageDir, maxImageBytes)
	if err != nil {
		return err
	}

	avatars, err := upload.New(staticDir, avatarImageDir, maxAvatarBytes)
	if err != nil {
		return err
	}

	// A stylesheet older than the templates it dresses is a failed boot in
	// production, for the same reason a malformed template is: it is a defect no
	// request will report. See checkStylesheetFresh.
	if err := checkStylesheetFresh(cfg, log); err != nil {
		return err
	}

	// Parsing every template at startup makes a malformed template a failed boot
	// instead of a 500 on the request that first reaches it.
	renderer, err := view.New(os.DirFS(templateDir), cfg, log, settings)
	if err != nil {
		return err
	}

	schedule := service.NewSchedule(store, settings, cfg.App.Location)

	// The Midtrans client holds no connection and opens none until a payment is
	// started, so a sandbox that is down does not stop the site from booting —
	// it stops exactly the one request that needs it.
	gateway := payment.NewMidtrans(cfg.Midtrans)

	// Email is off, not broken, when SMTP is unconfigured: a development instance
	// and a staging one that has no mail credentials yet must both run the whole
	// booking flow. The one INFO line here is how that is visible in production.
	var sender mail.Sender = mail.NewNoOp(log)
	if cfg.SMTP.Enabled() {
		sender = mail.NewSMTP(cfg.SMTP)
		log.Info("email enabled",
			slog.String("host", cfg.SMTP.Host),
			slog.Int("port", cfg.SMTP.Port),
			slog.String("from", cfg.SMTP.FromEmail))
	} else {
		log.Info("email disabled: SMTP_HOST or SMTP_FROM_EMAIL is unset")
	}

	// Parsed here for the same reason the pages are: a malformed email template
	// is a failed boot rather than a notification nobody notices going missing.
	email, err := service.NewEmail(os.DirFS(templateDir), store, sender, settings, log, service.EmailConfig{
		AppURL:       cfg.App.URL,
		Location:     cfg.App.Location,
		ReminderHour: cfg.App.ReminderHour,
		Development:  cfg.IsDevelopment(),
	})
	if err != nil {
		return err
	}

	deps := &app.Deps{
		Cfg:      cfg,
		Store:    store,
		Log:      log,
		View:     renderer,
		Settings: settings,
		Catalog:  service.NewCatalog(store, images),
		Schedule: schedule,
		// Booking shares the Schedule instance rather than building its own, so
		// the booking window has one definition (Schedule.Window) and the layanan
		// page cannot advertise a slot the booking form would refuse.
		Booking:   service.NewBooking(store, settings, schedule, email, cfg.Midtrans.ExpiryMinutes, cfg.App.Location),
		Payment:   service.NewPayment(store, gateway, log, email, cfg.Midtrans, cfg.App.URL, cfg.App.Location),
		Profile:   service.NewProfile(store, avatars),
		Users:     service.NewUsers(store),
		Dashboard: service.NewDashboard(store, log, cfg.App.Location),
		Audit:     service.NewAudit(store, log),
		Email:     email,
		Auth:      auth.New(cfg, store, log),
		Session:   sessions,
		Limits:    app.NewLimits(cfg),
	}

	srv := &http.Server{
		Addr:         cfg.Server.Addr(),
		Handler:      newRouter(deps),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Shut down on SIGINT/SIGTERM, letting in-flight requests finish.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// These two defers are ordered deliberately. They run last-registered-first,
	// so stop() cancels the ticker and then wg.Wait() lets a sweep already inside
	// a transaction commit or roll back — and both happen before the db.Close()
	// registered further up. That holds on every exit path, including a listener
	// error, not just the clean shutdown below.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stop()

	// All three background loops run on the signal context, so Ctrl-C ends them
	// at the same moment it stops the listener, and the WaitGroup above lets each
	// finish what it is holding before db.Close().
	wg.Go(func() { deps.RunExpiryTicker(ctx) })
	// The email worker drains its queue on cancellation, under its own grace
	// period, so a booking confirmed a moment before a restart still goes out.
	wg.Go(func() { deps.Email.Run(ctx) })
	wg.Go(func() { deps.RunReminderTicker(ctx) })

	errCh := make(chan error, 1)
	go func() {
		log.Info("server listening",
			slog.String("addr", srv.Addr),
			slog.String("env", cfg.Env),
			slog.String("url", cfg.App.URL),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("server stopped cleanly")
	return nil
}

// newRouter assembles the root router and mounts each subsystem.
func newRouter(d *app.Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	// No chi RealIP: it trusts X-Forwarded-For unconditionally and is spoofable.
	// appmw.ClientIP is the trusted-proxy-aware resolver the logger and the rate
	// limiter use instead, and it reads TRUSTED_PROXIES rather than any header.
	r.Use(appmw.SecureHeaders(d.Cfg.IsProduction()))
	r.Use(appmw.Logger(d.Log, d.Cfg.Server.TrustedProxies))
	// Deps.Recoverer rather than chi's: it renders the styled 500 page for HTML
	// requests and JSON under /api, where chi's can only write a bare string.
	r.Use(d.Recoverer)
	// Ahead of every mount, so it covers the webhook and both upload forms. The
	// per-upload caps still apply inside upload.ImageStore; this only stops a body
	// nobody meant to send.
	r.Use(appmw.MaxBody(d.Cfg.Server.MaxBodyBytes))
	r.Use(chimw.Timeout(d.Cfg.Server.WriteTimeout))

	r.Mount("/api", api.Routes(d))
	// The Midtrans webhook, on the root rather than inside public.Routes: that
	// router applies OptionalAuth and d.CSRF, and the webhook must have neither.
	// chi matches this literal segment ahead of the "/" mount below.
	r.Mount("/midtrans", api.WebhookRoutes(d))
	r.Mount("/admin", admin.Routes(d))
	r.Mount("/", public.Routes(d))

	// Static assets, including uploads under static/uploads. The handler sets
	// cache headers and nosniff; the directory must never be executable.
	r.Handle("/static/*", d.StaticHandler())

	// Crawler endpoints, mounted here rather than in public.Routes so they skip
	// OptionalAuth: they carry no session and must not consume an SSO callback
	// token. chi matches these literal paths ahead of the "/" mount.
	r.Get("/robots.txt", d.Robots)
	r.Get("/sitemap.xml", d.Sitemap)
	r.Get("/favicon.ico", d.Favicon)

	return r
}

// openDB opens the pool and verifies it answers before the server starts.
func openDB(cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.DB.DSN())
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(cfg.DB.MaxOpenConns)
	db.SetMaxIdleConns(cfg.DB.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.DB.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// stylesheet is the Tailwind build output, relative to the working directory.
const stylesheet = staticDir + "/css/app.css"

// checkStylesheetFresh refuses to boot production with a stylesheet older than
// the templates it dresses.
//
// Tailwind generates only the classes it finds in template/, so an app.css built
// against an older tree is missing precisely the utilities the newest markup
// added — and nothing else. The result is a site that is 99% styled with one
// feature silently inert, which is invisible to curl, invisible to the test
// suite, and invisible in the logs. Production served exactly that for the public
// mobile drawer: seven classes missing, all of them unique to that one element,
// so the hamburger flipped its checkbox and nothing appeared. Because app.css is
// gitignored it cannot arrive with a `git pull`, and because /static is served
// `immutable` for a year the stale copy sticks.
//
// `build: tailwind` in the Makefile is what stops this happening; this is the
// check that catches it happening anyway — a deploy that copies a binary without
// rebuilding, or a working tree edited after the last build.
//
// Development only warns: editing a template with the server already running is
// the normal loop there, and `make dev` has Tailwind watching alongside it.
func checkStylesheetFresh(cfg *config.Config, log *slog.Logger) error {
	css, err := os.Stat(stylesheet)
	if err != nil {
		if cfg.IsProduction() {
			return fmt.Errorf("stat %s: %w (run `make tailwind`)", stylesheet, err)
		}
		log.Warn("stylesheet missing, the site will render unstyled",
			slog.String("path", stylesheet),
			slog.String("fix", "make tailwind"))
		return nil
	}

	var newest time.Time
	var newestPath string
	err = filepath.WalkDir(templateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(newest) {
			newest, newestPath = fi.ModTime(), p
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanning %s: %w", templateDir, err)
	}

	if !newest.After(css.ModTime()) {
		return nil
	}

	if cfg.IsProduction() {
		return fmt.Errorf(
			"%s is older than %s (%s vs %s): the CSS was built against different templates, run `make tailwind`",
			stylesheet, newestPath,
			css.ModTime().Format(time.RFC3339), newest.Format(time.RFC3339))
	}
	log.Error("stylesheet is older than the templates: classes added since the last build are missing",
		slog.String("stylesheet", stylesheet),
		slog.String("newer_template", newestPath),
		slog.String("fix", "make tailwind"))
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelDebug
	if cfg.IsProduction() {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	if cfg.IsProduction() {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
