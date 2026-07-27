#!/usr/bin/env bash
# Vendor Hotwire Turbo into static/js/.
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
# This script writes turbo.js and nothing else. static/js/app.js is ours, hand
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
DEST=static/js
mkdir -p "$DEST"

url="https://unpkg.com/@hotwired/turbo@${TURBO_VERSION}/dist/turbo.es2017-umd.js"
curl -fsSL -o "$DEST/turbo.js" "$url" || {
  echo "FAILED: could not download $url" >&2
  exit 1
}

printf 'wrote %s/turbo.js (turbo %s, %s)\n' "$DEST" "$TURBO_VERSION" \
  "$(du -h "$DEST/turbo.js" | cut -f1 | tr -d ' ')"
