package main

// Minimal HS256 JWT mint/verify, stdlib only. go-sync's JWT auth middleware
// (Phase 2) must verify with the same shared secret and expect these claims.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Claims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"` // "x:<x-user-id>"
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func MintJWT(secret []byte, c Claims) (string, error) {
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64url(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64url(mac.Sum(nil)), nil
}

// VerifyJWT checks the HMAC and expiry. The alg header is ignored on purpose:
// we only ever accept a token whose signature matches our HS256 secret, so an
// attacker-supplied "alg" buys nothing.
func VerifyJWT(secret []byte, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("bad signature encoding")
	}
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("bad payload encoding")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	if time.Now().Unix() >= c.Exp {
		return nil, errors.New("token expired")
	}
	return &c, nil
}
