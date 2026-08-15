package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// TLSFiles holds optional TLS material for a listener.
type TLSFiles struct {
	Enabled  bool   `toml:"enabled"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
	CAFile   string `toml:"ca_file"`
}

// HTTPConfig is the public OpenAI-compatible ingress.
type HTTPConfig struct {
	Listen string   `toml:"listen"`
	TLS    TLSFiles `toml:"tls"`
}

// DBConfig is the embedded SQLite control-plane database.
type DBConfig struct {
	Path string `toml:"path"`
}

// SignerConfig is the persisted Ed25519 key used for PASETO enrollment tokens.
type SignerConfig struct {
	KeyPath string `toml:"key_path"`
}

// NNGConfig is the worker-mesh listener.
type NNGConfig struct {
	Listen string   `toml:"listen"`
	TLS    TLSFiles `toml:"tls"`
}

// AdminConfig is the admin dashboard's configuration. The dashboard
// runs on a separate localhost-only listener for a reverse proxy to
// terminate TLS in front of.
type AdminConfig struct {
	Listen            string `toml:"listen"`
	SessionSecretPath string `toml:"session_secret_path"`
}

// RouterConfig is the central router's configuration.
type RouterConfig struct {
	HTTP   HTTPConfig   `toml:"http"`
	DB     DBConfig     `toml:"db"`
	Signer SignerConfig `toml:"signer"`
	NNG    NNGConfig    `toml:"nng"`
	Admin  AdminConfig  `toml:"admin"`
}

func defaultRouterConfig() *RouterConfig {
	return &RouterConfig{
		HTTP: HTTPConfig{
			Listen: "0.0.0.0:8443",
			// TLS disabled by default: serve plain HTTP for local/reverse-proxy
			// use. Operators enable TLS and provide cert/key for production.
			TLS: TLSFiles{Enabled: false},
		},
		DB:     DBConfig{Path: "linguine.db"},
		Signer: SignerConfig{KeyPath: "linguine-signer.key"},
		NNG: NNGConfig{
			// Plaintext ws:// on loopback by default; a TLS-terminating reverse
			// proxy (nginx/Caddy) fronts it in production so the mesh, /v1 and
			// /admin share one public 443. Set wss:// + nng.tls to terminate
			// TLS in-process instead.
			Listen: "ws://127.0.0.1:9000/mesh",
		},
		Admin: AdminConfig{
			// Localhost-only, plain HTTP; a reverse proxy terminates TLS.
			Listen:            "127.0.0.1:8444",
			SessionSecretPath: "linguine-admin.key",
		},
	}
}

// LoadRouter reads a TOML config at path (optional), applies defaults and then
// environment overrides, and validates the result. A missing path yields the
// defaulted, env-overridden config.
func LoadRouter(path string) (*RouterConfig, error) {
	cfg := defaultRouterConfig()
	if err := readTOML(path, cfg); err != nil {
		return nil, err
	}
	applyRouterEnv(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks required fields and TLS material consistency.
func (c *RouterConfig) Validate() error {
	if c.HTTP.TLS.Enabled {
		if c.HTTP.TLS.CertFile == "" || c.HTTP.TLS.KeyFile == "" {
			return errors.New("config: http.tls.enabled requires cert_file and key_file")
		}
	}
	switch {
	case strings.HasPrefix(c.NNG.Listen, "wss://"):
		// wss:// terminates TLS in-process. cert/key are optional: when both
		// are absent the router auto-generates a self-signed cert and logs
		// its fingerprint for workers to pin. If one is set, both must be.
		if (c.NNG.TLS.CertFile == "") != (c.NNG.TLS.KeyFile == "") {
			return errors.New("config: nng wss:// requires both nng.tls.cert_file and key_file, or neither")
		}
	case strings.HasPrefix(c.NNG.Listen, "ws://"):
		// Plaintext ws:// — front with a TLS-terminating reverse proxy in
		// production; the mesh is unencrypted on the wire as presented.
	default:
		return fmt.Errorf("config: nng.listen must use ws:// or wss://, got %q", c.NNG.Listen)
	}
	return nil
}

func applyRouterEnv(c *RouterConfig) {
	envStr("LINGUINE_ROUTER_HTTP_LISTEN", &c.HTTP.Listen)
	envBool("LINGUINE_ROUTER_HTTP_TLS_ENABLED", &c.HTTP.TLS.Enabled)
	envStr("LINGUINE_ROUTER_HTTP_TLS_CERT", &c.HTTP.TLS.CertFile)
	envStr("LINGUINE_ROUTER_HTTP_TLS_KEY", &c.HTTP.TLS.KeyFile)
	envStr("LINGUINE_ROUTER_HTTP_TLS_CA", &c.HTTP.TLS.CAFile)
	envStr("LINGUINE_ROUTER_DB_PATH", &c.DB.Path)
	envStr("LINGUINE_ROUTER_SIGNER_KEY_PATH", &c.Signer.KeyPath)
	envStr("LINGUINE_ROUTER_NNG_LISTEN", &c.NNG.Listen)
	envStr("LINGUINE_ROUTER_NNG_TLS_CERT", &c.NNG.TLS.CertFile)
	envStr("LINGUINE_ROUTER_NNG_TLS_KEY", &c.NNG.TLS.KeyFile)
	envStr("LINGUINE_ROUTER_NNG_TLS_CA", &c.NNG.TLS.CAFile)
	envStr("LINGUINE_ROUTER_ADMIN_LISTEN", &c.Admin.Listen)
	envStr("LINGUINE_ROUTER_ADMIN_SESSION_SECRET_PATH", &c.Admin.SessionSecretPath)
}

// WorkerRouterConfig is the worker's view of the central router.
type WorkerRouterConfig struct {
	NNGAddr        string `toml:"nng_addr"`
	TLSCAFile      string `toml:"tls_ca_file"`
	TLSFingerprint string `toml:"tls_fingerprint"` // sha256/base64 pin for a self-signed router cert
	HTTPProxy      string `toml:"http_proxy"`       // CONNECT proxy URL; empty -> env vars
}

// WorkerEngineConfig is the local OpenAI-compatible endpoint the worker proxies to.
type WorkerEngineConfig struct {
	URL string `toml:"url"`
}

// WorkerConfig is the worker daemon's configuration.
type WorkerConfig struct {
	NodeID          string             `toml:"node_id"`
	EnrollmentToken string             `toml:"enrollment_token"`
	Router          WorkerRouterConfig `toml:"router"`
	Engine          WorkerEngineConfig `toml:"engine"`
}

func defaultWorkerConfig() *WorkerConfig {
	return &WorkerConfig{
		Router: WorkerRouterConfig{
			NNGAddr: "wss://127.0.0.1:9000/mesh",
		},
		Engine: WorkerEngineConfig{
			URL: "http://127.0.0.1:8080/v1/chat/completions",
		},
	}
}

// LoadWorker reads a TOML config at path (optional), applies defaults and then
// environment overrides, and validates the result.
func LoadWorker(path string) (*WorkerConfig, error) {
	cfg := defaultWorkerConfig()
	if err := readTOML(path, cfg); err != nil {
		return nil, err
	}
	applyWorkerEnv(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the required worker fields.
func (c *WorkerConfig) Validate() error {
	if c.NodeID == "" {
		return errors.New("config: worker node_id is required")
	}
	if c.EnrollmentToken == "" {
		return errors.New("config: worker enrollment_token is required")
	}
	if c.Router.NNGAddr == "" {
		return errors.New("config: worker router.nng_addr is required")
	}
	if c.Engine.URL == "" {
		return errors.New("config: worker engine.url is required")
	}
	if c.Router.HTTPProxy != "" {
		if _, err := url.Parse(c.Router.HTTPProxy); err != nil {
			return fmt.Errorf("config: worker router.http_proxy: %w", err)
		}
	}
	return nil
}

func applyWorkerEnv(c *WorkerConfig) {
	envStr("LINGUINE_WORKER_NODE_ID", &c.NodeID)
	envStr("LINGUINE_WORKER_ENROLLMENT_TOKEN", &c.EnrollmentToken)
	envStr("LINGUINE_WORKER_ROUTER_NNG_ADDR", &c.Router.NNGAddr)
	envStr("LINGUINE_WORKER_ROUTER_TLS_CA", &c.Router.TLSCAFile)
	envStr("LINGUINE_WORKER_ROUTER_TLS_FINGERPRINT", &c.Router.TLSFingerprint)
	envStr("LINGUINE_WORKER_ROUTER_HTTP_PROXY", &c.Router.HTTPProxy)
	envStr("LINGUINE_WORKER_ENGINE_URL", &c.Engine.URL)
}

// readTOML decodes path into dst when it exists; a missing path is not an error.
func readTOML(path string, dst interface{}) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func envBool(key string, dst *bool) {
	if v, ok := os.LookupEnv(key); ok {
		if b, ok := parseBoolEnv(v); ok {
			*dst = b
		}
	}
}

func parseBoolEnv(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}
