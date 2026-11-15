//go:build linux

// Command agent is the node-level gRPC daemon that performs vDSO clock
// injection on behalf of the epochd controller.
//
// It runs as a privileged DaemonSet pod with hostPID: true and CAP_SYS_PTRACE,
// which allows it to attach to any process on the node. That is exactly why its
// API is not open: it requires a bearer token (--auth-token-file) and/or
// mutual TLS (--tls-client-ca-file), and refuses to start with neither unless
// told --allow-unauthenticated.
//
// The controller reaches each agent directly by pod IP:
//
//	grpc://<node-agent-pod-ip>:9433
//
// Usage:
//
//	agent [--listen=:9433] [--auth-token-file=PATH]
//	      [--tls-cert-file=PATH --tls-key-file=PATH [--tls-client-ca-file=PATH]]
//	      [--allow-unauthenticated]
package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentpb"
	applog "github.com/bkaznowski/epochd/pkg/log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// defaultListen is deliberately not 9100: that is node-exporter's default port,
// which the agent's hostNetwork/hostPort would collide with on any cluster that
// runs it.
const defaultListen = ":9433"

func main() {
	listen := flag.String("listen", defaultListen, "gRPC listen address")
	var cfg config
	flag.StringVar(&cfg.TokenFile, "auth-token-file", "", "file holding the bearer token callers must present")
	flag.StringVar(&cfg.TLSCertFile, "tls-cert-file", "", "serve TLS with this certificate (needs --tls-key-file)")
	flag.StringVar(&cfg.TLSKeyFile, "tls-key-file", "", "private key for --tls-cert-file")
	flag.StringVar(&cfg.TLSClientCAFile, "tls-client-ca-file", "", "require client certificates signed by this CA (mutual TLS)")
	flag.BoolVar(&cfg.AllowUnauthenticated, "allow-unauthenticated", false, "run without any authentication (development only)")
	flag.Parse()

	logger := applog.New().With("component", "agent")

	opts, err := cfg.serverOptions(logger)
	if err != nil {
		logger.Error("configuration", "err", err)
		os.Exit(1)
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("listen", "addr", *listen, "err", err)
		os.Exit(1)
	}

	svc := newServer(logger)
	srv := grpc.NewServer(opts...)
	agentpb.RegisterAgentServiceServer(srv, svc)
	reflection.Register(srv) // lets grpcurl/grpc-health-probe work without a proto file; behind the same auth

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		logger.Info("received shutdown signal")
		srv.GracefulStop()
	}()

	logger.Info("listening", "addr", *listen, "security", cfg.String())
	if err := srv.Serve(lis); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}

	// GracefulStop has drained in-flight RPCs; now reset all injected handles so
	// target processes are not left running with a stale fake clock.
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	svc.drain(drainCtx)
	logger.Info("shutdown complete")
}
