package main

// Stateless signed-cookie web session for the account page. The cookie itself
// is the session (HMAC-signed with the shared secret); no server storage. This
// is separate from the sync access JWTs — it only gates the id.ngp.one account
// UI, never the sync server.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "ngp_session"
	sessionTTL        = 30 * 24 * time.Hour
)

func (a *App) sessionSig(msg string) string {
	mac := hmac.New(sha256.New, a.cfg.JWTSecret)
	mac.Write([]byte("ngp-web-session|" + msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signSession returns "<b64url(msg)>.<b64url(sig)>", msg = "<xid>|<exp-unix>".
func (a *App) signSession(xid string, exp int64) string {
	msg := xid + "|" + strconv.FormatInt(exp, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(msg)) + "." + a.sessionSig(msg)
}

// secureCookies is true in production (https base URL); false lets local http
// dev set the cookie.
func (a *App) secureCookies() bool { return strings.HasPrefix(a.cfg.BaseURL, "https://") }

func (a *App) setSession(w http.ResponseWriter, xid string) {
	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    a.signSession(xid, exp),
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(exp, 0),
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (a *App) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sessionXID returns the signed-in X id from the session cookie, or "" if it is
// missing, tampered with, or expired.
func (a *App) sessionXID(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	dot := strings.IndexByte(c.Value, '.')
	if dot < 0 {
		return ""
	}
	msgB, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		return ""
	}
	msg := string(msgB)
	if !hmac.Equal([]byte(a.sessionSig(msg)), []byte(c.Value[dot+1:])) {
		return ""
	}
	bar := strings.IndexByte(msg, '|')
	if bar < 0 {
		return ""
	}
	exp, err := strconv.ParseInt(msg[bar+1:], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	return msg[:bar]
}

// csrfToken is a stable per-account token tied to the signing secret, embedded
// in account forms and required on every state-changing POST.
func (a *App) csrfToken(xid string) string {
	mac := hmac.New(sha256.New, a.cfg.JWTSecret)
	mac.Write([]byte("ngp-web-csrf|" + xid))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) checkCSRF(xid, token string) bool {
	return token != "" && hmac.Equal([]byte(a.csrfToken(xid)), []byte(token))
}

// sameOrigin is a defense-in-depth check on state-changing POSTs: Origin must
// equal our base origin exactly (ADR 0006). With no Origin, the Referer's
// scheme and host must equal it. Never a prefix test: https://id.ngp.one.evil.com
// and https://id.ngp.one:8443 are other origins.
func (a *App) sameOrigin(r *http.Request) bool {
	base := urlOrigin(a.cfg.BaseURL)
	if base == "" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return o == base
	}
	ref := r.Header.Get("Referer")
	return ref != "" && urlOrigin(ref) == base
}

// urlOrigin is the lower-cased "scheme://host[:port]" of an absolute http(s)
// URL, or "" when it is not one.
func urlOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// postAuth validates session + origin + CSRF for a state-changing POST and
// returns the signed-in xid. On failure it writes the response and returns
// ("", false).
func (a *App) postAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	xid := a.sessionXID(r)
	if xid == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return "", false
	}
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return "", false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return "", false
	}
	if !a.checkCSRF(xid, r.PostForm.Get("csrf")) {
		http.Error(w, "bad csrf", http.StatusForbidden)
		return "", false
	}
	return xid, true
}
