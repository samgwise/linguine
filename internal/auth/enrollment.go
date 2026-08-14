package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EnrollmentToken is a worker onboarding record. The raw PASETO is returned
// from Create once and never stored.
type EnrollmentToken struct {
	ID        string
	NodeName  string
	Status    string
	ExpiresAt sql.NullTime
}

// EnrollmentRepo maps worker onboarding tokens to the node_enrollment_tokens
// table and issues their PASETO via a Signer.
type EnrollmentRepo struct {
	db     *sql.DB
	signer *Signer
}

// NewEnrollmentRepo wraps a database connection and the router's signer.
func NewEnrollmentRepo(db *sql.DB, signer *Signer) *EnrollmentRepo {
	return &EnrollmentRepo{db: db, signer: signer}
}

// Create inserts an enrollment token row and returns the signed PASETO (shown
// once to the operator). A ttl of 0 means no expiry.
func (r *EnrollmentRepo) Create(ctx context.Context, nodeName string, ttl time.Duration) (*EnrollmentToken, string, error) {
	id := uuid.NewString()
	expiresAt := sql.NullTime{Time: effectiveExpiry(time.Now(), ttl).UTC(), Valid: true}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO node_enrollment_tokens (id, node_name, status, expires_at) VALUES (?, ?, 'active', ?)`,
		id, nodeName, expiresAt); err != nil {
		return nil, "", fmt.Errorf("auth: insert enrollment token: %w", err)
	}
	tok, err := r.signer.Issue(ctx, id, nodeName, ttl)
	if err != nil {
		return nil, "", err
	}
	return &EnrollmentToken{ID: id, NodeName: nodeName, Status: "active", ExpiresAt: expiresAt}, tok, nil
}

// IsActive reports whether the enrollment token id is present, active and
// unexpired.
func (r *EnrollmentRepo) IsActive(ctx context.Context, id string) (bool, error) {
	var status string
	var expiresAt sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT status, expires_at FROM node_enrollment_tokens WHERE id = ?`, id).Scan(&status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: query enrollment token: %w", err)
	}
	if status != "active" {
		return false, nil
	}
	if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
		return false, nil
	}
	return true, nil
}
