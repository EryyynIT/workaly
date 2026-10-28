package catalogkit

import (
	"context"
	"database/sql"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// MonitorHealth wires a health server to database liveness. Honest start: it
// reports NOT_SERVING for the empty service and service until the monitor
// proves the database answers PingContext, then follows it once per second.
// The monitor stops when ctx is cancelled.
func MonitorHealth(ctx context.Context, db *sql.DB, service string) *health.Server {
	healthServer := health.NewServer()
	set := func(serving bool) {
		status := healthpb.HealthCheckResponse_NOT_SERVING
		if serving {
			status = healthpb.HealthCheckResponse_SERVING
		}
		healthServer.SetServingStatus("", status)
		healthServer.SetServingStatus(service, status)
	}
	set(false)
	go func() {
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
	}()
	return healthServer
}
