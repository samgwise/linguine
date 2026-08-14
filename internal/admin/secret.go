package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// LoadOrCreateSessionSecret loads the session-cookie HMAC key from path, or
// generates a 32-byte secret and persists it (mode 0600) when the file does
// not exist. The secret signs admin session cookies so they survive router
// restarts; a rotated secret invalidates outstanding sessions.
func LoadOrCreateSessionSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		sec, derr := hex.DecodeString(string(data))
		if derr != nil {
			return nil, fmt.Errorf("admin: decode session secret: %w", derr)
		}
		if len(sec) == 0 {
			return nil, errors.New("admin: empty session secret")
		}
		return sec, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("admin: read session secret: %w", err)
	}
	sec := make([]byte, 32)
	if _, rerr := rand.Read(sec); rerr != nil {
		return nil, fmt.Errorf("admin: generate session secret: %w", rerr)
	}
	enc := hex.EncodeToString(sec)
	if werr := os.WriteFile(path, []byte(enc), 0o600); werr != nil {
		return nil, fmt.Errorf("admin: write session secret: %w", werr)
	}
	return sec, nil
}
