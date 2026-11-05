#!/usr/bin/env bash
# Phase-1 validation: run isolated Brave instances against YOUR go-sync server.
# Brave speaks the same sync protocol and auth that stock go-sync expects,
# so this proves the server works before any Chromium patches exist.
#
#   ./scripts/brave-test.sh a     # first instance
#   ./scripts/brave-test.sh b     # second instance (new terminal)
#
# Then in instance A: brave://settings → Sync → "Start a new Sync Chain",
# in instance B: join it with the code words. Add a bookmark in A, watch it
# appear in B. Server logs: docker compose logs -f gosync
set -euo pipefail

PROFILE="${1:-a}"
SYNC_URL="${SYNC_URL:-http://localhost:8295/v2}"

exec open -na "Brave Browser" --args \
  --user-data-dir="/tmp/ngp-sync-brave-$PROFILE" \
  --sync-url="$SYNC_URL"
