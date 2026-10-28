package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/EryyynIT/workaly/catalogkit"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("task service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := catalogkit.LoadConfig(os.Getenv, "127.0.0.1:50053")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := catalogkit.OpenDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := catalogkit.Migrate(ctx, db, migrations); err != nil {
		return err
	}

	// Separate context: the monitor must run until the process exits, not
	// until the signal context is cancelled by shutdown.
	monitorCtx, cancelMonitor := context.WithCancel(context.Background())
	defer cancelMonitor()
	healthServer := catalogkit.MonitorHealth(monitorCtx, db, "task.TaskService")

	return catalogkit.Serve(ctx, cfg.GRPCAddr, healthServer, nil)
}
