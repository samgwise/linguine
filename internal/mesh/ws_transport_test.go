package mesh

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/protocol"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func wsAddr(port int) string  { return fmt.Sprintf("ws://127.0.0.1:%d/mesh", port) }
func wssAddr(port int) string { return fmt.Sprintf("wss://127.0.0.1:%d/mesh", port) }

// genCert generates a self-signed cert in a private temp dir, returning the
// cert path, key path, and leaf fingerprint.
func genCert(t *testing.T) (cert, key, fp string) {
	t.Helper()
	cert = filepath.Join(t.TempDir(), "cert.pem")
	key = filepath.Join(t.TempDir(), "key.pem")
	_, fp, _, err := LoadOrCreateSelfSignedCert(cert, key)
	if err != nil {
		t.Fatalf("gen cert: %v", err)
	}
	return cert, key, fp
}

// exerciseRoundTrip proves a connected router/worker pair can exchange a
// heartbeat, a dispatched job, and a streamed framed reply — the full mesh
// framing path — over whatever transport the pair is using.
func exerciseRoundTrip(t *testing.T, r *Router, w *Worker) {
	t.Helper()
	if err := w.Send(plainEnv("hb", "only")); err != nil {
		t.Fatalf("hb send: %v", err)
	}
	hb, err := r.Recv()
	if err != nil {
		t.Fatalf("recv hb: %v", err)
	}
	pipe, perr := PipeFromHeader(hb.Header)
	if perr != nil {
		hb.Free()
		t.Fatalf("pipe from header: %v", perr)
	}
	hb.Free()

	if err := r.SendTo(pipe, plainEnv("job", "do-work")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	job, err := w.Recv()
	if err != nil {
		t.Fatalf("worker recv job: %v", err)
	}
	backtrace := append([]byte{}, job.Header...)
	_, payload := decodePlain(t, job.Body)
	job.Free()
	if payload != "do-work" {
		t.Fatalf("job payload: got %q want do-work", payload)
	}

	reply := &protocol.Frame{Type: protocol.FrameTypeChunk, Payload: []byte("hello")}
	if err := w.SendReply(backtrace, streamFrame("rr-1", reply)); err != nil {
		t.Fatalf("worker reply: %v", err)
	}
	rm, err := r.Recv()
	if err != nil {
		t.Fatalf("recv reply: %v", err)
	}
	reqID, f := decodeStream(t, rm.Body)
	rm.Free()
	if reqID != "rr-1" {
		t.Errorf("reply reqID: got %q want rr-1", reqID)
	}
	if f.Type != protocol.FrameTypeChunk || string(f.Payload) != "hello" {
		t.Errorf("reply frame: got type=%d payload=%q", f.Type, f.Payload)
	}
}

func TestWSRoundTrip(t *testing.T) {
	port := freePort(t)
	addr := wsAddr(port)
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}
	w, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer w.Close()
	if err := w.Dial(addr, nil, ""); err != nil {
		t.Fatalf("dial: %v", err)
	}
	exerciseRoundTrip(t, r, w)
}

func TestWSSFingerprintRoundTrip(t *testing.T) {
	port := freePort(t)
	cert, key, fp := genCert(t)
	srvCfg, err := ServerTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(wssAddr(port), srvCfg); err != nil {
		t.Fatalf("listen: %v", err)
	}
	cliCfg, err := ClientTLSConfig("", "127.0.0.1", fp)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	w, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer w.Close()
	if err := w.Dial(wssAddr(port), cliCfg, ""); err != nil {
		t.Fatalf("dial: %v", err)
	}
	exerciseRoundTrip(t, r, w)
}

func TestWSSCARoundTrip(t *testing.T) {
	port := freePort(t)
	cert, key, _ := genCert(t)
	srvCfg, err := ServerTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(wssAddr(port), srvCfg); err != nil {
		t.Fatalf("listen: %v", err)
	}
	// The server's own self-signed cert file is its own trust root, so a
	// client using it as the CA verifies the presented cert.
	cliCfg, err := ClientTLSConfig(cert, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	w, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer w.Close()
	if err := w.Dial(wssAddr(port), cliCfg, ""); err != nil {
		t.Fatalf("dial: %v", err)
	}
	exerciseRoundTrip(t, r, w)
}

// dialExpectErr dials and asserts it errors within a bounded window, guarding
// against a hang on a handshake that should fail.
func dialExpectErr(t *testing.T, addr string, cfg *tls.Config, proxy, want string) {
	t.Helper()
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() {
		w, err := NewWorker()
		if err != nil {
			ch <- res{err}
			return
		}
		defer w.Close()
		ch <- res{w.Dial(addr, cfg, proxy)}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatalf("expected dial error (%s), got nil", want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("dial did not fail within 5s (%s) — possible hang", want)
	}
}

func TestWSSRejectsBadCA(t *testing.T) {
	port := freePort(t)
	certA, keyA, _ := genCert(t)
	certB, _, _ := genCert(t) // independent self-signed cert, used as the client's CA
	srvCfg, err := ServerTLSConfig(certA, keyA)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(wssAddr(port), srvCfg); err != nil {
		t.Fatalf("listen: %v", err)
	}
	cliCfg, err := ClientTLSConfig(certB, "127.0.0.1", "")
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	dialExpectErr(t, wssAddr(port), cliCfg, "", "bad CA")
}

func TestWSSRejectsFingerprintMismatch(t *testing.T) {
	port := freePort(t)
	certA, keyA, _ := genCert(t)
	_, _, fpB := genCert(t)
	srvCfg, err := ServerTLSConfig(certA, keyA)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(wssAddr(port), srvCfg); err != nil {
		t.Fatalf("listen: %v", err)
	}
	cliCfg, err := ClientTLSConfig("", "127.0.0.1", fpB)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	dialExpectErr(t, wssAddr(port), cliCfg, "", "fingerprint mismatch")
}

func TestWSSRejectsPlaintextDial(t *testing.T) {
	port := freePort(t)
	cert, key, _ := genCert(t)
	srvCfg, err := ServerTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(wssAddr(port), srvCfg); err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Dial the wss server as plain ws://: the TLS listener rejects the
	// plaintext WebSocket upgrade and the dial fails.
	dialExpectErr(t, wsAddr(port), nil, "", "plaintext dial to wss server")
}

func TestWSSProxyRoundTrip(t *testing.T) {
	port := freePort(t)
	cert, key, fp := genCert(t)
	srvCfg, err := ServerTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	r, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer r.Close()
	if err := r.Listen(wssAddr(port), srvCfg); err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxyURL := startConnectProxy(t)
	cliCfg, err := ClientTLSConfig("", "127.0.0.1", fp)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	w, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer w.Close()
	if err := w.Dial(wssAddr(port), cliCfg, proxyURL); err != nil {
		t.Fatalf("dial via proxy: %v", err)
	}
	exerciseRoundTrip(t, r, w)
}

// startConnectProxy runs a minimal HTTP CONNECT proxy on a random loopback
// port and returns its URL. It tunnels any CONNECT target byte-for-byte.
func startConnectProxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConnect(conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

func handleConnect(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		return
	}
	dst, err := net.Dial("tcp", req.URL.Host)
	if err != nil {
		return
	}
	defer dst.Close()
	if _, err := fmt.Fprint(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	go io.Copy(dst, conn)
	io.Copy(conn, dst)
}
