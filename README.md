# ngp-sync

Self-hosted Chromium sync backend for the ungoogled-chromium fork, with
"Log in with X" identity. The browser never talks to X — it only ever sees
`sync.ngp.one` and a bearer token:

```
browser ──(bearer JWT)──► sync.ngp.one (go-sync) ──► dynamodb-local
   │                                              └─► redis
   └──(one-time code)──► id.ngp.one (identity) ──(OAuth2 PKCE)──► X
                           mints the JWTs
```

Sync payloads are end-to-end encrypted by the client (custom passphrase), so
the server only ever stores ciphertext.

## Components

| Service    | What                                                        | Port |
|------------|-------------------------------------------------------------|------|
| `gosync`   | [brave/go-sync](https://github.com/brave/go-sync), built from source at the ref pinned in `.env` | 8295 |
| `dynamo`   | dynamodb-local (JVM capped at 256 MB), data on a volume     | 8000 |
| `dynamo-init` | one-shot table creation from `gosync/table.json`         | —    |
| `redis`    | redis 6.2, 64 MB cap, persistence on                        | 6379 |
| `identity` | X OAuth → device-link codes → refresh tokens → sync JWTs (`identity/`, pure-stdlib Go) | 8300 |
| `caddy`    | TLS for `sync.ngp.one` + `id.ngp.one` (prod profile only)   | 80/443 |

## Local quickstart (Phase 1)

```sh
cp .env.example .env            # set at least SYNC_JWT_SECRET (openssl rand -hex 32)
docker compose up -d --build    # first build compiles go-sync + identity
curl http://localhost:8300/healthz
```

Validate the sync server with stock Brave (same protocol, no Chromium patches
needed): `./scripts/brave-test.sh a` and `./scripts/brave-test.sh b`, start a
sync chain in A, join from B, watch a bookmark propagate.
Logs: `docker compose logs -f gosync`.

## Identity API

| Endpoint            | What                                                              |
|---------------------|-------------------------------------------------------------------|
| `GET /login`        | redirects to X authorization                                      |
| `GET /callback`     | X redirect target; shows a one-time device-link code (10 min, single use) |
| `POST /device/link` | `{"code","device"}` → `{account_id, refresh_token, access_token}` |
| `POST /token`       | `{"refresh_token"}` → fresh 15-min access JWT                     |
| `GET /verify`       | `Authorization: Bearer <jwt>` → claims (same check go-sync will do) |

JWTs are HS256 with the shared `SYNC_JWT_SECRET`; claims:
`iss` = identity base URL, `sub` = `x:<x-user-id>`, `aud` = `ngp-sync`.

## Deploy to GCP (Always Free e2-micro)

1. `PROJECT=<your-project> ./provision/gcloud.sh` — creates static IP,
   firewall, VM; prints the DNS records to create for `ngp.one`.
2. X developer app (developer.x.com): OAuth 2.0, type **Web App**
   (confidential), callback `https://id.ngp.one/callback`. Put client
   id/secret in `.env` **on the VM**.
3. Copy this directory to the VM and run `sudo bash provision/vm-setup.sh`
   (adds 2 GB swap, installs Docker, `docker compose --profile prod up -d`).

Expected cost: $0 plus possibly the external IPv4 fee (≈$3.65/mo worst case).

## Roadmap

- [x] Phase 1 — this scaffold: stock go-sync + identity skeleton
- [ ] Phase 1b — validate deployed server with Brave (`SYNC_URL=https://sync.ngp.one/v2 ./scripts/brave-test.sh a`)
- [ ] Phase 2 — fork go-sync: replace the Brave seed-signing auth middleware
      with JWT verification (shared `SYNC_JWT_SECRET`, claims above), then
      point `GO_SYNC_REF` at the fork. (This breaks Brave-client compatibility
      by design — do it after Phase 1b passes.)
- [ ] Phase 3 — ungoogled-chromium patches: sync URL default,
      `SyncAuthManager` stub-account/token seam, token-paste setup UI
      (see `../ungoogled-chromium-macos`)
- [ ] Hardening: refresh-token rotation + revocation UI, rate limiting,
      RS256/JWKS instead of shared secret if go-sync and identity ever
      live on different hosts
