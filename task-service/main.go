package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/EryyynIT/workaly/catalogkit"
	taskpb "github.com/EryyynIT/workaly/task-service/pb/taskpb"
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

	taskServer := &taskService{db: db}
	return catalogkit.Serve(ctx, cfg.GRPCAddr, healthServer, func(server *grpc.Server) {
		taskpb.RegisterTaskServiceServer(server, taskServer)
	})
}
