package main

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Phone codes are confirmed before sync access (ADR 0006). The numbers in the
// comments are the design's acceptance criteria on ngp-sync#18.

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return out
}

// redeemPending spends a phone code at /device/link and returns the poll ID.
func (e *testEnv) redeemPending(code, device string) string {
	e.t.Helper()
	rec := e.postJSON("/device/link", map[string]string{"code": code, "device": device})
	if rec.Code != http.StatusAccepted {
		e.t.Fatalf("POST /device/link with a phone code: %d %s", rec.Code, rec.Body)
	}
	pollID, _ := decodeBody(e.t, rec)["poll_id"].(string)
	return pollID
}

func (e *testEnv) actOn(action, id string) *httptest.ResponseRecorder {
	return e.postForm("/account/device/"+action, testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}, "id": {id}}, true)
}

func (e *testEnv) confirm(id string) {
	e.t.Helper()
	if rec := e.actOn("confirm", id); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" {
		e.t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
}

func (e *testEnv) poll(pollID string) *httptest.ResponseRecorder {
	return e.postJSON("/device/link/poll", map[string]string{"poll_id": pollID})
}

func (e *testEnv) pollConfirmed(pollID string) map[string]any {
	e.t.Helper()
	rec := e.poll(pollID)
	if rec.Code != 200 {
		e.t.Fatalf("poll: %d %s", rec.Code, rec.Body)
	}
	out := decodeBody(e.t, rec)
	if out["status"] != "confirmed" {
		e.t.Fatalf("poll status %v, want confirmed: %s", out["status"], rec.Body)
	}
	return out
}

func (e *testEnv) devices(xid string) int { return len(e.app.store.ListDevices(xid)) }

func (e *testEnv) pendingEntry(pollID string) (pendingLink, bool) {
	e.app.mu.Lock()
	defer e.app.mu.Unlock()
	p, ok := e.app.pendingLinks[hashToken(pollID)]
	return p, ok
}

func assertPollStatus(t *testing.T, rec *httptest.ResponseRecorder, code int, status string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("poll: %d %s, want %d", rec.Code, rec.Body, code)
	}
	out := decodeBody(t, rec)
	if out["status"] != status {
		t.Fatalf("poll status %v, want %q", out["status"], status)
	}
	if _, ok := out["refresh_token"]; ok && status != "confirmed" {
		t.Fatalf("a %s poll carries a refresh token", status)
	}
}

// 1: a phone code redeems to a pending request and issues nothing.
func TestPhoneCodeRedeemsPending(t *testing.T) {
	e := newTestApp(t)
	code := e.mintedCode()
	rec := e.postJSON("/device/link", map[string]any{"code": code, "device": testPhone, "ttl_days": 90})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d %s, want 202", rec.Code, rec.Body)
	}
	out := decodeBody(t, rec)
	if out["status"] != "pending" || out["expires_in"] != float64(300) || out["interval"] != float64(2) {
		t.Errorf("body %v", out)
	}
	if id, _ := out["poll_id"].(string); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Errorf("poll_id %q, want 64 hex", id)
	}
	for _, k := range []string{"refresh_token", "access_token"} {
		if _, ok := out[k]; ok {
			t.Errorf("pending response has %s", k)
		}
	}
	if n := e.devices(testXID); n != 0 {
		t.Errorf("%d refresh tokens issued, want 0", n)
	}
	if rec := e.postJSON("/device/link", map[string]string{"code": code}); rec.Code != 401 {
		t.Errorf("the code was not spent: %d", rec.Code)
	}
}

// 2 and 13: a browser code still answers with the token at once.
func TestBrowserCodeUnchanged(t *testing.T) {
	e := newTestApp(t)
	rec := e.postJSON("/device/link", map[string]any{"code": e.app.mintLinkCode(testXID), "device": "my-laptop", "ttl_days": 30})
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	out := decodeBody(t, rec)
	for _, k := range []string{"refresh_token", "access_token"} {
		if _, ok := out[k]; !ok {
			t.Errorf("no %s", k)
		}
	}
	if _, ok := out["status"]; ok {
		t.Error("browser response has a status field")
	}
	if out["expires_in"] != (30 * 24 * time.Hour).Seconds() {
		t.Errorf("expires_in %v, want 30 days", out["expires_in"])
	}
	if n := len(e.app.pendingLinks); n != 0 {
		t.Errorf("%d pending requests for a browser code", n)
	}
	if n := e.devices(testXID); n != 1 {
		t.Errorf("%d devices, want 1", n)
	}
}

// 3 and 7: the poll answers pending, then confirmed with a token /token
// accepts, once; the device is listed from Confirm on.
func TestPollPendingConfirmedOnce(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	assertPollStatus(t, e.poll(pollID), 200, "pending")
	assertPollStatus(t, e.poll(pollID), 200, "pending")

	e.confirm(hashToken(pollID))
	devs := e.app.store.ListDevices(testXID)
	if len(devs) != 1 || devs[0].Device != testPhone {
		t.Fatalf("devices after Confirm: %+v", devs)
	}
	if page := e.accountPage(); !strings.Contains(page, html.EscapeString(testPhone)) || !strings.Contains(page, "Devices (1)") {
		t.Error("confirmed phone not in the device list")
	}

	out := e.pollConfirmed(pollID)
	refresh, _ := out["refresh_token"].(string)
	if hashToken(refresh) != devs[0].ID {
		t.Error("the poll's refresh token is not the confirmed device's")
	}
	for _, k := range []string{"account_id", "access_token", "token_type", "expires_in", "username", "avatar_url"} {
		if _, ok := out[k]; !ok {
			t.Errorf("confirmed poll lacks %s", k)
		}
	}
	if out["expires_in"] != accessTokenTTL.Seconds() {
		t.Errorf("expires_in %v", out["expires_in"])
	}
	if rec := e.postJSON("/token", map[string]string{"refresh_token": refresh}); rec.Code != 200 {
		t.Errorf("/token with the polled refresh token: %d", rec.Code)
	}
	if rec := e.poll(pollID); rec.Code != 401 {
		t.Errorf("second collection: %d, want 401", rec.Code)
	}
}

// 3: an unknown or empty poll ID, and a pending ID in place of the poll ID,
// are refused.
func TestPollUnknown(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	for _, id := range []string{"", strings.Repeat("0", 64), hashToken(pollID)} {
		if rec := e.poll(id); rec.Code != 401 || !strings.Contains(rec.Body.String(), "unknown poll id") {
			t.Errorf("poll %q: %d %s", id, rec.Code, rec.Body)
		}
	}
	if rec := e.postJSON("/device/link/poll", "not an object"); rec.Code != 400 {
		t.Errorf("bad body: %d", rec.Code)
	}
}

// 8: Cancel refuses the poll and nothing is ever issued.
func TestCancel(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	if rec := e.actOn("cancel", hashToken(pollID)); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" {
		t.Fatalf("cancel: %d", rec.Code)
	}
	assertPollStatus(t, e.poll(pollID), 403, "cancelled")
	e.confirm(hashToken(pollID))
	assertPollStatus(t, e.poll(pollID), 403, "cancelled")
	if n := e.devices(testXID); n != 0 {
		t.Errorf("%d devices after Cancel, want 0", n)
	}
	if page := e.accountPage(); strings.Contains(page, html.EscapeString(testPhone)) {
		t.Error("cancelled request still on the page")
	}
}

// 4: a pending request older than 5 minutes polls cancelled, is off the page,
// and Confirm issues nothing.
func TestPendingExpires(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	e.app.mu.Lock()
	p := e.app.pendingLinks[hashToken(pollID)]
	if got := p.Expires.Sub(p.Requested); got != pendingLinkTTL || pendingLinkTTL != 5*time.Minute {
		t.Errorf("lifetime %v", got)
	}
	p.Requested = time.Now().Add(-pendingLinkTTL - time.Second)
	p.Expires = p.Requested.Add(pendingLinkTTL)
	e.app.pendingLinks[hashToken(pollID)] = p
	e.app.mu.Unlock()

	assertPollStatus(t, e.poll(pollID), 403, "cancelled")
	if page := e.accountPage(); strings.Contains(page, "/account/device/confirm") {
		t.Error("expired request on the page")
	}
	e.confirm(hashToken(pollID))
	if n := e.devices(testXID); n != 0 {
		t.Errorf("Confirm on an expired request issued %d", n)
	}
	assertPollStatus(t, e.poll(pollID), 403, "cancelled")

	// After its keep time the reaper deletes it and the poll ID is unknown.
	e.app.mu.Lock()
	e.app.reapPendingLinksLocked(time.Now().Add(pendingLinkTTL))
	e.app.mu.Unlock()
	if rec := e.poll(pollID); rec.Code != 401 {
		t.Errorf("reaped request: %d, want 401", rec.Code)
	}
}

// 5: the page shows the account's pending requests with the forms, never the
// poll ID, and not another account's.
func TestAccountPageListsPending(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	blank := e.redeemPending(e.mintedCode(), "  ")
	_ = e.app.store.UpsertUser("other", "bob")
	otherPoll := e.redeemPending(e.app.mintPhoneCode("other"), "Bob's phone")

	page := e.accountPage()
	if strings.Contains(page, pollID) || strings.Contains(page, otherPoll) || strings.Contains(page, blank) {
		t.Fatal("the page contains a poll ID")
	}
	if strings.Contains(page, html.EscapeString("Bob's phone")) || strings.Contains(page, hashToken(otherPoll)) {
		t.Fatal("another account's pending request is on the page")
	}
	if !strings.Contains(page, html.EscapeString(testPhone)) || !strings.Contains(page, "NGP Phone") || !strings.Contains(page, "Requested just now") {
		t.Errorf("pending names or times missing")
	}
	csrf := `name="csrf" value="` + e.app.csrfToken(testXID) + `"`
	for _, id := range []string{hashToken(pollID), hashToken(blank)} {
		for _, action := range []string{"confirm", "cancel"} {
			re := regexp.MustCompile(`(?s)<form[^>]*action="/account/device/` + action + `"[^>]*>(.*?)</form>`)
			found := false
			for _, m := range re.FindAllStringSubmatch(page, -1) {
				if strings.Contains(m[1], `name="id" value="`+id+`"`) && strings.Contains(m[1], csrf) {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s form with the CSRF token for %s", action, id)
			}
		}
	}
	if got := requestedAgo(3*time.Minute + 10*time.Second); got != "Requested 3 min ago" {
		t.Errorf("requestedAgo = %q", got)
	}
}

// 6: Confirm and Cancel need the session, the origin and the CSRF token, and
// do nothing to another account's request.
func TestConfirmCancelRefusals(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	id := hashToken(pollID)
	good := url.Values{"csrf": {e.app.csrfToken(testXID)}, "id": {id}}
	for _, action := range []string{"confirm", "cancel"} {
		path := "/account/device/" + action
		if rec := e.postForm(path, testBase, good, false); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s signed out: %d %q", action, rec.Code, rec.Header().Get("Location"))
		}
		if rec := e.postForm(path, "https://evil.example", good, true); rec.Code != http.StatusForbidden {
			t.Errorf("%s bad origin: %d", action, rec.Code)
		}
		if rec := e.postForm(path, testBase, url.Values{"csrf": {"nope"}, "id": {id}}, true); rec.Code != http.StatusForbidden {
			t.Errorf("%s bad csrf: %d", action, rec.Code)
		}
	}
	assertPollStatus(t, e.poll(pollID), 200, "pending")

	// Another account's request: the redirect looks like success, nothing changes.
	_ = e.app.store.UpsertUser("other", "bob")
	otherPoll := e.redeemPending(e.app.mintPhoneCode("other"), "Bob's phone")
	for _, action := range []string{"confirm", "cancel"} {
		if rec := e.actOn(action, hashToken(otherPoll)); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account" {
			t.Errorf("%s other account: %d", action, rec.Code)
		}
	}
	assertPollStatus(t, e.poll(otherPoll), 200, "pending")
	if n := e.devices("other") + e.devices(testXID); n != 0 {
		t.Errorf("%d devices issued", n)
	}
	if rec := e.actOn("confirm", "unknown"); rec.Code != http.StatusSeeOther {
		t.Errorf("unknown id: %d", rec.Code)
	}
}

// 9: a confirmed request nobody collects has its device removed when reaped.
func TestReaperRevokesUncollected(t *testing.T) {
	e := newTestApp(t)
	kept := e.redeemPending(e.mintedCode(), "kept")
	e.confirm(hashToken(kept))
	e.pollConfirmed(kept)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	e.confirm(hashToken(pollID))
	if n := e.devices(testXID); n != 2 {
		t.Fatalf("%d devices, want 2", n)
	}
	e.app.mu.Lock()
	e.app.reapPendingLinksLocked(time.Now().Add(pendingLinkTTL)) // still in its keep time
	e.app.mu.Unlock()
	if n := e.devices(testXID); n != 2 {
		t.Fatalf("reaped early: %d devices", n)
	}
	e.app.mu.Lock()
	e.app.reapPendingLinksLocked(time.Now().Add(2*pendingLinkTTL + time.Second))
	left := len(e.app.pendingLinks)
	e.app.mu.Unlock()
	devs := e.app.store.ListDevices(testXID)
	if len(devs) != 1 || devs[0].Device != "kept" || left != 0 {
		t.Errorf("after the reap: devices %+v, %d entries left", devs, left)
	}
	if rec := e.poll(pollID); rec.Code != 401 {
		t.Errorf("reaped poll: %d", rec.Code)
	}
}

// 10: Remove all devices and Log out cancel the account's pending requests,
// confirmed-but-uncollected ones included, and only that account's.
func TestLogoutAndRevokeAllCancelPending(t *testing.T) {
	for _, path := range []string{"/account/logout", "/account/revoke-all"} {
		e := newTestApp(t)
		pending := e.redeemPending(e.mintedCode(), testPhone)
		confirmed := e.redeemPending(e.mintedCode(), "confirmed")
		e.confirm(hashToken(confirmed))
		_ = e.app.store.UpsertUser("other", "bob")
		otherPoll := e.redeemPending(e.app.mintPhoneCode("other"), "Bob's phone")

		if rec := e.postForm(path, testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}}, true); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		assertPollStatus(t, e.poll(pending), 403, "cancelled")
		assertPollStatus(t, e.poll(confirmed), 403, "cancelled")
		assertPollStatus(t, e.poll(otherPoll), 200, "pending")
		if n := e.devices(testXID); n != 0 {
			t.Errorf("%s: %d devices left", path, n)
		}
		e.confirm(hashToken(pending))
		if n := e.devices(testXID); n != 0 {
			t.Errorf("%s: Confirm after cancel issued", path)
		}
	}
}

// A device removed between Confirm and the poll hands out nothing.
func TestPollAfterRemoveIsCancelled(t *testing.T) {
	e := newTestApp(t)
	pollID := e.redeemPending(e.mintedCode(), testPhone)
	e.confirm(hashToken(pollID))
	devs := e.app.store.ListDevices(testXID)
	if len(devs) != 1 {
		t.Fatalf("devices %+v", devs)
	}
	rec := e.postForm("/account/revoke", testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}, "id": {devs[0].ID}}, true)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	assertPollStatus(t, e.poll(pollID), 403, "cancelled")
}

// 11: same-name devices are two rows; Remove by id removes one; another
// account's id removes nothing.
func TestSameNameDevicesByID(t *testing.T) {
	e := newTestApp(t)
	first, _ := e.app.store.IssueRefreshToken(testXID, "Laptop")
	second, _ := e.app.store.IssueRefreshToken(testXID, "laptop ")
	other, _ := e.app.store.IssueRefreshToken("other", "Laptop")

	page := e.accountPage()
	if !strings.Contains(page, "Devices (2)") {
		t.Fatalf("same-name devices not shown as two rows")
	}
	for _, tok := range []string{first, second} {
		if !strings.Contains(page, `name="id" value="`+hashToken(tok)+`"`) {
			t.Errorf("no Remove form for %s", hashToken(tok))
		}
	}
	if strings.Contains(page, `name="name"`) {
		t.Error("a form still posts a device name")
	}

	revoke := func(id string) {
		t.Helper()
		if rec := e.postForm("/account/revoke", testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}, "id": {id}}, true); rec.Code != http.StatusSeeOther {
			t.Fatalf("revoke: %d", rec.Code)
		}
	}
	revoke(hashToken(other))
	if _, ok := e.app.store.LookupRefreshToken(other); !ok {
		t.Error("another account's device was removed")
	}
	revoke(hashToken(first))
	if _, ok := e.app.store.LookupRefreshToken(first); ok {
		t.Error("the removed device still works")
	}
	if _, ok := e.app.store.LookupRefreshToken(second); !ok {
		t.Error("removing one same-name device removed the other")
	}
	if !strings.Contains(e.accountPage(), "Devices (1)") {
		t.Error("page does not show one device left")
	}
	// A form without id, as the old page posted, removes nothing.
	if rec := e.postForm("/account/revoke", testBase, url.Values{"csrf": {e.app.csrfToken(testXID)}, "name": {"laptop"}}, true); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke by name: %d", rec.Code)
	}
	if e.devices(testXID) != 1 {
		t.Error("a name removed a device")
	}
}
