# 0003. The download page reads its link and version from the served update manifest

- **Status:** accepted
- **Date:** 2026-11-17

## Context

ngp.one's download page (`site/index.html`) hard-coded the release's download
link and version, so every release needed a hand edit to the page. The
manifest ngp.one serves at `/update/macos-arm64.json` lives in
`/var/lib/ngp-update` on the VM and is written only by `ngp-manifest-pull`,
after it verified the signature against a pinned key (ADR 0001). `site/` on
the VM is made to match `main`, so anything that rewrote `index.html` on the VM
would be reverted on the next pull.

## Decision

- The download page takes its link and version from the served update
  manifest, in the browser, when it loads: one inline script at the end of
  `site/index.html` fetches `/update/macos-arm64.json` with `cache: 'no-store'`.
  The hard-coded markup (the anchor `id="dl"` and the span `id="ver"`) is the
  fallback, and may go stale.
- The page applies the manifest only when `url` and `version` are both
  non-empty strings, as the puller's `parseManifest` requires; both are set or
  neither is. Any failure (network, non-OK, bad JSON, incomplete manifest)
  leaves the markup as it is.
- The page does not re-verify `sig`. The trust is the puller's verification
  before install (ADR 0001) plus same-origin TLS.
- Tests for `site/` are dependency-free `node --test` files that execute the
  page's real inline script against a stubbed `fetch` and DOM
  (`site/index.test.js`), so `make ci` requires Node 18 or newer on the PATH,
  and fails saying so when it is missing.

## What this rules out

- **Writing the version into `index.html` on the VM** (by the puller or a
  release step): the site puller reverts it, and it breaks "manifest-pull
  writes only `/var/lib/ngp-update`".
- **Writing it into `index.html` at release time**, as a commit to `main`: that
  is the manual edit this removes, or a new credential for the release step.
- **Rendering the version server-side with Caddy templates**: it changes
  `caddy/`, couples the page to the web server, and cannot be tested in
  `make ci` without running Caddy.
- **Having `make setup` download Node** when it is missing: a second
  downloaded toolchain; `make ci` fails and says so instead.

## What would make this worth revisiting

A Content-Security-Policy that forbids inline script on ngp.one, or a second
platform's manifest (the page would then need to choose among several).
