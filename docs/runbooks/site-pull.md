# Site puller (`ngp-site-pull`)

ngp.one serves the VM checkout's `site/`, which Caddy bind-mounts read-only as
`./site:/srv/ngp`. On the VM, `ngp-site-pull` keeps that `site/` current with
`orzelig/ngp-sync` `main`. It is a systemd timer that runs a one-shot container
every 15 minutes. **A merge to `main` that changes `site/` is live within about
17 minutes**, with no deploy step.

## What it does

Each run:

1. **Fetches.** It fetches `main` from `https://github.com/orzelig/ngp-sync.git`
   into the bare, shallow clone `/var/lib/ngp-site-pull/repo.git`, and creates
   the clone if there is none. The URL and branch are constants in
   `identity/cmd/site-pull/main.go`. The fetch is anonymous: git runs with no
   credential helper, no global or system config, and no prompt. The VM holds
   no token. If the fetch fails, the run stops before it touches anything.
2. **Checks the tree.** It lists `site/` on `main`, leaving out `site/update/`.
   It refuses the whole tree, writing nothing, if it has no `site/index.html` or
   holds anything but regular files under `site/` (a symlink or a submodule).
3. **Writes what differs.** It compares each file's git blob id with the bytes
   on disk. A file that differs is written to `.ngp-site-pull.<random>` in its
   own directory and renamed over the old one, so each file is whole at every
   moment. A file with only the wrong mode gets a chmod.
4. **Removes what `main` no longer has.** This covers files and then any
   directories left empty. Temp files left by a killed run go the same way.

It never writes, removes or renames `site/` itself, anything under
`site/update/` (that is `ngp-manifest-pull`'s, see `manifest-pull.md`), or
anything outside `site/`. A merged change to `caddy/`, `provision/` or
`docker-compose.yml` still takes effect only on a deliberate redeploy. The
puller never runs `docker compose` and never rebuilds a container.

It keeps no record of what it applied, because it compares content. A run at
`main` writes nothing, and a run after a redeploy put back an older `site/`
repairs it.

The container runs as the owner of `site/`, so a later `scp` redeploy can still
overwrite the files it wrote. It runs with `--read-only`, `--cap-drop ALL` and
`no-new-privileges`, and mounts only `/var/lib/ngp-site-pull` and `site/`.

## How it is installed

`sudo bash provision/vm-setup.sh`, run on every deploy, does all of it, and
each step is safe to repeat:

1. builds the `ngp-site-pull:local` image from `identity/site-pull.Dockerfile`;
2. writes `/etc/ngp-site-pull.env` with `NGP_SITE_DIR` (this checkout's `site`)
   and `NGP_SITE_UID` and `NGP_SITE_GID` (the owner of `site/`);
3. creates `/var/lib/ngp-site-pull`, owned by that same user;
4. installs `provision/systemd/ngp-site-pull.{service,timer}` into
   `/etc/systemd/system/` and enables the timer;
5. runs the puller once. If that fails, setup prints
   `first site pull failed; ...` and carries on.

The timer is `OnCalendar=*:5/15` with `RandomizedDelaySec=2min`. That is five
minutes after the manifest puller, so the two do not run together on the
1 GB VM.

Check it is scheduled:

```bash
systemctl list-timers ngp-site-pull.timer
```

## Did it run

```bash
journalctl -u ngp-site-pull -n 20
```

Each run logs one line:

| Line | Meaning | Unit result |
|---|---|---|
| `site-pull: updated to <sha>: <n> written, <m> removed` | `site/` now matches `main` at `<sha>` | success |
| `site-pull: current at <sha>` | Nothing to do; no file was touched | success |
| `site-pull: failed: <reason>` | See below. After a fetch or tree failure nothing changed | failed |

Run it now:

```bash
sudo systemctl start ngp-site-pull.service
journalctl -u ngp-site-pull -n 1
```

Compare what is served with `main`:

```bash
curl -s https://ngp.one/ | head
git -C /var/lib/ngp-site-pull/repo.git log -1 --oneline refs/remotes/origin/main   # run as the owner of site/
```

## Failure modes

- **`failed: fetch: git fetch: ...: could not read Username ...` or
  `... Authentication failed`**: the repository is not public. The VM
  deliberately has no credential, and it fails safely, serving what is there
  until `orzelig/ngp-sync` is public. Do not put a token on the VM: the
  decision is recorded in ADR 0002.
- **`failed: fetch: ... Could not resolve host` / `Failed to connect` /
  `context deadline exceeded`**: GitHub or the network is down. Nothing
  changed, and the next run retries.
- **`failed: main <sha>: tree has no site/index.html; refusing to apply it`**:
  `main` lost `site/index.html`. Restore it on `main`. The VM keeps serving
  the last good `site/`.
- **`failed: main <sha>: site/<path> is not a regular file`**: `main` has a
  symlink or submodule under `site/`. Replace it with a regular file on `main`.
- **`failed: main <sha>: write <path>: ...` or `remove <path>: ...`
  (`permission denied`)**: something under `site/` is not owned by the user in
  `/etc/ngp-site-pull.env`, for example after a root-owned copy. Fix it with
  `sudo chown -R <uid>:<gid> site`, or re-run `vm-setup.sh` if the owner of
  `site/` itself changed. Files written before the error stay written. The
  next run finishes the rest.
- **`failed: ... dubious ownership`, `bad object`, `not a git repository`, or
  any other git error about the clone**: the clone is corrupt or owned by the
  wrong user. It is disposable:
  `sudo rm -rf /var/lib/ngp-site-pull/repo.git`. The next run rebuilds it, and
  `vm-setup.sh` restores the directory's owner.
- **`failed: site directory: ...`**: `/etc/ngp-site-pull.env` points at a
  checkout that is not there. Re-run `sudo bash provision/vm-setup.sh` from
  the checkout.
- **The unit fails with `Unable to find image 'ngp-site-pull:local'`**: the
  image was pruned. Re-run `vm-setup.sh`, or build it with the `docker build`
  line from that script.

## Redeploy

The usual deploy is `gcloud compute scp --recurse . $NAME:~/ngp-sync`, then
`sudo bash provision/vm-setup.sh`. The `scp` copies your machine's `site/` over
the VM's, which may be older than `main`. It is served until the
`systemctl start ngp-site-pull.service` near the end of `vm-setup.sh` puts
`main` back. That start is in the same run.

**A file placed in `site/` on the VM by hand, or an unmerged `site/` change
deployed by `scp`, is reverted to `main` within one interval.** To serve
something else for a while, turn the puller off first (below).

## Turning it off

```bash
sudo systemctl disable --now ngp-site-pull.timer
```

`site/` stays as it is, and the next redeploy's `scp` is what is served. Note
that `vm-setup.sh` enables the timer again. To leave it off across redeploys,
also `sudo systemctl mask ngp-site-pull.timer`. Undo that with
`sudo systemctl unmask ngp-site-pull.timer` and re-run `vm-setup.sh`.
