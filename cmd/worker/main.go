// Command worker runs the worker daemon: an outbound NNG dial to the central
// router that proxies dispatched requests to a local OpenAI-compatible
// endpoint and streams tokens back across the mesh.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/samgw/linguine/internal/catalog"
	"github.com/samgw/linguine/internal/config"
	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/worker"
)

func main() {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file")
	_ = fs.Parse(os.Args[1:])

	cfg, err := config.LoadWorker(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := engine.NewProxyEngine(cfg.Engine.URL)
	// The catalog probe needs the engine base URL (it appends /v1/models);
	// strip the /v1/chat/completions suffix from the configured engine URL.
	baseURL := strings.TrimSuffix(cfg.Engine.URL, "/v1/chat/completions")
	probe := catalog.NewProbe(baseURL)
	go probe.Run(ctx)

	var meshTLS *tls.Config
	if strings.HasPrefix(cfg.Router.NNGAddr, "wss://") {
		meshTLS, err = mesh.ClientTLSConfig(cfg.Router.TLSCAFile, meshServerName(cfg.Router.NNGAddr), cfg.Router.TLSFingerprint)
		if err != nil {
			log.Fatalf("build mesh tls config: %v", err)
		}
	}
	d, err := worker.NewDaemon(cfg.Router.NNGAddr, cfg.NodeID, cfg.EnrollmentToken, eng,
		worker.WithProbe(probe),
		worker.WithTLSConfig(meshTLS),
		worker.WithProxyURL(cfg.Router.HTTPProxy))
	if err != nil {
		log.Fatalf("create daemon: %v", err)
	}
	defer d.Close()

	go func() {
		if err := d.Run(ctx); err != nil {
			log.Printf("[linguine] daemon: %v", err)
		}
	}()
	log.Printf("[linguine] worker %s connected to router %s, proxying to %s", cfg.NodeID, cfg.Router.NNGAddr, cfg.Engine.URL)

	<-ctx.Done()
	log.Printf("[linguine] worker shutting down...")
}

// meshServerName extracts the hostname from a ws:// or wss:// mesh address
// for TLS ServerName verification (skipped when a fingerprint pin is set).
func meshServerName(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
