# 0002. ngp.one's site/ is pulled from main into the checkout's site/ by the VM

- **Status:** accepted
- **Date:** 2026-11-17

## Context

ngp.one serves the VM checkout's `site/`, which Caddy bind-mounts as
`./site:/srv/ngp:ro`. The checkout was put there by a hand-run
`gcloud compute scp --recurse . $NAME:~/ngp-sync` and `vm-setup.sh`, so a
merged `site/` change was not live until Or redeployed. The issue (ngp-sync#7)
asked the VM to keep `site/` current from `main` by itself, with no GitHub
Actions, no credential on the VM, per-file atomic writes, and no change to
`caddy/`, `provision/` or `docker-compose.yml` except by a deliberate redeploy.

`orzelig/ngp-sync` was private, so an anonymous fetch was impossible. Or chose
to make it public rather than put a read credential on the VM. The issue
named `git reset --hard` in `~/ngp-sync`, but that unlinks and rewrites files
rather than renaming them into place. It also rewrites `caddy/`, `provision/`
and `docker-compose.yml` on disk, and `~/ngp-sync` is an `scp` copy, not a
reliable clone. Or chose a site-only pull from a VM-only clone instead.

## Decision

- **`site/` on the VM follows `main`.** `identity/cmd/site-pull` runs on a
  15-minute systemd timer (`*:5/15`, five minutes after the manifest puller)
  as a one-shot container. It fetches `main` and makes the checkout's `site/`
  match `main`'s `site/`. A merge is live within about 17 minutes.
- **The git state lives in `/var/lib/ngp-site-pull/repo.git`**, a bare,
  shallow clone outside the checkout, so a redeploy cannot overwrite it. It is
  disposable: the next run rebuilds it.
- **Only `site/` is written, and never `site/update/`**, which is the manifest
  puller's (ADR 0001). Each changed file is written to a temp file in its own
  directory and renamed into place. Files `main` removed are deleted.
  `site/` itself is never swapped or renamed, so the bind mount stays valid.
  A brief window across files is accepted.
- **Content is compared, not remembered.** Each run compares every file's git
  blob id with `main`'s tree. A run at `main` writes nothing, and a `site/` a
  redeploy put back is repaired.
- **The puller writes inside the checkout, as the checkout's owner.** ADR 0001
  listed writing inside the checkout as a cost of its option (b). Here it is
  chosen, because the served `site/` is the checkout's. The container runs
  with `--user` set to the owner of `site/` so a later `scp` redeploy can
  still overwrite what it wrote. `vm-setup.sh` records that owner in
  `/etc/ngp-site-pull.env`.
- **The repository is public, and the VM holds no credential.** The URL
  (`https://github.com/orzelig/ngp-sync.git`) and the branch are constants in
  source. Git runs so that it can never prompt for, read or send a
  credential. Nothing secret may be committed, including in history.
- **`site/` is trusted through `main`, not through a signature.** Unlike the
  update manifest, site content is not signed. It is trusted because it is on
  `main`, behind branch protection and review, and fetched over TLS from
  github.com. As guards against a wrong tree, a tree without
  `site/index.html` is refused, and so is one with a symlink or submodule
  under `site/`.

## What this rules out

- **`git reset --hard` in `~/ngp-sync`** (the issue's first wording). Git
  unlinks and rewrites each file rather than renaming, so a file is briefly
  missing or partial. It also rewrites `caddy/`, `provision/` and
  `docker-compose.yml` on disk, and Caddy bind-mounts `caddy/Caddyfile`, so a
  merged Caddyfile change would go live on the next restart rather than on a
  deliberate redeploy. And it needs `~/ngp-sync` to be a real clone, which an
  `scp` copy is not.
- **Swapping `site/` by symlink or rename** (turned down at intake). It
  replaces the directory the bind mount points at, so Caddy keeps serving the
  old inode.
- **A read-only credential on the VM.** Or chose a public repository instead,
  so there is no token to place, rotate or leak.
- **A public mirror of `site/`.** Something would have to publish it on every
  merge, and the issue rules out GitHub Actions.
- **A shell script on the host.** It needs git on the VM, and `make ci` could
  not test it without a `Makefile` change. The Go command follows the
  manifest puller and is covered by the existing `go test ./...`.
- **GitHub's tarball download instead of a clone.** It downloads the whole
  repository on every run.
- **A "last applied commit" marker.** A redeploy that put back an older
  `site/` would look current.

## What would make this worth revisiting

- `orzelig/ngp-sync` going private again. Every pull would then fail, and the
  choice between a credential and a mirror comes back.
- The deploy no longer being an `scp` as Or's user, for example a deploy that
  makes `~/ngp-sync` a real clone. Then running as the checkout's owner, or the
  separate clone, may no longer be needed.
- `site/` gaining content that must not go live on merge alone (drafts,
  generated files), or content worth signing as the manifest is.
- A site big enough that comparing every file each run, or keeping each blob
  in memory, costs more than the VM can spare.
