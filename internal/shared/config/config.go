// Package config loads and validates application configuration from the
// environment (and an optional .env file). Nothing else in the codebase reads
// os.Getenv directly — all configuration flows from Load().
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// Config is the fully resolved configuration for one process.
type Config struct {
	Env      string
	Server   ServerConfig
	App      AppConfig
	DB       DBConfig
	Auth     AuthConfig
	Session  SessionConfig
	Midtrans MidtransConfig
	SMTP     SMTPConfig
}

type ServerConfig struct {
	Host            string
	Port            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// MaxBodyBytes caps every request body. Larger than any legitimate upload —
	// the 2 MB service image cap is enforced by the upload store, which is what
	// produces the friendly message — so this only stops a body nobody meant to
	// send.
	MaxBodyBytes int64
	// TrustedProxies are the addresses whose X-Forwarded-For may be believed.
	// Empty by default: an unconditionally trusted header lets any caller claim
	// any address, which turns a per-IP rate limit into no limit at all.
	TrustedProxies []netip.Prefix

	// The rate limits, in requests per minute per client IP. Zero disables one.
	RateLimitBooking int
	RateLimitPayment int
	RateLimitWebhook int
}

// Addr is the listen address for http.Server.
func (s ServerConfig) Addr() string {
	return s.Host + ":" + s.Port
}

type AppConfig struct {
	Name     string
	URL      string
	PageSize int
	TZ       string
	Location *time.Location
	// ExpirySweepInterval is how often the background ticker releases the slots of
	// unpaid bookings that ran out of time. It is the upper bound on how long a
	// slot stays invisibly held after its booking expired, so it is short: a
	// visitor watching the picker should see the slot come back while they are
	// still on the page.
	ExpirySweepInterval time.Duration

	// ReminderHour is the local hour (0-23) at or after which the H-1 reminder
	// sweep will send. It is a wall-clock hour in Location, not an interval: a
	// reminder that arrives at 03:00 is worse than none.
	ReminderHour int
	// ReminderSweepInterval is how often the reminder ticker wakes to check
	// whether that hour has arrived. It is coarse on purpose — the sweep does
	// nothing at all before ReminderHour, and the guarded reminder_sent_at claim
	// means a booking is only ever picked up once however often it runs.
	ReminderSweepInterval time.Duration
}

type DBConfig struct {
	Host            string
	Port            string
	User            string
	Password        string
	Name            string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	// Loc is the application timezone. The driver interprets DATE and DATETIME
	// values in this location, so a time.Time round-trips as the same wall clock.
	Loc *time.Location
	// TZOffset is Loc's UTC offset in MySQL's time_zone syntax ("+07:00"). A
	// fixed offset is used rather than the zone name because a server's
	// mysql.time_zone tables are frequently empty, and SET time_zone with a named
	// zone then fails on every connection. See Load and hasDST.
	TZOffset string
}

// DSN builds the go-sql-driver connection string. parseTime is on so DATE and
// DATETIME columns scan into time.Time, and the session timezone is pinned to
// the app timezone so MySQL NOW() agrees with Go.
func (d DBConfig) DSN() string {
	c := mysql.NewConfig()
	c.User = d.User
	c.Passwd = d.Password
	c.Net = "tcp"
	c.Addr = d.Host + ":" + d.Port
	c.DBName = d.Name
	c.ParseTime = true
	c.Loc = d.Loc
	c.Collation = "utf8mb4_unicode_ci"
	c.MultiStatements = false
	// Pin the session clock so NOW() and time.Now().In(cfg.App.Location) are the
	// same wall clock. Without this the booking slots — which are timezone-less
	// DATE + TIME values in Jakarta wall time — would be compared against a NOW()
	// running on a different clock, silently misfiring the expiry ticker and the
	// "is this slot still in the future" check.
	//
	// The value carries its own SQL quotes: the driver emits the parameter as
	// "SET time_zone = <value>" verbatim on each new connection.
	c.Params = map[string]string{"time_zone": "'" + d.TZOffset + "'"}
	return c.FormatDSN()
}

type AuthConfig struct {
	// URL is the Appskep auth service base URL, e.g. https://dev-auth.appskep.id
	URL string
	// Secret is the HMAC key the Appskep auth service signs its JWTs with.
	Secret string
	// ClientID identifies this app to the auth service.
	ClientID string
	// AdminUserIDs bootstraps the local admin role from Appskep user IDs.
	AdminUserIDs []string
}

// LoginURL builds the SSO redirect for a request that should land back on
// clientBaseURL (an absolute URL including scheme, host and path) after login.
func (a AuthConfig) LoginURL(clientBaseURL string) string {
	return fmt.Sprintf("%s/v2/auth/login?client_base_url=%s",
		strings.TrimRight(a.URL, "/"), url.QueryEscape(clientBaseURL))
}

// RefreshURL builds the inline token-refresh endpoint for an existing token.
func (a AuthConfig) RefreshURL(token string) string {
	return fmt.Sprintf("%s/oauth/refresh-token?token=%s",
		strings.TrimRight(a.URL, "/"), url.QueryEscape(token))
}

// IsBootstrapAdmin reports whether an Appskep user ID is in the admin allowlist.
func (a AuthConfig) IsBootstrapAdmin(appskepUserID string) bool {
	for _, id := range a.AdminUserIDs {
		if id == appskepUserID {
			return true
		}
	}
	return false
}

type SessionConfig struct {
	// Key signs the session cookie. Must be random, never a hardcoded literal.
	Key    string
	Name   string
	MaxAge time.Duration
	Secure bool
}

type MidtransConfig struct {
	// Env follows the Appskep convention: the literal "midtrans.Production"
	// selects production, anything else is sandbox.
	Env       string
	ServerKey string
	ClientKey string
	// Prefix namespaces our order IDs inside the shared Appskep Midtrans
	// account: order_id = "<prefix>-<uuid-v4>".
	Prefix string
	// ExpiryMinutes is how long an unpaid booking holds its slot.
	ExpiryMinutes int
	// EnabledPayments are the Snap channel names sent as enabled_payments on
	// every transaction. Empty means the field is omitted and the customer is
	// offered whatever the account has active — which on a SHARED account is
	// whatever another Appskep system turned on, so it is not the default.
	//
	// The default is QRIS plus the two e-wallet deeplinks — on a phone, opening
	// GoPay or ShopeePay directly beats scanning a QR off the same screen. A
	// SINGLE entry makes Snap skip its method picker and open that channel's page
	// directly; that is still reachable, as a one-line env change, by naming one
	// channel.
	EnabledPayments []string
	// Timeout bounds one call to the Midtrans API. It must stay well under
	// SERVER_WRITE_TIMEOUT: a customer pressing "Bayar sekarang" waits on this
	// call, and a Midtrans that never answers must not hold the request open
	// until the server kills the response half-written.
	Timeout time.Duration
}

// OrderID mints a new Midtrans order_id: "<prefix>-<uuid-v4>".
//
// The prefix is what separates our transactions from ukom's and every other
// Appskep system's on the shared merchant account (PLAN.md R5). It is applied
// here rather than at the call site so the same string is used to mint an order
// and to recognise an inbound webhook as ours.
func (m MidtransConfig) OrderID(uuid string) string {
	return m.Prefix + "-" + uuid
}

// OwnsOrderID reports whether an order_id belongs to this system.
//
// Notifications for other prefixes arrive constantly on a shared account and
// must be ignored cleanly, not treated as an error.
func (m MidtransConfig) OwnsOrderID(orderID string) bool {
	return strings.HasPrefix(orderID, m.Prefix+"-")
}

// IsProduction reports whether Midtrans should run against the production API.
func (m MidtransConfig) IsProduction() bool {
	return m.Env == "midtrans.Production"
}

// midtransPaymentsAll is the escape hatch: MIDTRANS_ENABLED_PAYMENTS=all sends
// no enabled_payments at all, restoring the account-wide channel list. It exists
// so a channel that turns out not to be active on the shared account can be
// worked around from .env rather than from a deploy.
const midtransPaymentsAll = "all"

// midtransPayments are the Snap channel names Midtrans accepts in
// enabled_payments. The strings are written out rather than taken from the SDK
// because config imports nothing from the payment provider, the same way Env
// carries the literal "midtrans.Production" — and because the pinned SDK v1.3.8
// has no constant for "other_qris", which Midtrans has accepted for years.
//
// An unknown value here is a Snap page that errors for every customer, so it
// fails the boot instead.
//
// "qris" is deliberately ABSENT: it is a Core API payment_type, not a Snap
// channel, and Snap drops an unrecognised name silently rather than rejecting
// the transaction. Shipping it left every customer on "Metode pembayaran tidak
// tersedia" with a valid token, a valid redirect and nothing in our logs. The
// generic QRIS channel is "other_qris", which needs GoPay or ShopeePay QRIS
// active on the merchant account.
var midtransPayments = map[string]bool{
	"other_qris": true,
	"gopay":      true, "shopeepay": true,
	"credit_card":   true,
	"bank_transfer": true,
	"bca_va":        true, "bni_va": true, "bri_va": true,
	"permata_va": true, "other_va": true, "echannel": true,
	"cstore": true, "indomaret": true, "alfamart": true, "kioson": true,
	"akulaku": true, "kredivo": true, "danamon_online": true, "uob_ezpay": true,
	"bca_klikbca": true, "bca_klikpay": true, "bri_epay": true,
	"cimb_clicks": true, "mandiri_clickpay": true, "mandiri_ecash": true,
	"telkomsel_cash": true,
}

type SMTPConfig struct {
	Host      string
	Port      int
	Username  string
	Password  string
	FromName  string
	FromEmail string
	// Timeout bounds one delivery, connection through QUIT. Every external call
	// in this codebase carries one; a mail server that accepts the connection and
	// then stops answering would otherwise hold a worker goroutine for as long as
	// the OS keeps the socket open, and the send queue behind it with it.
	Timeout time.Duration
}

// Enabled reports whether SMTP is configured. When false the app uses a no-op
// email service instead of failing.
func (s SMTPConfig) Enabled() bool {
	return s.Host != "" && s.FromEmail != ""
}

// Load reads .env (if present), resolves every setting, and validates the
// result. A missing .env is not an error — real environments set real env vars.
func Load() (*Config, error) {
	_ = godotenv.Load()

	env := getString("ENV", EnvDevelopment)

	tz := getString("APP_TZ", "Asia/Jakarta")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("config: invalid APP_TZ %q: %w", tz, err)
	}

	cfg := &Config{
		Env: env,
		Server: ServerConfig{
			Host:            getString("SERVER_HOST", ""),
			Port:            getString("SERVER_PORT", "8080"),
			ReadTimeout:     getDuration("SERVER_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    getDuration("SERVER_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:     getDuration("SERVER_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: getDuration("SERVER_SHUTDOWN_TIMEOUT", 15*time.Second),
			MaxBodyBytes:    int64(getInt("SERVER_MAX_BODY_BYTES", 4<<20)),

			// Per client IP, per minute. Booking and payment are the routes that
			// cost something on the way through; the webhook's is generous because
			// Midtrans retries legitimately and a dropped notification is worse than
			// an accepted flood.
			RateLimitBooking: getInt("RATE_LIMIT_BOOKING", 20),
			RateLimitPayment: getInt("RATE_LIMIT_PAYMENT", 20),
			RateLimitWebhook: getInt("RATE_LIMIT_WEBHOOK", 120),
		},
		App: AppConfig{
			Name:     getString("APP_NAME", "Terapi Pemuda Jompo"),
			URL:      strings.TrimRight(getString("APP_URL", "http://localhost:8080"), "/"),
			PageSize: getInt("APP_PAGE_SIZE", 20),
			TZ:       tz,
			Location: loc,

			ExpirySweepInterval: getDuration("EXPIRY_SWEEP_INTERVAL", time.Minute),

			ReminderHour:          getInt("REMINDER_HOUR", 9),
			ReminderSweepInterval: getDuration("REMINDER_SWEEP_INTERVAL", 15*time.Minute),
		},
		DB: DBConfig{
			Host:            getString("DB_HOST", "127.0.0.1"),
			Port:            getString("DB_PORT", "3306"),
			User:            getString("DB_USER", "root"),
			Password:        getString("DB_PASSWORD", ""),
			Name:            getString("DB_NAME", "appskep_tpj"),
			MaxOpenConns:    getInt("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getInt("DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: getDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute),
		},
		Auth: AuthConfig{
			URL:          strings.TrimRight(getString("AUTH_URL", "https://dev-auth.appskep.id"), "/"),
			Secret:       getString("AUTH_SECRET", ""),
			ClientID:     getString("AUTH_CLIENT_ID", ""),
			AdminUserIDs: getStringSlice("ADMIN_USER_IDS"),
		},
		Session: SessionConfig{
			Key:    getString("SESSION_KEY", ""),
			Name:   getString("SESSION_NAME", "tpj_session"),
			MaxAge: getDuration("SESSION_MAX_AGE", 24*time.Hour),
			Secure: getBool("SESSION_SECURE", env == EnvProduction),
		},
		Midtrans: MidtransConfig{
			Env:           getString("MIDTRANS_ENV", "midtrans.Sandbox"),
			ServerKey:     getString("MIDTRANS_SERVER_KEY", ""),
			ClientKey:     getString("MIDTRANS_CLIENT_KEY", ""),
			Prefix:        getString("MIDTRANS_PREFIX", "tpj"),
			ExpiryMinutes: getInt("PAYMENT_EXPIRY_MINUTES", 60),
			Timeout:       getDuration("MIDTRANS_TIMEOUT", 15*time.Second),
		},
		SMTP: SMTPConfig{
			Host:      getString("SMTP_HOST", ""),
			Port:      getInt("SMTP_PORT", 587),
			Username:  getString("SMTP_USERNAME", ""),
			Password:  getString("SMTP_PASSWORD", ""),
			FromName:  getString("SMTP_FROM_NAME", "Terapi Pemuda Jompo"),
			FromEmail: getString("SMTP_FROM_EMAIL", ""),
			Timeout:   getDuration("SMTP_TIMEOUT", 10*time.Second),
		},
	}

	// The driver needs the resolved location, which is only available here.
	cfg.DB.Loc = loc
	cfg.DB.TZOffset = utcOffset(loc)

	// Not in the struct literal above because getStringSlice has no fallback
	// parameter. Unset means QRIS plus the two e-wallet deeplinks; naming one
	// channel is what makes Snap open that channel's page directly instead of a
	// method picker; "all" is the escape hatch back to the account's own list.
	cfg.Midtrans.EnabledPayments = getStringSlice("MIDTRANS_ENABLED_PAYMENTS")
	switch {
	case len(cfg.Midtrans.EnabledPayments) == 0:
		cfg.Midtrans.EnabledPayments = []string{"other_qris", "gopay", "shopeepay"}
	case len(cfg.Midtrans.EnabledPayments) == 1 &&
		cfg.Midtrans.EnabledPayments[0] == midtransPaymentsAll:
		cfg.Midtrans.EnabledPayments = nil
	}

	// A malformed TRUSTED_PROXIES is a failed boot, not a silently empty list:
	// the difference between the two is whether the rate limiter can be bypassed
	// by anyone who sends a header, and it must not be discoverable only in
	// production.
	proxies, err := parsePrefixes(getStringSlice("TRUSTED_PROXIES"))
	if err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}
	cfg.Server.TrustedProxies = proxies

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parsePrefixes reads the trusted-proxy list. A bare address is accepted as a
// single-host prefix, because "TRUSTED_PROXIES=10.0.0.5" is the obvious thing to
// write and rejecting it would be pedantry.
func parsePrefixes(values []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR block", v)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// utcOffset renders loc's current UTC offset in MySQL's time_zone syntax,
// e.g. "+07:00".
func utcOffset(loc *time.Location) string {
	_, secs := time.Now().In(loc).Zone()
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	return fmt.Sprintf("%s%02d:%02d", sign, secs/3600, (secs%3600)/60)
}

// hasDST reports whether loc's UTC offset differs between January and July,
// which is what makes the fixed-offset session timezone in DBConfig.DSN unsafe.
func hasDST(loc *time.Location) bool {
	y := time.Now().Year()
	_, jan := time.Date(y, time.January, 15, 12, 0, 0, 0, loc).Zone()
	_, jul := time.Date(y, time.July, 15, 12, 0, 0, 0, loc).Zone()
	return jan != jul
}

// IsProduction reports whether the app runs in production mode.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// IsDevelopment reports whether the app runs in development mode. Templates are
// reparsed per request when true.
func (c *Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// validate fails fast on configuration that would produce a broken or insecure
// process. Production is strict: every secret must be present.
func (c *Config) validate() error {
	var problems []string

	if c.App.PageSize <= 0 {
		problems = append(problems, "APP_PAGE_SIZE must be greater than 0")
	}
	// Below the largest upload the app itself accepts, every service image over
	// the cap would be refused by the transport with a bare error instead of the
	// 422 that names the problem.
	if c.Server.MaxBodyBytes < 3<<20 {
		problems = append(problems, "SERVER_MAX_BODY_BYTES must be at least 3145728 (3 MB)")
	}
	for _, rl := range []struct {
		key   string
		value int
	}{
		{"RATE_LIMIT_BOOKING", c.Server.RateLimitBooking},
		{"RATE_LIMIT_PAYMENT", c.Server.RateLimitPayment},
		{"RATE_LIMIT_WEBHOOK", c.Server.RateLimitWebhook},
	} {
		// Negative is a typo; 0 is the documented way to turn one off.
		if rl.value < 0 {
			problems = append(problems, rl.key+" cannot be negative (0 disables the limit)")
		}
	}
	if c.Midtrans.ExpiryMinutes <= 0 {
		problems = append(problems, "PAYMENT_EXPIRY_MINUTES must be greater than 0")
	}
	// A zero or negative interval would make time.NewTicker panic at boot, and a
	// disabled sweep leaks a slot for every abandoned checkout. There is no
	// "off" setting on purpose.
	if c.App.ExpirySweepInterval <= 0 {
		problems = append(problems, "EXPIRY_SWEEP_INTERVAL must be greater than 0")
	}
	// Same reason, and one more: a negative hour would make the sweep's
	// "has the hour arrived" test true at midnight, sending tomorrow's reminders
	// while the recipients are asleep.
	if c.App.ReminderSweepInterval <= 0 {
		problems = append(problems, "REMINDER_SWEEP_INTERVAL must be greater than 0")
	}
	if c.App.ReminderHour < 0 || c.App.ReminderHour > 23 {
		problems = append(problems, "REMINDER_HOUR must be between 0 and 23")
	}
	// Only checked when SMTP is configured at all: an unconfigured mailer is the
	// documented no-op, not a misconfiguration.
	if c.SMTP.Enabled() && c.SMTP.Timeout <= 0 {
		problems = append(problems, "SMTP_TIMEOUT must be greater than 0")
	}
	if c.Midtrans.Timeout <= 0 {
		problems = append(problems, "MIDTRANS_TIMEOUT must be greater than 0")
	}
	// The prefix is the only thing separating our transactions from the other
	// Appskep systems on the shared merchant account. Empty, every inbound
	// notification would look like ours; with a dash, OwnsOrderID would build
	// "tpj--" and match nothing we ever mint.
	if c.Midtrans.Prefix == "" || strings.Contains(c.Midtrans.Prefix, "-") {
		problems = append(problems, "MIDTRANS_PREFIX is required and must not contain '-'")
	}
	// A channel Midtrans does not recognise is rejected for every transaction,
	// so "Bayar sekarang" would fail for every customer with nothing wrong in
	// our own logs. Cheaper to catch here than in production.
	for _, p := range c.Midtrans.EnabledPayments {
		if !midtransPayments[p] {
			problems = append(problems, "MIDTRANS_ENABLED_PAYMENTS contains unknown channel "+
				strconv.Quote(p))
		}
	}
	if c.DB.Name == "" {
		problems = append(problems, "DB_NAME is required")
	}
	if c.Auth.URL == "" {
		problems = append(problems, "AUTH_URL is required")
	}
	// Both are required in every environment, not only production: identity comes
	// entirely from a token verified with AUTH_SECRET and a cookie signed with
	// SESSION_KEY. Missing either does not degrade the app, it removes the only
	// thing standing between a visitor and any account — better a failed boot
	// than a development instance that quietly accepts forged sessions.
	if strings.TrimSpace(c.Auth.Secret) == "" {
		problems = append(problems, "AUTH_SECRET is required")
	}
	if len(c.Session.Key) < 32 {
		problems = append(problems, "SESSION_KEY is required and must be at least 32 bytes")
	}
	// DBConfig.DSN pins the MySQL session to a fixed UTC offset. That is exact
	// for Asia/Jakarta, which has never observed DST, but it would silently drift
	// by an hour twice a year in a zone that does. Fail at startup rather than
	// corrupt booking times.
	if hasDST(c.App.Location) {
		problems = append(problems, "APP_TZ="+c.App.TZ+" observes DST; the fixed-offset "+
			"session time_zone in DBConfig.DSN would drift — load the MySQL time zone "+
			"tables and use the zone name instead")
	}

	if c.IsProduction() {
		for _, req := range []struct{ name, value string }{
			{"DB_PASSWORD", c.DB.Password},
			{"MIDTRANS_SERVER_KEY", c.Midtrans.ServerKey},
		} {
			if strings.TrimSpace(req.value) == "" {
				problems = append(problems, req.name+" is required when ENV=production")
			}
		}
	}

	if len(problems) > 0 {
		return errors.New("config: " + strings.Join(problems, "; "))
	}
	return nil
}

// --- env readers ---

func getString(key, fallback string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	if v, ok := lookup(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getBool(key string, fallback bool) bool {
	if v, ok := lookup(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// getDuration accepts a Go duration string ("30s", "5m"); a bare number is read
// as seconds so DB_CONN_MAX_LIFETIME=3600 keeps working.
func getDuration(key string, fallback time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return fallback
}

// getStringSlice splits a comma-separated value, trimming spaces and dropping
// empties.
func getStringSlice(key string) []string {
	v, ok := lookup(key)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}
