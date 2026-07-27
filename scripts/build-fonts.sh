#!/usr/bin/env bash
# Download the self-hosted woff2 fonts into static/fonts/.
#
# Both families are variable fonts under the SIL Open Font License, served from
# Google's CDN but stored locally: PLAN.md Phase 2 requires self-hosted fonts and
# no CDN at runtime. The files are committed, so this script only runs when a
# family or subset changes.
#
#   Bricolage Grotesque — display face (hero, section heads)
#   Plus Jakarta Sans   — body and UI face
#
# Only the `latin` subset is kept. Indonesian is written in plain Latin script
# with no diacritics beyond what latin covers, so latin-ext would be dead weight.
set -euo pipefail

cd "$(dirname "$0")/.."

DEST=static/fonts
mkdir -p "$DEST"

# A modern browser UA is required: the css2 endpoint serves ttf to unknown
# clients and woff2 only to browsers that advertise support for it.
UA='Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36'

# fetch_latin <css-url> <output-basename>
# Pulls the css2 stylesheet, isolates the @font-face block commented /* latin */,
# and downloads the woff2 it points at.
fetch_latin() {
  local url="$1" out="$2" css subset_url

  css=$(curl -fsSL -H "User-Agent: $UA" "$url") || {
    echo "FAILED: could not fetch $url" >&2
    exit 1
  }

  # The stylesheet lists one @font-face per unicode-range, each preceded by a
  # /* subset */ comment. Take the first src: url after the /* latin */ marker.
  subset_url=$(printf '%s\n' "$css" \
    | awk '/\/\* latin \*\//{f=1} f && /src:/{print; exit}' \
    | grep -o 'https://[^)]*\.woff2')

  if [ -z "$subset_url" ]; then
    echo "FAILED: no latin woff2 found in $url" >&2
    exit 1
  fi

  curl -fsSL -o "$DEST/$out.woff2" "$subset_url" || {
    echo "FAILED: could not download $subset_url" >&2
    exit 1
  }
  printf '  %-28s %s\n' "$out.woff2" "$(du -h "$DEST/$out.woff2" | cut -f1 | tr -d ' ')"
}

echo "downloading fonts into $DEST"

# Variable axes are pinned to the ranges the design actually uses: weight only
# for the body face, weight + optical size for the display face.
fetch_latin \
  'https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,400..800&display=swap' \
  'bricolage-grotesque-var'

fetch_latin \
  'https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400..700&display=swap' \
  'plus-jakarta-sans-var'

cat >"$DEST/LICENSE.txt" <<'EOF'
Bricolage Grotesque — SIL Open Font License 1.1
  Copyright (c) 2023 The Bricolage Project Authors
  https://github.com/ateliertriay/bricolage

Plus Jakarta Sans — SIL Open Font License 1.1
  Copyright (c) 2020 Tokotype
  https://github.com/tokotype/PlusJakartaSans

Full licence text: https://openfontlicense.org
EOF

echo "done"
