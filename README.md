# ngp-sync

Server side for **NGP** (the private macOS ungoogled-chromium fork — browser
repo: [`orzelig/ngp-browser`](https://github.com/orzelig/ngp-browser)). This
repo holds everything that runs on `ngp.one`:

- a **self-hosted Chromium sync backend** (`sync.ngp.one`),
- an **identity service** with "Log in with X" (`id.ngp.one`), and
- the **`ngp.one` site** (home / terms / privacy + the signed update manifests
  the browser checks),

all wired together with Docker Compose and fronted by Caddy for TLS.

The browser never talks to X — it only ever sees `sync.ngp.one` and a bearer
token:

```
browser ──(bearer JWT)──► sync.ngp.one (go-sync) ──► dynamodb-local
   │                                              └─► redis
   └──(one-time code)──► id.ngp.one (identity) ──(OAuth2 PKCE)──► X
                           mints the JWTs
```

By default, sync uses Chromium's keystore encryption, whose key the server
derives itself (`KeystoreKeyForClient`) — so payloads are encrypted in transit
and at rest, but the server **can** read them (it holds the key). Setting a sync
passphrase in the browser makes sync end-to-end encrypted; only then does the
server store ciphertext it cannot read.

## Components

| Service    | What                                                        | Port |
|------------|-------------------------------------------------------------|------|
| `gosync`   | [brave/go-sync](https://github.com/brave/go-sync), built from source at the ref pinned in `.env`, with the NGP JWT-auth patch applied | 8295 |
| `dynamo`   | dynamodb-local (JVM capped at 256 MB), data on a volume     | 8000 |
| `dynamo-init` | one-shot table creation from `gosync/table.json`         | —    |
| `redis`    | redis 6.2, 64 MB cap, persistence on                        | 6379 |
| `identity` | X OAuth → device-link codes → refresh tokens → sync JWTs (`identity/`, pure-stdlib Go) | 8300 |
| `caddy`    | TLS + routing for `sync.ngp.one`, `id.ngp.one`, and the `ngp.one` static site (prod profile only) | 80/443 |

## Repo layout

| Path             | What                                                                 |
|------------------|----------------------------------------------------------------------|
| `gosync/`        | `Dockerfile` + `0001-ngp-jwt-auth.patch` (applied at image build) + `table.json` |
| `identity/`      | the "Log in with X" identity service (Go, stdlib only)               |
| `site/`          | the `ngp.one` static site — `index.html` (download + source + account links), `terms.html`, `privacy.html`, favicons, `robots.txt`, and `update/macos-arm64.json` (the signed update manifest) |
| `caddy/`         | `Caddyfile` — reverse-proxies the two services and serves `site/`; sends `noindex` headers + a disallow-all `robots.txt` on all three hosts |
| `provision/`     | `gcloud.sh` (create IP/firewall/VM), `vm-setup.sh` (swap + Docker + manifest puller on the VM) and `systemd/` (the puller's service and timer) |
| `scripts/`       | launch/test helpers (see below)                                      |
| `docker-compose.yml` | the stack; `--profile prod` adds Caddy                          |

## Local quickstart

```sh
cp .env.example .env            # set at least SYNC_JWT_SECRET (openssl rand -hex 32)
docker compose up -d --build    # first build compiles go-sync + identity
curl http://localhost:8300/healthz
```

Validate the sync server with stock Brave (same protocol, no Chromium patches
needed): `./scripts/brave-test.sh a` and `./scripts/brave-test.sh b` — start a
sync chain in A, join from B, watch a bookmark propagate. For the NGP browser
instead, use `./scripts/chromium-test.sh a` / `b`.
Logs: `docker compose logs -f gosync`.

## Identity service (`id.ngp.one`)

Pure-stdlib Go (`identity/`). Flow: user authorizes on X → `/callback` shows a
one-time device-link code → the browser (or a script) exchanges it for a
refresh token + a short-lived access JWT.

| Endpoint            | What                                                              |
|---------------------|-------------------------------------------------------------------|
| `GET /login`        | redirects to X authorization                                      |
| `GET /callback`     | X redirect target; shows a one-time device-link code (10 min, single use) |
| `POST /device/link` | `{"code","device","ttl_days"?}` → `{account_id, refresh_token, access_token}`; `ttl_days` is ignored for phone codes; 429 after 10 failed codes a minute per client IP |
| `POST /token`       | `{"refresh_token","ttl_days"?}` → fresh access JWT                |
| `GET /verify`       | `Authorization: Bearer <jwt>` → claims (same check go-sync does)  |
| `POST /account/phone` | account page (session + CSRF, X login within 15 min, else via `/login?reauth=1` back to `GET /account/phone`): mints the account's one live phone code, replacing the earlier one, and shows it as a QR of `<BASE_URL>/pair?code=<code>` and as text |
| `GET /pair`         | static page for camera apps: "open NGP on your phone and scan this code there"; never reads or spends a code |
| `GET /me`           | `Authorization: Bearer <jwt>` → `{handle, name, avatar}` (`name` is the handle for now) |

`ttl_days` (optional, clamped to 90) mints a long-lived access token — the
browser patch reads one static token from a file and has no refresh loop yet.
Without it, access tokens last 15 minutes. `scripts/get-token.sh` wraps both
endpoints: device code → token file, and `--renew` via the stored refresh token.
Identity responses also carry the X username + avatar (with a PNG avatar proxy).

JWTs are HS256 with the shared `SYNC_JWT_SECRET`; claims:
`iss` = identity base URL, `sub` = `x:<x-user-id>`, `aud` = `ngp-sync`.
go-sync verifies the same secret via `gosync/0001-ngp-jwt-auth.patch`
(dual-mode: NGP JWTs **and** Brave-style seed tokens; `ClientID =
sha256(subject)`; per-account derived keystore keys). With `DEV_MODE=1`,
`POST /dev/seed` mints tokens without X for local testing.

> **Rotating `SYNC_JWT_SECRET` wipes sync, by design:** go-sync's per-account
> keystore keys are HMAC-derived from the same secret, so after a rotation the
> stored chains can no longer be decrypted and clients land in exponential
> backoff against protocol errors (HTTP stays 200 — go-sync only logs 4xx/5xx,
> so the server looks quiet). After rotating: `docker compose down -v`,
> bring the stack back up, mint new tokens, and let clients re-upload.

## The `ngp.one` site

`site/` is served statically by Caddy at the apex. It contains the home page
(download link to the current `ngp-releases` DMG, a "Source on GitHub" link,
and a "Log in / My account" link to `id.ngp.one`), the Terms and Privacy pages,
and `update/macos-arm64.json` — the ed25519-signed update manifest the browser
polls (`version`, `url`, `sha256`, `sig`). All three hosts are marked
`noindex`/disallow-all so the site is not crawled.

In production `/update/` is not served from the checkout: Caddy mounts the
VM-only directory `/var/lib/ngp-update` over it, and `ngp-manifest-pull`
(`identity/cmd/manifest-pull`, on a 15-minute systemd timer) fills it from the
latest release of `orzelig/ngp-releases`, after verifying the signature against
a key pinned in its source. The tracked `site/update/macos-arm64.json` only
seeds an empty directory on a fresh VM, so a release no longer needs to commit
the manifest here, and a redeploy never puts an older one back. See
[`docs/runbooks/manifest-pull.md`](docs/runbooks/manifest-pull.md) and
[ADR 0001](docs/adr/0001-update-manifest-served-from-vm-only-directory.md).
The home page fills its download link and version from `/update/macos-arm64.json`
when it loads, so an installed release shows there with no edit; the values
hard-coded in `site/index.html` are only the fallback shown when the manifest
cannot be read ([ADR 0003](docs/adr/0003-download-page-reads-the-update-manifest.md)).

## Deploy to GCP (Always Free e2-micro)

**Deployment:** your own GCP project, VM `ngp-sync` in `us-central1-a`, a
reserved static IP, stack under the `prod` profile (`DEV_MODE=0`, the same
`SYNC_JWT_SECRET` as the laptop `.env`). Manage it with
`gcloud --project <your-gcp-project> --account <your-gcloud-account> compute ssh ngp-sync --zone us-central1-a`.
Note: an inherited org policy denies VM external IPs; the project carries a
non-inheriting `compute.vmExternalIpAccess` policy allowing exactly this
instance.

For a fresh deployment:

1. `PROJECT=<your-project> ./provision/gcloud.sh` — creates static IP,
   firewall, VM; prints the DNS records to create for `ngp.one`.
2. X developer app (developer.x.com): OAuth 2.0, type **Web App**
   (confidential), callback `https://id.ngp.one/callback`. Put client
   id/secret in `.env` **on the VM**.
3. Copy this directory to the VM and run `sudo bash provision/vm-setup.sh`
   (adds 2 GB swap, installs Docker, sets up `/var/lib/ngp-update` and the
   `ngp-manifest-pull` timer and runs it once, `docker compose --profile prod up -d`).
   Redeploys are the same copy and the same script; neither touches the served
   update manifest.

Expected cost: $0 plus possibly the external IPv4 fee (≈$3.65/mo worst case).

## Everyday use

The NGP browser needs no flags: it defaults to `https://sync.ngp.one/v2` with
sync **off**. To enable sync, open <https://id.ngp.one/login>, sign in with X,
and click **"Enable sync in the NGP browser"** — the browser exchanges the
one-time code itself (`ngp://enable-sync?code=…`), stores a 30-day token in its
profile storage (`<User Data>/ngp-sync-token`), and restarts with sync on.
Enrollment only accepts user clicks originating on `id.ngp.one` pages.

```sh
./scripts/ngp.sh                            # launch (profile ~/.ngp-chromium/main)
./scripts/ngp.sh --local                    # against the laptop stack instead
```

Script-based tokens still work and take precedence when present:
`./scripts/get-token.sh <device-code>` (or `--renew`) writes
`.ngp-sync-token`, which the launcher passes through.

## Status

Live in production since 2026-11-05: DNS A records resolve, Let's Encrypt certs
are issued, real "Log in with X" works end-to-end (X consent → device code →
30-day token), and two browser instances sync bookmarks both ways through
`https://sync.ngp.one/v2`.

Still open (hardening):

- refresh-token rotation + revocation UI, rate limiting
- RS256/JWKS instead of a shared secret if go-sync and identity ever live on
  different hosts
- browser-side token refresh (the patch currently reads one long-lived token)
- separate `KEYSTORE_SECRET` from `SYNC_JWT_SECRET` so rotating tokens stops
  wiping sync data
