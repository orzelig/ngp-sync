#!/usr/bin/env bash
# Provision the Always Free VM for ngp-sync.
# RUN THIS YOURSELF — it needs your gcloud auth and billing project.
# Each step is safe to re-run (existing resources are left alone).
#
#   PROJECT=my-gcp-project ./provision/gcloud.sh
set -euo pipefail

PROJECT="${PROJECT:?set PROJECT=your-gcp-project-id}"
REGION="${REGION:-us-central1}" # Always Free: us-central1 / us-west1 / us-east1
ZONE="${ZONE:-us-central1-a}"
NAME="${NAME:-ngp-sync}"

gcloud config set project "$PROJECT"

# Static external IPv4 (same in-use price as ephemeral; survives VM recreation).
gcloud compute addresses create "$NAME-ip" --region "$REGION" 2>/dev/null || true
IP=$(gcloud compute addresses describe "$NAME-ip" --region "$REGION" --format='value(address)')

# Firewall for Caddy.
gcloud compute firewall-rules create "$NAME-web" \
  --allow tcp:80,tcp:443 --target-tags "$NAME" 2>/dev/null || true

# The Always Free e2-micro: non-preemptible, 30 GB standard disk.
gcloud compute instances create "$NAME" \
  --zone "$ZONE" \
  --machine-type e2-micro \
  --image-family debian-12 --image-project debian-cloud \
  --boot-disk-size 30GB --boot-disk-type pd-standard \
  --address "$IP" \
  --tags "$NAME"

cat <<EOF

VM created. Static IP: $IP

1. Create DNS A records at your ngp.one registrar:
     sync.ngp.one  A  $IP
     id.ngp.one    A  $IP

2. Copy this directory to the VM and set it up:
     gcloud compute scp --recurse --zone $ZONE . $NAME:~/ngp-sync
     gcloud compute ssh --zone $ZONE $NAME
     cd ngp-sync && cp .env.example .env && nano .env   # fill in secrets
     sudo bash provision/vm-setup.sh

3. (Optional, pennies/month) nightly disk snapshots:
     gcloud compute resource-policies create snapshot-schedule $NAME-backup \\
       --region $REGION --max-retention-days 14 --daily-schedule --start-time 08:00
     gcloud compute disks add-resource-policies $NAME --zone $ZONE \\
       --resource-policies $NAME-backup
EOF
