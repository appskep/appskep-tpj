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
// No third-party origin appears here because the app loads none. Midtrans' Snap
// is reached by a server-side 303 to their hosted page, not by their snap.js, so
// there is no script or frame origin to allow. Fonts, CSS, the icon sprite and
// Turbo are all self-hosted.
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

const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' " + turboProgressBarCSS + "; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
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
			// policy in StaticHandler.
			if !strings.HasPrefix(r.URL.Path, "/api") {
				h.Set("Content-Security-Policy", contentSecurityPolicy)
			}

			if production {
				h.Set("Strict-Transport-Security", hstsValue)
			}

			next.ServeHTTP(w, r)
		})
	}
}
