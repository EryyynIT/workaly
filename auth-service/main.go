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

	jobpb "github.com/EryyynIT/workaly/auth-service/pb/jobpb"
	taskpb "github.com/EryyynIT/workaly/auth-service/pb/taskpb"
	pb "github.com/EryyynIT/workaly/auth-service/pb/userpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("auth service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	db, err := openDB(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(context.Background(), db); err != nil {
		return err
	}
	// shortcut: plaintext gRPC is local-only; require service authentication/TLS before public deployment.
	userConn, err := grpc.NewClient(cfg.UserServiceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer userConn.Close()
	jobConn, err := grpc.NewClient(cfg.JobServiceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer jobConn.Close()
	taskConn, err := grpc.NewClient(cfg.TaskServiceAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer taskConn.Close()
	app := &application{
		cfg:        cfg,
		db:         db,
		users:      pb.NewUserServiceClient(userConn),
		vacancies:  jobpb.NewJobServiceClient(jobConn),
		orders:     taskpb.NewTaskServiceClient(taskConn),
		health:     healthpb.NewHealthClient(userConn),
		jobHealth:  healthpb.NewHealthClient(jobConn),
		taskHealth: healthpb.NewHealthClient(taskConn),
		mail:       mailerFromEnv(),
		limiter:    newRateLimiter(),
	}
	app.startProfileRetryWorker(context.Background())
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stopped := make(chan error, 1)
	go func() { stopped <- server.ListenAndServe() }()
	slog.Info("auth service starting", "address", cfg.HTTPAddr)
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-stopped; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
