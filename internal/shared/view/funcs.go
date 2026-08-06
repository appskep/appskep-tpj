package view

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
	"github.com/remorac/appskep-tpj/internal/shared/util"
)

// Badge is a status rendered as a label plus the Tailwind classes that colour it.
//
// A struct rather than raw HTML on purpose: returning markup from a template
// helper means template.HTML, and PLAN.md line 463 requires every use of that to
// be audited. A struct rendered by the badge partial needs no audit at all.
type Badge struct {
	Label string
	Class string
}

// bookingBadges maps every value of the bookings.status enum to its Indonesian
// label and colour. Keyed by the generated enum type so adding a status to the
// schema and regenerating surfaces the gap here as a missing key rather than a
// blank badge.
var bookingBadges = map[sqlc.BookingsStatus]Badge{
	sqlc.BookingsStatusPendingPayment: {"Menunggu pembayaran", "bg-warning-soft text-warning"},
	sqlc.BookingsStatusPaid:           {"Sudah dibayar", "bg-info-soft text-info"},
	sqlc.BookingsStatusConfirmed:      {"Dikonfirmasi", "bg-forest-100 text-forest-700"},
	sqlc.BookingsStatusCompleted:      {"Selesai", "bg-success-soft text-success"},
	sqlc.BookingsStatusCancelled:      {"Dibatalkan", "bg-danger-soft text-danger"},
	sqlc.BookingsStatusExpired:        {"Kedaluwarsa", "bg-paper-2 text-ink-soft"},
}

// paymentBadges maps the four derived payment lifecycle states to their
// Indonesian label and colour.
//
// A payment has no status column — its state is computed from paid_at /
// expired_at / cancelled_at, in SQL for the list and in Go for the single row
// (service.PaymentState). Those keys are the strings that computation produces.
//
// This lives beside bookingBadges rather than in a template for the reason
// Phase 9 settled: the admin filter chips and the badge they filter to must draw
// their labels from one table, or one state ends up with two names on one page.
var paymentBadges = map[string]Badge{
	"pending":   {"Menunggu", "bg-warning-soft text-warning"},
	"paid":      {"Dibayar", "bg-success-soft text-success"},
	"expired":   {"Kedaluwarsa", "bg-paper-2 text-ink-soft"},
	"cancelled": {"Dibatalkan", "bg-danger-soft text-danger"},
}

// PaymentBadge maps a payment lifecycle state to its label and colour.
//
// Exported for the same reason StatusBadge is: the admin chips need the labels
// the badges use. An unknown value shows the raw state, so a gap is visible in
// the UI rather than rendering an empty pill.
func PaymentBadge(state string) Badge {
	if b, ok := paymentBadges[state]; ok {
		return b
	}
	return Badge{Label: state, Class: "bg-paper-2 text-ink-soft"}
}

// NavItem is one entry in a navigation menu.
type NavItem struct {
	Href  string
	Label string
	// Icon is a sprite symbol id. Empty for text-only items.
	Icon string
	// Exact marks a section index — a link that is a prefix of its siblings and so
	// matches only its own path, never the subtree beneath it. Without it /admin
	// highlights "Dasbor" on every admin page, because every one of them starts
	// with /admin/.
	Exact bool
}

// The site navigation, defined here rather than repeated across layouts so the
// header, the mobile menu and the admin sidebar cannot drift apart. Typed values
// rather than template-side map literals: html/template has no dict builtin, and
// a typo in a field name should be a template parse error, not a blank link.
var (
	publicNav = []NavItem{
		{Href: "/layanan", Label: "Layanan"},
		{Href: "/terapis", Label: "Terapis"},
		{Href: "/booking", Label: "Booking"},
		{Href: "/riwayat", Label: "Riwayat"},
	}

	// Phase 4 onward fills these routes in; the sidebar links to them now so the
	// chrome is complete and each phase only has to add its handler.
	adminNav = []NavItem{
		{Href: "/admin", Label: "Dasbor", Icon: "layout-dashboard", Exact: true},
		{Href: "/admin/booking", Label: "Booking", Icon: "receipt"},
		{Href: "/admin/jadwal", Label: "Penjadwalan", Icon: "calendar-days"},
		{Href: "/admin/layanan", Label: "Layanan", Icon: "sparkles"},
		// "user", not "users": the latter is Pengguna's, and two sidebar entries
		// sharing an icon is two entries the operator has to read to tell apart.
		{Href: "/admin/terapis", Label: "Terapis", Icon: "user"},
		{Href: "/admin/pembayaran", Label: "Pembayaran", Icon: "credit-card"},
		{Href: "/admin/users", Label: "Pengguna", Icon: "users"},
		{Href: "/admin/pengaturan", Label: "Pengaturan", Icon: "settings"},
	}
)

// iconNamePattern and iconClassPattern bound what the icon helper will emit.
// Names must match a symbol id in the sprite; classes are Tailwind utilities.
var (
	iconNamePattern  = regexp.MustCompile(`^[a-z0-9-]+$`)
	iconClassPattern = regexp.MustCompile(`^[a-zA-Z0-9 :_/\[\].%-]*$`)
)

// funcMap is the template function set. Every entry is a thin wrapper over util
// or over a value already in the envelope — no business logic lives here.
func (r *Renderer) funcMap() template.FuncMap {
	return template.FuncMap{
		// --- Formatting: all of it delegates to util ---
		"rupiah":      util.Rupiah,
		"dateID":      func(t time.Time) string { return util.DateID(r.inAppTZ(t)) },
		"dateShort":   func(t time.Time) string { return util.DateShortID(r.inAppTZ(t)) },
		"dateCompact": func(t time.Time) string { return util.DateCompactID(r.inAppTZ(t)) },
		"dateTimeID":  func(t time.Time) string { return util.DateTimeID(r.inAppTZ(t)) },
		"monthID":     func(t time.Time) string { return util.MonthYearID(r.inAppTZ(t)) },
		"clock":       util.HourMinute,
		"timeRange":   util.TimeRange,
		"duration":    util.Duration,
		"truncate":    util.Truncate,
		// paragraphs returns []string, not markup: the template writes its own <p>
		// tags so every paragraph still goes through html/template's escaping.
		"paragraphs": util.Paragraphs,

		// --- Templates cannot unwrap sql.Null* types themselves ---
		"nullstr": nullString,
		"nullint": nullInt64,

		// --- Markup helpers ---
		"icon":         icon,
		"statusBadge":  StatusBadge,
		"paymentBadge": PaymentBadge,
		"csrfField":    csrfField,
		"asset":        r.asset,

		// --- Links and navigation ---
		"waLink":    util.WhatsAppLink,
		"loginURL":  r.loginURL,
		"authURL":   func() string { return r.cfg.Auth.URL },
		"activeNav": activeNav,
		"publicNav": r.visiblePublicNav,
		"adminNav":  func() []NavItem { return adminNav },
		// hasTerapis exists for the footer's "Jelajahi" list, which is hardcoded
		// rather than driven by publicNav — its labels differ and it carries
		// /profil, which is not a nav entry. The two must be kept in step by hand.
		"hasTerapis": func() bool { return r.therapists.HasActive() },

		// --- Small predicates templates would otherwise fake with string tricks ---
		"hasPrefix": strings.HasPrefix,
		"add":       func(a, b int) int { return a + b },
	}
}

// inAppTZ moves a time into the application location before formatting. DATE and
// DATETIME columns already arrive in that location because the driver loc is
// pinned (config.DBConfig.DSN), but a time built in Go elsewhere might not be.
func (r *Renderer) inAppTZ(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.In(r.cfg.App.Location)
}

// icon renders a sprite reference: {{icon "calendar" "w-5 h-5"}}.
//
// This is the only template.HTML in the codebase. It is safe because the shape of
// the output is fixed and neither argument reaches it unvalidated: the name must
// be a lowercase sprite id and the class must look like Tailwind utilities.
// Anything else renders nothing rather than being escaped into visible garbage or
// interpolated into the attribute.
//
// aria-hidden because every icon in this app sits beside its own text label; an
// icon that ever stands alone needs a visually-hidden label next to it, not a
// title element here.
func icon(name, class string) template.HTML {
	if !iconNamePattern.MatchString(name) || !iconClassPattern.MatchString(class) {
		return ""
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="%s" aria-hidden="true" focusable="false"><use href="/static/img/icons.svg#%s"></use></svg>`,
		class, name,
	))
}

// StatusBadge maps a booking status to its label and colour. An unknown value —
// only reachable if the enum gains a member and this map is not updated — shows
// the raw status rather than an empty badge, so the gap is visible in the UI.
//
// Exported as well as bound into the FuncMap because Phase 9's riwayat filter
// chips need the same labels the badges use. A handler that retyped them would
// eventually call the same status something else on the two halves of one page.
func StatusBadge(status sqlc.BookingsStatus) Badge {
	if b, ok := bookingBadges[status]; ok {
		return b
	}
	return Badge{Label: string(status), Class: "bg-paper-2 text-ink-soft"}
}

// csrfField renders the hidden CSRF input for a form.
//
// An empty token renders nothing, which is the anonymous case: no page reachable
// without a session carries a form, and the middleware rejects a POST that
// arrives without a valid token either way.
func csrfField(token string) template.HTML {
	if token == "" {
		return ""
	}
	return template.HTML(fmt.Sprintf(
		`<input type="hidden" name="_csrf" value="%s">`, template.HTMLEscapeString(token),
	))
}

// asset appends the cache-busting version to a static path, so a deploy
// invalidates the cached CSS without a fingerprinting build step.
func (r *Renderer) asset(path string) string {
	return path + "?v=" + r.assetVersion
}

// waLink builds a wa.me deep link. The rule lives in util so the Phase 11 emails
// — which cannot import this package — build the identical link.

// loginURL is the SSO entry point for a "Masuk" link, returning the user to next
// after login. The auth host is never hardcoded in a template — PLAN.md Phase 3
// lists that as one of the reference app's defects not to repeat.
func (r *Renderer) loginURL(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	return "/login?next=" + url.QueryEscape(next)
}

// hrefTerapis is the one nav entry that depends on data existing.
const hrefTerapis = "/terapis"

// visiblePublicNav drops nav entries whose section has nothing in it.
//
// Terapis is the only conditional one, and it is named here rather than given a
// general mechanism: Layanan, Booking and Riwayat are structural — the business
// always has them — so a predicate field on NavItem would be scaffolding built
// for a single case. Generalise if a second appears.
//
// Why it is conditional at all: the section is content an operator fills in, and
// until they have, a menu entry leading to "Belum ada profil terapis" advertises
// a room with nothing in it. Same rule as the coming-soon CTA, which is replaced
// rather than left looking clickable.
//
// The filtered result is a copy. Filtering the package slice in place would
// mutate shared state and the entry could never come back.
func (r *Renderer) visiblePublicNav() []NavItem {
	if r.therapists.HasActive() {
		return publicNav
	}

	out := make([]NavItem, 0, len(publicNav))
	for _, item := range publicNav {
		if item.Href != hrefTerapis {
			out = append(out, item)
		}
	}
	return out
}

// activeNav reports whether a nav item should be marked current. A section index
// — the root link, or any item flagged Exact — matches only its own path; every
// other link matches its own subtree, so /layanan/urut-therapeutic still
// highlights "Layanan".
func activeNav(current string, item NavItem) bool {
	target := item.Href
	if item.Exact || target == "/" {
		return current == target || current == target+"/"
	}
	return current == target || strings.HasPrefix(current, target+"/")
}

// nullString unwraps sql.NullString, which html/template would otherwise render
// as "{text true}".
func nullString(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// nullInt64 unwraps sql.NullInt64 the same way.
func nullInt64(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}
