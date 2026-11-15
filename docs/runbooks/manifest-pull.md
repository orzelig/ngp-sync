# Update manifest puller (`ngp-manifest-pull`)

ngp.one/update/macos-arm64.json is what every NGP browser polls to decide
whether to update. On the VM it is kept current by `ngp-manifest-pull`, a
systemd timer that runs a one-shot container every 15 minutes.

## Where the manifest comes from

- **The source is `orzelig/ngp-releases`.** Each cut publishes the DMG and a
  signed `macos-arm64.json` as assets of a non-prerelease release marked
  latest. The puller takes the asset from `GET /releases/latest`, verifies its
  ed25519 signature against the key pinned in
  `identity/cmd/manifest-pull/main.go`, and installs it only if its version is
  strictly newer than the one served. It writes only the four signed fields
  (`version`, `url`, `sha256`, `sig`).
- **What ngp.one serves is `/var/lib/ngp-update/macos-arm64.json` on the VM.**
  `docker-compose.yml` mounts that directory read-only into Caddy over
  `/srv/ngp/update`. No deploy command touches it.
- **The tracked `site/update/macos-arm64.json` is only the seed** for an empty
  `/var/lib/ngp-update` on a fresh VM. A release no longer needs to commit the
  manifest to ngp-sync `main`. The file stays in the tree: it is the seed, and
  its directory is the mountpoint Caddy's mount lands on.
- `site/index.html`'s download link and version are separate and are still a
  manual edit.

## How it is installed

`sudo bash provision/vm-setup.sh` (run on every deploy) does all of it, and
each step is safe to repeat:

1. creates `/var/lib/ngp-update` (owned by uid 65532, the container's user)
   and seeds it from the tracked file only when it is empty;
2. builds the `ngp-manifest-pull:local` image from
   `identity/manifest-pull.Dockerfile`;
3. installs `provision/systemd/ngp-manifest-pull.{service,timer}` into
   `/etc/systemd/system/` and enables the timer;
4. runs the puller once. If that fails, setup prints
   `first manifest pull failed; ...` and carries on.

The timer is `OnCalendar=*:0/15` with `RandomizedDelaySec=2min`: a cut reaches
ngp.one within about 17 minutes.

Check it is scheduled:

```bash
systemctl list-timers ngp-manifest-pull.timer
```

## Did it run

```bash
journalctl -u ngp-manifest-pull -n 20
```

Each run logs one line:

| Line | Meaning | Unit result |
|---|---|---|
| `manifest-pull: installed <version> (was <old or none>) from release <tag>` | A newer manifest is now served | success |
| `manifest-pull: current: serving <version>, release <tag> has the same` | Nothing to do | success |
| `manifest-pull: kept <served>: release <tag> has older <version>` | The latest release is older than what is served; nothing changed | success |
| `manifest-pull: failed: <reason>` | Nothing changed; the served manifest is untouched | failed |

Run it now: `sudo systemctl start ngp-manifest-pull.service`.

Compare what is served with what is on disk:

```bash
curl -s https://ngp.one/update/macos-arm64.json
cat /var/lib/ngp-update/macos-arm64.json
```

## Expected failures

- **`release <tag> has no macos-arm64.json asset`** on every run until the
  first OpenADLC cut (`ngp.30`) exists, and as the warning from
  `vm-setup.sh`'s first pull. The seeded `ngp.29` stays served and setup
  finishes.
- The same failure for a moment during a cut, between the release being
  created and its asset being uploaded. The next run picks it up.
- **`HTTP 403 (GitHub rate limit ...)`** or `HTTP 429`: each run makes two
  unauthenticated requests (8 an hour, against GitHub's 60 per IP). If it
  persists, something else on the VM is using the API.

Any other `failed:` line — `bad signature`, `signature is N bytes`,
`manifest has no "..."`, `version ... is not ...`, `is a draft or prerelease` —
means the release's manifest is wrong, not the VM. It stays unserved; fix it on
the release side.

## Redeploy

Option (a) of ngp-sync#4 was chosen
([ADR 0001](../adr/0001-update-manifest-served-from-vm-only-directory.md)):
the served manifest lives outside the copied checkout, so the usual deploy
(`gcloud compute scp --recurse . $NAME:~/ngp-sync`, then
`sudo bash provision/vm-setup.sh`) cannot put an older one back.

Check it after a redeploy:

1. Note the version: `curl -s https://ngp.one/update/macos-arm64.json`.
2. Redeploy with `scp --recurse` and `vm-setup.sh`.
3. Run the `curl` again. The version is the same, and matches
   `cat /var/lib/ngp-update/macos-arm64.json` on the VM, not the checkout's
   `site/update/macos-arm64.json`.

## Manual fallback

Kept until the puller is proven. It takes the manifest from the release, copies
it into the VM-only directory (not the checkout), and renames it into place so
the swap stays atomic:

```bash
gh release download <tag> --repo orzelig/ngp-releases --pattern macos-arm64.json --dir /tmp/ngp-manifest
gcloud compute scp /tmp/ngp-manifest/macos-arm64.json $NAME:/tmp/macos-arm64.json
gcloud compute ssh $NAME -- 'sudo cp /tmp/macos-arm64.json /var/lib/ngp-update/.manual && sudo chmod 0644 /var/lib/ngp-update/.manual && sudo mv /var/lib/ngp-update/.manual /var/lib/ngp-update/macos-arm64.json'
```

`$NAME` is the VM's name, as in `provision/gcloud.sh`. The live one is
`ngp-sync`, in project `ngp-sync-one` (zone `us-central1-a`).

## Taking a release back

The puller never downgrades.

1. On GitHub, mark the bad release as a prerelease, or delete it, so that
   `/releases/latest` is the good one again. Otherwise the next run installs
   the bad one again.
2. Install the older manifest with the manual fallback above.

## A re-cut under the same version is not picked up

An equal version is a no-op. Cut a new `ngp.N` instead.

## Turning it off

```bash
sudo systemctl disable --now ngp-manifest-pull.timer
```

The served manifest stays as it is. To go back to serving the checkout's file
entirely, also remove the `/var/lib/ngp-update:/srv/ngp/update:ro` line from
`docker-compose.yml` and run `docker compose --profile prod up -d`.
