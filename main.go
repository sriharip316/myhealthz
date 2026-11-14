// myhealthz is an HTTP health check server for MySQL and MariaDB that tells
// load balancers whether the local server is acting as primary or replica.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sriharip316/myhealthz/internal/config"
	"github.com/sriharip316/myhealthz/internal/httpserver"
	"github.com/sriharip316/myhealthz/internal/mysql"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("myhealthz: %v", err)
	}
}

func run(args []string) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return err
	}
	if cfg.Version {
		fmt.Printf("myhealthz version %s\n", version)
		return nil
	}

	ep := mysql.ResolveEndpoint(cfg)
	log.Printf("myhealthz: checking %s (chosen via %s)", ep, ep.Source)

	db, err := mysql.Open(cfg, ep)
	if err != nil {
		return err
	}
	defer db.Close()

	collector := mysql.NewCollector(db, cfg.QueryTimeout)

	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		// Not fatal: the sidecar should come up before the database and
		// report 503 until the checks succeed. The privilege preflight is
		// skipped; per-request errors carry the same remediation hints.
		log.Printf("myhealthz: warning: database not reachable yet (%v); serving 503s until it is", err)
	} else {
		log.Printf("myhealthz: database reachable")
		preCtx, preCancel := context.WithTimeout(context.Background(), cfg.QueryTimeout)
		defer preCancel()
		if err := collector.CheckPrivileges(preCtx); err != nil {
			log.Printf("myhealthz: WARNING: monitor user privileges incomplete: %v", err)
		}
	}

	// ReadTimeout/WriteTimeout/IdleTimeout are required alongside
	// ReadHeaderTimeout: with them zero, idle keep-alive connections (and the
	// goroutine + fd behind each) are never reaped. WriteTimeout covers
	// handler execution, which Facts bounds under a single -query-timeout
	// context, so it must stay above that value.
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpserver.New(collector, cfg.MinReplicas, cfg.MaxLagSeconds).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	log.Printf("myhealthz: listening on %s", cfg.ListenAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		log.Printf("myhealthz: received %s, shutting down", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
