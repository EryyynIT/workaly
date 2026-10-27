package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Catalog bodies stay small; the limit keeps a malformed request from hogging memory.
const grpcMaxMessageSize = 8 << 10

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("task service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := openDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}

	healthServer := health.NewServer()
	// Honest start: NOT_SERVING until the monitor proves the database answers.
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	healthServer.SetServingStatus("task.TaskService", healthpb.HealthCheckResponse_NOT_SERVING)
	monitorCtx, cancelMonitor := context.WithCancel(context.Background())
	defer cancelMonitor()
	go monitorHealth(monitorCtx, db, healthServer)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(grpcMaxMessageSize),
		grpc.MaxSendMsgSize(grpcMaxMessageSize),
	)
	healthpb.RegisterHealthServer(server, healthServer)

	go func() {
		<-ctx.Done()
		stopped := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			server.Stop()
		}
	}()

	slog.Info("task service starting", "address", cfg.GRPCAddr)
	if err := server.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

func monitorHealth(ctx context.Context, db *sql.DB, healthServer *health.Server) {
	set := func(serving bool) {
		status := healthpb.HealthCheckResponse_NOT_SERVING
		if serving {
			status = healthpb.HealthCheckResponse_SERVING
		}
		healthServer.SetServingStatus("", status)
		healthServer.SetServingStatus("task.TaskService", status)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := db.PingContext(pingCtx)
		cancel()
		set(err == nil)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
