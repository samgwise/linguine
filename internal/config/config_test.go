package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTOML(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	return path
}

func TestRouterDefaults(t *testing.T) {
	cfg, err := LoadRouter("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Listen != "0.0.0.0:8443" {
		t.Errorf("http.listen default: got %q", cfg.HTTP.Listen)
	}
	if cfg.HTTP.TLS.Enabled {
		t.Error("http.tls.enabled default should be false")
	}
	if cfg.DB.Path != "linguine.db" {
		t.Errorf("db.path default: got %q", cfg.DB.Path)
	}
	if cfg.NNG.Listen != "ws://127.0.0.1:9000/mesh" {
		t.Errorf("nng.listen default: got %q", cfg.NNG.Listen)
	}
}

func TestRouterTOMLOverridesDefaults(t *testing.T) {
	path := writeTOML(t, "router.toml", `
http.listen = "127.0.0.1:9000"
http.tls.enabled = true
http.tls.cert_file = "/etc/cert.pem"
http.tls.key_file = "/etc/key.pem"
db.path = "/var/linguine.db"
nng.listen = "wss://0.0.0.0:9000/mesh"
nng.tls.cert_file = "/etc/nng-cert.pem"
nng.tls.key_file = "/etc/nng-key.pem"
`)
	cfg, err := LoadRouter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Listen != "127.0.0.1:9000" {
		t.Errorf("http.listen: got %q", cfg.HTTP.Listen)
	}
	if !cfg.HTTP.TLS.Enabled {
		t.Error("http.tls.enabled should be true")
	}
	if cfg.DB.Path != "/var/linguine.db" {
		t.Errorf("db.path: got %q", cfg.DB.Path)
	}
	if cfg.NNG.Listen != "wss://0.0.0.0:9000/mesh" {
		t.Errorf("nng.listen: got %q", cfg.NNG.Listen)
	}
	// Defaults not overridden must survive the unmarshal merge.
	if cfg.Signer.KeyPath != "linguine-signer.key" {
		t.Errorf("signer.key_path default lost: got %q", cfg.Signer.KeyPath)
	}
}

func TestRouterEnvOverridesTOML(t *testing.T) {
	path := writeTOML(t, "router.toml", `http.listen = "127.0.0.1:9000"`)
	t.Setenv("LINGUINE_ROUTER_HTTP_LISTEN", "0.0.0.0:7000")
	t.Setenv("LINGUINE_ROUTER_DB_PATH", "/env/linguine.db")

	cfg, err := LoadRouter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Listen != "0.0.0.0:7000" {
		t.Errorf("env http.listen should win: got %q", cfg.HTTP.Listen)
	}
	if cfg.DB.Path != "/env/linguine.db" {
		t.Errorf("env db.path should win: got %q", cfg.DB.Path)
	}
}

func TestRouterEnvBool(t *testing.T) {
	t.Setenv("LINGUINE_ROUTER_HTTP_TLS_ENABLED", "true")
	t.Setenv("LINGUINE_ROUTER_HTTP_TLS_CERT", "/c.pem")
	t.Setenv("LINGUINE_ROUTER_HTTP_TLS_KEY", "/k.pem")
	cfg, err := LoadRouter("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.HTTP.TLS.Enabled {
		t.Error("env should enable http.tls")
	}
}

func TestRouterValidateTLSRequiresCerts(t *testing.T) {
	t.Setenv("LINGUINE_ROUTER_HTTP_TLS_ENABLED", "true")
	// No cert/key set.
	if _, err := LoadRouter(""); err == nil {
		t.Error("expected validation error when TLS enabled without cert/key")
	}
}

func TestRouterValidateNNGWSSRequiresBothCertAndKey(t *testing.T) {
	t.Setenv("LINGUINE_ROUTER_NNG_LISTEN", "wss://0.0.0.0:9000/mesh")
	t.Setenv("LINGUINE_ROUTER_NNG_TLS_CERT", "/etc/nng-cert.pem")
	// key not set: wss:// with a cert but no key is a misconfiguration.
	if _, err := LoadRouter(""); err == nil {
		t.Error("expected validation error when nng wss:// has cert but no key")
	}
}

func TestRouterValidateNNGWSSAcceptsNoCerts(t *testing.T) {
	t.Setenv("LINGUINE_ROUTER_NNG_LISTEN", "wss://0.0.0.0:9000/mesh")
	// Neither cert nor key: valid — the router auto-generates a self-signed cert.
	if _, err := LoadRouter(""); err != nil {
		t.Errorf("wss:// with no cert/key should validate (auto-gen): %v", err)
	}
}

func TestRouterRejectsNonWSMeshScheme(t *testing.T) {
	t.Setenv("LINGUINE_ROUTER_NNG_LISTEN", "tls+tcp://0.0.0.0:9000")
	if _, err := LoadRouter(""); err == nil {
		t.Error("expected validation error for a non-ws/wss mesh scheme")
	}
}

func TestWorkerDefaults(t *testing.T) {
	t.Setenv("LINGUINE_WORKER_NODE_ID", "node-1")
	t.Setenv("LINGUINE_WORKER_ENROLLMENT_TOKEN", "tok")
	cfg, err := LoadWorker("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Router.NNGAddr != "wss://127.0.0.1:9000/mesh" {
		t.Errorf("router.nng_addr default: got %q", cfg.Router.NNGAddr)
	}
	if cfg.Engine.URL != "http://127.0.0.1:8080/v1/chat/completions" {
		t.Errorf("engine.url default: got %q", cfg.Engine.URL)
	}
}

func TestWorkerValidateRequiresFields(t *testing.T) {
	// No node_id set.
	if _, err := LoadWorker(""); err == nil {
		t.Error("expected validation error when worker node_id missing")
	}
}

func TestWorkerProxyAndFingerprintEnv(t *testing.T) {
	t.Setenv("LINGUINE_WORKER_NODE_ID", "node-1")
	t.Setenv("LINGUINE_WORKER_ENROLLMENT_TOKEN", "tok")
	t.Setenv("LINGUINE_WORKER_ROUTER_HTTP_PROXY", "http://corp-proxy:3128")
	t.Setenv("LINGUINE_WORKER_ROUTER_TLS_FINGERPRINT", "sha256/abc=")
	cfg, err := LoadWorker("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Router.HTTPProxy != "http://corp-proxy:3128" {
		t.Errorf("http_proxy: got %q", cfg.Router.HTTPProxy)
	}
	if cfg.Router.TLSFingerprint != "sha256/abc=" {
		t.Errorf("tls_fingerprint: got %q", cfg.Router.TLSFingerprint)
	}
}

func TestWorkerRejectsBadProxyURL(t *testing.T) {
	t.Setenv("LINGUINE_WORKER_NODE_ID", "node-1")
	t.Setenv("LINGUINE_WORKER_ENROLLMENT_TOKEN", "tok")
	t.Setenv("LINGUINE_WORKER_ROUTER_HTTP_PROXY", "http://corp proxy:3128") // space in host is invalid
	if _, err := LoadWorker(""); err == nil {
		t.Error("expected validation error for an unparseable http_proxy")
	}
}

func TestWorkerEnvOverrides(t *testing.T) {
	path := writeTOML(t, "worker.toml", `
node_id = "from-file"
enrollment_token = "from-file-token"
router.nng_addr = "wss://file:9000/mesh"
engine.url = "http://file:8080/v1/chat/completions"
`)
	t.Setenv("LINGUINE_WORKER_NODE_ID", "from-env")
	t.Setenv("LINGUINE_WORKER_ENGINE_URL", "http://env:9000/v1/chat/completions")

	cfg, err := LoadWorker(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.NodeID != "from-env" {
		t.Errorf("env node_id should win: got %q", cfg.NodeID)
	}
	if cfg.EnrollmentToken != "from-file-token" {
		t.Errorf("file enrollment_token should remain: got %q", cfg.EnrollmentToken)
	}
	if cfg.Engine.URL != "http://env:9000/v1/chat/completions" {
		t.Errorf("env engine.url should win: got %q", cfg.Engine.URL)
	}
}
