package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPasetoIssueAndParse(t *testing.T) {
	s := NewRandomSigner()
	tok, err := s.Issue(context.Background(), "enr-123", "node-gpu-sydney", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	enr, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if enr.ID != "enr-123" {
		t.Errorf("id: got %q want enr-123", enr.ID)
	}
	if enr.NodeName != "node-gpu-sydney" {
		t.Errorf("node: got %q want node-gpu-sydney", enr.NodeName)
	}
}

func TestPasetoTampered(t *testing.T) {
	s := NewRandomSigner()
	tok, _ := s.Issue(context.Background(), "enr-1", "node", time.Hour)
	pos := len(tok) - 3
	repl := "A"
	if tok[pos] == 'A' {
		repl = "B"
	}
	bad := tok[:pos] + repl + tok[pos+1:]
	if _, err := s.Parse(bad); err == nil {
		t.Error("expected error for tampered token")
	}
}

func TestPasetoExpired(t *testing.T) {
	s := NewRandomSigner()
	tok, _ := s.Issue(context.Background(), "enr-1", "node", -time.Second)
	if _, err := s.Parse(tok); err == nil {
		t.Error("expected error for expired token")
	}
}

func TestPasetoWrongKey(t *testing.T) {
	a := NewRandomSigner()
	b := NewRandomSigner()
	tok, _ := a.Issue(context.Background(), "enr-1", "node", time.Hour)
	if _, err := b.Parse(tok); err == nil {
		t.Error("expected error when verifying with the wrong key")
	}
}

func TestPasetoLoadOrCreatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signer.key")
	s1, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	tok, _ := s1.Issue(context.Background(), "enr-1", "node", time.Hour)

	s2, err := LoadOrCreateSigner(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if _, err := s2.Parse(tok); err != nil {
		t.Errorf("reloaded signer should verify a token issued by the first: %v", err)
	}
}
