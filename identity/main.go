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
	cfg   Config
	store *Store
	http  *http.Client

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

	log.Printf("identity service listening on %s (public base %s)", cfg.ListenAddr, cfg.BaseURL)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}

func (a *App) handleHome(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, "ngp-sync identity service. Start at %s/login\n", a.cfg.BaseURL)
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.XClientID == "" || a.cfg.XClientSecret == "" {
		http.Error(w, "X_CLIENT_ID / X_CLIENT_SECRET are not configured yet", http.StatusServiceUnavailable)
		return
	}
	state := randHex(16)
	verifier := randHex(32) // 64 chars — within PKCE's 43–128 requirement
	a.mu.Lock()
	a.pending[state] = pendingAuth{Verifier: verifier, Expires: time.Now().Add(pendingAuthTTL)}
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
	xid, username, err := a.fetchXIdentity(ctx, xAccessToken)
	if err != nil {
		log.Printf("users/me: %v", err)
		http.Error(w, "could not fetch X identity", http.StatusBadGateway)
		return
	}
	if err := a.store.UpsertUser(xid, username); err != nil {
		log.Printf("store: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
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
	a.writeTokenResponse(w, entry.XID, refresh)
}

func (a *App) handleToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
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
	a.writeTokenResponse(w, xid, "")
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

func (a *App) writeTokenResponse(w http.ResponseWriter, xid, refresh string) {
	now := time.Now()
	tok, err := MintJWT(a.cfg.JWTSecret, Claims{
		Iss: a.cfg.BaseURL,
		Sub: "x:" + xid,
		Aud: jwtAudience,
		Iat: now.Unix(),
		Exp: now.Add(accessTokenTTL).Unix(),
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "token mint failed")
		return
	}
	out := map[string]any{
		"account_id":   "x:" + xid,
		"access_token": tok,
		"token_type":   "bearer",
		"expires_in":   int(accessTokenTTL.Seconds()),
	}
	if refresh != "" {
		out["refresh_token"] = refresh
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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
</style>
<h1>Signed in as @{{.Username}}</h1>
<p>Enter this one-time code in your browser to link it to your sync account.
It is valid for 10 minutes and can be used once:</p>
<p><code class="big">{{.Code}}</code></p>
<p>Or link manually from a terminal:</p>
<pre>curl -s -X POST {{.BaseURL}}/device/link \
  -H 'Content-Type: application/json' \
  -d '{"code":"{{.Code}}","device":"my-laptop"}'</pre>
`))
