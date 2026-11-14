#!/usr/bin/env bash
# Open a Google IAP tunnel to the operator-only telemetry read endpoint on the
# VM and forward it to a local port. Leave this running; Ctrl-C closes it.
#
#   ./scripts/telemetry-tunnel.sh         # forward localhost:8301 -> VM:8301
#   # then open http://localhost:8301/ in a browser
#
# Access is gated entirely by IAP: the endpoint binds to the VM's loopback (not
# exposed publicly), and only an IAP-authenticated SSH session can reach it.
# Override the target with NGP_PROJECT / NGP_ACCOUNT / NGP_ZONE / NGP_VM, or the
# local port with NGP_TELEMETRY_PORT.
set -euo pipefail

PROJECT="${NGP_PROJECT:-ngp-sync-one}"
ACCOUNT="${NGP_ACCOUNT:-or@zelig.me}"
ZONE="${NGP_ZONE:-us-central1-a}"
VM="${NGP_VM:-ngp-sync}"
LOCAL_PORT="${NGP_TELEMETRY_PORT:-8301}"

echo "IAP tunnel open → http://localhost:${LOCAL_PORT}/   (Ctrl-C to close)" >&2

# Reconnect loop: a single gcloud/ssh forward can drop — an idle IAP timeout, a
# network blip, or (when run detached) stdin reaching EOF and taking ssh down
# with a broken pipe. We respawn it so the forward stays up for a whole testing
# session. Stop it with Ctrl-C, or by killing the job.
#   -n                       ssh takes stdin from /dev/null (no broken-pipe exit)
#   < /dev/null              clean EOF for gcloud's stdin forwarder, not a pipe
#   ServerAlive*             keepalive so an idle tunnel is not torn down
#   ExitOnForwardFailure     fail fast if the local port cannot bind
while true; do
  gcloud --project "$PROJECT" --account "$ACCOUNT" \
    compute ssh "$VM" --zone "$ZONE" --tunnel-through-iap \
    -- -N -n \
    -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
    -o ExitOnForwardFailure=yes \
    -L "${LOCAL_PORT}:localhost:8301" < /dev/null || true
  echo "telemetry-tunnel: forward dropped; reconnecting in 3s (Ctrl-C to stop)…" >&2
  sleep 3
done
