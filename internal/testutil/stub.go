package testutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NewStreamingEngineStub returns an httptest.Server that responds to any POST
// with the given lines (written verbatim) and HTTP 200, flushing after each
// line so streaming behaviour is exercised. The server is closed on test
// cleanup.
func NewStreamingEngineStub(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, l := range lines {
			io.WriteString(w, l)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewNonStreamingEngineStub returns an httptest.Server that responds to any
// POST with a fixed JSON body and HTTP 200.
func NewNonStreamingEngineStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// SSELines builds canonical OpenAI-style SSE lines from token deltas, ending
// with a data: [DONE] marker.
func SSELines(deltas ...string) []string {
	lines := make([]string, 0, len(deltas)+1)
	for _, d := range deltas {
		lines = append(lines, "data: {\"choices\":[{\"delta\":{\"content\":\""+d+"\"}}]}\n\n")
	}
	lines = append(lines, "data: [DONE]\n\n")
	return lines
}

// JoinLines concatenates SSE lines for byte comparison.
func JoinLines(lines []string) string { return strings.Join(lines, "") }
