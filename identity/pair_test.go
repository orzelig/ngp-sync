package main

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	testBase  = "http://id.test"
	testXID   = "4242"
	testPhone = "Pixel 9 · NGP Phone"
)

type testEnv struct {
	t   *testing.T
	app *App
	mux *http.ServeMux
}

// newTestApp is an App with a store in a temp dir, no dynamo, one user with an
// X avatar, and a mux with the routes these tests drive.
func newTestApp(t *testing.T) *testEnv {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUserWithAvatar(testXID, "alice", "https://pbs.example/alice.jpg"); err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:       Config{BaseURL: testBase, JWTSecret: []byte("test-secret")},
		store:     store,
		http:      &http.Client{Timeout: time.Second},
		pending:   map[string]pendingAuth{},
		linkCodes: map[string]linkEntry{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /device/link", app.handleDeviceLink)
	mux.HandleFunc("POST /token", app.handleToken)
	mux.HandleFunc("GET /verify", app.handleVerify)
	mux.HandleFunc("GET /account", app.handleAccount)
	mux.HandleFunc("POST /account/revoke", app.handleRevoke)
	app.registerPairRoutes(mux)
	return &testEnv{t: t, app: app, mux: mux}
}

func (e *testEnv) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) sessionCookie() *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: e.app.signSession(testXID, time.Now().Add(time.Hour).Unix())}
}

// postForm sends a signed-in (unless signedIn is false) form POST from origin.
func (e *testEnv) postForm(path, origin string, form url.Values, signedIn bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if signedIn {
		req.AddCookie(e.sessionCookie())
	}
	return e.do(req)
}

func (e *testEnv) addPhone() *httptest.ResponseRecorder {
	return e.postForm("/account/phone", testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}}, true)
}

func (e *testEnv) codes() []string {
	e.app.mu.Lock()
	defer e.app.mu.Unlock()
	var out []string
	for k := range e.app.linkCodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (e *testEnv) mintedCode() string {
	e.t.Helper()
	rec := e.addPhone()
	if rec.Code != 200 {
		e.t.Fatalf("POST /account/phone: %d %s", rec.Code, rec.Body)
	}
	m := regexp.MustCompile(`<code class="big">([0-9a-f]{8})</code>`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		e.t.Fatalf("no code on the page: %s", rec.Body)
	}
	return m[1]
}

func (e *testEnv) postJSON(path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	return e.do(req)
}

func (e *testEnv) link(code string) map[string]any {
	e.t.Helper()
	rec := e.postJSON("/device/link", map[string]string{"code": code, "device": testPhone})
	if rec.Code != 200 {
		e.t.Fatalf("POST /device/link: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *testEnv) getBearer(path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return e.do(req)
}

func (e *testEnv) accountPage() string {
	req := httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(e.sessionCookie())
	rec := e.do(req)
	if rec.Code != 200 {
		e.t.Fatalf("GET /account: %d", rec.Code)
	}
	return rec.Body.String()
}

// Check 1: the account page has the "Add a phone" form with the CSRF field.
func TestAccountHasAddPhoneForm(t *testing.T) {
	e := newTestApp(t)
	page := e.accountPage()
	form := regexp.MustCompile(`(?s)<form[^>]*action="/account/phone"[^>]*>(.*?)</form>`).FindStringSubmatch(page)
	if form == nil {
		t.Fatal("no form posting to /account/phone")
	}
	if !strings.Contains(form[0], `method="post"`) ||
		!strings.Contains(form[1], `name="csrf" value="`+e.app.csrfToken(testXID)+`"`) ||
		!strings.Contains(form[1], "Add a phone") {
		t.Fatalf("form is missing method, csrf or label: %s", form[0])
	}
}

// Check 2: a signed-in, same-origin POST with the right CSRF mints one code,
// shows it as text and as the QR for <BASE_URL>/pair?code=<code>.
func TestAddPhoneMints(t *testing.T) {
	e := newTestApp(t)
	before := time.Now()
	rec := e.addPhone()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	codes := e.codes()
	if len(codes) != 1 || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(codes[0]) {
		t.Fatalf("link codes = %v, want one 8-hex code", codes)
	}
	code := codes[0]
	entry := e.app.linkCodes[code]
	if entry.XID != testXID {
		t.Errorf("code is for %q, want %q", entry.XID, testXID)
	}
	if entry.Expires.After(time.Now().Add(linkCodeTTL)) || entry.Expires.Before(before.Add(linkCodeTTL)) {
		t.Errorf("code expires %v, want %v after minting", entry.Expires, linkCodeTTL)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<code class="big">`+code+`</code>`) {
		t.Error("code not shown as text")
	}
	m, err := qrEncode(testBase + "/pair?code=" + code)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, string(qrSVG(m))) {
		t.Error("page does not hold the QR for <BASE_URL>/pair?code=<code>")
	}
	if strings.Contains(body, "<script") {
		t.Error("page has a script")
	}
}

// Check 3: no session, a wrong or missing CSRF, or a foreign Origin mint
// nothing.
func TestAddPhoneRejects(t *testing.T) {
	e := newTestApp(t)
	good := url.Values{"csrf": {e.app.csrfToken(testXID)}}
	cases := []struct {
		name     string
		origin   string
		form     url.Values
		signedIn bool
		status   int
	}{
		{"signed out", testBase, good, false, http.StatusFound},
		{"wrong csrf", testBase, url.Values{"csrf": {"nope"}}, true, http.StatusForbidden},
		{"missing csrf", testBase, url.Values{}, true, http.StatusForbidden},
		{"foreign origin", "https://evil.example", good, true, http.StatusForbidden},
		{"no origin", "", good, true, http.StatusForbidden},
	}
	for _, c := range cases {
		rec := e.postForm("/account/phone", c.origin, c.form, c.signedIn)
		if rec.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.status)
		}
		if c.status == http.StatusFound && rec.Header().Get("Location") != "/login" {
			t.Errorf("%s: redirect to %q, want /login", c.name, rec.Header().Get("Location"))
		}
		if n := len(e.codes()); n != 0 {
			t.Fatalf("%s: %d codes minted", c.name, n)
		}
	}
}

// Check 5: GET /pair shows the instruction and leaves the code unspent.
func TestPairDoesNotConsume(t *testing.T) {
	e := newTestApp(t)
	code := e.mintedCode()
	const sentence = "Open NGP on your phone, go to Settings, NGP account, and scan this code there."
	var pages []string
	for _, path := range []string{"/pair?code=" + code, "/pair", "/pair?code=ffffffff"} {
		rec := e.do(httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), sentence) {
			t.Fatalf("%s: instruction missing", path)
		}
		if strings.Contains(rec.Body.String(), code) {
			t.Fatalf("%s: page echoes the code", path)
		}
		pages = append(pages, rec.Body.String())
	}
	if pages[0] != pages[1] || pages[1] != pages[2] {
		t.Error("/pair differs by code; it must not tell valid codes apart")
	}
	if got := e.codes(); len(got) != 1 || got[0] != code {
		t.Fatalf("link codes after /pair = %v, want [%s]", got, code)
	}
}

// Checks 6, 7, 9 and 10: link the phone with a minted code, read /me, see it
// on the account page, remove it.
func TestPhoneLinkMeAndRevoke(t *testing.T) {
	e := newTestApp(t)
	code := e.mintedCode()
	tok := e.link(code)
	for _, k := range []string{"access_token", "refresh_token", "account_id", "token_type", "expires_in"} {
		if _, ok := tok[k]; !ok {
			t.Errorf("/device/link response lacks %q", k)
		}
	}
	if tok["account_id"] != "x:"+testXID {
		t.Errorf("account_id = %v", tok["account_id"])
	}
	if rec := e.postJSON("/device/link", map[string]string{"code": code, "device": testPhone}); rec.Code != 401 {
		t.Errorf("second use of the code: status %d, want 401", rec.Code)
	}
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)

	rec := e.getBearer("/me", access)
	if rec.Code != 200 {
		t.Fatalf("/me: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/me Content-Type = %q", ct)
	}
	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"handle": "alice", "name": "alice", "avatar": testBase + "/avatar/" + testXID}
	if len(me) != len(want) {
		t.Errorf("/me keys = %v, want exactly handle, name, avatar", me)
	}
	for k, v := range want {
		if me[k] != v {
			t.Errorf("/me %s = %v, want %v", k, me[k], v)
		}
	}

	// /verify still answers as before (bearerClaims refactor).
	if rec := e.getBearer("/verify", access); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sub":"x:`+testXID+`"`) {
		t.Errorf("/verify with a valid token: %d %s", rec.Code, rec.Body)
	}
	if rec := e.getBearer("/verify", ""); rec.Code != 401 || !strings.Contains(rec.Body.String(), "missing bearer token") {
		t.Errorf("/verify without a token: %d %s", rec.Code, rec.Body)
	}

	if page := e.accountPage(); !strings.Contains(page, html.EscapeString(testPhone)) {
		t.Fatal("phone not in the device list")
	}
	rec = e.postForm("/account/revoke", testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}, "name": {testPhone}}, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if page := e.accountPage(); strings.Contains(page, html.EscapeString(testPhone)) {
		t.Fatal("phone still in the device list after Remove")
	}
	if rec := e.postJSON("/token", map[string]string{"refresh_token": refresh}); rec.Code != 401 {
		t.Fatalf("refresh after Remove: %d, want 401", rec.Code)
	}
}

// Check 8: /me refuses a missing, garbage, wrong-audience or unknown-account
// token.
func TestMeRejects(t *testing.T) {
	e := newTestApp(t)
	now := time.Now()
	claims := func(aud, sub string) string {
		tok, err := MintJWT(e.app.cfg.JWTSecret, Claims{Iss: testBase, Sub: sub, Aud: aud, Iat: now.Unix(), Exp: now.Add(time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	otherSecret, _ := MintJWT([]byte("other"), Claims{Sub: "x:" + testXID, Aud: jwtAudience, Exp: now.Add(time.Minute).Unix()})
	expired, _ := MintJWT(e.app.cfg.JWTSecret, Claims{Sub: "x:" + testXID, Aud: jwtAudience, Exp: now.Add(-time.Minute).Unix()})
	cases := map[string]string{
		"missing":        "",
		"garbage":        "not.a.jwt",
		"wrong audience": claims("something-else", "x:"+testXID),
		"wrong secret":   otherSecret,
		"expired":        expired,
		"unknown user":   claims(jwtAudience, "x:nobody"),
	}
	for name, tok := range cases {
		rec := e.getBearer("/me", tok)
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// /me returns an empty avatar for a user without one, and a dev avatar as is.
func TestMeAvatarRule(t *testing.T) {
	e := newTestApp(t)
	_ = e.app.store.UpsertUser("7", "noavatar")
	_ = e.app.store.UpsertUserWithAvatar("8", "dev", testBase+"/dev/avatar.png")
	for xid, want := range map[string]string{"7": "", "8": testBase + "/dev/avatar.png"} {
		tok, _ := e.app.mintAccessToken(xid, time.Minute)
		var me map[string]string
		_ = json.Unmarshal(e.getBearer("/me", tok).Body.Bytes(), &me)
		if got, ok := me["avatar"]; !ok || got != want {
			t.Errorf("xid %s: avatar = %q (present %v), want %q", xid, got, ok, want)
		}
	}
}
