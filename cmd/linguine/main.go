// Command linguine runs the central router or performs admin actions.
//
// Usage:
//
//	linguine [--config FILE] serve
//	linguine [--config FILE] admin create-key --name <label>
//	linguine [--config FILE] admin create-enrollment-token --node <label> [--ttl <duration>]
//
// Global flags (before the subcommand) are parsed by the top-level flag set.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	adminpkg "github.com/samgw/linguine/internal/admin"
	"github.com/samgw/linguine/internal/audit"
	"github.com/samgw/linguine/internal/auth"
	"github.com/samgw/linguine/internal/config"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/router"
	"github.com/samgw/linguine/internal/store"
)

func main() {
	fs := flag.NewFlagSet("linguine", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file")
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()

	if len(args) == 0 {
		exitOnErr(serve(*configPath))
		return
	}
	switch args[0] {
	case "serve":
		exitOnErr(serve(*configPath))
	case "admin":
		exitOnErr(admin(*configPath, args[1:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: linguine [--config FILE] <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve                          run the central router (default)")
	fmt.Fprintln(os.Stderr, "  admin create-key                create an ingress API key")
	fmt.Fprintln(os.Stderr, "  admin create-enrollment-token    create a worker enrollment token")
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(configPath string) error {
	cfg, err := config.LoadRouter(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	warnInsecureConfig(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DB.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	signer, err := auth.LoadOrCreateSigner(cfg.Signer.KeyPath)
	if err != nil {
		return fmt.Errorf("load signer: %w", err)
	}

	auditRepo := audit.NewRepo(st.DB(), 256)
	defer auditRepo.Close()

	nng, err := mesh.NewRouter()
	if err != nil {
		return fmt.Errorf("create mesh router: %w", err)
	}
	defer nng.Close()
	var nngTLS *tls.Config
	if strings.HasPrefix(cfg.NNG.Listen, "wss://") {
		nngTLS, err = buildMeshServerTLS(cfg.NNG.TLS)
		if err != nil {
			return fmt.Errorf("mesh tls: %w", err)
		}
	}
	if err := nng.Listen(cfg.NNG.Listen, nngTLS); err != nil {
		return fmt.Errorf("mesh listen: %w", err)
	}

	srv := router.New(router.Deps{
		NNG:         nng,
		Signer:      signer,
		Keys:        auth.NewAPIKeyRepo(st.DB()),
		Enrollments: auth.NewEnrollmentRepo(st.DB(), signer),
		Audit:       auditRepo,
		DB:          st.DB(),
		HTTPListen:  cfg.HTTP.Listen,
		TLS:         cfg.HTTP.TLS,
	})
	ln, err := srv.Start(ctx)
	if err != nil {
		return fmt.Errorf("start router: %w", err)
	}
	log.Printf("[linguine] router HTTP on %s, mesh on %s", ln.Addr(), cfg.NNG.Listen)

	secret, err := adminpkg.LoadOrCreateSessionSecret(cfg.Admin.SessionSecretPath)
	if err != nil {
		return fmt.Errorf("load admin session secret: %w", err)
	}
	adminSrv := adminpkg.New(adminpkg.Deps{
		Keys:         auth.NewAPIKeyRepo(st.DB()),
		Audit:        auditRepo,
		Nodes:        srv.NodesSnapshot,
		Listen:       cfg.Admin.Listen,
		SessionSecret: secret,
	})
	adminLn, err := adminSrv.Start()
	if err != nil {
		return fmt.Errorf("start admin: %w", err)
	}
	log.Printf("[linguine] admin HTTP on %s", adminLn.Addr())

	<-ctx.Done()
	log.Printf("[linguine] shutting down...")
	return srv.Shutdown()
}

func admin(configPath string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("admin requires a subcommand: create-key | create-enrollment-token")
	}
	switch args[0] {
	case "create-key":
		return adminCreateKey(configPath, args[1:])
	case "create-enrollment-token":
		return adminCreateEnrollment(configPath, args[1:])
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func adminCreateKey(configPath string, args []string) error {
	fs := flag.NewFlagSet("admin create-key", flag.ExitOnError)
	name := fs.String("name", "", "human-readable label for the API key")
	role := fs.String("role", "client", "key role: client or admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	switch *role {
	case "client", "admin":
	default:
		return fmt.Errorf("--role must be client or admin, got %q", *role)
	}
	cfg, err := config.LoadRouter(configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DB.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	raw := auth.GenerateAPIKey()
	ak, err := auth.NewAPIKeyRepo(st.DB()).Create(ctx, *name, raw)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	if *role == "admin" {
		if _, err := st.DB().Exec(`UPDATE api_keys SET role = 'admin' WHERE id = ?`, ak.ID); err != nil {
			return fmt.Errorf("set admin role: %w", err)
		}
	}
	fmt.Printf("API key (shown once — store it securely):\n  %s\nid:    %s\nname:  %s\nrole:  %s\n", raw, ak.ID, ak.Name, *role)
	return nil
}

func adminCreateEnrollment(configPath string, args []string) error {
	fs := flag.NewFlagSet("admin create-enrollment-token", flag.ExitOnError)
	node := fs.String("node", "", "expected hostname/label for the worker")
	ttl := fs.Duration("ttl", 0, "token lifetime (0 = no expiry)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *node == "" {
		return fmt.Errorf("--node is required")
	}
	cfg, err := config.LoadRouter(configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DB.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	signer, err := auth.LoadOrCreateSigner(cfg.Signer.KeyPath)
	if err != nil {
		return fmt.Errorf("load signer: %w", err)
	}
	et, token, err := auth.NewEnrollmentRepo(st.DB(), signer).Create(ctx, *node, *ttl)
	if err != nil {
		return fmt.Errorf("create enrollment token: %w", err)
	}
	fmt.Printf("Enrollment token (shown once — pass it to the worker):\n  %s\nid:    %s\nnode:  %s\n", token, et.ID, et.NodeName)
	return nil
}

// buildMeshServerTLS builds the in-process TLS config for a wss:// mesh
// listener. Configured cert/key files are loaded if present; otherwise a
// self-signed certificate is generated and persisted, and its fingerprint is
// logged for workers to pin in router.tls_fingerprint.
func buildMeshServerTLS(t config.TLSFiles) (*tls.Config, error) {
	certFile, keyFile := t.CertFile, t.KeyFile
	if certFile == "" {
		certFile = "linguine-mesh-cert.pem"
	}
	if keyFile == "" {
		keyFile = "linguine-mesh-key.pem"
	}
	cert, fp, generated, err := mesh.LoadOrCreateSelfSignedCert(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if generated {
		log.Printf("[linguine] generated self-signed mesh cert at %s/%s; fingerprint %s — pin this in each worker's router.tls_fingerprint", certFile, keyFile, fp)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// warnInsecureConfig logs warnings for deployment configurations that expose
// credentials or the admin plane in cleartext. It does not stop the serve.
func warnInsecureConfig(cfg *config.RouterConfig) {
	if !cfg.HTTP.TLS.Enabled && !isLoopbackHostPort(cfg.HTTP.Listen) {
		log.Printf("[linguine] WARNING: http.listen %s is non-loopback with TLS disabled; client API keys travel in cleartext. Enable http.tls or front /v1 with a TLS-terminating reverse proxy.", cfg.HTTP.Listen)
	}
	if !isLoopbackHostPort(cfg.Admin.Listen) {
		log.Printf("[linguine] WARNING: admin.listen %s is non-loopback; the admin dashboard is plaintext HTTP with no TLS of its own. Bind loopback and front with a reverse proxy.", cfg.Admin.Listen)
	}
	if isPlaintextMeshNonLoopback(cfg.NNG.Listen) {
		log.Printf("[linguine] WARNING: nng.listen %s is a non-loopback plaintext ws:// mesh; traffic is unencrypted. Use wss:// or front /mesh with a TLS reverse proxy.", cfg.NNG.Listen)
	}
}

// isLoopbackHostPort reports whether a "host:port" address binds only the
// loopback interface. An empty host (":port") binds all interfaces and is
// therefore treated as non-loopback.
func isLoopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// isPlaintextMeshNonLoopback reports whether a mesh listen address is a
// plaintext ws:// bound to a non-loopback interface.
func isPlaintextMeshNonLoopback(addr string) bool {
	if !strings.HasPrefix(addr, "ws://") {
		return false
	}
	u, err := url.Parse(addr)
	if err != nil {
		return false
	}
	return !isLoopbackHostPort(u.Host)
}
