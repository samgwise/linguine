// Package audit records request history to the request_audit_logs table.
// The repo is written from the /v1 hot path, so Record pushes entries onto a
// buffered channel and a background goroutine batch-inserts them; a slow
// SQLite write therefore never blocks a completion.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Entry is one request audit record.
type Entry struct {
	APIKeyID        string
	NodeID          string
	ModelRequested  string
	ModelServed     string
	PromptTokens    int
	CompletionTokens int
	TTFTMs          int
	TotalDurationMs int
	WasStreamed     bool
	WasModelSwapped bool
	StatusCode      int
	CreatedAt       time.Time
}

// Repo writes request audit entries to request_audit_logs asynchronously.
type Repo struct {
	db     *sql.DB
	ch     chan Entry
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewRepo starts a background writer that drains entries and batch-inserts
// them. buffer is the channel depth before Record drops entries (it never
// blocks the hot path). Call Close to stop the writer and flush.
func NewRepo(db *sql.DB, buffer int) *Repo {
	r := &Repo{db: db, ch: make(chan Entry, buffer)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(1)
	go r.run(ctx)
	return r
}

// Record enqueues an entry for async insertion. It never blocks: if the
// buffer is full (SQLite writer fell behind), the entry is dropped with a
// log line. Dropping audit history is preferable to stalling a completion.
func (r *Repo) Record(e Entry) {
	select {
	case r.ch <- e:
	default:
		log.Printf("[audit] drop entry (buffer full); model=%s node=%s", e.ModelRequested, e.NodeID)
	}
}

// run drains the channel and batch-inserts entries every flushInterval or
// when a batch fills, whichever comes first.
func (r *Repo) run(ctx context.Context) {
	defer r.wg.Done()
	const batchSize = 64
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]Entry, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.insertBatch(batch); err != nil {
			log.Printf("[audit] insert batch: %v", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			// Drain anything still buffered so Close reliably flushes pending
			// entries before returning, even if cancel raced a queued Record.
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
				default:
					flush()
					return
				}
			}
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Repo) insertBatch(batch []Entry) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	stmt, err := tx.Prepare(
		`INSERT INTO request_audit_logs
		   (api_key_id, node_id, model_requested, model_served,
		    prompt_tokens, completion_tokens, ttft_ms, total_duration_ms,
		    was_streamed, was_model_swapped, status_code)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	for _, e := range batch {
		if _, err := stmt.Exec(
			nullable(e.APIKeyID), nullable(e.NodeID),
			e.ModelRequested, e.ModelServed,
			e.PromptTokens, e.CompletionTokens,
			nullableInt(e.TTFTMs), nullableInt(e.TotalDurationMs),
			e.WasStreamed, e.WasModelSwapped, e.StatusCode,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Close stops the background writer and waits for it to flush the final batch.
func (r *Repo) Close() error {
	r.cancel()
	r.wg.Wait()
	return nil
}

// Recent returns up to limit most-recent audit entries, newest first, optionally
// filtered by node id. It reads directly from SQLite so it reflects flushed
// state (the in-memory channel may still hold unflushed entries).
func (r *Repo) Recent(ctx context.Context, limit int, nodeID string) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT api_key_id, node_id, model_requested, model_served,
	              prompt_tokens, completion_tokens, ttft_ms, total_duration_ms,
	              was_streamed, was_model_swapped, status_code, created_at
	       FROM request_audit_logs`
	args := []any{}
	if nodeID != "" {
		q += " WHERE node_id = ?"
		args = append(args, nodeID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: query: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var apiKeyID, nodeIDCol sql.NullString
		var ttft, total sql.NullInt64
		var created sql.NullTime
		if err := rows.Scan(
			&apiKeyID, &nodeIDCol, &e.ModelRequested, &e.ModelServed,
			&e.PromptTokens, &e.CompletionTokens, &ttft, &total,
			&e.WasStreamed, &e.WasModelSwapped, &e.StatusCode, &created,
		); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		e.APIKeyID = apiKeyID.String
		e.NodeID = nodeIDCol.String
		if ttft.Valid {
			e.TTFTMs = int(ttft.Int64)
		}
		if total.Valid {
			e.TotalDurationMs = int(total.Int64)
		}
		if created.Valid {
			e.CreatedAt = created.Time
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// ErrClosed is returned by Record/Recent after Close.
var ErrClosed = errors.New("audit: repo closed")
