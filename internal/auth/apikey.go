package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

const (
	keyLabel       = "sk-mesh-"
	keyRandomBytes = 24
	keyPrefixLen   = 16 // leading chars stored for prefix lookup
	defaultKeyRole = "client"

	argonTime    uint32 = 1
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ErrInvalidAPIKey is returned when a bearer token does not match a valid,
// active, unexpired API key.
var ErrInvalidAPIKey = errors.New("auth: invalid api key")

// APIKey is a resolved ingress credential. The raw key is never stored.
type APIKey struct {
	ID        string
	Name      string
	Prefix    string
	Role      string
	Status    string
	ExpiresAt sql.NullTime
}

// GenerateAPIKey returns a fresh raw API key. It is shown once to the operator
// and never persisted.
func GenerateAPIKey() string {
	b := make([]byte, keyRandomBytes)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(fmt.Sprintf("auth: read random: %v", err))
	}
	return keyLabel + base64.RawURLEncoding.EncodeToString(b)
}

// PrefixOf returns the leading characters used for prefix lookup.
func PrefixOf(raw string) string {
	if len(raw) < keyPrefixLen {
		return raw
	}
	return raw[:keyPrefixLen]
}

// HashAPIKey returns a PHC-format argon2id hash of the raw key.
func HashAPIKey(raw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(raw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return phcEncode(salt, hash), nil
}

// VerifyAPIKey reports whether raw matches the PHC-format hash, compared in
// constant time over the derived hash.
func VerifyAPIKey(raw, encoded string) (bool, error) {
	salt, hash, p, err := phcDecode(encoded)
	if err != nil {
		return false, err
	}
	other := argon2.IDKey([]byte(raw), salt, p.time, p.memory, p.threads, uint32(len(hash)))
	return subtle.ConstantTimeCompare(hash, other) == 1, nil
}

func phcEncode(salt, hash []byte) string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash))
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func phcDecode(encoded string) (salt, hash []byte, p argonParams, err error) {
	parts := strings.Split(encoded, "$")
	// Expect ["", "argon2id", "v=19", "m=..,t=..,p=..", "salt", "hash"].
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, p, errors.New("auth: not an argon2id hash")
	}
	for _, kv := range strings.Split(parts[3], ",") {
		eq := strings.SplitN(kv, "=", 2)
		if len(eq) != 2 {
			continue
		}
		switch eq[0] {
		case "m":
			v, _ := strconv.ParseUint(eq[1], 10, 32)
			p.memory = uint32(v)
		case "t":
			v, _ := strconv.ParseUint(eq[1], 10, 32)
			p.time = uint32(v)
		case "p":
			v, _ := strconv.ParseUint(eq[1], 10, 8)
			p.threads = uint8(v)
		}
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return nil, nil, p, errors.New("auth: invalid argon2id parameters")
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, p, fmt.Errorf("auth: decode salt: %w", err)
	}
	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, p, fmt.Errorf("auth: decode hash: %w", err)
	}
	return salt, hash, p, nil
}

// APIKeyRepo maps ingress credentials to the api_keys table.
type APIKeyRepo struct {
	db *sql.DB
}

// NewAPIKeyRepo wraps a database connection for API key storage.
func NewAPIKeyRepo(db *sql.DB) *APIKeyRepo { return &APIKeyRepo{db: db} }

// Create hashes raw, stores a new api_keys row, and returns the record.
func (r *APIKeyRepo) Create(ctx context.Context, name, raw string) (*APIKey, error) {
	hash, err := HashAPIKey(raw)
	if err != nil {
		return nil, err
	}
	ak := &APIKey{
		ID:     uuid.NewString(),
		Name:   name,
		Prefix: PrefixOf(raw),
		Role:   defaultKeyRole,
		Status: "active",
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, token_hash, prefix, role, status) VALUES (?, ?, ?, ?, ?, ?)`,
		ak.ID, ak.Name, hash, ak.Prefix, ak.Role, ak.Status)
	if err != nil {
		return nil, fmt.Errorf("auth: insert api key: %w", err)
	}
	return ak, nil
}

// Verify resolves a bearer token to a valid (active, unexpired) API key. It
// narrows candidates by prefix, then argon2-verifies each in constant time.
func (r *APIKeyRepo) Verify(ctx context.Context, bearer string) (*APIKey, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, prefix, role, status, expires_at, token_hash FROM api_keys WHERE prefix = ?`,
		PrefixOf(bearer))
	if err != nil {
		return nil, fmt.Errorf("auth: query api keys: %w", err)
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var ak APIKey
		var hash string
		if err := rows.Scan(&ak.ID, &ak.Name, &ak.Prefix, &ak.Role, &ak.Status, &ak.ExpiresAt, &hash); err != nil {
			return nil, fmt.Errorf("auth: scan api key: %w", err)
		}
		if ak.Status != "active" {
			continue
		}
		if ak.ExpiresAt.Valid && ak.ExpiresAt.Time.Before(now) {
			continue
		}
		ok, vErr := VerifyAPIKey(bearer, hash)
		if vErr != nil {
			return nil, vErr
		}
		if ok {
			return &ak, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: iterate api keys: %w", err)
	}
	return nil, ErrInvalidAPIKey
}
