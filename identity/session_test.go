package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// sameOrigin compares the exact origin, never a prefix (ADR 0006).
func TestSameOriginExact(t *testing.T) {
	a := &App{cfg: Config{BaseURL: "https://id.ngp.one"}}
	cases := []struct {
		name, origin, referer string
		want                  bool
	}{
		{"exact origin", "https://id.ngp.one", "", true},
		{"prefix lookalike", "https://id.ngp.one.evil.com", "", false},
		{"other port", "https://id.ngp.one:8443", "", false},
		{"other scheme", "http://id.ngp.one", "", false},
		{"null", "null", "", false},
		{"origin with a path", "https://id.ngp.one/account", "", false},
		{"bad origin, good referer", "https://evil.example", "https://id.ngp.one/account", false},
		{"referer only", "", "https://id.ngp.one/account", true},
		{"referer upper-case host", "", "https://ID.NGP.ONE/account", true},
		{"referer lookalike", "", "https://id.ngp.one.evil.com/account", false},
		{"referer other port", "", "https://id.ngp.one:8443/account", false},
		{"referer with userinfo", "", "https://id.ngp.one@evil.com/", false},
		{"referer unparseable", "", "://id.ngp.one", false},
		{"neither", "", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/account/revoke", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.referer != "" {
			r.Header.Set("Referer", c.referer)
		}
		if got := a.sameOrigin(r); got != c.want {
			t.Errorf("%s: sameOrigin = %v, want %v", c.name, got, c.want)
		}
	}

	// A base URL with a port and a trailing path still compares scheme and host.
	local := &App{cfg: Config{BaseURL: "http://localhost:8300"}}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Origin", "http://localhost:8300")
	if !local.sameOrigin(r) {
		t.Error("dev base origin refused")
	}
	r.Header.Set("Origin", "http://localhost:83001")
	if local.sameOrigin(r) {
		t.Error("dev base port prefix accepted")
	}
}

// A state-changing POST from the lookalike origin is refused and changes
// nothing.
func TestPostAuthRefusesLookalikeOrigin(t *testing.T) {
	e := newTestApp(t)
	e.app.cfg.BaseURL = "https://id.ngp.one"
	tok, _ := e.app.store.IssueRefreshToken(testXID, "Laptop")
	form := url.Values{"csrf": {e.app.csrfToken(testXID)}}
	if rec := e.postForm("/account/revoke-all", "https://id.ngp.one.evil.com", form, true); rec.Code != http.StatusForbidden {
		t.Errorf("lookalike origin: %d, want 403", rec.Code)
	}
	if _, ok := e.app.store.LookupRefreshToken(tok); !ok {
		t.Fatal("the lookalike origin removed a device")
	}
	if rec := e.postForm("/account/revoke-all", "https://id.ngp.one", form, true); rec.Code != http.StatusSeeOther {
		t.Errorf("exact origin: %d, want 303", rec.Code)
	}
}

// /login keeps only a safe same-site next path: a browser reads "/\evil.com"
// as "//evil.com", so it and its kin are dropped (no open redirect).
func TestLoginNextIsNotAnOpenRedirect(t *testing.T) {
	cases := map[string]string{
		"/account":               "/account",
		"/account/phone":         "/account/phone",
		"":                       "",
		"/\\evil.com":            "",
		"\\/evil.com":            "",
		"/\\/evil.com":           "",
		"//evil.com":             "",
		"///evil.com":            "",
		"https://evil.com":       "",
		"evil.com":               "",
		"/%2f%2fevil.com":        "",
		"/%5cevil.com":           "",
		"/\tevil.com":            "",
		"/account?x=//evil.com":  "",
		"/account#//evil.com":    "",
		"/..//evil.com":          "",
		"/account\\..\\evil.com": "",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}

	e := newTestApp(t)
	e.app.cfg.XClientID, e.app.cfg.XClientSecret = "cid", "csecret"
	for in, want := range map[string]string{"/\\evil.com": "", "/account/phone": "/account/phone"} {
		e.app.mu.Lock()
		e.app.pending = map[string]pendingAuth{}
		e.app.mu.Unlock()
		rec := e.do(httptest.NewRequest("GET", "/login?next="+url.QueryEscape(in), nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("/login next=%q: %d", in, rec.Code)
		}
		e.app.mu.Lock()
		got := "unset"
		for _, p := range e.app.pending {
			got = p.Next
		}
		e.app.mu.Unlock()
		if got != want {
			t.Errorf("/login next=%q stored %q, want %q", in, got, want)
		}
	}
}
