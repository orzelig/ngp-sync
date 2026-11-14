#!/usr/bin/env bash
# Fetch the prod telemetry store and decode it into readable reports in one go.
#
#   ./scripts/telemetry-read.sh            # human-readable
#   ./scripts/telemetry-read.sh -json | jq # structured, for aggregation
#   ./scripts/telemetry-read.sh -raw       # also show any unknown proto fields
#
# Flags are passed straight through to cmd/telemetry-decode. Requires the Go
# toolchain locally (the decoder has no other dependencies).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"

"$here/telemetry-fetch.sh" | ( cd "$root/identity" && go run ./cmd/telemetry-decode "$@" )
