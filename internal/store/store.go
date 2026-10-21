// Package store keeps the bridge's state in one AES-256-GCM encrypted file.
//
// The state holds the Eleza session (refresh token and the device key that
// signs its refresh), so the file is as sensitive as a logged-in phone.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	magic = "TGB1"
	aad   = "eleza-tgbridge/state/v1"
)

// State is everything that survives a restart.
type State struct {
	// Eleza device. DeviceID stays across logins; DeviceKey (an Ed25519
	// private key) is new for every login.
	DeviceID     string    `json:"device_id,omitempty"`
	DeviceKey    []byte    `json:"device_key,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	Handle       string    `json:"handle,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Nonce        string    `json:"device_nonce,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	AccessExp    time.Time `json:"access_exp,omitempty"`
	LoggedInAt   time.Time `json:"logged_in_at,omitempty"`
	ExpiryWarned bool      `json:"expiry_warned,omitempty"`
	SavedChatID  string    `json:"saved_chat_id,omitempty"`

	// Bot.
	Target   string `json:"target,omitempty"`    // ask | eli | saved | both
	TGOffset int64  `json:"tg_offset,omitempty"` // next Telegram update id
}

// LoggedIn reports whether an Eleza session is stored.
func (s State) LoggedIn() bool { return s.RefreshToken != "" }

// ClearSession forgets the Eleza session (the device id stays).
func (s *State) ClearSession() {
	s.DeviceKey, s.UserID, s.Handle, s.RefreshToken, s.Nonce, s.AccessToken = nil, "", "", "", "", ""
	s.AccessExp, s.LoggedInAt, s.ExpiryWarned, s.SavedChatID = time.Time{}, time.Time{}, false, ""
}

// Store is the state file. Safe for concurrent use.
type Store struct {
	path string
	aead cipher.AEAD
	mu   sync.Mutex
	st   State
}

// Open loads (or starts) the state file at path, encrypted with key (32 bytes).
func Open(path string, key []byte) (*Store, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, aead: aead}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(raw) < len(magic)+ns || string(raw[:len(magic)]) != magic {
		return nil, fmt.Errorf("store: %s is not a bridge state file", path)
	}
	body := raw[len(magic):]
	plain, err := aead.Open(nil, body[:ns], body[ns:], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("store: cannot decrypt %s (wrong BRIDGE_SECRET_KEY?)", path)
	}
	if err := json.Unmarshal(plain, &s.st); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return s, nil
}

// Get returns a copy of the state.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.DeviceKey = append([]byte(nil), s.st.DeviceKey...)
	return st
}

// Update changes the state and writes it to disk before returning. On a
// write error the in-memory state is left unchanged.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st
	next.DeviceKey = append([]byte(nil), s.st.DeviceKey...)
	fn(&next)
	if err := s.write(next); err != nil {
		return err
	}
	s.st = next
	return nil
}

func (s *Store) write(st State) error {
	plain, err := json.Marshal(st)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append([]byte(magic), nonce...)
	out = s.aead.Seal(out, nonce, plain, []byte(aad))
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
