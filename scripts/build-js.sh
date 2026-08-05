#!/usr/bin/env bash
# Vendor Hotwire Turbo and Leaflet into static/.
#
# Self-hosted, no CDN (PLAN.md Phase 2). The file is committed so a clean
# checkout runs offline.
#
# Named turbo.js, not turbo.min.js as PLAN.md line 226 anticipated: @hotwired/turbo
# 8 publishes only two unminified builds (es2017-esm and es2017-umd), so there is
# no minified artifact upstream to vendor. Calling it .min.js would be a lie about
# the file. It gzips from 212K to roughly 50K, which the reverse proxy handles.
#
# The UMD build is used rather than the ESM one so the script tag needs no
# type="module", which in turn keeps it working with `defer` ordering.
#
# Leaflet is the map on the booking and profile forms. Vendored for the same
# reason Turbo is, and with more at stake: it is the one place the app was asked
# for a third-party map, and loading it from a CDN would mean a script-src origin
# in a Content-Security-Policy that currently has none. Self-hosted, it is an
# ordinary 'self' script and the only cross-origin thing left is the tiles, which
# are images.
#
# It ships as three kinds of file, in three places, and the split is deliberate:
#
#   static/js/leaflet.js      lazy-loaded by app.js, only on pages with a map
#   static/css/leaflet.css    @imported by input.css, so it lands INSIDE app.css
#                             and is covered by view.assetVersion's hash — a
#                             separate <link> would be served immutable for a
#                             year with nothing to bust it
#   static/css/images/*.png   the marker sprites. Under css/ because that is
#                             where leaflet.css's own relative url() references
#                             resolve to once it is inlined into app.css.
#
# This script writes those and nothing else. static/js/app.js is ours, hand
# written, and `make js` must never touch it.
#
# AFTER BUMPING TURBO_VERSION: re-derive turboProgressBarCSS in
# internal/shared/middleware/secure.go. Turbo injects its progress-bar stylesheet
# as an inline <style>, which the Content-Security-Policy admits by hash. A
# changed stylesheet makes the hash stale and the loading bar silently stops
# appearing — the browser console is the only place that says so.
set -euo pipefail

cd "$(dirname "$0")/.."

TURBO_VERSION=8.0.23
LEAFLET_VERSION=1.9.4
DEST=static/js
CSS_DEST=static/css
IMG_DEST=static/css/images
mkdir -p "$DEST" "$CSS_DEST" "$IMG_DEST"

# fetch <url> <destination>
fetch() {
  curl -fsSL -o "$2" "$1" || {
    echo "FAILED: could not download $1" >&2
    exit 1
  }
}

fetch "https://unpkg.com/@hotwired/turbo@${TURBO_VERSION}/dist/turbo.es2017-umd.js" \
  "$DEST/turbo.js"

printf 'wrote %s/turbo.js (turbo %s, %s)\n' "$DEST" "$TURBO_VERSION" \
  "$(du -h "$DEST/turbo.js" | cut -f1 | tr -d ' ')"

leaflet="https://unpkg.com/leaflet@${LEAFLET_VERSION}/dist"
fetch "$leaflet/leaflet.js"  "$DEST/leaflet.js"
fetch "$leaflet/leaflet.css" "$CSS_DEST/leaflet.css"
# All five leaflet.css references, not only the three the marker uses: the layers
# control is not built by app.js today, but a stylesheet whose url() 404s is a
# thing nobody notices until it renders.
for img in marker-icon.png marker-icon-2x.png marker-shadow.png layers.png layers-2x.png; do
  fetch "$leaflet/images/$img" "$IMG_DEST/$img"
done

printf 'wrote %s/leaflet.js, %s/leaflet.css and 5 images (leaflet %s, %s)\n' \
  "$DEST" "$CSS_DEST" "$LEAFLET_VERSION" \
  "$(du -h "$DEST/leaflet.js" | cut -f1 | tr -d ' ')"
