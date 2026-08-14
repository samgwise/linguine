package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Engine abstracts the local inference backend a worker proxies to.
type Engine interface {
	// Proxy forwards the request body to the local OpenAI-compatible
	// endpoint and returns the response body reader. The caller owns the
	// reader and must close it. The context bounds the request lifetime.
	Proxy(ctx context.Context, body []byte) (io.ReadCloser, error)
}

// ProxyEngine forwards to an already-running OpenAI-compatible endpoint. It
// does not manage any process lifecycle or model swapping — that arrives in a
// later phase as a ManagedEngine.
type ProxyEngine struct {
	Endpoint string
	Client   *http.Client
}

// NewProxyEngine creates a ProxyEngine for the given endpoint URL. The HTTP
// client has no overall timeout so long streaming completions are not cut off
// (per-request deadlines are controlled by the caller's context).
func NewProxyEngine(endpoint string) *ProxyEngine {
	return &ProxyEngine{Endpoint: endpoint, Client: &http.Client{}}
}

// Proxy implements Engine.
func (e *ProxyEngine) Proxy(ctx context.Context, body []byte) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("engine: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("engine: upstream request: %w", err)
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("engine: upstream returned status %d", resp.StatusCode)
	}
	return resp.Body, nil
}
