package main

import (
	"encoding/json"
	"fmt"
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
	mux.HandleFunc("POST /account/revoke-all", app.handleRevokeAll)
	mux.HandleFunc("POST /account/logout", app.handleLogout)
	mux.HandleFunc("GET /login", app.handleLogin)
	app.registerPairRoutes(mux)
	return &testEnv{t: t, app: app, mux: mux}
}

func (e *testEnv) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// sessionCookie is the cookie an X login sets now: it expires sessionTTL later.
func (e *testEnv) sessionCookie() *http.Cookie {
	return e.sessionCookieAt(time.Now())
}

// sessionCookieAt is the cookie an X login at loginAt set.
func (e *testEnv) sessionCookieAt(loginAt time.Time) *http.Cookie {
	return &http.Cookie{Name: sessionCookieName, Value: e.app.signSession(testXID, loginAt.Add(sessionTTL).Unix())}
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

// ---- Round 2 (zelig on #17): one phone code per account, recent login,
// codes dropped on log out and remove all, phone lifetime, failure limit.

func (e *testEnv) entry(code string) (linkEntry, bool) {
	e.app.mu.Lock()
	defer e.app.mu.Unlock()
	v, ok := e.app.linkCodes[code]
	return v, ok
}

// A new Add a phone replaces the account's earlier phone code; a browser code
// of the same account and another account's phone code stay.
func TestPhoneCodeReplacesEarlier(t *testing.T) {
	e := newTestApp(t)
	browser := e.app.mintLinkCode(testXID)
	other := e.app.mintPhoneCode("other")
	first := e.mintedCode()
	second := e.mintedCode()
	if first == second {
		t.Fatalf("same code twice: %s", first)
	}
	if _, ok := e.entry(first); ok {
		t.Errorf("earlier phone code %s is still live", first)
	}
	if v, ok := e.entry(second); !ok || !v.Phone || v.XID != testXID {
		t.Errorf("new phone code %s: %+v %v", second, v, ok)
	}
	if v, ok := e.entry(browser); !ok || v.Phone {
		t.Errorf("browser code %s: %+v %v", browser, v, ok)
	}
	if v, ok := e.entry(other); !ok || v.XID != "other" {
		t.Errorf("other account's code %s: %+v %v", other, v, ok)
	}
	if got := e.codes(); len(got) != 3 {
		t.Errorf("codes = %v, want 3", got)
	}
	if rec := e.postJSON("/device/link", map[string]string{"code": first, "device": testPhone}); rec.Code != 401 {
		t.Errorf("replaced code redeemed: %d %s", rec.Code, rec.Body)
	}
}

// A random collision draws again and never overwrites another account's code.
func TestMintCodeCollisionDrawsAgain(t *testing.T) {
	e := newTestApp(t)
	taken := linkEntry{XID: "other", Expires: time.Now().Add(linkCodeTTL), Phone: true}
	e.app.linkCodes["aaaaaaaa"] = taken
	draws := []string{"aaaaaaaa", "aaaaaaaa", "bbbbbbbb"}
	saved := newLinkCode
	t.Cleanup(func() { newLinkCode = saved })
	newLinkCode = func() string { c := draws[0]; draws = draws[1:]; return c }

	if got := e.app.mintPhoneCode(testXID); got != "bbbbbbbb" {
		t.Errorf("minted %q, want the third draw bbbbbbbb", got)
	}
	if v, _ := e.entry("aaaaaaaa"); v != taken {
		t.Errorf("other account's code overwritten: %+v", v)
	}
	draws = []string{"bbbbbbbb", "cccccccc"}
	if got := e.app.mintLinkCode("third"); got != "cccccccc" {
		t.Errorf("browser mint %q, want cccccccc", got)
	}
	if v, _ := e.entry("bbbbbbbb"); v.XID != testXID {
		t.Errorf("phone code overwritten by a browser mint: %+v", v)
	}
}

// Minting drops expired codes, so linkCodes cannot grow without bound.
func TestMintDropsExpiredCodes(t *testing.T) {
	e := newTestApp(t)
	past := time.Now().Add(-time.Second)
	for i, xid := range []string{"a", "b", "c"} {
		e.app.linkCodes[strings.Repeat(string(rune('0'+i)), 8)] = linkEntry{XID: xid, Expires: past}
	}
	code := e.mintedCode()
	if got := e.codes(); len(got) != 1 || got[0] != code {
		t.Errorf("codes = %v, want only %s", got, code)
	}
}

// Add a phone mints only within 15 minutes of an X login; otherwise it sends
// the person through X login and back to Add a phone.
func TestAddPhoneNeedsRecentLogin(t *testing.T) {
	e := newTestApp(t)
	e.app.cfg.XClientID, e.app.cfg.XClientSecret = "cid", "csecret"
	csrf := url.Values{"csrf": {e.app.csrfToken(testXID)}}

	post := func(loginAt time.Time) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/account/phone", strings.NewReader(csrf.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", testBase)
		req.AddCookie(e.sessionCookieAt(loginAt))
		return e.do(req)
	}
	get := func(path string, loginAt time.Time) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(e.sessionCookieAt(loginAt))
		return e.do(req)
	}

	stale := time.Now().Add(-16 * time.Minute)
	if rec := post(stale); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != addPhoneLogin {
		t.Errorf("stale POST: %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := e.codes(); len(got) != 0 {
		t.Fatalf("a stale session minted %v", got)
	}
	// A 30-day cookie set 29 days ago is good for the account page, not for minting.
	if rec := post(time.Now().Add(-29 * 24 * time.Hour)); rec.Code != http.StatusSeeOther || len(e.codes()) != 0 {
		t.Errorf("old session: %d, codes %v", rec.Code, e.codes())
	}

	// /login?reauth=1 goes to X even with a session; plain /login still goes to /account.
	if rec := get(addPhoneLogin, stale); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), xAuthorizeURL) {
		t.Errorf("reauth login: %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
	e.app.mu.Lock()
	var next string
	for _, p := range e.app.pending {
		next = p.Next
	}
	e.app.mu.Unlock()
	if next != "/account/phone" {
		t.Errorf("after X login the person goes to %q, want /account/phone", next)
	}
	if rec := get("/login", stale); rec.Header().Get("Location") != "/account" {
		t.Errorf("plain /login with a session: Location %q", rec.Header().Get("Location"))
	}

	// Back from X: GET /account/phone shows one CSRF form and mints nothing.
	rec := get("/account/phone", time.Now())
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `action="/account/phone"`) || !strings.Contains(body, html.EscapeString(e.app.csrfToken(testXID))) {
		t.Errorf("GET /account/phone after login: %d %s", rec.Code, body)
	}
	if got := e.codes(); len(got) != 0 {
		t.Errorf("GET minted %v", got)
	}
	if rec := get("/account/phone", stale); rec.Code != http.StatusFound || rec.Header().Get("Location") != addPhoneLogin {
		t.Errorf("stale GET: %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
	signedOut := e.do(httptest.NewRequest("GET", "/account/phone", nil))
	if signedOut.Code != http.StatusFound || !strings.HasPrefix(signedOut.Header().Get("Location"), "/login") {
		t.Errorf("signed-out GET: %d Location %q", signedOut.Code, signedOut.Header().Get("Location"))
	}

	// Within the window the POST mints.
	if rec := post(time.Now().Add(-14 * time.Minute)); rec.Code != 200 || len(e.codes()) != 1 {
		t.Errorf("recent POST: %d, codes %v", rec.Code, e.codes())
	}
}

// Log out and Remove all devices delete the account's live codes, and only
// that account's.
func TestLogoutAndRevokeAllDropCodes(t *testing.T) {
	for _, path := range []string{"/account/logout", "/account/revoke-all"} {
		e := newTestApp(t)
		phone := e.mintedCode()
		browser := e.app.mintLinkCode(testXID)
		other := e.app.mintPhoneCode("other")
		rec := e.postForm(path, testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}}, true)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		for _, c := range []string{phone, browser} {
			if _, ok := e.entry(c); ok {
				t.Errorf("%s: code %s is still live", path, c)
			}
		}
		if _, ok := e.entry(other); !ok {
			t.Errorf("%s: dropped another account's code", path)
		}
		if rec := e.postJSON("/device/link", map[string]string{"code": phone, "device": testPhone}); rec.Code != 401 {
			t.Errorf("%s: phone code redeemed afterwards: %d", path, rec.Code)
		}
	}
}

// /device/link ignores ttl_days for a phone code; a browser code keeps it.
func TestPhoneCodeIgnoresTTLDays(t *testing.T) {
	e := newTestApp(t)
	expiresIn := func(code string) float64 {
		rec := e.postJSON("/device/link", map[string]any{"code": code, "device": testPhone, "ttl_days": 90})
		if rec.Code != 200 {
			t.Fatalf("link: %d %s", rec.Code, rec.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		tok, _ := out["access_token"].(string)
		claims, err := VerifyJWT(e.app.cfg.JWTSecret, tok)
		if err != nil {
			t.Fatal(err)
		}
		exp, _ := out["expires_in"].(float64)
		if got := float64(claims.Exp - claims.Iat); got != exp {
			t.Errorf("token lifetime %vs disagrees with expires_in %v", got, exp)
		}
		return exp
	}
	if got := expiresIn(e.mintedCode()); got != accessTokenTTL.Seconds() {
		t.Errorf("phone code with ttl_days 90: expires_in %v, want %v", got, accessTokenTTL.Seconds())
	}
	if got := expiresIn(e.app.mintLinkCode(testXID)); got != (90 * 24 * time.Hour).Seconds() {
		t.Errorf("browser code with ttl_days 90: expires_in %v, want 90 days", got)
	}
}

// /device/link answers 429 after 10 failed codes a minute from one client,
// without spending a valid code; other clients are not affected.
func TestDeviceLinkFailureLimit(t *testing.T) {
	e := newTestApp(t)
	try := func(remote, xff, code string) int {
		b, _ := json.Marshal(map[string]string{"code": code, "device": "x"})
		req := httptest.NewRequest("POST", "/device/link", strings.NewReader(string(b)))
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return e.do(req).Code
	}
	const attacker = "203.0.113.7:4000"
	for i := 0; i < linkFailLimit; i++ {
		if got := try(attacker, "", "00000000"); got != 401 {
			t.Fatalf("failure %d: %d, want 401", i+1, got)
		}
	}
	code := e.mintedCode()
	if got := try(attacker, "", code); got != http.StatusTooManyRequests {
		t.Errorf("after %d failures: %d, want 429", linkFailLimit, got)
	}
	if _, ok := e.entry(code); !ok {
		t.Fatal("a refused attempt spent the code")
	}
	// A forged X-Forwarded-For from a public peer does not change the client.
	if got := try(attacker, "198.51.100.1", code); got != http.StatusTooManyRequests {
		t.Errorf("forged X-Forwarded-For: %d, want 429", got)
	}
	// Behind Caddy (a private peer) the client is the rightmost X-Forwarded-For entry.
	if got := try("172.18.0.5:5000", "1.2.3.4, 203.0.113.7", code); got != http.StatusTooManyRequests {
		t.Errorf("same client through Caddy: %d, want 429", got)
	}
	// Another client is unaffected and can redeem.
	if got := try("172.18.0.5:5000", "203.0.113.7, 198.51.100.9", code); got != 200 {
		t.Errorf("other client: %d, want 200", got)
	}
	// One IPv6 /64 is one client.
	for i := 0; i < linkFailLimit; i++ {
		try(fmt.Sprintf("[2001:db8:1:2::%x]:443", i+1), "", "00000000")
	}
	if got := try("[2001:db8:1:2::ffff]:443", "", "00000000"); got != http.StatusTooManyRequests {
		t.Errorf("IPv6 /64: %d, want 429", got)
	}
	// The window ends: the janitor's sweep forgets it, and the client may try again.
	e.app.mu.Lock()
	for k, v := range e.app.linkFails {
		v.Start = v.Start.Add(-linkFailWindow)
		e.app.linkFails[k] = v
	}
	e.app.dropExpiredLocked(time.Now())
	left := len(e.app.linkFails)
	e.app.mu.Unlock()
	if left != 0 {
		t.Errorf("%d failure windows left after the sweep", left)
	}
	if got := try(attacker, "", "00000000"); got != 401 {
		t.Errorf("after the window: %d, want 401", got)
	}
}

// The account page claims no more about sync data than privacy.html does.
func TestAccountSyncNoteMatchesPrivacy(t *testing.T) {
	for _, v := range []accountView{
		{Username: "alice", SyncOK: true, SyncTotal: 1, Synced: []TypeCount{{Name: "Bookmarks", Count: 1}}},
		{Username: "alice", SyncOK: false},
	} {
		var b strings.Builder
		if err := accountTmpl.Execute(&b, v); err != nil {
			t.Fatal(err)
		}
		page := b.String()
		for _, bad := range []string{"readable only", "at rest"} {
			if strings.Contains(page, bad) {
				t.Errorf("SyncOK=%v: the page says %q", v.SyncOK, bad)
			}
		}
		for _, want := range []string{"encrypted in transit", "With a sync passphrase set in the browser, we can&rsquo;t read it."} {
			if !strings.Contains(page, want) {
				t.Errorf("SyncOK=%v: the page lacks %q", v.SyncOK, want)
			}
		}
	}
}
