#!/usr/bin/env bash
# Launch the locally built NGP browser (ungoogled-chromium fork).
#
# Sync needs no flags anymore: the browser defaults to
# https://sync.ngp.one/v2 and stays off until enabled. To enable it, open
# https://id.ngp.one/login, sign in with X, and click "Enable sync in the
# NGP browser" — the browser stores a token in its profile storage and
# restarts with sync on.
#
#   ./scripts/ngp.sh                 # production
#   ./scripts/ngp.sh --local         # laptop stack (dev identity + sync)
#   ./scripts/ngp.sh --profile work  # separate profile dir
#
# A token file (.ngp-sync-token, from scripts/get-token.sh) is optional; if
# present it is passed through and overrides in-browser enrollment.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/.." && pwd)"
APP="${NGP_APP:-${CHROMIUM_APP:-$DIR/../ungoogled-chromium-macos/build/src/out/Default/NGP.app}}"
BIN="$APP/Contents/MacOS/NGP"
PROFILE="main"
EXTRA=()

while [ $# -gt 0 ]; do
  case "$1" in
    --local)
      EXTRA+=(--sync-url=http://localhost:8295/v2 --ngp-id-url=http://localhost:8300)
      [ -s "$DIR/.dev-sync-token" ] && EXTRA+=(--ngp-sync-token-file="$DIR/.dev-sync-token")
      shift ;;
    --profile) PROFILE="$2"; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) break ;;
  esac
done

[ -x "$BIN" ] || { echo "No NGP binary at: $BIN (set NGP_APP)"; exit 1; }

# Legacy/scripted token takes precedence when present.
if [ ${#EXTRA[@]} -eq 0 ] && [ -s "$DIR/.ngp-sync-token" ]; then
  EXTRA+=(--ngp-sync-token-file="$DIR/.ngp-sync-token")
fi

exec "$BIN" \
  --user-data-dir="$HOME/.ngp-chromium/$PROFILE" \
  "${EXTRA[@]+"${EXTRA[@]}"}" \
  "$@"
