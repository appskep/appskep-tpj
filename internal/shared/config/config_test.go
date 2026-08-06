package config_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/remorac/appskep-tpj/internal/shared/config"
)

// These tests drive Load() through the environment, which means they cannot run
// in parallel — t.Setenv forbids it, and Load reads process-wide state.
//
// Load also calls godotenv.Load(), which reads a .env relative to the working
// directory. Under `go test` that is this package's own directory, where there is
// none, so the environment set here is the whole input. (godotenv does not
// override an already-set variable either way.)

// configKeys is every variable config.Load reads. setMinimum clears all of them.
//
// Clearing is not tidiness. `make test` runs through a Makefile that does
// `include .env` and `export`, so the developer's real DB_PASSWORD and
// MIDTRANS_SERVER_KEY are in the environment — and the production-secret tests
// below, which assert that Load REFUSES to boot without them, passed under
// `go test ./...` and failed under `make test`. A test whose result depends on
// whose machine it runs on is worse than no test.
//
// lookup() treats an empty value as unset, so setting "" is how a variable is
// removed for the duration of a test.
var configKeys = []string{
	"ENV",
	"SERVER_HOST", "SERVER_PORT", "SERVER_READ_TIMEOUT", "SERVER_WRITE_TIMEOUT",
	"SERVER_IDLE_TIMEOUT", "SERVER_SHUTDOWN_TIMEOUT", "SERVER_MAX_BODY_BYTES",
	"RATE_LIMIT_BOOKING", "RATE_LIMIT_PAYMENT", "RATE_LIMIT_WEBHOOK", "TRUSTED_PROXIES",
	"APP_NAME", "APP_URL", "APP_PAGE_SIZE", "APP_TZ",
	"EXPIRY_SWEEP_INTERVAL", "REMINDER_HOUR", "REMINDER_SWEEP_INTERVAL",
	"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
	"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	"AUTH_URL", "AUTH_SECRET", "AUTH_CLIENT_ID", "ADMIN_USER_IDS",
	"SESSION_KEY", "SESSION_NAME", "SESSION_MAX_AGE", "SESSION_SECURE",
	"MIDTRANS_ENV", "MIDTRANS_SERVER_KEY", "MIDTRANS_CLIENT_KEY", "MIDTRANS_PREFIX",
	"MIDTRANS_ENABLED_PAYMENTS", "PAYMENT_EXPIRY_MINUTES", "MIDTRANS_TIMEOUT",
	"SMTP_HOST", "SMTP_PORT", "SMTP_USERNAME", "SMTP_PASSWORD",
	"SMTP_FROM_NAME", "SMTP_FROM_EMAIL", "SMTP_TIMEOUT",
	"MAP_TILE_URL", "MAP_TILE_ATTRIBUTION",
	"MAP_DEFAULT_LAT", "MAP_DEFAULT_LNG", "MAP_DEFAULT_ZOOM",
}

// setMinimum clears every configuration variable and then sets only the values
// every environment requires, so a test can break exactly one of them and know
// that nothing else is in play.
func setMinimum(t *testing.T) {
	t.Helper()

	for _, key := range configKeys {
		t.Setenv(key, "")
	}

	t.Setenv("AUTH_SECRET", "a-shared-secret")
	t.Setenv("SESSION_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DB_NAME", "appskep_tpj")
	t.Setenv("ENV", "development")
	t.Setenv("APP_TZ", "Asia/Jakarta")
}

func TestLoadDefaults(t *testing.T) {
	setMinimum(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != "8080" {
		t.Errorf("SERVER_PORT default = %q, want 8080", cfg.Server.Port)
	}
	if cfg.App.PageSize != 20 {
		t.Errorf("APP_PAGE_SIZE default = %d, want 20", cfg.App.PageSize)
	}
	if cfg.Midtrans.Prefix != "tpj" {
		t.Errorf("MIDTRANS_PREFIX default = %q, want tpj", cfg.Midtrans.Prefix)
	}
	// Sandbox unless the literal says otherwise — failing safe matters more than
	// failing loudly, because the error case is charging real cards.
	if cfg.Midtrans.IsProduction() {
		t.Error("the default Midtrans environment is production")
	}
	// TRUSTED_PROXIES is empty out of the box, so X-Forwarded-For is ignored.
	if len(cfg.Server.TrustedProxies) != 0 {
		t.Errorf("TRUSTED_PROXIES defaults to %v, want empty", cfg.Server.TrustedProxies)
	}
	// Load is the only place these two are computed, and DSN is broken without
	// them.
	if cfg.DB.Loc == nil {
		t.Error("DB.Loc is nil — the driver would reject every DATETIME")
	}
	if cfg.DB.TZOffset != "+07:00" {
		t.Errorf("DB.TZOffset = %q, want +07:00", cfg.DB.TZOffset)
	}
	if cfg.App.Location == nil {
		t.Fatal("App.Location is nil")
	}
	if !cfg.IsDevelopment() || cfg.IsProduction() {
		t.Error("ENV=development did not select development")
	}
}

// TestLoadResolvesEnabledPayments covers the one setting whose default is not
// "whatever the upstream does". Unset must mean QRIS plus the two e-wallet
// deeplinks — never the empty list, because sending no enabled_payments at all
// offers whatever ANOTHER Appskep system turned on for the shared merchant
// account.
//
// The "explicit single" case is the one that must not rot: naming one channel is
// what makes Snap skip its method picker, and it is the supported way back to a
// QRIS-only checkout now that the default carries three.
//
// The spelling is "other_qris", Snap's name for the generic QRIS channel. See
// the "core api payment_type is not a snap channel" case in
// TestLoadRejectsInvalidValues for why the wrong one cost a day.
func TestLoadResolvesEnabledPayments(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{
			name: "unset", value: "",
			want: []string{"other_qris", "gopay", "shopeepay"},
		},
		{name: "explicit single", value: "other_qris", want: []string{"other_qris"}},
		{
			name: "whitespace around each channel", value: "other_qris, gopay ,shopeepay",
			want: []string{"other_qris", "gopay", "shopeepay"},
		},
		// The escape hatch: send no enabled_payments at all, so a channel that is
		// not active on the shared account can be worked around from .env.
		{name: "all", value: "all", want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimum(t)
			t.Setenv("MIDTRANS_ENABLED_PAYMENTS", tc.value)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.Midtrans.EnabledPayments
			if len(got) != len(tc.want) {
				t.Fatalf("EnabledPayments = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("EnabledPayments = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestLoadRequiresSecretsInEveryEnvironment. Not just production: identity comes
// entirely from a token verified with AUTH_SECRET and a cookie signed with
// SESSION_KEY. Missing either does not degrade the app, it removes the only thing
// standing between a visitor and any account — so a failed boot beats a
// development instance that quietly accepts forged sessions.
func TestLoadRequiresSecretsInEveryEnvironment(t *testing.T) {
	t.Run("no AUTH_SECRET", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("AUTH_SECRET", "")

		assertLoadRejects(t, "AUTH_SECRET")
	})

	t.Run("no SESSION_KEY", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("SESSION_KEY", "")

		assertLoadRejects(t, "SESSION_KEY")
	})

	t.Run("SESSION_KEY one byte short", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("SESSION_KEY", strings.Repeat("a", 31))

		assertLoadRejects(t, "SESSION_KEY")
	})

	t.Run("the reference app's hardcoded key", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("SESSION_KEY", "secret")

		assertLoadRejects(t, "SESSION_KEY")
	})
}

// TestLoadRejectsADSTTimezone. DBConfig.DSN pins the MySQL session to a fixed UTC
// offset. That is exact for Asia/Jakarta, which has never observed DST, but it
// would drift by an hour twice a year in a zone that does — silently corrupting
// every booking time. Failing at startup is the design.
func TestLoadRejectsADSTTimezone(t *testing.T) {
	for _, tz := range []string{"Europe/London", "America/New_York", "Australia/Sydney"} {
		t.Run(tz, func(t *testing.T) {
			setMinimum(t)
			t.Setenv("APP_TZ", tz)

			assertLoadRejects(t, "DST")
		})
	}

	// Zones without DST are accepted, so the check is about DST and not about
	// being Jakarta specifically.
	for _, tz := range []string{"Asia/Jakarta", "Asia/Tokyo", "UTC"} {
		t.Run("accepts "+tz, func(t *testing.T) {
			setMinimum(t)
			t.Setenv("APP_TZ", tz)

			if _, err := config.Load(); err != nil {
				t.Errorf("Load rejected %s: %v", tz, err)
			}
		})
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "zero page size", key: "APP_PAGE_SIZE", value: "0", want: "APP_PAGE_SIZE"},
		{name: "negative page size", key: "APP_PAGE_SIZE", value: "-1", want: "APP_PAGE_SIZE"},
		{
			// Below the largest upload the app accepts, every oversized image would
			// be refused by the transport with a bare error rather than the 422 that
			// names the problem.
			name: "body cap under 3 MB", key: "SERVER_MAX_BODY_BYTES", value: "1048576",
			want: "SERVER_MAX_BODY_BYTES",
		},
		{
			// A negative is a typo; 0 is the documented way to turn a limit off.
			name: "negative rate limit", key: "RATE_LIMIT_BOOKING", value: "-1",
			want: "RATE_LIMIT_BOOKING",
		},
		{
			name: "zero payment expiry", key: "PAYMENT_EXPIRY_MINUTES", value: "0",
			want: "PAYMENT_EXPIRY_MINUTES",
		},
		{
			// A zero interval makes time.NewTicker panic at boot, and a disabled
			// sweep leaks a slot for every abandoned checkout. There is no "off".
			name: "zero expiry sweep", key: "EXPIRY_SWEEP_INTERVAL", value: "0s",
			want: "EXPIRY_SWEEP_INTERVAL",
		},
		{
			name: "zero reminder sweep", key: "REMINDER_SWEEP_INTERVAL", value: "0s",
			want: "REMINDER_SWEEP_INTERVAL",
		},
		{
			// A negative hour would make the sweep's "has the hour arrived" test true
			// at midnight, sending tomorrow's reminders while people are asleep.
			name: "reminder hour out of range", key: "REMINDER_HOUR", value: "24",
			want: "REMINDER_HOUR",
		},
		{name: "negative reminder hour", key: "REMINDER_HOUR", value: "-1", want: "REMINDER_HOUR"},
		{name: "zero midtrans timeout", key: "MIDTRANS_TIMEOUT", value: "0s", want: "MIDTRANS_TIMEOUT"},
		{
			// The prefix is the only thing separating our transactions from the other
			// Appskep systems on the shared merchant account. With a dash, OwnsOrderID
			// would build "tpj--" and match nothing we ever mint.
			name: "prefix with a dash", key: "MIDTRANS_PREFIX", value: "tpj-x",
			want: "MIDTRANS_PREFIX",
		},
		{
			// A channel Midtrans does not recognise is not rejected by Snap — it is
			// dropped. The transaction is created, the token and redirect come back
			// clean, and the customer reaches a page offering nothing at all. Boot is
			// the only place this can be caught.
			name: "unknown payment channel", key: "MIDTRANS_ENABLED_PAYMENTS", value: "qriss",
			want: "MIDTRANS_ENABLED_PAYMENTS",
		},
		{
			// The regression. "qris" is a Core API payment_type and looks entirely
			// plausible in a Snap request; it shipped, and every customer got "Metode
			// pembayaran tidak tersedia" while the logs stayed silent. Snap's generic
			// QRIS channel is "other_qris".
			name: "core api payment_type is not a snap channel",
			key:  "MIDTRANS_ENABLED_PAYMENTS", value: "qris",
			want: "MIDTRANS_ENABLED_PAYMENTS",
		},
		{
			name: "one bad channel among good ones", key: "MIDTRANS_ENABLED_PAYMENTS",
			value: "other_qris,gopay,paypal", want: "MIDTRANS_ENABLED_PAYMENTS",
		},
		{
			// A malformed list must be a failed boot, not a silently empty one: the
			// difference is whether the rate limiter can be bypassed by anyone who
			// sends a header, and that must not be discoverable only in production.
			name: "malformed trusted proxies", key: "TRUSTED_PROXIES", value: "not-an-ip",
			want: "TRUSTED_PROXIES",
		},
		{
			name: "trusted proxies with one bad entry", key: "TRUSTED_PROXIES",
			value: "10.0.0.0/8,garbage", want: "TRUSTED_PROXIES",
		},
		{
			// The CSP's img-src is derived from this value, so a URL no origin can
			// be taken from must not reach a header. Everything wrong with the map
			// fails silently at runtime — a blank grey square in the browser and
			// nothing at all in the server log — which is why all of it is checked
			// at boot instead.
			name: "tile url with no host", key: "MAP_TILE_URL",
			value: "{z}/{x}/{y}.png", want: "MAP_TILE_URL",
		},
		{
			name: "tile url with no scheme", key: "MAP_TILE_URL",
			value: "tile.example.org/{z}/{x}/{y}.png", want: "MAP_TILE_URL",
		},
		{
			// A template missing a placeholder is a valid URL that fetches the same
			// tile forever, so nothing errors anywhere.
			name: "tile url missing {y}", key: "MAP_TILE_URL",
			value: "https://tile.example.org/{z}/{x}.png", want: "MAP_TILE_URL",
		},
		{name: "latitude out of range", key: "MAP_DEFAULT_LAT", value: "91", want: "MAP_DEFAULT_LAT"},
		{name: "latitude not a number", key: "MAP_DEFAULT_LAT", value: "padang", want: "MAP_DEFAULT_LAT"},
		{name: "longitude out of range", key: "MAP_DEFAULT_LNG", value: "-181", want: "MAP_DEFAULT_LNG"},
		{name: "zoom of zero", key: "MAP_DEFAULT_ZOOM", value: "0", want: "MAP_DEFAULT_ZOOM"},
		{name: "zoom past what tiles exist for", key: "MAP_DEFAULT_ZOOM", value: "25", want: "MAP_DEFAULT_ZOOM"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimum(t)
			t.Setenv(tc.key, tc.value)

			assertLoadRejects(t, tc.want)
		})
	}
}

func TestLoadAcceptsTrustedProxies(t *testing.T) {
	setMinimum(t)
	// A bare address is accepted as a single-host prefix, because that is the
	// obvious thing to write and rejecting it would be pedantry.
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 127.0.0.1, ::1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Server.TrustedProxies) != 3 {
		t.Fatalf("parsed %d prefixes, want 3", len(cfg.Server.TrustedProxies))
	}
	if got := cfg.Server.TrustedProxies[1].Bits(); got != 32 {
		t.Errorf("a bare IPv4 became a /%d, want /32", got)
	}
}

// TestLoadProductionRequiresMoreSecrets. The two that only matter once real money
// and a real database are involved.
func TestLoadProductionRequiresMoreSecrets(t *testing.T) {
	t.Run("no DB_PASSWORD", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("ENV", "production")
		t.Setenv("MIDTRANS_SERVER_KEY", "SB-Mid-server-x")

		assertLoadRejects(t, "DB_PASSWORD")
	})

	t.Run("no MIDTRANS_SERVER_KEY", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("ENV", "production")
		t.Setenv("DB_PASSWORD", "s3cret")

		assertLoadRejects(t, "MIDTRANS_SERVER_KEY")
	})

	t.Run("both present", func(t *testing.T) {
		setMinimum(t)
		t.Setenv("ENV", "production")
		t.Setenv("DB_PASSWORD", "s3cret")
		t.Setenv("MIDTRANS_SERVER_KEY", "SB-Mid-server-x")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.IsProduction() {
			t.Error("ENV=production did not select production")
		}
		// SESSION_SECURE defaults to whether this is production, so a production
		// cookie is Secure without anyone having to remember the flag.
		if !cfg.Session.Secure {
			t.Error("the session cookie is not Secure in production")
		}
	})
}

// TestMapTileCSPSource. The Content-Security-Policy's img-src is built from
// MAP_TILE_URL at boot, so this function decides whether the tiles load at all.
// Getting it wrong is a blank grey map and a console warning — invisible to
// curl, invisible to the server log, and the reason this is derived from the
// setting rather than written out beside it.
func TestMapTileCSPSource(t *testing.T) {
	tests := []struct {
		name    string
		tileURL string
		want    string
	}{
		{
			// The default. {s} is Leaflet's subdomain rotation, so the source has
			// to be a wildcard or two thirds of the tiles are refused — a map that
			// loads in patches, which reads as a network problem rather than a
			// policy one.
			name:    "the openstreetmap default",
			tileURL: "https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png",
			want:    "https://*.tile.openstreetmap.org",
		},
		{
			name:    "a provider with no subdomain rotation",
			tileURL: "https://tiles.example.org/{z}/{x}/{y}.png",
			want:    "https://tiles.example.org",
		},
		{
			// An API key in the query string must not end up in a header.
			name:    "a key in the query string is dropped",
			tileURL: "https://api.maptiler.com/maps/streets/{z}/{x}/{y}.png?key=SECRET",
			want:    "https://api.maptiler.com",
		},
		{
			name:    "a port is part of the origin",
			tileURL: "http://localhost:8081/{z}/{x}/{y}.png",
			want:    "http://localhost:8081",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setMinimum(t)
			t.Setenv("MAP_TILE_URL", tc.tileURL)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Map.TileCSPSource(); got != tc.want {
				t.Errorf("TileCSPSource() = %q, want %q", got, tc.want)
			}
			if strings.Contains(cfg.Map.TileCSPSource(), "SECRET") {
				t.Error("the tile URL's query string leaked into the CSP source")
			}
		})
	}
}

// TestLoadReportsEveryProblemAtOnce: validate accumulates rather than returning
// on the first, so a fresh deployment sees the whole list in one boot.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setMinimum(t)
	t.Setenv("AUTH_SECRET", "")
	t.Setenv("SESSION_KEY", "short")
	t.Setenv("APP_PAGE_SIZE", "0")
	t.Setenv("MIDTRANS_PREFIX", "a-b")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load accepted four broken settings")
	}
	for _, want := range []string{"AUTH_SECRET", "SESSION_KEY", "APP_PAGE_SIZE", "MIDTRANS_PREFIX"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestDSN(t *testing.T) {
	setMinimum(t)
	t.Setenv("DB_HOST", "127.0.0.1")
	t.Setenv("DB_PORT", "3306")
	t.Setenv("DB_USER", "tpj")
	t.Setenv("DB_PASSWORD", "s3cret")
	t.Setenv("DB_NAME", "appskep_tpj")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dsn := cfg.DB.DSN()

	// parseTime, so DATE and DATETIME scan into time.Time rather than []byte.
	if !strings.Contains(dsn, "parseTime=true") {
		t.Errorf("DSN carries no parseTime: %s", dsn)
	}
	// The session clock is pinned so NOW() and time.Now().In(loc) are the same
	// wall clock — without it the expiry ticker misfires silently.
	if !strings.Contains(dsn, url.QueryEscape("'+07:00'")) && !strings.Contains(dsn, "%27%2B07%3A00%27") {
		t.Errorf("DSN does not pin the session time_zone: %s", dsn)
	}
	if !strings.Contains(dsn, "time_zone") {
		t.Errorf("DSN carries no time_zone parameter: %s", dsn)
	}
	if !strings.Contains(dsn, "appskep_tpj") {
		t.Errorf("DSN names no database: %s", dsn)
	}
	// Off, so nothing in the app can send two statements in one round trip.
	if strings.Contains(dsn, "multiStatements=true") {
		t.Errorf("DSN enables multiStatements: %s", dsn)
	}
}

func TestOrderID(t *testing.T) {
	m := config.MidtransConfig{Prefix: "tpj"}

	// The prefix is what separates our transactions from ukom's and every other
	// Appskep system's on the shared merchant account (PLAN.md R5).
	if got := m.OrderID("4f0a1e9c-8b2d"); got != "tpj-4f0a1e9c-8b2d" {
		t.Errorf("OrderID = %q", got)
	}

	tests := []struct {
		orderID string
		ours    bool
	}{
		{orderID: "tpj-4f0a1e9c", ours: true},
		{orderID: "tpj-", ours: true},
		// Another Appskep system on the same account. These arrive constantly and
		// must be ignored cleanly, not treated as an error.
		{orderID: "ukom-4f0a1e9c"},
		{orderID: "tpjx-4f0a1e9c"},
		{orderID: "tpj"},
		{orderID: "TPJ-4f0a1e9c"},
		{orderID: ""},
		{orderID: "x-tpj-4f0a1e9c"},
	}
	for _, tc := range tests {
		if got := m.OwnsOrderID(tc.orderID); got != tc.ours {
			t.Errorf("OwnsOrderID(%q) = %v, want %v", tc.orderID, got, tc.ours)
		}
	}

	// Minted ids are always recognised as ours — the property that keeps a webhook
	// for an order we opened from being ignored.
	for _, uuid := range []string{"a", "4f0a1e9c-8b2d-4a77-9f1e-2c3d4e5f6a7b"} {
		if !m.OwnsOrderID(m.OrderID(uuid)) {
			t.Errorf("OwnsOrderID rejected an id it minted: %q", m.OrderID(uuid))
		}
	}
}

func TestAuthURLs(t *testing.T) {
	a := config.AuthConfig{URL: "https://dev-auth.appskep.id/", AdminUserIDs: []string{"1", "19"}}

	// The return URL is escaped, so a path with a query string cannot break out of
	// the client_base_url parameter.
	got := a.LoginURL("http://localhost:8080/booking?layanan=urut&tanggal=2026-07-27")
	if strings.Count(got, "?") != 1 {
		t.Errorf("LoginURL did not escape the return URL: %s", got)
	}
	if !strings.HasPrefix(got, "https://dev-auth.appskep.id/v2/auth/login?client_base_url=") {
		t.Errorf("LoginURL = %s", got)
	}
	// The trailing slash on AUTH_URL must not become a double slash.
	if strings.Contains(got, "id//v2") {
		t.Errorf("LoginURL has a double slash: %s", got)
	}

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("LoginURL is not a URL: %v", err)
	}
	if back := parsed.Query().Get("client_base_url"); back != "http://localhost:8080/booking?layanan=urut&tanggal=2026-07-27" {
		t.Errorf("client_base_url round-tripped to %q", back)
	}

	if r := a.RefreshURL("a.jwt.value"); !strings.Contains(r, "oauth/refresh-token?token=a.jwt.value") {
		t.Errorf("RefreshURL = %s", r)
	}

	if !a.IsBootstrapAdmin("1") || !a.IsBootstrapAdmin("19") {
		t.Error("IsBootstrapAdmin missed a listed id")
	}
	if a.IsBootstrapAdmin("2") || a.IsBootstrapAdmin("") {
		t.Error("IsBootstrapAdmin accepted an unlisted id")
	}
}

func TestSMTPEnabled(t *testing.T) {
	// Off, not broken, when unconfigured: a development instance and a staging one
	// without mail credentials must both run the whole booking flow.
	tests := []struct {
		host, from string
		want       bool
	}{
		{host: "smtp.example", from: "halo@tpj.example", want: true},
		{host: "smtp.example", from: ""},
		{host: "", from: "halo@tpj.example"},
		{host: "", from: ""},
	}
	for _, tc := range tests {
		s := config.SMTPConfig{Host: tc.host, FromEmail: tc.from}
		if got := s.Enabled(); got != tc.want {
			t.Errorf("Enabled(host=%q, from=%q) = %v, want %v", tc.host, tc.from, got, tc.want)
		}
	}
}

func TestServerAddr(t *testing.T) {
	if got := (config.ServerConfig{Host: "", Port: "8080"}).Addr(); got != ":8080" {
		t.Errorf("Addr = %q, want :8080", got)
	}
	if got := (config.ServerConfig{Host: "127.0.0.1", Port: "9090"}).Addr(); got != "127.0.0.1:9090" {
		t.Errorf("Addr = %q", got)
	}
}

// TestEmptyEnvVarMeansUnset: lookup treats an empty or whitespace value as
// absent, so `FOO=` in a .env means "use the default" rather than "use an empty
// string" — which for AUTH_SECRET is the difference between a failed boot and a
// silently insecure one.
func TestEmptyEnvVarMeansUnset(t *testing.T) {
	setMinimum(t)
	t.Setenv("APP_URL", "   ")
	t.Setenv("MIDTRANS_PREFIX", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.App.URL != "http://localhost:8080" {
		t.Errorf("APP_URL = %q, want the default", cfg.App.URL)
	}
	if cfg.Midtrans.Prefix != "tpj" {
		t.Errorf("MIDTRANS_PREFIX = %q, want the default", cfg.Midtrans.Prefix)
	}
}

func assertLoadRejects(t *testing.T, mention string) {
	t.Helper()

	_, err := config.Load()
	if err == nil {
		t.Fatalf("Load accepted a configuration it should refuse (expected it to mention %q)", mention)
	}
	if !strings.Contains(err.Error(), mention) {
		t.Errorf("error = %q, want it to mention %q", err, mention)
	}
}
