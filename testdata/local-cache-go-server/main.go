// Command local-cache-go-server runs the production cache service without a
// manager for isolated, signed-client acceptance tests. It is not installed by
// the Runscaler build or started by a normal manager configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ysya/runscaler/internal/actionscache"
	"github.com/ysya/runscaler/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if os.Getenv("RUNSCALER_NATIVE_CACHE_FIXTURE") != "1" {
		return errors.New("isolated cache fixture must be explicitly enabled")
	}
	listen := flag.String("listen-address", "", "private IP and port inside the isolated network")
	advertise := flag.String("advertise-url", "", "cache URL reachable by the fixture runners")
	dataDir := flag.String("data-dir", "", "fresh private fixture data directory")
	maxBytes := flag.Int64("max-size-bytes", 2<<30, "local cache byte budget")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional argument")
	}
	cfg := config.ActionsCacheConfig{
		Enabled: true, ListenAddress: *listen, AdvertiseURL: *advertise,
		DataDir: *dataDir, MaxSizeBytes: *maxBytes,
	}
	service, err := actionscache.NewService(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Start(ctx); err != nil {
		_ = service.Close()
		return err
	}
	fmt.Fprintln(os.Stdout, "native cache fixture ready")
	var failure error
	select {
	case <-ctx.Done():
	case failure = <-service.Failures():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return failure
}
