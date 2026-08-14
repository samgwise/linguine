// Command worker runs the worker daemon: an outbound NNG dial to the central
// router that proxies dispatched requests to a local OpenAI-compatible
// endpoint and streams tokens back across the mesh.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/samgw/linguine/internal/config"
	"github.com/samgw/linguine/internal/engine"
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
	d, err := worker.NewDaemon(cfg.Router.NNGAddr, cfg.NodeID, cfg.EnrollmentToken, eng)
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
