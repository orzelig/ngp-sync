package main

// Adding NGP Phone to an account (ADR 0005, ADR 0006). The phone joins through
// the same one-time link code and POST /device/link as the browser, but a phone
// code gets no token until the account owner confirms it (devicelink.go):
//
//   1. Account page  POST /account/phone  → page with the code as a QR and text
//   2. Phone scans   <BASE_URL>/pair?code=<code>, takes the code, and calls
//                    POST /device/link → 202 pending with a poll ID
//   3. Owner         Confirm on the account page; the phone's
//                    POST /device/link/poll → refresh + access token, once
//   4. Phone         GET /me (Bearer)      → who it is signed in as
//
// GET /pair is only what an ordinary camera app opens: a static instruction
// page that never reads or spends a code.

import (
	"encoding/base64"
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// phoneLoginWindow: Add a phone mints a code only this soon after an X
	// login. The 30-day session cookie alone cannot mint a device credential.
	phoneLoginWindow = 15 * time.Minute
	// linkFailLimit failed POST /device/link redemptions per client per
	// linkFailWindow, so the 32-bit code cannot be guessed.
	linkFailLimit  = 10
	linkFailWindow = time.Minute
	// addPhoneLogin is where a stale session is sent: X login, then back to
	// Add a phone.
	addPhoneLogin = "/login?reauth=1&next=/account/phone"
)

// newLinkCode draws a candidate link code. A variable so a test can force a
// collision.
var newLinkCode = func() string { return randHex(4) }

type failWindow struct {
	Start time.Time
	N     int
}

func (a *App) registerPairRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /account/phone", a.handleAddPhone)
	mux.HandleFunc("GET /account/phone", a.handleAddPhoneAfterLogin)
	mux.HandleFunc("GET /pair", a.handlePair)
	mux.HandleFunc("GET /me", a.handleMe)
}

// mintLinkCode stores a fresh one-time device-link code for xid: 8 hex
// characters, single use, valid for linkCodeTTL. /callback and /dev/enroll use
// it; Add a phone uses mintPhoneCode.
func (a *App) mintLinkCode(xid string) string { return a.mintCode(xid, false) }

// mintPhoneCode is mintLinkCode for Add a phone: it replaces the account's
// earlier phone code, so an account has at most one live phone code.
func (a *App) mintPhoneCode(xid string) string { return a.mintCode(xid, true) }

func (a *App) mintCode(xid string, phone bool) string {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropExpiredLocked(now)
	if phone {
		for k, v := range a.linkCodes {
			if v.Phone && v.XID == xid {
				delete(a.linkCodes, k)
			}
		}
	}
	code := newLinkCode()
	for { // a collision draws again; it never overwrites another code
		if _, taken := a.linkCodes[code]; !taken {
			break
		}
		code = newLinkCode()
	}
	a.linkCodes[code] = linkEntry{XID: xid, Expires: now.Add(linkCodeTTL), Phone: phone}
	return code
}

// dropLinkCodes deletes every live link code of xid (log out, remove all).
func (a *App) dropLinkCodes(xid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.linkCodes {
		if v.XID == xid {
			delete(a.linkCodes, k)
		}
	}
}

// dropExpiredLocked deletes expired link codes and finished failure windows.
// The caller holds a.mu.
func (a *App) dropExpiredLocked(now time.Time) {
	for k, v := range a.linkCodes {
		if now.After(v.Expires) {
			delete(a.linkCodes, k)
		}
	}
	for k, v := range a.linkFails {
		if now.Sub(v.Start) >= linkFailWindow {
			delete(a.linkFails, k)
		}
	}
}

// linkAttemptAllowed reports whether ip may try a code at POST /device/link:
// fewer than linkFailLimit failures in its current window.
func (a *App) linkAttemptAllowed(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.linkFails[ip]
	if !ok || time.Since(f.Start) >= linkFailWindow {
		return true
	}
	return f.N < linkFailLimit
}

// noteLinkFailure counts a failed redemption against ip.
func (a *App) noteLinkFailure(ip string) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.linkFails == nil {
		a.linkFails = map[string]failWindow{}
	}
	f, ok := a.linkFails[ip]
	if !ok || now.Sub(f.Start) >= linkFailWindow {
		f = failWindow{Start: now}
	}
	f.N++
	a.linkFails[ip] = f
}

// linkClientIP is the address failures are counted against. Behind Caddy (a
// loopback or private peer) it is the rightmost X-Forwarded-For entry, the one
// Caddy wrote; from any other peer the header is ignored. IPv6 is counted per
// /64, which one client usually holds whole.
func linkClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if p := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); p != nil {
				ip = p
			}
		}
	}
	if ip == nil {
		return host
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

// sessionLoginAt is when the session's X login happened. The session cookie is
// set only at login, expiring sessionTTL later, so the login time is that
// expiry minus sessionTTL. Zero when there is no valid session.
func (a *App) sessionLoginAt(r *http.Request) time.Time {
	if a.sessionXID(r) == "" { // checks the signature and the expiry
		return time.Time{}
	}
	c, _ := r.Cookie(sessionCookieName)
	b64, _, _ := strings.Cut(c.Value, ".")
	msg, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return time.Time{}
	}
	_, expStr, _ := strings.Cut(string(msg), "|")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(exp, 0).Add(-sessionTTL)
}

// recentLogin reports whether the session's X login is within phoneLoginWindow.
func (a *App) recentLogin(r *http.Request) bool {
	at := a.sessionLoginAt(r)
	return !at.IsZero() && time.Since(at) <= phoneLoginWindow
}

// bearerClaims checks the request's Bearer access token the way go-sync does
// (signature, expiry, audience). On failure it writes a 401 and returns false.
// It does not check revocation: a removed device's access token works until it
// expires.
func (a *App) bearerClaims(w http.ResponseWriter, r *http.Request) (*Claims, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		jsonError(w, http.StatusUnauthorized, "missing bearer token")
		return nil, false
	}
	claims, err := VerifyJWT(a.cfg.JWTSecret, strings.TrimPrefix(h, prefix))
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return nil, false
	}
	if claims.Aud != jwtAudience {
		jsonError(w, http.StatusUnauthorized, "wrong audience")
		return nil, false
	}
	return claims, true
}

// publicAvatarURL is the avatar a client should fetch: the PNG proxy, or the
// stored URL when it is already ours (dev), or "" when there is none.
func (a *App) publicAvatarURL(u User) string {
	switch {
	case u.AvatarURL == "":
		return ""
	case strings.HasPrefix(u.AvatarURL, a.cfg.BaseURL):
		return u.AvatarURL
	default:
		return a.cfg.BaseURL + "/avatar/" + u.XID
	}
}

type addPhoneView struct {
	Code string
	QR   template.HTML
}

func (a *App) handleAddPhone(w http.ResponseWriter, r *http.Request) {
	xid, ok := a.postAuth(w, r)
	if !ok {
		return
	}
	if !a.recentLogin(r) {
		http.Redirect(w, r, addPhoneLogin, http.StatusSeeOther)
		return
	}
	code := a.mintPhoneCode(xid)
	view := addPhoneView{Code: code}
	if m, err := qrEncode(a.cfg.BaseURL + "/pair?code=" + code); err != nil {
		log.Printf("add a phone: qr: %v", err) // the page still shows the text code
	} else {
		view.QR = qrSVG(m)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = addPhoneTmpl.Execute(w, view)
}

// handleAddPhoneAfterLogin is where X login returns a person who pressed Add a
// phone with a stale session. A GET never mints: it shows one button that
// POSTs to Add a phone with the CSRF token.
func (a *App) handleAddPhoneAfterLogin(w http.ResponseWriter, r *http.Request) {
	xid := a.sessionXID(r)
	if xid == "" {
		http.Redirect(w, r, "/login?next=/account/phone", http.StatusFound)
		return
	}
	if !a.recentLogin(r) {
		http.Redirect(w, r, addPhoneLogin, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = addPhoneConfirmTmpl.Execute(w, map[string]string{"CSRF": a.csrfToken(xid)})
}

func (a *App) handlePair(w http.ResponseWriter, _ *http.Request) {
	// Same page whatever the code is, or with none: never an oracle for codes.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = pairTmpl.Execute(w, nil)
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := a.bearerClaims(w, r)
	if !ok {
		return
	}
	u, ok := a.store.GetUser(strings.TrimPrefix(claims.Sub, "x:"))
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unknown account")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Identity stores only the X handle, so name is the handle (ADR 0005).
	_ = json.NewEncoder(w).Encode(map[string]string{
		"handle": u.Username,
		"name":   u.Username,
		"avatar": a.publicAvatarURL(u),
	})
}

// pageStyle is the account page's look, for the two pages below.
const pageStyle = `<style>
  :root{ --ink:#1b1b1f; --mut:#6b6b74; --bg:#fafafa; --card:#fff; --line:#e4e4e8; }
  @media (prefers-color-scheme:dark){ :root{ --ink:#ececf0; --mut:#9a9aa4; --bg:#131316; --card:#1d1d22; --line:#33333b; } }
  *{ box-sizing:border-box; margin:0; padding:0; }
  body{ font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif; background:var(--bg); color:var(--ink); line-height:1.5; }
  .wrap{ max-width:40rem; margin:0 auto; padding:2.5rem 1.25rem 4rem; }
  h1{ font-size:1.35rem; font-weight:700; margin-bottom:1rem; }
  .card{ background:var(--card); border:1px solid var(--line); border-radius:14px; padding:1.05rem 1.2rem; }
  .note{ font-size:.92rem; color:var(--mut); }
  .note b{ color:var(--ink); font-weight:600; }
  svg.qr{ display:block; width:min(18rem,100%); height:auto; margin:0 auto 1rem; border-radius:8px; }
  code.big{ display:block; text-align:center; font-size:1.6rem; letter-spacing:.2em; font-weight:700; margin:.4rem 0 1rem; }
  p + p{ margin-top:.65rem; }
  a{ color:var(--ink); }
  .back{ margin-top:1.4rem; font-size:.92rem; }
  button{ font:inherit; cursor:pointer; border-radius:9px; border:1px solid var(--line); background:var(--card); color:var(--ink); padding:.5rem .95rem; font-weight:600; margin-top:1rem; }
</style>`

var addPhoneTmpl = template.Must(template.New("add-phone").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Add a phone · NGP account</title>
` + pageStyle + `
<div class="wrap">
  <h1>Add a phone</h1>
  <div class="card">
    {{if .QR}}{{.QR}}{{end}}
    <code class="big">{{.Code}}</code>
    <p class="note">On your phone, open <b>NGP</b>, go to <b>Settings</b>, <b>NGP account</b>, and scan this code. It works once and expires in 10 minutes.</p>
    <p class="note">Then go back to your account and press <b>Confirm</b> next to your phone. It gets no access to your sync data until you do.</p>
  </div>
  <p class="back"><a href="/account">Back to your account</a></p>
</div>
`))

var addPhoneConfirmTmpl = template.Must(template.New("add-phone-confirm").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Add a phone · NGP account</title>
` + pageStyle + `
<div class="wrap">
  <h1>Add a phone</h1>
  <div class="card">
    <p class="note">You signed in again. Show a one-time code to scan with NGP on your phone.</p>
    <form method="post" action="/account/phone">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <button type="submit">Show the code</button>
    </form>
  </div>
  <p class="back"><a href="/account">Back to your account</a></p>
</div>
`))

var pairTmpl = template.Must(template.New("pair").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Add a phone · NGP</title>
` + pageStyle + `
<div class="wrap">
  <h1>Add a phone</h1>
  <div class="card">
    <p>Open NGP on your phone, go to Settings, NGP account, and scan this code there.</p>
  </div>
</div>
`))
