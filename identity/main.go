package main

// ngp-sync identity service: "Log in with X" → one-time device-link code →
// refresh token + short-lived HS256 JWTs that the sync server will verify.
//
// Flow:
//   1. User opens  GET /login            → redirected to X, authorizes
//   2. X calls     GET /callback         → page shows a one-time link code
//   3. Browser     POST /device/link     {"code": "..."} → refresh + access token
//   4. Browser     POST /token           {"refresh_token": "..."} → fresh access token
//   5. Sync server verifies the JWT with the shared SYNC_JWT_SECRET
//      (GET /verify here does the same check, for curl testing)

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	accessTokenTTL = 15 * time.Minute
	linkCodeTTL    = 10 * time.Minute
	pendingAuthTTL = 15 * time.Minute
	jwtAudience    = "ngp-sync"
)

type Config struct {
	BaseURL       string
	XClientID     string
	XClientSecret string
	JWTSecret     []byte
	ListenAddr    string
	DataFile      string
}

type linkEntry struct {
	XID     string
	Expires time.Time
}

type App struct {
	cfg    Config
	store  *Store
	http   *http.Client
	dynamo *dynamoClient

	mu        sync.Mutex
	pending   map[string]pendingAuth // OAuth state → PKCE verifier
	linkCodes map[string]linkEntry   // one-time device-link codes
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := Config{
		BaseURL:       strings.TrimRight(envOr("BASE_URL", "http://localhost:8300"), "/"),
		XClientID:     os.Getenv("X_CLIENT_ID"),
		XClientSecret: os.Getenv("X_CLIENT_SECRET"),
		ListenAddr:    envOr("LISTEN_ADDR", ":8300"),
		DataFile:      envOr("DATA_FILE", "data/identity.json"),
	}
	secret := os.Getenv("SYNC_JWT_SECRET")
	if secret == "" {
		log.Fatal("SYNC_JWT_SECRET is required (generate one: openssl rand -hex 32)")
	}
	cfg.JWTSecret = []byte(secret)

	store, err := OpenStore(cfg.DataFile)
	if err != nil {
		log.Fatalf("open store %s: %v", cfg.DataFile, err)
	}

	app := &App{
		cfg:       cfg,
		store:     store,
		http:      &http.Client{Timeout: 15 * time.Second},
		dynamo:    newDynamoFromEnv(),
		pending:   map[string]pendingAuth{},
		linkCodes: map[string]linkEntry{},
	}
	go app.janitor()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /{$}", app.handleHome)
	mux.HandleFunc("GET /login", app.handleLogin)
	mux.HandleFunc("GET /callback", app.handleCallback)
	mux.HandleFunc("POST /device/link", app.handleDeviceLink)
	mux.HandleFunc("POST /token", app.handleToken)
	mux.HandleFunc("GET /verify", app.handleVerify)
	mux.HandleFunc("GET /avatar/{xid}", app.handleAvatarPNG)
	mux.HandleFunc("GET /linked", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>ngp-sync</title>`+
			`<body style="font-family:system-ui;max-width:34rem;margin:4rem auto">`+
			`<h1>Device linked</h1><p>Sync is set up in this browser. You can close this tab.</p>`)
	})
	mux.HandleFunc("GET /account", app.handleAccount)
	mux.HandleFunc("POST /account/revoke", app.handleRevoke)
	mux.HandleFunc("POST /account/revoke-all", app.handleRevokeAll)
	mux.HandleFunc("POST /account/logout", app.handleLogout)

	// Anonymous, hardened telemetry sink (see telemetry.go). Currently the
	// opt-in settings-reset report the NGP browser POSTs. Decoupled from the
	// auth App on purpose: no store access, no JWT secret.
	telemetry := newTelemetryCollector(telemetryPathFor(cfg.DataFile))
	mux.HandleFunc("POST /t/reset", telemetry.handleResetReport)

	if os.Getenv("DEV_MODE") == "1" {
		mux.HandleFunc("POST /dev/seed", app.handleDevSeed)
		mux.HandleFunc("GET /dev/enroll", app.handleDevEnroll)
		mux.HandleFunc("GET /dev/avatar.png", handleDevAvatar)
		log.Printf("DEV_MODE=1: /dev/seed and /dev/enroll enabled — never set this in production")
	}

	log.Printf("identity service listening on %s (public base %s)", cfg.ListenAddr, cfg.BaseURL)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // slowloris guard
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second, // generous: the OAuth callback does upstream calls
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func (a *App) handleHome(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, "ngp-sync identity service. Start at %s/login\n", a.cfg.BaseURL)
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if xid := a.sessionXID(r); xid != "" {
		http.Redirect(w, r, "/account", http.StatusFound)
		return
	}
	if a.cfg.XClientID == "" || a.cfg.XClientSecret == "" {
		http.Error(w, "X_CLIENT_ID / X_CLIENT_SECRET are not configured yet", http.StatusServiceUnavailable)
		return
	}
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "" // only same-site absolute paths; no open redirects
	}
	state := randHex(16)
	verifier := randHex(32) // 64 chars — within PKCE's 43–128 requirement
	a.mu.Lock()
	a.pending[state] = pendingAuth{Verifier: verifier, Next: next, Expires: time.Now().Add(pendingAuthTTL)}
	a.mu.Unlock()
	http.Redirect(w, r, a.authorizeURL(state, verifier), http.StatusFound)
}

func (a *App) handleCallback(w http.ResponseWriter, r *http.Request) {
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		http.Error(w, "X authorization failed: "+errMsg, http.StatusBadRequest)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	a.mu.Lock()
	p, ok := a.pending[state]
	delete(a.pending, state) // single use
	a.mu.Unlock()
	if !ok || time.Now().After(p.Expires) || code == "" {
		http.Error(w, "invalid or expired login attempt; start again at /login", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	xAccessToken, err := a.exchangeCode(ctx, code, p.Verifier)
	if err != nil {
		log.Printf("token exchange: %v", err)
		http.Error(w, "token exchange with X failed", http.StatusBadGateway)
		return
	}
	xid, username, avatarURL, err := a.fetchXIdentity(ctx, xAccessToken)
	if err != nil {
		log.Printf("users/me: %v", err)
		http.Error(w, "could not fetch X identity", http.StatusBadGateway)
		return
	}
	if err := a.store.UpsertUserWithAvatar(xid, username, avatarURL); err != nil {
		log.Printf("store: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.setSession(w, xid) // web session for the account page (id.ngp.one/account)
	if p.Next != "" {
		// Came from the website (e.g. the "My account" link) — go to the page
		// instead of the browser device-link screen.
		http.Redirect(w, r, p.Next, http.StatusFound)
		return
	}

	deviceCode := randHex(4) // 8 hex chars, single-use, short-lived
	a.mu.Lock()
	a.linkCodes[deviceCode] = linkEntry{XID: xid, Expires: time.Now().Add(linkCodeTTL)}
	a.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = linkPageTmpl.Execute(w, map[string]string{
		"Username": username,
		"Code":     deviceCode,
		"BaseURL":  a.cfg.BaseURL,
	})
}

func (a *App) handleDeviceLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code   string `json:"code"`
		Device string `json:"device"`
		// Optional: access-token lifetime in days (clamped to 90). The browser
		// patch reads one static token from a file and has no refresh loop yet,
		// so device linking can mint a long-lived token directly.
		TTLDays int `json:"ttl_days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request body")
		return
	}
	code := strings.ToLower(strings.TrimSpace(in.Code))

	a.mu.Lock()
	entry, ok := a.linkCodes[code]
	delete(a.linkCodes, code) // single use
	a.mu.Unlock()
	if !ok || time.Now().After(entry.Expires) {
		jsonError(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}

	refresh, err := a.store.IssueRefreshToken(entry.XID, in.Device)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "storage error")
		return
	}
	a.writeTokenResponseTTL(w, entry.XID, refresh, clampTTLDays(in.TTLDays))
}

func (a *App) handleToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
		TTLDays      int    `json:"ttl_days"` // optional, clamped to 90 days
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request body")
		return
	}
	xid, ok := a.store.LookupRefreshToken(in.RefreshToken)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unknown refresh token")
		return
	}
	a.writeTokenResponseTTL(w, xid, "", clampTTLDays(in.TTLDays))
}

// clampTTLDays maps an optional ttl_days request field to a token lifetime:
// 0 (absent) keeps the short default; anything else is clamped to [1, 90] days.
func clampTTLDays(days int) time.Duration {
	if days <= 0 {
		return accessTokenTTL
	}
	if days > 90 {
		days = 90
	}
	return time.Duration(days) * 24 * time.Hour
}

func (a *App) handleVerify(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		jsonError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	claims, err := VerifyJWT(a.cfg.JWTSecret, strings.TrimPrefix(h, prefix))
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if claims.Aud != jwtAudience {
		jsonError(w, http.StatusUnauthorized, "wrong audience")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(claims)
}

func (a *App) mintAccessToken(xid string, ttl time.Duration) (string, error) {
	now := time.Now()
	return MintJWT(a.cfg.JWTSecret, Claims{
		Iss: a.cfg.BaseURL,
		Sub: "x:" + xid,
		Aud: jwtAudience,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
	})
}

func (a *App) writeTokenResponseTTL(w http.ResponseWriter, xid, refresh string, ttl time.Duration) {
	tok, err := a.mintAccessToken(xid, ttl)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "token mint failed")
		return
	}
	out := map[string]any{
		"account_id":   "x:" + xid,
		"access_token": tok,
		"token_type":   "bearer",
		"expires_in":   int(ttl.Seconds()),
	}
	if u, ok := a.store.GetUser(xid); ok {
		out["username"] = u.Username
		if u.AvatarURL != "" {
			if strings.HasPrefix(u.AvatarURL, a.cfg.BaseURL) {
				out["avatar_url"] = u.AvatarURL // dev: already a local PNG
			} else {
				out["avatar_url"] = a.cfg.BaseURL + "/avatar/" + xid
			}
		}
	}
	if refresh != "" {
		out["refresh_token"] = refresh
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (a *App) writeTokenResponse(w http.ResponseWriter, xid, refresh string) {
	a.writeTokenResponseTTL(w, xid, refresh, accessTokenTTL)
}

// handleDevSeed is a DEV_MODE-only path to mint tokens without the X OAuth
// flow, so the sync pipeline can be tested before X app credentials exist.
func (a *App) handleDevSeed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		XID      string `json:"x_id"`
		Username string `json:"username"`
		TTLDays  int    `json:"ttl_days"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) // all fields optional
	if in.XID == "" {
		in.XID = "dev1"
	}
	if in.Username == "" {
		in.Username = "devuser"
	}
	if in.TTLDays <= 0 {
		in.TTLDays = 30
	}
	if err := a.store.UpsertUser(in.XID, in.Username); err != nil {
		jsonError(w, http.StatusInternalServerError, "storage error")
		return
	}
	refresh, err := a.store.IssueRefreshToken(in.XID, "dev-seed")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "storage error")
		return
	}
	a.writeTokenResponseTTL(w, in.XID, refresh, time.Duration(in.TTLDays)*24*time.Hour)
}

// handleDevEnroll (DEV_MODE only) renders the same post-login page as the X
// callback, with a fresh device code for a seeded dev user — so the browser's
// native ngp://enable-sync flow can be exercised without X credentials.
func (a *App) handleDevEnroll(w http.ResponseWriter, r *http.Request) {
	xid := r.URL.Query().Get("x_id")
	if xid == "" {
		xid = "dev1"
	}
	if err := a.store.UpsertUserWithAvatar(xid, "devuser", a.cfg.BaseURL+"/dev/avatar.png"); err != nil {
		jsonError(w, http.StatusInternalServerError, "storage error")
		return
	}
	a.setSession(w, xid)
	deviceCode := randHex(4)
	a.mu.Lock()
	a.linkCodes[deviceCode] = linkEntry{XID: xid, Expires: time.Now().Add(linkCodeTTL)}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = linkPageTmpl.Execute(w, map[string]string{
		"Username": "devuser",
		"Code":     deviceCode,
		"BaseURL":  a.cfg.BaseURL,
	})
}

func (a *App) janitor() {
	for range time.Tick(time.Minute) {
		now := time.Now()
		a.mu.Lock()
		for k, v := range a.pending {
			if now.After(v.Expires) {
				delete(a.pending, k)
			}
		}
		for k, v := range a.linkCodes {
			if now.After(v.Expires) {
				delete(a.linkCodes, k)
			}
		}
		a.mu.Unlock()
	}
}

// handleAvatarPNG proxies the user's X profile image, re-encoded as PNG so
// the browser's enrollment code needs only a PNG decoder. Served for linked
// users only; no auth needed (avatars are public on X anyway).
func (a *App) handleAvatarPNG(w http.ResponseWriter, r *http.Request) {
	u, ok := a.store.GetUser(r.PathValue("xid"))
	if !ok || u.AvatarURL == "" {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.AvatarURL, nil)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	resp, err := a.http.Do(req)
	if err != nil {
		http.Error(w, "avatar fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "avatar fetch failed", http.StatusBadGateway)
		return
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		http.Error(w, "avatar decode failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_ = png.Encode(w, img)
}

// handleDevAvatar serves a tiny generated PNG so the browser's avatar
// download path can be exercised without X.
func handleDevAvatar(w http.ResponseWriter, _ *http.Request) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		switch i % 4 {
		case 0:
			img.Pix[i] = 0x1d // r
		case 1:
			img.Pix[i] = 0x9b // g
		case 2:
			img.Pix[i] = 0xf0 // b
		default:
			img.Pix[i] = 0xff // a
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, img)
}

// ---- Account page (web-session gated; see session.go) --------------------

type deviceView struct {
	Name       string
	RevokeName string
	Created    string
}

type accountView struct {
	Username  string
	AvatarURL string
	Joined    string
	Synced    []TypeCount
	SyncOK    bool
	SyncTotal int
	Devices   []deviceView
	CSRF      string
}

func (a *App) handleAccount(w http.ResponseWriter, r *http.Request) {
	xid := a.sessionXID(r)
	if xid == "" {
		http.Redirect(w, r, "/login?next=/account", http.StatusFound)
		return
	}
	u, ok := a.store.GetUser(xid)
	if !ok {
		a.clearSession(w)
		http.Redirect(w, r, "/login?next=/account", http.StatusFound)
		return
	}

	// What's synced — best-effort read-only count from the sync DB.
	var synced []TypeCount
	syncOK := false
	total := 0
	if a.dynamo != nil {
		if s, err := a.dynamo.SyncSummary(syncClientID(xid)); err != nil {
			log.Printf("sync summary: %v", err)
		} else {
			synced, syncOK = s, true
			for _, t := range s {
				total += t.Count
			}
		}
	}

	// Devices — collapse duplicates (one install per device; repeats are dev
	// artifacts) by display name, newest first. Remove targets every token of
	// that name.
	seen := map[string]bool{}
	var devs []deviceView
	for _, d := range a.store.ListDevices(xid) {
		raw := strings.TrimSpace(d.Device)
		disp := raw
		if disp == "" {
			disp = "NGP browser"
		}
		key := strings.ToLower(disp)
		if seen[key] {
			continue
		}
		seen[key] = true
		devs = append(devs, deviceView{Name: disp, RevokeName: raw, Created: d.Created.Format("Jan 2, 2006")})
	}

	avatar := ""
	if u.AvatarURL != "" {
		if strings.HasPrefix(u.AvatarURL, a.cfg.BaseURL) {
			avatar = u.AvatarURL
		} else {
			avatar = a.cfg.BaseURL + "/avatar/" + xid
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = accountTmpl.Execute(w, accountView{
		Username:  u.Username,
		AvatarURL: avatar,
		Joined:    u.CreatedAt.Format("January 2, 2006"),
		Synced:    synced,
		SyncOK:    syncOK,
		SyncTotal: total,
		Devices:   devs,
		CSRF:      a.csrfToken(xid),
	})
}

func (a *App) handleRevoke(w http.ResponseWriter, r *http.Request) {
	xid, ok := a.postAuth(w, r)
	if !ok {
		return
	}
	// "name" is the device's raw name (may be empty — the unnamed-device
	// bucket); revoke every token that shares it.
	if _, err := a.store.RevokeDeviceByName(xid, r.PostForm.Get("name")); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (a *App) handleRevokeAll(w http.ResponseWriter, r *http.Request) {
	xid, ok := a.postAuth(w, r)
	if !ok {
		return
	}
	if _, err := a.store.RevokeAllDevices(xid); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.postAuth(w, r); !ok {
		return
	}
	a.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

var accountTmpl = template.Must(template.New("account").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Your NGP account</title>
<style>
  :root{ --orange:#EE5A05; --orange-hi:#FF8A2A; --green:#13B45A; --ink:#1b1b1f; --mut:#6b6b74; --bg:#fafafa; --card:#fff; --line:#e4e4e8; --danger:#c0362c; }
  @media (prefers-color-scheme:dark){ :root{ --ink:#ececf0; --mut:#9a9aa4; --bg:#131316; --card:#1d1d22; --line:#33333b; --danger:#ff6a5e; } }
  *{ box-sizing:border-box; margin:0; padding:0; }
  body{ font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif; background:var(--bg); color:var(--ink); line-height:1.5; }
  .wrap{ max-width:40rem; margin:0 auto; padding:2.5rem 1.25rem 4rem; }
  header.id{ display:flex; align-items:center; gap:.9rem; margin-bottom:.5rem; }
  header.id .logo{ width:44px; height:44px; flex:none; }
  header.id img.avatar{ width:52px; height:52px; border-radius:50%; flex:none; background:var(--line); object-fit:cover; }
  header.id .who .handle{ font-size:1.35rem; font-weight:700; }
  header.id .who .sub{ font-size:.85rem; color:var(--mut); }
  h2{ font-size:.78rem; text-transform:uppercase; letter-spacing:.06em; color:var(--mut); margin:2rem 0 .7rem; }
  .card{ background:var(--card); border:1px solid var(--line); border-radius:14px; padding:1.05rem 1.2rem; }
  .card + .card{ margin-top:.6rem; }
  .row{ display:flex; align-items:center; gap:1rem; }
  .row .grow{ flex:1; min-width:0; }
  .row .name{ font-weight:600; }
  .row .meta{ font-size:.82rem; color:var(--mut); }
  .note{ font-size:.92rem; color:var(--mut); }
  .note b{ color:var(--ink); font-weight:600; }
  .note.sub{ margin-top:.65rem; }
  .list{ padding:.3rem 1.2rem; }
  .srow{ display:flex; justify-content:space-between; align-items:center; padding:.6rem 0; border-top:1px solid var(--line); }
  .srow:first-child{ border-top:none; }
  .sname{ font-weight:500; }
  .scount{ color:var(--mut); font-weight:600; font-variant-numeric:tabular-nums; }
  button{ font:inherit; cursor:pointer; border-radius:9px; border:1px solid var(--line); background:var(--card); color:var(--ink); padding:.5rem .95rem; font-weight:600; }
  button:hover{ border-color:var(--mut); }
  button.link{ border:none; background:none; color:var(--danger); padding:.4rem .2rem; }
  button.link:hover{ text-decoration:underline; }
  .actions{ display:flex; gap:.6rem; flex-wrap:wrap; margin-top:1.4rem; }
  .btn-danger{ color:#fff; background:var(--danger); border-color:var(--danger); }
  form.inline{ display:inline; margin:0; }
  footer{ margin-top:2.5rem; font-size:.8rem; color:var(--mut); line-height:1.6; }
  footer a{ color:var(--mut); }
</style>
<div class="wrap">
  <header class="id">
    <svg class="logo" viewBox="0 0 1024 1024" aria-hidden="true"><defs><radialGradient id="o" cx="38%" cy="30%" r="85%"><stop offset="0%" stop-color="#FF8A2A"/><stop offset="100%" stop-color="#EE5A05"/></radialGradient></defs><circle cx="512" cy="512" r="480" fill="url(#o)"/><circle cx="512" cy="512" r="262" fill="#fff"/><circle cx="512" cy="512" r="198" fill="#13B45A"/></svg>
    {{if .AvatarURL}}<img class="avatar" src="{{.AvatarURL}}" alt="">{{end}}
    <div class="who">
      <div class="handle">@{{.Username}}</div>
      <div class="sub">NGP account · joined {{.Joined}}</div>
    </div>
  </header>

  <h2>What&rsquo;s synced{{if .SyncOK}} &middot; {{.SyncTotal}} items{{end}}</h2>
  {{if and .SyncOK .Synced}}
  <div class="card list">
    {{range .Synced}}<div class="srow"><span class="sname">{{.Name}}</span><span class="scount">{{.Count}}</span></div>{{end}}
  </div>
  <p class="note sub">Stored encrypted on <b>sync.ngp.one</b> and readable only in the NGP browser. Choose which types sync in the browser&rsquo;s <b>Settings</b>.</p>
  {{else if .SyncOK}}
  <div class="card"><p class="note">Nothing is syncing yet. Open <b>id.ngp.one/login</b> in the NGP browser and click <b>Enable sync</b>.</p></div>
  {{else}}
  <div class="card"><p class="note">Your data syncs through <b>sync.ngp.one</b> (encrypted in transit and at rest) and is managed in the NGP browser. Live item counts are temporarily unavailable.</p></div>
  {{end}}

  <h2>Devices ({{len .Devices}})</h2>
  {{if .Devices}}
    {{range .Devices}}
    <div class="card row">
      <div class="grow">
        <div class="name">{{.Name}}</div>
        <div class="meta">Linked {{.Created}}</div>
      </div>
      <form class="inline" method="post" action="/account/revoke">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="name" value="{{.RevokeName}}">
        <button class="link" type="submit">Remove</button>
      </form>
    </div>
    {{end}}
  {{else}}
    <div class="card"><p class="note">No devices are linked yet. Open <b>id.ngp.one/login</b> in the NGP browser and click <b>Enable sync</b>.</p></div>
  {{end}}

  <div class="actions">
    {{if .Devices}}
    <form class="inline" method="post" action="/account/revoke-all">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <button class="btn-danger" type="submit">Remove all devices</button>
    </form>
    {{end}}
    <form class="inline" method="post" action="/account/logout">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <button type="submit">Log out</button>
    </form>
  </div>

  <footer>Removing a device stops it from renewing its sync token; it keeps access until its current token next expires (up to its original lifetime). · <a href="https://ngp.one">ngp.one</a></footer>
</div>
`))

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

var linkPageTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ngp-sync — link a device</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 34rem; margin: 4rem auto; padding: 0 1rem; }
  code.big { font-size: 1.6rem; letter-spacing: .2em; background: #f2f2f2; padding: .4rem .8rem; border-radius: 6px; }
  pre { background: #f8f8f8; padding: .8rem; border-radius: 6px; overflow-x: auto; }
  a.enroll { display: inline-block; background: #111; color: #fff; padding: .7rem 1.2rem; border-radius: 8px; text-decoration: none; font-weight: 600; }
</style>
<h1>Signed in as @{{.Username}}</h1>
<script>
  // In the NGP browser the ngp:// navigation is handled natively: it stores a
  // sync token and restarts the browser. Elsewhere it is a silent no-op and
  // the manual options below remain. Strip the one-time OAuth params first so
  // session restore after the restart does not replay the callback.
  history.replaceState({}, "", "/linked");
  location.href = "ngp://enable-sync?code={{.Code}}";
</script>
<p><a class="enroll" href="ngp://enable-sync?code={{.Code}}">Enable sync in the NGP browser</a><br>
<small>(NGP browser only — stores a sync token and restarts; in the NGP
browser this happens automatically a moment after this page opens)</small></p>
<p><a href="/account">Manage your account &rarr;</a></p>
<p>From any other browser, enter this one-time code to link a device to your
sync account. It is valid for 10 minutes and can be used once:</p>
<p><code class="big">{{.Code}}</code></p>
<p>Or link manually from a terminal:</p>
<pre>curl -s -X POST {{.BaseURL}}/device/link \
  -H 'Content-Type: application/json' \
  -d '{"code":"{{.Code}}","device":"my-laptop"}'</pre>
`))
