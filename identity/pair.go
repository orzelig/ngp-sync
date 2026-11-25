package main

// Adding NGP Phone to an account (ADR 0005). The phone joins through the same
// one-time link code and POST /device/link as the browser:
//
//   1. Account page  POST /account/phone  → page with the code as a QR and text
//   2. Phone scans   <BASE_URL>/pair?code=<code>, takes the code, and calls
//                    POST /device/link (unchanged) → refresh + access token
//   3. Phone         GET /me (Bearer)      → who it is signed in as
//
// GET /pair is only what an ordinary camera app opens: a static instruction
// page that never reads or spends a code.

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

func (a *App) registerPairRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /account/phone", a.handleAddPhone)
	mux.HandleFunc("GET /pair", a.handlePair)
	mux.HandleFunc("GET /me", a.handleMe)
}

// mintLinkCode stores a fresh one-time device-link code for xid: 8 hex
// characters, single use, valid for linkCodeTTL. Every route that hands out a
// link code goes through it.
func (a *App) mintLinkCode(xid string) string {
	code := randHex(4)
	a.mu.Lock()
	a.linkCodes[code] = linkEntry{XID: xid, Expires: time.Now().Add(linkCodeTTL)}
	a.mu.Unlock()
	return code
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
	code := a.mintLinkCode(xid)
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
