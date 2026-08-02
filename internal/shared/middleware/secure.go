package middleware

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy is deliberately strict, and the app was changed to fit
// it rather than the other way round.
//
// script-src has no 'unsafe-inline' and no nonce because there is no inline
// script left: the four that existed (three onclick handlers opening a <dialog>,
// and the payment poller) moved into static/js/app.js in Phase 12. A nonce would
// have been the easier route and a worse one — it does not cover attribute
// handlers, so those had to be rewritten regardless, and once they are gone the
// nonce buys nothing.
//
// No third-party origin appears here as a source the app LOADS from. Midtrans'
// Snap is reached by a server-side 303 to their hosted page, not by their
// snap.js, so there is no script or frame origin to allow. Fonts, CSS, the icon
// sprite and Turbo are all self-hosted. form-action is the one exception, and it
// is about where a form may SEND the customer rather than what the page pulls in;
// see snapOrigins.
//
// data: is allowed for images only, for inline SVG data URIs; object-src 'none'
// and base-uri 'none' close the two classic injection escapes, and
// frame-ancestors 'none' is the modern X-Frame-Options that also covers browsers
// that ignore the header.
// turboProgressBarCSS is the SHA-256 of the one stylesheet Turbo injects at
// runtime: the three-pixel loading bar it shows during a slow Drive navigation
// (ProgressBar.defaultCSS, static/js/turbo.js:3526). It arrives as a <style>
// element inserted into <head>, so style-src 'self' alone refuses it and the
// progress bar silently never appears — found in a browser, invisible to curl.
//
// A hash rather than 'unsafe-inline', because 'unsafe-inline' would also permit
// every future inline style anywhere on the site and this is the only one that
// exists. The cost is that re-vendoring Turbo can change the string: if `make js`
// bumps the version, re-derive this or the bar goes missing again. That warning
// is repeated in scripts/build-js.sh, which is where someone would do it.
const turboProgressBarCSS = "'sha256-WAyOw4V+FqDc35lQPyRADLBWbuNK8ahvYEaQIYF1+Ps='"

// snapOrigins is where "Bayar sekarang" actually lands. That POST answers with a
// 303 to Midtrans' hosted page, and form-action is checked against every hop of a
// form navigation's redirect chain in Firefox and Safari — so 'self' alone blocks
// the customer's only route to paying, in the browsers that check.
//
// Both hosts, always: which one is live depends on MIDTRANS_ENV, and a form target
// the app never points at costs nothing.
//
// form-action ONLY. Nothing is loaded from Midtrans, so there is still no script,
// frame or connect origin to allow — and connect-src in particular must stay
// 'self', which is why the pay form carries data-turbo="false" rather than being
// let through as a fetch.
const snapOrigins = "https://app.midtrans.com https://app.sandbox.midtrans.com"

const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' " + turboProgressBarCSS + "; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self' " + snapOrigins + "; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

// permissionsPolicy switches off the capabilities this site never uses, so a
// script that somehow does run cannot reach for them.
const permissionsPolicy = "geolocation=(), camera=(), microphone=(), payment=(), usb=()"

// hstsValue is a year, with subdomains. No preload directive: this app is one
// system on a shared Appskep domain, and preloading commits every sibling
// subdomain to HTTPS from a header only this one serves.
const hstsValue = "max-age=31536000; includeSubDomains"

// SecureHeaders sets the response headers that tell a browser what this site is
// allowed to do. Applied at the root, so it covers static files, the JSON API and
// every page.
//
// production controls HSTS alone: sending it from a development server on plain
// HTTP would pin localhost to HTTPS in the developer's browser for a year, which
// is remarkably hard to undo.
func SecureHeaders(production bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", permissionsPolicy)

			// The API answers JSON to a program, and a policy about what a document
			// may load says nothing about that. Static files get their own, tighter
			// policy in StaticHandler. /midtrans is the payment webhook, JSON for
			// the same reason; app.Deps.Recoverer lists the same pair and the two
			// must agree — this package cannot import internal/app to share it.
			if !strings.HasPrefix(r.URL.Path, "/api") && !strings.HasPrefix(r.URL.Path, "/midtrans") {
				h.Set("Content-Security-Policy", contentSecurityPolicy)
			}

			if production {
				h.Set("Strict-Transport-Security", hstsValue)
			}

			next.ServeHTTP(w, r)
		})
	}
}
