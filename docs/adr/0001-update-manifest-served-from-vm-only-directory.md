# 0001. The update manifest is served from a VM-only directory, pulled and verified by the VM

- **Status:** accepted
- **Date:** 2026-11-15

## Context

OpenADLC's cut publishes the DMG and a signed `macos-arm64.json` as assets of a
release in `orzelig/ngp-releases`, and does not push to the VM. Until now
ngp.one served the tracked `site/update/macos-arm64.json` from the copied
checkout, put there by a hand-run `gcloud compute scp`. The deploy
(`gcloud compute scp --recurse . $NAME:~/ngp-sync`, then
`sudo bash provision/vm-setup.sh`) overwrites the whole checkout every time, so
anything the VM fetched into it would be put back to the tracked, older file on
the next redeploy. Or chose where the served manifest lives (ngp-sync#4).

## Decision

- ngp.one's update manifest is served from `/var/lib/ngp-update` on the VM,
  which no deploy command touches. `docker-compose.yml` mounts it read-only into
  Caddy over `/srv/ngp/update`; `caddy/Caddyfile` is unchanged.
- The tracked `site/update/macos-arm64.json` is only the seed for an empty
  `/var/lib/ngp-update`, and its directory is the mountpoint. A release no
  longer commits the manifest to ngp-sync `main`.
- The VM pulls: `identity/cmd/manifest-pull`, on a 15-minute systemd timer in a
  one-shot container, takes `macos-arm64.json` from GitHub's
  `/releases/latest` of `orzelig/ngp-releases`, verifies its ed25519 signature
  against a key pinned in source, serves only the four verified fields, and
  never downgrades. GitHub is trusted for availability, never for content; no
  token, flag or environment variable can change the key or the source.

## What this rules out

- **Option (b), the deploy skipping the file.** `gcloud compute scp` has no
  exclude, so the deploy would become a tar-over-ssh copy, and the old
  `scp --recurse` would still put the tracked manifest back until the next
  timer run. The puller would also have to write inside the checkout as its
  owner. Only (a) holds whatever deploy command is typed.
- **Listing releases and picking the highest version.** Every release in
  `ngp-releases` tags the same commit, so the list is not in version order and
  every page would have to be read; with about three releases a day that soon
  exceeds the unauthenticated rate limit.
- **`releases/latest/download/macos-arm64.json` without the API.** A 404
  cannot tell "no release" from "no asset", and the tool could not check
  `prerelease` itself.
- **Serving the downloaded bytes unchanged.** It would serve bytes the
  signature does not cover: extra fields, or duplicate keys the browser's
  parser might read differently from Go's.
- **A compose service for the puller.** More change to `docker-compose.yml`
  than the one mount line; the unit runs `docker run` directly.

## What would make this worth revisiting

- A second platform's manifest (another file under `/update/`).
- A version 2 of the signature format.
- A publisher that stops marking each release as latest, which would make
  `/releases/latest` wrong.
