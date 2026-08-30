package admin

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// nonceTTL bounds how long a freshly created key may sit in memory waiting
// for the operator to view it.
const nonceTTL = 5 * time.Minute

// nonceStore hands the raw API key to the operator exactly once. Create
// stashes the key under a random nonce; the created page takes it (deleting
// the entry) so a replayed URL, a refresh, or a shared link shows nothing.
// Entries expire from memory after nonceTTL if never viewed.
type nonceStore struct {
	mu      sync.Mutex
	entries map[string]nonceEntry
}

type nonceEntry struct {
	value     string
	expiresAt time.Time
}

func newNonceStore() *nonceStore {
	return &nonceStore{entries: make(map[string]nonceEntry)}
}

// put stores value under a fresh random nonce and returns it.
func (s *nonceStore) put(value string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(time.Now())
	s.entries[nonce] = nonceEntry{value: value, expiresAt: time.Now().Add(nonceTTL)}
	return nonce, nil
}

// take returns and deletes the value for nonce. Unknown, expired, and
// already-used nonces all behave identically (ok=false) so a replayed URL
// reveals nothing.
func (s *nonceStore) take(nonce string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[nonce]
	if !ok {
		return "", false
	}
	delete(s.entries, nonce)
	if time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.value, true
}

// purgeLocked drops expired entries so the map cannot grow unboundedly.
// Caller must hold s.mu.
func (s *nonceStore) purgeLocked(now time.Time) {
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
}
