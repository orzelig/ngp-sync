#!/usr/bin/env bash
# Fetch the anonymous telemetry store off the prod VM and print it to stdout.
#
# The store is /data/telemetry.jsonl inside the identity container (the
# identity_data volume). This streams it out over `gcloud compute ssh`; if no
# reports have arrived yet the file is absent and this prints nothing (exit 0).
#
#   ./scripts/telemetry-fetch.sh > reports.jsonl          # save a copy
#   ./scripts/telemetry-fetch.sh | (cd identity && go run ./cmd/telemetry-decode)
#
# Override the target with NGP_PROJECT / NGP_ACCOUNT / NGP_ZONE / NGP_VM.
set -euo pipefail

PROJECT="${NGP_PROJECT:?set NGP_PROJECT to your GCP project}"
ACCOUNT="${NGP_ACCOUNT:?set NGP_ACCOUNT to your gcloud account}"
ZONE="${NGP_ZONE:-us-central1-a}"
VM="${NGP_VM:-ngp-sync}"

# Find the running identity container by name (compose names it
# <project>-identity-1) and cat the store; missing file => empty, not an error.
REMOTE='cid=$(sudo docker ps -q -f name=identity | head -n1); '\
'[ -n "$cid" ] || { echo "telemetry-fetch: identity container not running" >&2; exit 1; }; '\
'sudo docker exec "$cid" sh -c "cat /data/telemetry.jsonl 2>/dev/null || true"'

exec gcloud --project "$PROJECT" --account "$ACCOUNT" \
  compute ssh "$VM" --zone "$ZONE" --tunnel-through-iap --command "$REMOTE"
