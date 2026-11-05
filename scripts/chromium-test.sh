#!/usr/bin/env bash
# Launch the patched ungoogled-chromium against the ngp sync server.
#
#   ./scripts/chromium-test.sh a      # first instance
#   ./scripts/chromium-test.sh b     # second instance (new terminal)
#
# Needs: the stack up (docker compose up -d), a token in .dev-sync-token
# (curl -s -X POST localhost:8300/dev/seed | extract .access_token), and a
# built browser with the ngp-sync patch. Both instances share the dev token,
# which is what puts them in one sync chain. Opens chrome://sync-internals —
# watch "Transport state: Active" and the type counters.
set -euo pipefail

PROFILE="${1:-a}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
APP="${2:-${CHROMIUM_APP:-$DIR/../ungoogled-chromium-macos/build/src/out/Default/Chromium.app}}"
BIN="$APP/Contents/MacOS/Chromium"
TOKEN_FILE="$DIR/.dev-sync-token"
SYNC_URL="${SYNC_URL:-http://localhost:8295/v2}"

if [ ! -x "$BIN" ]; then
  echo "No Chromium binary at: $BIN"
  echo "Build the browser first, or pass the .app path as the 2nd argument."
  exit 1
fi
if [ ! -s "$TOKEN_FILE" ]; then
  echo "No token at $TOKEN_FILE — mint one:"
  echo "  curl -s -X POST http://localhost:8300/dev/seed | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"access_token\"])' > $TOKEN_FILE"
  exit 1
fi

exec "$BIN" \
  --user-data-dir="/tmp/ngp-chromium-$PROFILE" \
  --no-first-run \
  --sync-url="$SYNC_URL" \
  --ngp-sync-token-file="$TOKEN_FILE" \
  "chrome://sync-internals"
