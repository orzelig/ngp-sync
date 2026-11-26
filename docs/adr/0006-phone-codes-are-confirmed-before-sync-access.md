# 0006. Phone codes are confirmed before sync access

- **Status:** accepted
- **Date:** 2026-11-26

## Context

Add a phone (ADR 0005, ngp-sync#14) shows a one-time link code on screen as a
QR. A code on a screen can be photographed, and whoever redeems it first at
`POST /device/link` got a refresh token for the account. The security read of
#14 (ngp-sync#18) asked that a new device get no sync access until the account
owner confirms it, that devices stop being merged and removed by a name the
device chose, and that `sameOrigin` stop being a prefix test.

Shipped NGP browsers (ngp.31) redeem a link code at `POST /device/link` and do
not poll. A server that answered them with a pending response would break
their sync setup the day it deployed (zelig, 2026-11-26). NGP Phone is
unreleased.

## Decision

- **A phone code redeems to a pending request; a browser code does not.** The
  flow is keyed to the code's source, `linkEntry.Phone`, set by
  `mintPhoneCode`. A phone code answers 202
  `{"status":"pending","poll_id":"<64 hex>","expires_in":300,"interval":2}`
  and issues nothing. A code from `/callback` or `/dev/enroll` keeps the
  immediate token, unchanged.
- **The poll is `POST /device/link/poll` with `{"poll_id"}` in the body.** 200
  `{"status":"pending"}`; 200 `{"status":"confirmed", ...}` with the fields
  `/device/link` returns (access token for `accessTokenTTL`), once, after which
  the ID is unknown; 403 `{"status":"cancelled"}` for a cancelled request, one
  past its 5 minutes, or one whose device was removed before it was collected;
  401 for an unknown ID. The poll ID is a credential: 256 bits, returned once,
  never in a URL or on a page.
- **Pending requests live in memory**, in `App.pendingLinks` under `a.mu`,
  keyed by `hashToken(poll ID)`. That key is the pending ID the account page's
  Confirm and Cancel forms post (`POST /account/device/confirm`,
  `/account/device/cancel`, through `postAuth`). A request is pending for
  `pendingLinkTTL`, 5 minutes, kept as long again (reading as cancelled), then
  reaped.
- **The refresh token is issued at Confirm** and held in memory until the poll
  collects it, so the device is in the list when the owner returns to the
  account page. A confirmed request nobody collects has its device removed when
  reaped. Log out and Remove all devices cancel the account's requests,
  confirmed-but-uncollected ones included, removing their devices.
- **Devices are keyed by the refresh-token hash, never by name.** The account
  page lists one row per refresh token and `POST /account/revoke` takes `id`,
  through `store.RevokeDevice`. `RevokeDeviceByName` is gone.
- **`sameOrigin` compares the exact origin.** `Origin` must equal the
  lower-cased scheme and host of `BASE_URL`; with no `Origin`, the `Referer`'s
  parsed scheme and host must equal it. No prefix test decides trust.
- **Lock order in identity is `a.mu`, then `store.mu`.** Confirm holds `a.mu`
  across `IssueRefreshToken` so two Confirms cannot both issue.

This supersedes the part of ADR 0005 that says a phone's `/device/link` request
and response are unchanged.

## What this rules out

- **Pending for browser codes.** ngp.31 does not poll. Switching browsers over
  is a new issue once a browser release that polls has shipped.
- **A client flag (`"poll": true`) choosing the flow.** Whoever holds a
  photographed code could pick the unconfirmed path. The code's source cannot
  be chosen by the redeemer.
- **Pending requests in `identity.json`.** They would survive a restart, but at
  the cost of a stored schema for something that lives 5 minutes; link codes
  are already memory-only with the same restart behaviour. A restart drops
  pending requests, and a confirmed-but-uncollected one leaves a device row the
  owner can remove.
- **Issuing the token at the poll instead of at Confirm.** No plaintext held in
  memory, but the device would not be in the list when Confirm returns the
  owner to the account page.
- **`GET /device/link/poll?id=...`.** The poll ID would land in Caddy's access
  log.
- **Merging devices by display name.** The name is chosen by the device; two
  devices with one name are two devices.

## What would make this worth revisiting

- A browser release that polls ships: browser codes can then go pending too.
- Identity runs more than one instance, or restarts often enough that dropped
  pending requests matter: pending requests would need shared storage.
- The pending card's device name proves not enough to tell a person's own phone
  from a photographed code's: a short matching code shown on both screens would
  close that, as an additive field on the 202 response.
