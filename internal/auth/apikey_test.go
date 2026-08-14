package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/store"
)

func newAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}

func TestGenerateAPIKeyUniqueWithPrefix(t *testing.T) {
	a := GenerateAPIKey()
	b := GenerateAPIKey()
	if a == b {
		t.Error("generated keys are not unique")
	}
	if got := PrefixOf(a); got == "" || len(got) > keyPrefixLen {
		t.Errorf("prefix: got %q", got)
	}
	if a[:len(PrefixOf(a))] != PrefixOf(a) {
		t.Error("prefix is not a leading slice of the raw key")
	}
}

func TestHashAndVerifyAPIKey(t *testing.T) {
	raw := GenerateAPIKey()
	hash, err := HashAPIKey(raw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyAPIKey(raw, hash)
	if err != nil || !ok {
		t.Errorf("verify correct key: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyAPIKey("sk-mesh-wrongvalue", hash)
	if err != nil {
		t.Fatalf("verify wrong key: %v", err)
	}
	if ok {
		t.Error("wrong key should not verify")
	}
}

func TestVerifyAPIKeyMalformed(t *testing.T) {
	ok, err := VerifyAPIKey("whatever", "not-a-hash")
	if ok {
		t.Error("malformed hash should not verify")
	}
	if err == nil {
		t.Error("malformed hash should return an error")
	}
}

func TestAPIKeyRepoRoundTrip(t *testing.T) {
	repo := NewAPIKeyRepo(newAuthTestDB(t))
	raw := GenerateAPIKey()
	ak, err := repo.Create(context.Background(), "test-key", raw)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ak.ID == "" || ak.Prefix == "" {
		t.Fatalf("missing id/prefix: %+v", ak)
	}
	got, err := repo.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != ak.ID {
		t.Errorf("id: got %q want %q", got.ID, ak.ID)
	}
}

func TestAPIKeyRepoRejectsUnknown(t *testing.T) {
	repo := NewAPIKeyRepo(newAuthTestDB(t))
	if _, err := repo.Verify(context.Background(), GenerateAPIKey()); !errors.Is(err, ErrInvalidAPIKey) {
		t.Errorf("unknown key: got %v, want ErrInvalidAPIKey", err)
	}
}

func TestAPIKeyRepoRevoked(t *testing.T) {
	db := newAuthTestDB(t)
	repo := NewAPIKeyRepo(db)
	raw := GenerateAPIKey()
	ak, _ := repo.Create(context.Background(), "k", raw)
	if _, err := db.Exec(`UPDATE api_keys SET status = 'revoked' WHERE id = ?`, ak.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := repo.Verify(context.Background(), raw); err == nil {
		t.Error("revoked key should not verify")
	}
}

func TestAPIKeyRepoExpired(t *testing.T) {
	db := newAuthTestDB(t)
	repo := NewAPIKeyRepo(db)
	raw := GenerateAPIKey()
	ak, _ := repo.Create(context.Background(), "k", raw)
	if _, err := db.Exec(`UPDATE api_keys SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Hour).UTC(), ak.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := repo.Verify(context.Background(), raw); err == nil {
		t.Error("expired key should not verify")
	}
}
