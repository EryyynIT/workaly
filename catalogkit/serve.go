package catalogkit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Catalog list pages carry up to 50 rows with description bodies of a few
// KB each; the limit must fit a maximal page (~350 KB) while still keeping a
// malformed request from hogging memory. 1 MiB covers it with headroom.
const grpcMaxMessageSize = 1 << 20

// Serve wires the common gRPC surface of a catalog service: listener, message
// size limits, standard health registration and graceful shutdown. register
// adds domain services to the server.
func Serve(ctx context.Context, addr string, healthServer *health.Server, register func(*grpc.Server)) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(grpcMaxMessageSize),
		grpc.MaxSendMsgSize(grpcMaxMessageSize),
	)
	healthpb.RegisterHealthServer(server, healthServer)
	if register != nil {
		register(server)
	}

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

	if err := server.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
