package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	jobpb "github.com/EryyynIT/workaly/job-service/pb/jobpb"

	"github.com/EryyynIT/workaly/catalogkit"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("job service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := catalogkit.LoadConfig(os.Getenv, "127.0.0.1:50052")
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
	healthServer := catalogkit.MonitorHealth(monitorCtx, db, "job.JobService")

	jobServer := &jobService{db: db}
	return catalogkit.Serve(ctx, cfg.GRPCAddr, healthServer, func(server *grpc.Server) {
		jobpb.RegisterJobServiceServer(server, jobServer)
	})
}
