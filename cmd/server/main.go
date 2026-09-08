package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ramkirangaruda/strata"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := configFromEnv()

	db, err := strata.Open(cfg.dataDir, strata.Options{Sync: cfg.sync})
	if err != nil {
		log.Error("failed to open database", "dir", cfg.dataDir, "err", err)
		os.Exit(1)
	}
	defer db.Close()

	srv := newServer(db, log)
	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Shut down on SIGINT/SIGTERM rather than dying mid-request. strata does
	// not need a clean shutdown to be safe - recovery on the next Open covers
	// a hard kill just as well, per the crash-recovery tests in db_test.go -
	// but an in-flight HTTP request deserves a response rather than a reset
	// connection, so we drain the server before closing the database anyway.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", cfg.addr, "data_dir", cfg.dataDir, "sync", cfg.sync)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}
