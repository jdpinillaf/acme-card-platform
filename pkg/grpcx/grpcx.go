// Package grpcx runs gRPC servers with health checks, panic recovery and graceful shutdown,
// and dials internal clients.
package grpcx

import (
	"context"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// Serve listens on addr and serves until ctx is cancelled.
func Serve(ctx context.Context, addr string, register func(*grpc.Server)) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ServeListener(ctx, lis, register)
}

// ServeListener serves on lis until ctx is cancelled, then drains gracefully (max 10s).
func ServeListener(ctx context.Context, lis net.Listener, register func(*grpc.Server)) error {
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(recoverUnary))
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	register(srv)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	hs.Shutdown()
	stopped := make(chan struct{})
	go func() { srv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		srv.Stop()
	}
	return nil
}

func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "grpc panic", "method", info.FullMethod, "panic", p, "stack", string(debug.Stack()))
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return h(ctx, req)
}

// Dial opens a client connection to an internal service (plaintext inside the cluster network;
// TLS 1.3 terminates at the load balancer / service mesh in AWS).
func Dial(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}
