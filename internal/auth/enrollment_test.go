package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/store"
)

// newEnrollmentRepo builds a repo over a throwaway SQLite database. A distinct
// signer per database mirrors production, where the router's keypair signs
// every token it mints.
func newEnrollmentRepo(t *testing.T) (*EnrollmentRepo, *Signer) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "enrol-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer := NewRandomSigner()
	return NewEnrollmentRepo(st.DB(), signer), signer
}

// TestEnrollmentCreateCarriesMetadata checks that Create returns the row's
// metadata with an explicit created_at timestamp (second-granularity SQLite
// defaults would otherwise leave same-second creations tied).
func TestEnrollmentCreateCarriesMetadata(t *testing.T) {
	repo, signer := newEnrollmentRepo(t)
	ctx := context.Background()

	before := time.Now().UTC()
	et, tok, err := repo.Create(ctx, "gpu-loopback", 24*time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if et.ID == "" || et.NodeName != "gpu-loopback" || et.Status != "active" {
		t.Errorf("metadata: got id=%q node=%q status=%q", et.ID, et.NodeName, et.Status)
	}
	if et.CreatedAt.Before(before.Add(-time.Second)) || et.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("created_at %v not near test time", et.CreatedAt)
	}
	if !et.ExpiresAt.Valid {
		t.Fatal("expires_at should be set for a 24h ttl")
	}
	// The issued token's jti must match the stored row id so IsActive gates
	// on the same identity the signature carries.
	enr, err := signer.Parse(tok)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if enr.ID != et.ID || enr.NodeName != et.NodeName {
		t.Errorf("token claims id=%q node=%q; row has id=%q node=%q", enr.ID, enr.NodeName, et.ID, et.NodeName)
	}
}

// TestEnrollmentListNewestFirst checks the worker-keys page's ordering: the
// most recently minted token heads the list. Timestamps are spaced a second
// apart because ids are random UUIDs and cannot break created_at ties.
func TestEnrollmentListNewestFirst(t *testing.T) {
	repo, _ := newEnrollmentRepo(t)
	ctx := context.Background()

	for _, node := range []string{"node-a", "node-b", "node-c"} {
		if _, _, err := repo.Create(ctx, node, 0); err != nil {
			t.Fatalf("create %s: %v", node, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}
	toks, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 3 {
		t.Fatalf("list: got %d tokens, want 3", len(toks))
	}
	want := []string{"node-c", "node-b", "node-a"}
	for i, w := range want {
		if toks[i].NodeName != w {
			t.Errorf("position %d: got %q, want %q (newest first)", i, toks[i].NodeName, w)
		}
	}
}

// TestEnrollmentRevokeFlow checks that revoke flips IsActive immediately,
// that revoking twice is idempotent, and that an unknown id errors.
func TestEnrollmentRevokeFlow(t *testing.T) {
	repo, _ := newEnrollmentRepo(t)
	ctx := context.Background()

	et, _, err := repo.Create(ctx, "gpu-victim", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ok, err := repo.IsActive(ctx, et.ID); err != nil || !ok {
		t.Fatalf("fresh token IsActive: got %v, %v; want true, nil", ok, err)
	}
	if err := repo.Revoke(ctx, et.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if ok, err := repo.IsActive(ctx, et.ID); err != nil || ok {
		t.Errorf("revoked token IsActive: got %v, %v; want false, nil", ok, err)
	}
	// Idempotent: re-revoking an already-revoked token is a no-op.
	if err := repo.Revoke(ctx, et.ID); err != nil {
		t.Errorf("re-revoke: %v", err)
	}
	if err := repo.Revoke(ctx, "no-such-id"); err == nil {
		t.Error("revoking an unknown id should error")
	}
	// The list reflects the revoked status for the dashboard.
	toks, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 1 || toks[0].Status != "revoked" {
		t.Errorf("list after revoke: got %d tokens, status %q; want 1, revoked", len(toks), toks[0].Status)
	}
}
