package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyEngineStreamsBody(t *testing.T) {
	want := "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{"data: {\"a\":1}\n\n", "data: {\"a\":2}\n\n", "data: [DONE]\n\n"} {
			io.WriteString(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	eng := NewProxyEngine(srv.URL)
	body, err := eng.Proxy(context.Background(), []byte(`{"stream":true}`))
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != want {
		t.Errorf("body:\n got %q\nwant %q", got, want)
	}
}

func TestProxyEngineUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	eng := NewProxyEngine(srv.URL)
	if _, err := eng.Proxy(context.Background(), []byte(`{}`)); err == nil {
		t.Error("expected error for upstream 500")
	}
}
