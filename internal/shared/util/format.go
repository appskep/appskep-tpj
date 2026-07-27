package util

import (
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// This file is the single place that formats money, dates and times for display.
// Handlers and templates never format inline — CLAUDE.md: "One helper does all
// formatting."

// Indonesian day and month names. time.Time.Format has no locale support, so the
// names are indexed directly by time.Weekday and time.Month.
var (
	dayNamesID = [...]string{
		"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu",
	}
	monthNamesID = [...]string{
		"", "Januari", "Februari", "Maret", "April", "Mei", "Juni",
		"Juli", "Agustus", "September", "Oktober", "November", "Desember",
	}
	monthShortID = [...]string{
		"", "Jan", "Feb", "Mar", "Apr", "Mei", "Jun",
		"Jul", "Agu", "Sep", "Okt", "Nov", "Des",
	}
)

// Rupiah formats a DECIMAL(12,2) value as "Rp150.000".
//
// The input is a string because that is what sqlc returns for DECIMAL columns on
// MySQL (services.price, bookings.price_amount, payments.gross_amount). Parsing
// it into a float to format it would put a float64 on a payment path, which
// CLAUDE.md forbids — so the fractional part is split off textually and never
// converted to a number at all.
//
// A zero fraction (".00", which is every price in practice — rupiah has no
// circulating sub-unit) prints nothing. A non-zero fraction is shown after a
// comma rather than rounded away, so a bad amount is visible instead of silent.
func Rupiah(amount string) string {
	s := strings.TrimSpace(amount)
	if s == "" {
		return "Rp0"
	}

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}

	var b strings.Builder
	if neg {
		b.WriteString("-")
	}
	b.WriteString("Rp")
	b.WriteString(groupThousands(whole))

	// "00" and "" are the overwhelmingly common cases and print nothing.
	if f := strings.TrimRight(frac, "0"); f != "" {
		b.WriteString(",")
		b.WriteString(f)
	}
	return b.String()
}

// groupThousands inserts "." every three digits from the right. Anything that is
// not a digit is passed through untouched, so a malformed value degrades to
// itself rather than to a panic or a wrong number.
func groupThousands(digits string) string {
	for _, r := range digits {
		if !unicode.IsDigit(r) {
			return digits
		}
	}

	n := len(digits)
	if n <= 3 {
		return digits
	}

	var b strings.Builder
	b.Grow(n + n/3)
	lead := n % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteString(".")
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// DateID formats a date as "Sabtu, 26 Juli 2026".
//
// The caller is responsible for handing over a time already in the application
// location; the renderer does that centrally. DATE columns scan with the driver
// loc pinned to APP_TZ (see config.DBConfig.DSN), so they arrive correct already.
func DateID(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return dayNamesID[int(t.Weekday())] + ", " + DateShortID(t)
}

// DateShortID formats a date as "26 Juli 2026", without the weekday.
func DateShortID(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return strconv.Itoa(t.Day()) + " " + monthNamesID[int(t.Month())] + " " + strconv.Itoa(t.Year())
}

// DateCompactID formats a date as "26 Jul 2026", for table cells.
func DateCompactID(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return strconv.Itoa(t.Day()) + " " + monthShortID[int(t.Month())] + " " + strconv.Itoa(t.Year())
}

// MonthYearID formats a month as "Juli 2026", for the calendar heading.
func MonthYearID(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return monthNamesID[int(t.Month())] + " " + strconv.Itoa(t.Year())
}

// DateTimeID formats a timestamp as "26 Juli 2026, 14:30".
func DateTimeID(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return DateShortID(t) + ", " + t.Format("15:04")
}

// DateISO formats a date as "2026-07-26" — W3C Datetime, which is what a
// sitemap <lastmod> takes. The only formatter here that is not user-facing, and
// it lives with the others because one package does all the date formatting.
func DateISO(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

// HourMinute trims a MySQL TIME value to "08:00".
//
// schedule_slots.start_time and end_time are TIME columns, which sqlc.yaml maps
// to string because go-sql-driver always returns TIME as a string. Values arrive
// as "08:00:00".
func HourMinute(clock string) string {
	s := strings.TrimSpace(clock)
	if len(s) >= 5 {
		return s[:5]
	}
	return s
}

// TimeRange formats a slot's window as "08:00 – 10:30", using an en dash.
func TimeRange(start, end string) string {
	return HourMinute(start) + " – " + HourMinute(end)
}

// Duration renders a minute count the way the copy does: "2,5 jam", "45 menit",
// "3 jam 30 menit". The decimal comma is Indonesian convention, and the spec
// itself writes the default slot length as "2,5 jam".
func Duration(minutes int32) string {
	switch {
	case minutes <= 0:
		return "-"
	case minutes < 60:
		return strconv.Itoa(int(minutes)) + " menit"
	}

	hours := minutes / 60
	rest := minutes % 60
	switch rest {
	case 0:
		return strconv.Itoa(int(hours)) + " jam"
	case 30:
		// Half hours read better as a decimal than as "2 jam 30 menit".
		return strconv.Itoa(int(hours)) + ",5 jam"
	default:
		return strconv.Itoa(int(hours)) + " jam " + strconv.Itoa(int(rest)) + " menit"
	}
}

// Paragraphs splits free text into paragraphs on blank lines, so a multi-line
// description renders as prose instead of one run-on block.
//
// It returns []string rather than markup: a helper that emitted <p> tags would
// have to return template.HTML, and icon is deliberately the only template.HTML
// in this codebase. The template ranges over the result and writes its own tags,
// so every paragraph is still escaped by html/template.
//
// A single newline is left alone — it is almost always wrapping in a textarea,
// not a paragraph break.
func Paragraphs(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")

	out := make([]string, 0, 4)
	for block := range strings.SplitSeq(s, "\n\n") {
		if p := strings.TrimSpace(block); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Truncate shortens s to at most max runes, appending an ellipsis when it cuts.
// It breaks on the last space before the limit so words stay whole, and counts
// runes rather than bytes so multi-byte characters are not split.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}

	cut := string(runes[:max])
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

// WhatsAppLink builds a wa.me deep link. number is the settings value in
// international form without a plus ("6281234567890"); text is prefilled into
// the chat and may be empty. An unset or punctuation-only number yields "", so a
// caller can test the result rather than the input.
//
// It lives here, and not in view, because both the templates (through the waLink
// FuncMap entry) and the Phase 11 emails need it — and an email cannot reach
// view, which imports service. Same rule as every other formatter: one helper,
// one behaviour.
func WhatsAppLink(number, text string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, number)

	if digits == "" {
		return ""
	}
	if text == "" {
		return "https://wa.me/" + digits
	}
	return "https://wa.me/" + digits + "?text=" + url.QueryEscape(text)
}
