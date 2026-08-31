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
	CreatedAt time.Time
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
	now := time.Now().UTC()
	expiresAt := sql.NullTime{Time: effectiveExpiry(now, ttl).UTC(), Valid: true}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO node_enrollment_tokens (id, node_name, status, expires_at, created_at) VALUES (?, ?, 'active', ?, ?)`,
		id, nodeName, expiresAt, now); err != nil {
		return nil, "", fmt.Errorf("auth: insert enrollment token: %w", err)
	}
	tok, err := r.signer.Issue(ctx, id, nodeName, ttl)
	if err != nil {
		return nil, "", err
	}
	return &EnrollmentToken{ID: id, NodeName: nodeName, Status: "active", ExpiresAt: expiresAt, CreatedAt: now}, tok, nil
}

// List returns every enrollment token, newest first, for the admin worker-key
// management page. It never exposes the raw PASETO (which was shown once at
// creation and is not stored).
func (r *EnrollmentRepo) List(ctx context.Context) ([]EnrollmentToken, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, node_name, status, expires_at, created_at FROM node_enrollment_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("auth: list enrollment tokens: %w", err)
	}
	defer rows.Close()
	var out []EnrollmentToken
	for rows.Next() {
		var et EnrollmentToken
		if err := rows.Scan(&et.ID, &et.NodeName, &et.Status, &et.ExpiresAt, &et.CreatedAt); err != nil {
			return nil, fmt.Errorf("auth: scan enrollment token: %w", err)
		}
		out = append(out, et)
	}
	return out, rows.Err()
}

// Revoke flips an enrollment token's status to 'revoked' so IsActive (and
// therefore the router's per-heartbeat check) rejects it immediately — a
// connected worker is dropped on its next heartbeat. Revoking is idempotent;
// it errors only when no token exists with the given id.
func (r *EnrollmentRepo) Revoke(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE node_enrollment_tokens SET status = 'revoked' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("auth: revoke enrollment token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: revoke enrollment token: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("auth: no enrollment token with id %s", id)
	}
	return nil
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
