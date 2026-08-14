// Package catalog probes a local OpenAI-compatible engine's /v1/models
// endpoint and exposes the current model catalog as a slice of model ids.
// The probe runs once at startup and on a slow ticker so the advertised
// catalog stays current without blocking the worker's request path.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultRefreshInterval is the gap between automatic catalog refreshes.
// It is slow because model sets change rarely; a fast ticker would waste
// requests against the engine.
const DefaultRefreshInterval = 60 * time.Second

// Probe polls a local engine's /v1/models endpoint and caches the result.
type Probe struct {
	endpoint  string
	client    *http.Client
	interval   time.Duration

	mu      sync.RWMutex
	current []string
	err     error
}

// NewProbe creates a Probe for the given engine base URL (e.g.
// "http://127.0.0.1:8080"). The HTTP client has no overall timeout so a
// slow engine doesn't abort the probe; the caller's context bounds each fetch.
func NewProbe(baseURL string, opts ...Option) *Probe {
	p := &Probe{
		endpoint: baseURL + "/v1/models",
		client:   &http.Client{},
		interval:  DefaultRefreshInterval,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Option configures a Probe.
type Option func(*Probe)

// WithRefreshInterval sets the automatic refresh period.
func WithRefreshInterval(d time.Duration) Option {
	return func(p *Probe) { p.interval = d }
}

// Refresh fetches the catalog once and caches it. It is safe for concurrent use.
func (p *Probe) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint, nil)
	if err != nil {
		return fmt.Errorf("catalog: build request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.set(nil, err)
		return fmt.Errorf("catalog: fetch %s: %w", p.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("catalog: %s returned status %d", p.endpoint, resp.StatusCode)
		p.set(nil, err)
		return err
	}
	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		p.set(nil, err)
		return fmt.Errorf("catalog: decode: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	p.set(ids, nil)
	return nil
}

// modelsResponse mirrors the OpenAI-standard /v1/models payload.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (p *Probe) set(ids []string, err error) {
	p.mu.Lock()
	p.current = ids
	p.err = err
	p.mu.Unlock()
}

// Current returns the last-probed catalog and any error from the last fetch.
func (p *Probe) Current() ([]string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current, p.err
}

// Run starts a background refresh loop until ctx is cancelled, refreshing every
// interval. It is non-blocking; the first refresh happens immediately.
func (p *Probe) Run(ctx context.Context) {
	_ = p.Refresh(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.Refresh(ctx)
		}
	}
}
