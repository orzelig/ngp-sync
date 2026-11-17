#!/usr/bin/env bash
# VM setup: swap + Docker + the update-manifest and site pullers + bring the stack up.
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

# The served site/ follows orzelig/ngp-sync main: the site puller fetches main
# into a clone in /var/lib/ngp-site-pull, outside this checkout, and writes
# only this checkout's site/ (never site/update/). It runs as the owner of
# site/, so a later scp redeploy can still overwrite what it wrote
# (docs/runbooks/site-pull.md, ADR 0002).
docker build -t ngp-site-pull:local -f identity/site-pull.Dockerfile identity
site_uid="$(stat -c %u site)"
site_gid="$(stat -c %g site)"
printf 'NGP_SITE_DIR=%s\nNGP_SITE_UID=%s\nNGP_SITE_GID=%s\n' "$(pwd)/site" "$site_uid" "$site_gid" >/etc/ngp-site-pull.env
chmod 0644 /etc/ngp-site-pull.env
install -d -o "$site_uid" -g "$site_gid" -m 0755 /var/lib/ngp-site-pull
chown -R "$site_uid:$site_gid" /var/lib/ngp-site-pull
# A masked timer is the operator's way to keep the puller off across redeploys
# (docs/runbooks/site-pull.md): leave the units alone and do not pull. The
# `|| true` is because is-enabled exits non-zero for masked, and for not-found
# on the first run, which set -e would otherwise turn into a failed setup.
site_pull_timer_state="$(systemctl is-enabled ngp-site-pull.timer 2>/dev/null || true)"
if [ "$site_pull_timer_state" = masked ]; then
  echo "ngp-site-pull.timer is masked; leaving the site puller off and not pulling (systemctl unmask ngp-site-pull.timer, then re-run this script, turns it back on)"
else
  install -m 0644 provision/systemd/ngp-site-pull.service provision/systemd/ngp-site-pull.timer /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable --now ngp-site-pull.timer
  # Pull once now: this also corrects a site/ the redeploy just put back to an
  # older state. A failed pull (repository not public yet, GitHub down) must
  # not fail the setup: the timer retries.
  systemctl start ngp-site-pull.service ||
    echo "first site pull failed; the timer retries every 15 minutes (journalctl -u ngp-site-pull)"
fi

docker compose --profile prod up -d --build
docker compose ps
