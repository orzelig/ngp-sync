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
	"sort"
	"strings"
	"sync"
	"time"
)

type User struct {
	XID       string    `json:"x_id"`
	Username  string    `json:"username"`
	AvatarURL string    `json:"avatar_url,omitempty"`
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
	return s.UpsertUserWithAvatar(xid, username, "")
}

func (s *Store) UpsertUserWithAvatar(xid, username, avatarURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Data.Users[xid]
	if !ok {
		u = User{XID: xid, CreatedAt: time.Now().UTC()}
	}
	u.Username = username
	if avatarURL != "" {
		u.AvatarURL = avatarURL
	}
	s.Data.Users[xid] = u
	return s.save()
}

// GetUser returns the stored user for an X id, if any.
func (s *Store) GetUser(xid string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Data.Users[xid]
	return u, ok
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

// DeviceInfo describes one linked device (a refresh token) for the account UI.
// ID is the token's sha256 hash (the store's map key) — safe to show and to
// accept back as a revoke target: it is a hash, not the token itself.
type DeviceInfo struct {
	ID      string
	Device  string
	Created time.Time
}

// ListDevices returns the account's linked devices, newest first.
func (s *Store) ListDevices(xid string) []DeviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DeviceInfo
	for hash, rt := range s.Data.RefreshTokens {
		if rt.XID == xid {
			out = append(out, DeviceInfo{ID: hash, Device: rt.Device, Created: rt.CreatedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// RevokeDevice removes one refresh token by its hash id, but only if it belongs
// to xid. Returns true if a token was removed.
func (s *Store) RevokeDevice(xid, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.Data.RefreshTokens[id]
	if !ok || rt.XID != xid {
		return false, nil
	}
	delete(s.Data.RefreshTokens, id)
	return true, s.save()
}

// RevokeAllDevices removes every refresh token for xid. Returns the count removed.
func (s *Store) RevokeAllDevices(xid string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for hash, rt := range s.Data.RefreshTokens {
		if rt.XID == xid {
			delete(s.Data.RefreshTokens, hash)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save()
}

// RevokeDeviceByName removes every refresh token for xid whose device name
// matches (case-insensitive, trimmed) — so removing a de-duplicated device
// clears all of its tokens, including development duplicates. Returns the count.
func (s *Store) RevokeDeviceByName(xid, name string) (int, error) {
	target := strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for hash, rt := range s.Data.RefreshTokens {
		if rt.XID == xid && strings.EqualFold(strings.TrimSpace(rt.Device), target) {
			delete(s.Data.RefreshTokens, hash)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save()
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
