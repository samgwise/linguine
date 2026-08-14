package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeParsesModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path: got %q want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[{"id":"llama-3.1-8b-instruct"},{"id":"mistral-7b-v0.3"},{"id":"deepseek-coder-6.7b"}]}`))
	}))
	defer srv.Close()

	p := NewProbe(srv.URL)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := p.Current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	want := []string{"llama-3.1-8b-instruct", "mistral-7b-v0.3", "deepseek-coder-6.7b"}
	if len(got) != len(want) {
		t.Fatalf("catalog: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("catalog[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestProbeUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewProbe(srv.URL)
	if err := p.Refresh(context.Background()); err == nil {
		t.Error("expected error for upstream 500")
	}
	got, err := p.Current()
	if err == nil {
		t.Error("expected cached error for upstream 500")
	}
	if got != nil {
		t.Errorf("expected nil catalog on error, got %v", got)
	}
}

func TestProbeEmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := NewProbe(srv.URL)
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := p.Current()
	if len(got) != 0 {
		t.Errorf("expected empty catalog, got %v", got)
	}
}

func TestProbeRunRefreshesPeriodically(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	p := NewProbe(srv.URL, WithRefreshInterval(50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	defer cancel()

	// The immediate refresh runs once; wait for at least one periodic refresh.
	time.Sleep(130 * time.Millisecond)
	got, err := p.Current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if len(got) != 1 || got[0] != "m1" {
		t.Errorf("catalog: got %v want [m1]", got)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 fetches (startup + periodic), got %d", calls)
	}
}
