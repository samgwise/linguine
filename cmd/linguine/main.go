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
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
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
	if err := nng.Listen(cfg.NNG.Listen); err != nil {
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
