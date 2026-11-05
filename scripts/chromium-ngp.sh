#!/usr/bin/env bash
# Launch the locally built ungoogled-chromium against the ngp-sync server
# for everyday use (persistent profile, no debug ports).
#
#   ./scripts/chromium-ngp.sh                 # prod: https://sync.ngp.one/v2, token .ngp-sync-token
#   ./scripts/chromium-ngp.sh --local         # local stack: http://localhost:8295/v2, .dev-sync-token
#   ./scripts/chromium-ngp.sh --profile work  # separate profile dir
#
# Profiles live under ~/.ngp-chromium/<name>. Extra args pass through.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/.." && pwd)"
APP="${CHROMIUM_APP:-$DIR/../ungoogled-chromium-macos/build/src/out/Default/Chromium.app}"
BIN="$APP/Contents/MacOS/Chromium"
PROFILE="main"
SYNC_URL="https://sync.ngp.one/v2"
TOKEN_FILE="$DIR/.ngp-sync-token"

while [ $# -gt 0 ]; do
  case "$1" in
    --local) SYNC_URL="http://localhost:8295/v2"; TOKEN_FILE="$DIR/.dev-sync-token"; shift ;;
    --profile) PROFILE="$2"; shift 2 ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) break ;;
  esac
done

[ -x "$BIN" ] || { echo "No Chromium binary at: $BIN (set CHROMIUM_APP)"; exit 1; }
[ -s "$TOKEN_FILE" ] || { echo "No token at $TOKEN_FILE — run scripts/get-token.sh first."; exit 1; }

exec "$BIN" \
  --user-data-dir="$HOME/.ngp-chromium/$PROFILE" \
  --sync-url="$SYNC_URL" \
  --ngp-sync-token-file="$TOKEN_FILE" \
  "$@"
