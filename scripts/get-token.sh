#!/usr/bin/env bash
# Turn a "Log in with X" device code into a saved sync token.
#
#   1. Open https://id.ngp.one/login and sign in with X.
#   2. The page shows a one-time device code.
#   3. ./scripts/get-token.sh <code>
#
# Writes a long-lived access token to .ngp-sync-token (chmod 600) and the
# refresh token to .ngp-sync-token.refresh. Renew later without X:
#
#   ./scripts/get-token.sh --renew
#
#   -u URL    identity service base (default https://id.ngp.one;
#             use http://localhost:8300 against the local stack)
#   -o FILE   token file (default .ngp-sync-token in the repo root)
#   -d NAME   device name recorded with the link (default: this hostname)
#   -t DAYS   token lifetime in days, max 90 (default 30)
set -euo pipefail

DIR="$(cd "$(dirname "$0")/.." && pwd)"
BASE_URL="https://id.ngp.one"
OUT="$DIR/.ngp-sync-token"
DEVICE="$(hostname -s 2>/dev/null || echo laptop)"
TTL_DAYS=30
RENEW=0
CODE=""

while [ $# -gt 0 ]; do
  case "$1" in
    -u) BASE_URL="$2"; shift 2 ;;
    -o) OUT="$2"; shift 2 ;;
    -d) DEVICE="$2"; shift 2 ;;
    -t) TTL_DAYS="$2"; shift 2 ;;
    --renew) RENEW=1; shift ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
    *) CODE="$1"; shift ;;
  esac
done

if [ "$RENEW" = 1 ]; then
  [ -s "$OUT.refresh" ] || { echo "No refresh token at $OUT.refresh — log in once first."; exit 1; }
  BODY=$(printf '{"refresh_token":"%s","ttl_days":%d}' "$(cat "$OUT.refresh")" "$TTL_DAYS")
  RESP=$(curl -fsS -X POST "$BASE_URL/token" -d "$BODY")
else
  [ -n "$CODE" ] || { echo "Usage: $0 [-u url] [-t days] <device-code>   (or --renew)"; exit 1; }
  BODY=$(printf '{"code":"%s","device":"%s","ttl_days":%d}' "$CODE" "$DEVICE" "$TTL_DAYS")
  RESP=$(curl -fsS -X POST "$BASE_URL/device/link" -d "$BODY")
fi

python3 - "$OUT" "$RESP" <<'EOF'
import json, os, sys
out = sys.argv[1]
r = json.loads(sys.argv[2])
def write(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(value)
write(out, r["access_token"])
if r.get("refresh_token"):
    write(out + ".refresh", r["refresh_token"])
days = r.get("expires_in", 0) / 86400
print(f'Linked {r.get("account_id", "?")} — token valid {days:.0f} days')
print(f"Saved to {out}")
EOF
