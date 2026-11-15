#!/usr/bin/env bash
# VM setup: swap + Docker + the update-manifest puller + bring the stack up.
# Run as root on the VM, from the ngp-sync directory: sudo bash provision/vm-setup.sh
# It is re-run on every redeploy, so each step is safe to repeat.
set -euo pipefail

# 2 GB swap — required headroom on the 1 GB e2-micro.
if ! swapon --show | grep -q /swapfile; then
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile
  swapon /swapfile
  echo '/swapfile none swap sw 0 0' >>/etc/fstab
  echo "swap enabled"
fi

# Docker from the official repo (Debian 12).
if ! command -v docker >/dev/null 2>&1; then
  apt-get update
  apt-get install -y ca-certificates curl gnupg
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    >/etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
fi

cd "$(dirname "$0")/.."
if [ ! -f .env ]; then
  cp .env.example .env
  echo "Created .env from .env.example — EDIT IT (secrets, domains), then re-run this script."
  exit 1
fi

# The update manifest ngp.one serves lives in /var/lib/ngp-update, outside this
# checkout, so a redeploy never puts an older one back. The tracked
# site/update/macos-arm64.json only seeds an empty directory; the puller keeps
# it current from orzelig/ngp-releases (docs/runbooks/manifest-pull.md).
install -d -o 65532 -g 65532 -m 0755 /var/lib/ngp-update
[ -e /var/lib/ngp-update/macos-arm64.json ] ||
  install -m 0644 site/update/macos-arm64.json /var/lib/ngp-update/macos-arm64.json
docker build -t ngp-manifest-pull:local -f identity/manifest-pull.Dockerfile identity
install -m 0644 provision/systemd/ngp-manifest-pull.service provision/systemd/ngp-manifest-pull.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now ngp-manifest-pull.timer
# Pull once now so the VM is current at once. A failed pull (no release with
# the asset yet, GitHub down) must not fail the setup: the timer retries.
systemctl start ngp-manifest-pull.service ||
  echo "first manifest pull failed; the timer retries every 15 minutes (journalctl -u ngp-manifest-pull)"

docker compose --profile prod up -d --build
docker compose ps
