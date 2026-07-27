package util

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// This file is the inverse of format.go: it turns what a human typed into a form
// back into the canonical shape the database stores. format.go renders, this
// parses, and neither ever produces a float64 — see ParsePrice.

// ErrPriceInvalid is returned by ParsePrice for anything that is not a
// non-negative amount. Callers turn it into a user-facing message; the error
// itself stays in English like the rest of the code.
var ErrPriceInvalid = errors.New("util: price is not a valid amount")

// maxPriceDigits is the integer width of DECIMAL(12,2) — twelve significant
// digits, two of which are the fraction.
const maxPriceDigits = 10

// ParsePrice normalises an admin-typed amount into the canonical DECIMAL(12,2)
// string the database stores: "150000", "150.000" and "Rp150.000" all become
// "150000.00".
//
// It works on the string alone and never converts to a number, for the same
// reason Rupiah does not: services.price is snapshotted into
// bookings.price_amount and from there into payments.gross_amount, so a float64
// anywhere on that path is a rounding bug waiting for a large enough total.
// CLAUDE.md forbids it outright.
//
// Separators follow Indonesian convention, which is the inverse of English:
//
//   - a comma is the decimal point, and every dot is a thousands separator —
//     "150.000,50" is 150000.50;
//   - with no comma, a lone dot followed by one or two digits is read as a
//     decimal point ("150000.5"), because three digits after it is the
//     unambiguous thousands case ("150.000" is one hundred fifty thousand);
//   - anything else, every dot is a thousands separator.
//
// The form hint next to the input states the two shapes that matter ("Contoh:
// 150000 atau 150.000") so the rule above is never something a user has to
// infer.
func ParsePrice(s string) (string, error) {
	// Strip every space rather than only trimming the ends, so "Rp 150 000"
	// parses. unicode.IsSpace rather than a literal " " because a paste out of a
	// spreadsheet carries non-breaking spaces, which are invisible in the form
	// and would otherwise reject a perfectly reasonable amount.
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)

	if len(s) >= 2 && strings.EqualFold(s[:2], "rp") {
		s = s[2:]
	}
	if s == "" {
		return "", ErrPriceInvalid
	}

	// Negative amounts are rejected rather than clamped: chk_services_price
	// requires >= 0, and silently turning -5 into 5 would be worse than saying no.
	if strings.HasPrefix(s, "-") {
		return "", ErrPriceInvalid
	}

	whole, frac, ok := splitAmount(s)
	if !ok {
		return "", ErrPriceInvalid
	}

	if whole == "" || !allDigits(whole) || !allDigits(frac) {
		return "", ErrPriceInvalid
	}
	if len(frac) > 2 {
		return "", ErrPriceInvalid
	}

	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	if len(whole) > maxPriceDigits {
		return "", ErrPriceInvalid
	}

	return whole + "." + (frac + "00")[:2], nil
}

// splitAmount separates the integer and fractional digits of an amount whose
// currency prefix and spaces have already been stripped, applying the separator
// rules documented on ParsePrice. ok is false when the dots cannot be read as a
// well-formed thousands grouping.
func splitAmount(s string) (whole, frac string, ok bool) {
	if i := strings.LastIndex(s, ","); i >= 0 {
		// A comma is present, so it is the decimal point and every dot before it
		// is a thousands separator. A second comma leaves a non-digit in whole,
		// which the caller rejects.
		whole, ok = stripThousands(s[:i])
		return whole, s[i+1:], ok
	}

	if i := strings.LastIndex(s, "."); i >= 0 {
		tail := s[i+1:]
		if n := len(tail); (n == 1 || n == 2) && !strings.Contains(s[:i], ".") {
			return s[:i], tail, true
		}
		whole, ok = stripThousands(s)
		return whole, "", ok
	}

	return s, "", true
}

// stripThousands removes dot separators after checking that they actually group
// the digits into thousands.
//
// Without the check, any dotted string parses: "150000.555" would read as
// 150,000,555 — a thousandfold error from one mistyped separator, silently
// accepted and then charged to a customer. Requiring every group after the first
// to be exactly three digits rejects that while accepting "150.000" and
// "1.234.567".
func stripThousands(s string) (string, bool) {
	if !strings.Contains(s, ".") {
		return s, true
	}

	groups := strings.Split(s, ".")
	if len(groups[0]) < 1 || len(groups[0]) > 3 {
		return "", false
	}
	for _, g := range groups[1:] {
		if len(g) != 3 {
			return "", false
		}
	}
	return strings.Join(groups, ""), true
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ErrPriceFraction is returned by PriceToRupiah for an amount carrying a
// non-zero fraction, which Midtrans cannot represent.
var ErrPriceFraction = errors.New("util: amount has a fractional part")

// PriceToRupiah converts a canonical DECIMAL(12,2) string into whole rupiah:
// "150000.00" becomes 150000.
//
// Midtrans takes gross_amount as an integer for IDR — it has no concept of sen —
// so a price with a fraction cannot be charged, and rounding one silently would
// mean billing an amount the customer never agreed to. It is rejected instead.
//
// Textual, like ParsePrice and Rupiah: the value comes from services.price via
// bookings.price_amount and goes to payments.gross_amount, and CLAUDE.md forbids
// a float64 anywhere on that path.
func PriceToRupiah(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrPriceInvalid
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac {
		if !allDigits(frac) {
			return 0, ErrPriceInvalid
		}
		if strings.Trim(frac, "0") != "" {
			return 0, ErrPriceFraction
		}
	}
	if whole == "" || !allDigits(whole) {
		return 0, ErrPriceInvalid
	}

	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrPriceInvalid
	}
	return n, nil
}

// ErrClockInvalid is returned by ParseClock for anything that is not a time of
// day inside a single calendar day.
var ErrClockInvalid = errors.New("util: not a valid time of day")

// MinutesPerDay bounds a clock value. A slot cannot cross midnight —
// chk_slots_window requires end_time > start_time, and both are TIME columns on
// the same date.
const MinutesPerDay = 24 * 60

// ParseClock reads "08:00" or "08:00:00" as minutes since midnight.
//
// Minutes rather than a time.Time on purpose: schedule_slots.start_time and
// end_time are TIME columns, which sqlc maps to string (go-sql-driver always
// returns TIME that way), and a slot has no date until it is paired with one.
// Building a time.Time to add 150 minutes would invent a date and a timezone the
// column does not carry, and the generator's arithmetic would then be at the
// mercy of whatever day it happened to pick.
//
// Seconds are accepted so a value round-tripped out of the database parses, but
// they must be zero: the schedule is built on whole minutes and a stray
// "08:00:30" is a typo, not an intent.
func ParseClock(s string) (int, error) {
	s = strings.TrimSpace(s)

	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, ErrClockInvalid
	}
	for _, p := range parts {
		if len(p) != 2 || !allDigits(p) {
			return 0, ErrClockInvalid
		}
	}

	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	if h > 23 || m > 59 {
		return 0, ErrClockInvalid
	}
	if len(parts) == 3 && parts[2] != "00" {
		return 0, ErrClockInvalid
	}

	return h*60 + m, nil
}

// FormatClock renders minutes since midnight as the "HH:MM:SS" a MySQL TIME
// column takes: 510 becomes "08:30:00".
//
// A value of exactly MinutesPerDay renders as "24:00:00", which MySQL rejects —
// callers must not produce one. The generator's window walk stops before it can
// (see Schedule.expand), and ParseClock cannot return one.
func FormatClock(minutes int) string {
	if minutes < 0 {
		minutes = 0
	}
	return fmt.Sprintf("%02d:%02d:00", minutes/60, minutes%60)
}

// maxSlugLen matches services.slug VARCHAR(150).
const maxSlugLen = 150

// Slugify turns a name into the URL-safe slug that becomes /layanan/{slug}:
// "Urut Therapeutic" becomes "urut-therapeutic". It returns "" when nothing
// usable survives, which the caller replaces with a fallback stem.
//
// Every rune outside [a-z0-9] becomes a separator, so accented and non-Latin
// characters are dropped rather than transliterated. Adding a transliteration
// table would mean golang.org/x/text — the project's first new dependency — for
// a cosmetic gain on copy that is written in Indonesian, which is ASCII.
func Slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		// Collapse runs of separators as they are written rather than in a
		// second pass. The leading-dash case is handled by not emitting one
		// before any real character has been written.
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if len(out) > maxSlugLen {
		// Cutting can land mid-word, which is fine for a slug, and can leave a
		// trailing dash, which is not.
		out = strings.TrimRight(out[:maxSlugLen], "-")
	}
	return out
}

// EscapeLike escapes the MySQL LIKE metacharacters so a search box entry matches
// literally.
//
// Without it, typing "%" into the admin search returns every row and "_" matches
// any single character — a filter that quietly ignores what was typed. The
// queries wrap the result in %…% themselves and rely on the default backslash
// escape character, so they carry no ESCAPE clause.
func EscapeLike(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	).Replace(s)
}
