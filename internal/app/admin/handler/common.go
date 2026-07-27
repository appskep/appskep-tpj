package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/service"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Shared helpers for the Phase 10 admin modules: the filter chips, the CSV
// writer, and the small conversions three pages would otherwise each define.

// filterChip is one link-shaped filter control.
//
// Distinct from filterOption, which is a <select> option on the jadwal page and
// carries a Value instead of an Href. Two types rather than one with both
// fields: a chip whose Href was empty would render as a dead link, and the
// compiler is a better place to catch that than the page.
type filterChip struct {
	Label    string
	Href     string
	Selected bool
}

// baseURL builds the prefix the pagination partial appends "page=" to. It must
// carry the active filter and end in ? or &, or paging would drop it.
func baseURL(path string, q url.Values) string {
	q = cloneValues(q)
	q.Del("page")
	if len(q) == 0 {
		return path + "?"
	}
	return path + "?" + q.Encode() + "&"
}

// baseURLNoTrailer is baseURL for links that are complete in themselves — a
// filter chip, not a pagination prefix.
func baseURLNoTrailer(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// orEmpty keeps a template from having to test a nil map before indexing it.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// withoutCancel detaches a context from the request that carried it, for work
// that must finish after the response is written — closing a Midtrans order
// after a cancellation commits.
func withoutCancel(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// clientIP is the best available address for the audit trail.
//
// It reads RemoteAddr only. X-Forwarded-For is attacker-controlled unless a
// trusted proxy list says otherwise, and Phase 12 owns that resolver — main.go
// already refuses chi's RealIP for the same reason. An audit row that names the
// proxy is honest; one that names whatever the client typed is not.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Timeline
// ---------------------------------------------------------------------------

// timelineLabels translates an audit action into the operator's language. An
// unknown action shows its raw name rather than nothing, so a missing entry here
// is visible instead of silently blank.
var timelineLabels = map[string]string{
	service.ActionBookingConfirm:    "Booking dikonfirmasi",
	service.ActionBookingComplete:   "Booking diselesaikan",
	service.ActionBookingCancel:     "Booking dibatalkan",
	service.ActionBookingReschedule: "Jadwal dipindahkan",
	service.ActionBookingNotes:      "Catatan diperbarui",
	service.ActionPaymentSync:       "Status pembayaran diperiksa ulang",
	service.ActionUserRole:          "Peran pengguna diubah",
	service.ActionUserActive:        "Status pengguna diubah",
	service.ActionSettingsUpdate:    "Pengaturan disimpan",
}

func buildTimeline(logs []sqlc.ActivityLog) []timelineEntry {
	out := make([]timelineEntry, 0, len(logs))
	for _, l := range logs {
		label, ok := timelineLabels[l.Action]
		if !ok {
			label = l.Action
		}
		out = append(out, timelineEntry{
			At:     l.CreatedAt,
			Label:  label,
			Detail: timelineDetail(l.Meta),
		})
	}
	return out
}

// timelineDetail renders the meta column as a compact "key: value" line.
//
// It decodes into a map rather than rendering the raw JSON, so a stored value
// reaches the page as text that html/template escapes normally. Anything that
// does not decode is dropped: the label above it already says what happened.
func timelineDetail(meta sql.NullString) string {
	if !meta.Valid || strings.TrimSpace(meta.String) == "" {
		return ""
	}

	var fields map[string]any
	if err := json.Unmarshal([]byte(meta.String), &fields); err != nil {
		return ""
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sortStrings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+formatMetaValue(fields[k]))
	}
	return strings.Join(parts, " · ")
}

func formatMetaValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// encoding/json decodes every number as float64. Integers are the only
		// numbers this audit trail writes, so render them without a decimal tail.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "ya"
		}
		return "tidak"
	case nil:
		return "-"
	default:
		return ""
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// slotOptions turns free slots into the reschedule picker's entries, dropping
// the one the booking already holds.
func slotOptions(slots []sqlc.ScheduleSlot, currentID int64) []slotOption {
	out := make([]slotOption, 0, len(slots))
	for _, s := range slots {
		if s.ID == currentID {
			continue
		}
		out = append(out, slotOption{
			ID: s.ID,
			Label: util.DateShortID(s.SlotDate) + " · " +
				util.TimeRange(s.StartTime, s.EndTime),
			Free: s.Capacity - s.BookedCount,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// CSV export
// ---------------------------------------------------------------------------

// writeCSV streams records as a downloadable spreadsheet.
//
// Three things here are load-bearing:
//
//   - It does NOT go through view.Renderer. That forces Content-Type text/html
//     and only globs pages/*.html — the same constraint that made sitemap.xml an
//     encoding/xml document rather than a template.
//   - A UTF-8 BOM is written first. Without it Excel reads the file as the local
//     ANSI codepage and every Indonesian name with an accent arrives mangled.
//   - Every field goes through csvSafe. A spreadsheet treats a leading =, +, -
//     or @ as a formula, and the customer name and booking note in these files
//     are text a stranger typed into a public form.
func (h *Booking) writeCSV(w http.ResponseWriter, r *http.Request, name string, records [][]string, truncated bool) {
	writeCSV(w, r, h.deps.Log, name, records, truncated)
}

func (h *Pembayaran) writeCSV(w http.ResponseWriter, r *http.Request, name string, records [][]string, truncated bool) {
	writeCSV(w, r, h.deps.Log, name, records, truncated)
}

func writeCSV(
	w http.ResponseWriter,
	r *http.Request,
	log *slog.Logger,
	name string,
	records [][]string,
	truncated bool,
) {
	filename := name + "-" + time.Now().Format("20060102-150405") + ".csv"

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// A filtered export is live data; a cached copy would answer tomorrow's click
	// with today's numbers.
	w.Header().Set("Cache-Control", "no-store")
	if truncated {
		// The row cap bit. A header rather than an extra CSV row, which would
		// corrupt the column layout of the file itself.
		w.Header().Set("X-Export-Truncated", strconv.Itoa(len(records)-1))
	}
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return
	}

	cw := csv.NewWriter(w)
	for _, rec := range records {
		for i := range rec {
			rec[i] = csvSafe(rec[i])
		}
		if err := cw.Write(rec); err != nil {
			// The status line is already sent, so there is no error page to render.
			// Logging is all that is left, and the truncated download is visible.
			log.ErrorContext(r.Context(), "admin: writing csv", slog.Any("error", err))
			return
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.ErrorContext(r.Context(), "admin: flushing csv", slog.Any("error", err))
	}
}

// csvSafe neutralises spreadsheet formula injection.
//
// A cell beginning =, +, - or @ is evaluated as a formula by Excel, LibreOffice
// and Sheets, and these files carry names, addresses and notes typed by whoever
// made the booking. Prefixing with an apostrophe is the standard remedy: the
// cell displays its text and computes nothing. A tab or carriage return leading
// the value is stripped first, because those bypass the check.
func csvSafe(s string) string {
	s = strings.TrimLeft(s, "\t\r\n")
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// prettyJSON re-indents a stored payload for the detail pages.
//
// It returns plain text, which the template renders inside a <pre> and
// html/template escapes like anything else — this is a gateway's response body
// and a stranger's webhook payload, and neither becomes markup. A body that does
// not parse is shown exactly as stored, because an unreadable payload is
// precisely the one worth reading.
func prettyJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}

func csvDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func csvTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func csvNullTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return csvTime(t.Time)
}
