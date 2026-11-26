# 0005. Phones join by the device-link flow

- **Status:** accepted
- **Date:** 2026-11-25

## Context

NGP Phone has to join a person's NGP account (ngp-sync#14, with the phone side
in orzelig/ngp-phone#15). The browser already joins through a one-time link
code: `/callback` mints an 8-hex code into the in-memory `linkCodes` store
(single use, `linkCodeTTL`, 10 minutes), and the browser spends it at
`POST /device/link` for a refresh token and an access JWT. The identity
service is standard-library Go, and its account page is the one place a
signed-in person already manages devices. Identity stores only the X handle,
not a display name.

## Decision

- **A phone joins through the same one-time link code and `POST /device/link`
  as the browser. There is no second pairing mechanism.** `POST /account/phone`
  (session, origin and CSRF checked by `postAuth`) mints the code with
  `mintPhoneCode`, which shares `mintCode` with the `mintLinkCode` that
  `/callback` and `/dev/enroll` use, and
  renders it on the page from the POST. The phone sends
  `{"code": "<code>", "device": "<phone model> · NGP Phone"}`; the request and
  response are unchanged.
- **The QR payload is `<BASE_URL>/pair?code=<code>`, and `GET /pair` never
  consumes a code.** `/pair` is what an ordinary camera app opens: a static
  instruction page, the same for any code or none, that never reads
  `linkCodes` or echoes the code. The phone takes the `code` parameter and
  calls the identity host it is configured for, not the host in the scanned
  URL.
- **`GET /me` returns the handle as `name`** until identity stores a display
  name (decided on ngp-sync#14). It returns `{handle, name, avatar}` and
  authenticates through `bearerClaims`, the same checks `/verify` makes. Like
  `/verify`, it does not check revocation: a removed phone's access token works
  until it expires, and the phone learns it was removed when `POST /token`
  returns 401.
- **A phone code is guarded more tightly than the browser's** (zelig on
  #17, 2026-11-26):
  - An account has one live phone code. `mintPhoneCode` replaces the earlier
    one. Every mint draws again on a collision, so it never overwrites another
    account's code, and drops expired codes.
  - Add a phone mints only within 15 minutes of an X login. The login time is
    the session cookie's expiry minus `sessionTTL`, because the cookie is set
    only at login. A staler session goes through `/login?reauth=1` and back
    to `GET /account/phone`, a page that POSTs and never mints on a GET. The
    30-day cookie alone cannot mint a device credential.
  - Log out and Remove all devices delete the account's live codes.
  - `POST /device/link` ignores `ttl_days` for a phone code, so phones get the
    15-minute default and renew with `POST /token`.
  - `POST /device/link` answers 429 after 10 failed codes a minute from one
    client IP, before it looks at the code. The IP is the rightmost
    `X-Forwarded-For` entry only from a loopback or private peer (Caddy), and
    an IPv6 /64 counts as one client.
- **The QR encoder is in-house** (`identity/qr.go`), limited to byte mode,
  error-correction level M and versions 1 to 6 (up to 106 bytes), drawn as an
  inline black-on-white SVG. A longer payload is an error, and the page falls
  back to the text code.

## What this rules out

- **A separate pairing protocol for phones** (its own codes, its own redeem
  route): two flows to secure and keep in step, for no capability the link
  code lacks.
- **Redirecting after the POST to `/account?code=...`**: it would put a live
  code in the address bar and the history.
- **`/pair` checking whether a code is valid**: anyone could then test codes
  without spending them.
- **A QR library dependency**: identity is standard-library only, and the
  payload is short and fixed in shape.
- **A PNG `data:` URI for the QR**: an inline SVG is sharper at any size and
  needs no image encoding step.
- **Level L, versions 1 to 5 (no block interleaving)**: slightly less code,
  but level M costs no extra size for the production URL (version 3, 29 × 29)
  and tolerates screen glare better.
- **Storing X's display name now**: it widens the change into the stored data
  and `identity/xoauth.go`; a real name is a separate change.

## What would make this worth revisiting

- Identity starting to store a display name: `/me`'s `name` should then return
  it.
- A phone needing something the link code cannot carry (a different lifetime,
  a capability the browser does not have), which would argue for its own flow.
- A payload longer than 106 bytes (a much longer `BASE_URL` or code), which
  needs versions above 6 and their version-information blocks.
- Revocation needing to take effect before the access token expires, for
  `/verify` and `/me` alike.
