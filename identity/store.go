package main

// Single-file JSON store, good for one user and a handful of devices.
// Swap for SQLite/Postgres if this ever grows beyond personal use.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type User struct {
	XID       string    `json:"x_id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

type RefreshToken struct {
	XID       string    `json:"x_id"`
	Device    string    `json:"device,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	mu   sync.Mutex
	path string
	Data struct {
		Users         map[string]User         `json:"users"`          // keyed by X user id
		RefreshTokens map[string]RefreshToken `json:"refresh_tokens"` // keyed by sha256(token) hex
	}
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path}
	s.Data.Users = map[string]User{}
	s.Data.RefreshTokens = map[string]RefreshToken{}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.Data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// save must be called with s.mu held.
func (s *Store) save() error {
	b, err := json.MarshalIndent(&s.Data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) UpsertUser(xid, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Data.Users[xid]
	if !ok {
		u = User{XID: xid, CreatedAt: time.Now().UTC()}
	}
	u.Username = username
	s.Data.Users[xid] = u
	return s.save()
}

// IssueRefreshToken returns the plaintext token; only its hash is stored.
func (s *Store) IssueRefreshToken(xid, device string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := "ngpr_" + randHex(32)
	s.Data.RefreshTokens[hashToken(tok)] = RefreshToken{
		XID:       xid,
		Device:    device,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.save(); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Store) LookupRefreshToken(tok string) (xid string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.Data.RefreshTokens[hashToken(tok)]
	if !ok {
		return "", false
	}
	return rt.XID, true
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system CSPRNG failing is not recoverable
	}
	return hex.EncodeToString(b)
}
