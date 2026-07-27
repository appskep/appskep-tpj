// Package service holds the business logic the handlers call into. Handlers stay
// thin: parse → validate → call service → render.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/repository"
)

// Settings is a read-through cache over the settings table.
//
// Every page render needs site_name, the WhatsApp number and the contact block,
// so reading them from the database on each request would add a query to every
// response for values that change a few times a year. The whole table is a
// handful of short rows, so it is loaded once at startup and kept in memory.
//
// Where a key also exists in the environment (payment_expiry_minutes vs
// PAYMENT_EXPIRY_MINUTES), the caller passes the config value as the fallback:
// the DB row wins when present and parseable, exactly as CLAUDE.md specifies.
// config remains the only reader of os.Getenv.
type Settings struct {
	store *repository.Store

	mu     sync.RWMutex
	values map[string]string
}

// Well-known setting keys. Named constants rather than bare strings so a typo is
// a compile error and every consumer can be found by reference.
const (
	KeySiteName             = "site_name"
	KeySiteTagline          = "site_tagline"
	KeySiteDescription      = "site_description"
	KeySiteOGImage          = "site_og_image"
	KeyContactEmail         = "contact_email"
	KeyContactPhone         = "contact_phone"
	KeyContactAddress       = "contact_address"
	KeyWhatsAppNumber       = "whatsapp_number"
	KeyInstagramURL         = "instagram_url"
	KeyBookingTerms         = "booking_terms"
	KeyBookingLeadMinutes   = "booking_lead_time_minutes"
	KeyBookingMaxDaysAhead  = "booking_max_days_ahead"
	KeyPaymentExpiryMinutes = "payment_expiry_minutes"
	KeySlotDefaultDuration  = "slot_default_duration_minutes"
	KeySlotDefaultCapacity  = "slot_default_capacity"
)

// NewSettings builds the cache and performs the initial load. A failure to read
// the table is returned rather than swallowed: the app would otherwise boot and
// silently render every page with fallback copy.
func NewSettings(ctx context.Context, store *repository.Store) (*Settings, error) {
	s := &Settings{store: store, values: make(map[string]string)}
	if err := s.Reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload replaces the cache with the current table contents. Phase 10 calls this
// after /admin/pengaturan writes, so an edit takes effect without a restart.
func (s *Settings) Reload(ctx context.Context) error {
	rows, err := s.store.Queries.ListSettings(ctx)
	if err != nil {
		return err
	}

	next := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.SettingValue.Valid {
			next[row.SettingKey] = row.SettingValue.String
		}
	}

	s.mu.Lock()
	s.values = next
	s.mu.Unlock()
	return nil
}

// String returns the setting, or fallback when the key is missing or empty.
func (s *Settings) String(key, fallback string) string {
	s.mu.RLock()
	v, ok := s.values[key]
	s.mu.RUnlock()

	if !ok {
		return fallback
	}
	if v = strings.TrimSpace(v); v == "" {
		return fallback
	}
	return v
}

// Int returns the setting parsed as an integer, or fallback when the key is
// missing or does not parse. An unparseable row is not an error — the env value
// is the documented fallback and a broken row must not take the site down.
func (s *Settings) Int(key string, fallback int) int {
	v := s.String(key, "")
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// All returns a copy of the cache, for the Phase 10 settings form.
func (s *Settings) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return maps.Clone(s.values)
}

// ---------------------------------------------------------------------------
// The settings form (Phase 10)
// ---------------------------------------------------------------------------

// SettingField describes one editable row: which key it writes, what to call it,
// and how to check it.
//
// The form is driven by this table rather than by whatever the request happens
// to contain. A settings table is a key/value store, and a handler that wrote
// every posted field would let anyone who can reach the page invent keys — or
// overwrite one the code reads with a value it cannot parse. Only these keys are
// writable, and a POST naming anything else is ignored rather than refused,
// because an extra field is a stale form, not an attack worth a page about.
type SettingField struct {
	Key   string
	Label string
	// Numeric fields are parsed and bounded; the rest are trimmed text.
	Numeric  bool
	Min, Max int
	// MaxLen bounds a text field. Zero means the settings TEXT column's practical
	// limit, applied as defaultSettingMaxLen.
	MaxLen int
	// Multiline drives the template's choice of input vs textarea.
	Multiline bool
}

// defaultSettingMaxLen keeps a single setting from becoming a document. The
// column is TEXT; this is about the page, not the storage.
const defaultSettingMaxLen = 500

// EditableSettings is the whole editable surface, in render order. Adding a row
// here adds it to the form; nothing else changes.
//
// The numeric bounds mirror what the readers of each key actually tolerate:
// config.validate() refuses a non-positive expiry at boot, and a lead time
// longer than the window ahead would leave the public picker permanently empty.
var EditableSettings = []SettingField{
	{Key: KeySiteName, Label: "Nama situs", MaxLen: 100},
	{Key: KeySiteTagline, Label: "Tagline", MaxLen: 200},
	{Key: KeySiteDescription, Label: "Deskripsi situs", MaxLen: 300, Multiline: true},
	{Key: KeySiteOGImage, Label: "Gambar Open Graph (path)", MaxLen: 255},
	{Key: KeyContactEmail, Label: "Email kontak", MaxLen: 150},
	{Key: KeyContactPhone, Label: "Telepon kontak", MaxLen: 30},
	{Key: KeyContactAddress, Label: "Area layanan", MaxLen: 300, Multiline: true},
	{Key: KeyWhatsAppNumber, Label: "Nomor WhatsApp", MaxLen: 30},
	{Key: KeyInstagramURL, Label: "URL Instagram", MaxLen: 255},
	{Key: KeyBookingTerms, Label: "Ketentuan booking", MaxLen: 1000, Multiline: true},
	{Key: KeyBookingLeadMinutes, Label: "Jeda minimum booking (menit)", Numeric: true, Min: 0, Max: 10080},
	{Key: KeyBookingMaxDaysAhead, Label: "Maksimal hari ke depan", Numeric: true, Min: 1, Max: 365},
	{Key: KeyPaymentExpiryMinutes, Label: "Batas waktu pembayaran (menit)", Numeric: true, Min: 5, Max: 1440},
	{Key: KeySlotDefaultDuration, Label: "Durasi slot default (menit)", Numeric: true, Min: 15, Max: 480},
	{Key: KeySlotDefaultCapacity, Label: "Kapasitas slot default", Numeric: true, Min: 1, Max: 50},
}

// Update validates and writes the editable settings, then refreshes the cache.
//
// Values are keyed by setting key, and only the keys in EditableSettings are
// read out of the map. Every field is validated before anything is written, so a
// rejected form leaves the table exactly as it was — the same all-or-nothing
// rule every other form in this codebase follows.
//
// The reload happens after the commit, so an edit takes effect on the next
// request with no restart. It is the reason this method exists on the cache
// rather than beside the handler.
func (s *Settings) Update(ctx context.Context, values map[string]string) error {
	ve := NewValidationError()
	writes := make(map[string]string, len(EditableSettings))

	// Iterating the whitelist, never the submitted map, is what makes an unknown
	// posted key a no-op instead of a new settings row.
	for _, f := range EditableSettings {
		raw, ok := values[f.Key]
		if !ok {
			// Not on the submitted form. Leave the stored value alone rather than
			// blanking it — a partial form must not silently clear what it omits.
			continue
		}
		v := strings.TrimSpace(raw)

		if f.Numeric {
			if !required(ve, f.Key, f.Label, v) {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				ve.Add(f.Key, f.Label+" harus berupa angka.")
				continue
			}
			if n < f.Min || n > f.Max {
				ve.Add(f.Key, fmt.Sprintf("%s harus antara %d dan %d.", f.Label, f.Min, f.Max))
				continue
			}
			writes[f.Key] = strconv.Itoa(n)
			continue
		}

		max := f.MaxLen
		if max <= 0 {
			max = defaultSettingMaxLen
		}
		if !maxLen(ve, f.Key, f.Label, v, max) {
			continue
		}
		writes[f.Key] = v
	}

	if ve.Any() {
		return ve
	}

	// One transaction: a half-applied settings page would leave the site
	// describing itself inconsistently, and the whole set is a handful of rows.
	err := s.store.WithTx(ctx, func(q *sqlc.Queries) error {
		for key, value := range writes {
			if err := q.UpsertSetting(ctx, sqlc.UpsertSettingParams{
				SettingKey:   key,
				SettingValue: sql.NullString{String: value, Valid: true},
			}); err != nil {
				return fmt.Errorf("saving setting %q: %w", key, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return s.Reload(ctx)
}
