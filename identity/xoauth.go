package main

// OAuth 2.0 Authorization Code + PKCE against X, as a confidential client
// (client secret stays on this server; the browser never talks to X).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// X (Twitter) OAuth 2.0 endpoints. These move rarely but do move — verify
// against https://docs.x.com/fundamentals/authentication before going live.
const (
	xAuthorizeURL = "https://x.com/i/oauth2/authorize"
	xTokenURL     = "https://api.x.com/2/oauth2/token"
	xMeURL        = "https://api.x.com/2/users/me"
	// users.read requires tweet.read; offline.access is requested in case we
	// later want to re-verify the X account server-side.
	xScopes = "users.read tweet.read offline.access"
)

type pendingAuth struct {
	Verifier string
	Expires  time.Time
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *App) authorizeURL(state, verifier string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.cfg.XClientID},
		"redirect_uri":          {a.cfg.BaseURL + "/callback"},
		"scope":                 {xScopes},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	return xAuthorizeURL + "?" + q.Encode()
}

func (a *App) exchangeCode(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {a.cfg.BaseURL + "/callback"},
		"code_verifier": {verifier},
		"client_id":     {a.cfg.XClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(a.cfg.XClientID, a.cfg.XClientSecret)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("x token endpoint: %s: %s", resp.Status, snippet(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("no access_token in X response")
	}
	return out.AccessToken, nil
}

func (a *App) fetchXIdentity(ctx context.Context, accessToken string) (id, username string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xMeURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := a.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("x users/me: %s: %s", resp.Status, snippet(body))
	}
	var out struct {
		Data struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	if out.Data.ID == "" {
		return "", "", errors.New("no user id in X response")
	}
	return out.Data.ID, out.Data.Username, nil
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
