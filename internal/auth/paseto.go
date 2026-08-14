package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"aidanwoods.dev/go-paseto"
)

const workerAudience = "linguine-worker"

// noExpiryTTL is the effective lifetime used when a caller asks for an
// non-expiring token (ttl == 0). PASETO's NotExpired rule requires an exp
// claim, so expiry cannot be omitted; 100 years stands in for "never".
const noExpiryTTL = 100 * 365 * 24 * time.Hour

// effectiveExpiry maps a requested ttl to a concrete expiration time relative
// to now: ttl > 0 is a real lifetime, ttl == 0 means "no expiry" (far future),
// and ttl < 0 yields a past time so expired-token paths can be exercised.
func effectiveExpiry(now time.Time, ttl time.Duration) time.Time {
	switch {
	case ttl > 0:
		return now.Add(ttl)
	case ttl == 0:
		return now.Add(noExpiryTTL)
	default:
		return now.Add(ttl)
	}
}

// Signer issues and verifies PASETO v4 public tokens used for worker
// enrollment. The Ed25519 private key is persisted to a file so tokens remain
// verifiable across router and admin-CLI process restarts.
type Signer struct {
	sec paseto.V4AsymmetricSecretKey
	pub paseto.V4AsymmetricPublicKey
}

// LoadOrCreateSigner loads the Ed25519 private key from path, or generates a
// new keypair and persists it (mode 0600) when the file does not exist.
func LoadOrCreateSigner(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		sec, perr := paseto.NewV4AsymmetricSecretKeyFromHex(strings.TrimSpace(string(data)))
		if perr != nil {
			return nil, fmt.Errorf("auth: load signer key: %w", perr)
		}
		return &Signer{sec: sec, pub: sec.Public()}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("auth: read signer key: %w", err)
	}
	_, priv, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		return nil, fmt.Errorf("auth: generate signer key: %w", gerr)
	}
	enc := hex.EncodeToString(priv) // 64-byte extended private key
	if werr := os.WriteFile(path, []byte(enc), 0o600); werr != nil {
		return nil, fmt.Errorf("auth: write signer key: %w", werr)
	}
	sec, perr := paseto.NewV4AsymmetricSecretKeyFromHex(enc)
	if perr != nil {
		return nil, fmt.Errorf("auth: construct signer: %w", perr)
	}
	return &Signer{sec: sec, pub: sec.Public()}, nil
}

// NewRandomSigner generates an in-memory keypair without persistence. It is
// intended for tests.
func NewRandomSigner() *Signer {
	sec := paseto.NewV4AsymmetricSecretKey()
	return &Signer{sec: sec, pub: sec.Public()}
}

// Enrollment holds the verified claims of a worker enrollment token.
type Enrollment struct {
	ID       string // jti — matches a node_enrollment_tokens row
	NodeName string // subject — expected hostname label
}

// Issue signs an enrollment token for id (jti) and nodeName (subject), expiring
// after ttl.
func (s *Signer) Issue(_ context.Context, id, nodeName string, ttl time.Duration) (string, error) {
	tok := paseto.NewToken()
	now := time.Now()
	tok.SetAudience(workerAudience)
	tok.SetJti(id)
	tok.SetSubject(nodeName)
	tok.SetIssuedAt(now)
	tok.SetExpiration(effectiveExpiry(now, ttl))
	return tok.V4Sign(s.sec, nil), nil
}

// Parse verifies a token's signature, audience and expiry, returning its
// claims.
func (s *Signer) Parse(token string) (*Enrollment, error) {
	parser := paseto.NewParser() // includes NotExpired
	parser.AddRule(paseto.ForAudience(workerAudience))
	tok, err := parser.ParseV4Public(s.pub, token, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: verify enrollment token: %w", err)
	}
	id, _ := tok.GetJti()
	node, _ := tok.GetSubject()
	return &Enrollment{ID: id, NodeName: node}, nil
}
