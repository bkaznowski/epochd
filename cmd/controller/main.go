// Command controller is the epochd HTTP+JSON service that resolves pods and
// orchestrates time injection via per-node gRPC agents.
//
// The API can change the clock of any pod the controller can list, so it
// requires a bearer token (--auth-token-file) and refuses to start without one
// unless told --allow-unauthenticated. The agents it drives are privileged and
// protected the same way; --agent-token-file and the --agent-* TLS flags give
// the controller its credentials for them.
//
// Usage:
//
//	controller [--listen=:8080] [--agent-port=9433] [--sweep-interval=30s] [--kubeconfig=PATH]
//	           [--auth-token-file=PATH | --allow-unauthenticated]
//	           [--agent-token-file=PATH] [--agent-ca-file=PATH]
//	           [--agent-client-cert-file=PATH --agent-client-key-file=PATH]
//	           [--agent-server-name=NAME] [--agent-timeout=15s] [--agent-concurrency=8]
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentclient"
	applog "github.com/bkaznowski/epochd/pkg/log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	agentPort := flag.String("agent-port", "9433", "gRPC port of node agents")
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig (empty = in-cluster)")
	sweepInterval := flag.Duration("sweep-interval", 30*time.Second, "how often to sweep expired skews")
	concurrency := flag.Int("agent-concurrency", defaultConcurrency, "agent calls made at once within one request")

	var api apiConfig
	flag.StringVar(&api.TokenFile, "auth-token-file", "", "file holding the bearer token API clients must present")
	flag.BoolVar(&api.AllowUnauthenticated, "allow-unauthenticated", false, "serve the API without authentication (development only)")

	agents := agentConfig{}
	flag.StringVar(&agents.TokenFile, "agent-token-file", "", "file holding the bearer token the agents require")
	flag.StringVar(&agents.CAFile, "agent-ca-file", "", "CA that signed the agents' TLS certificates (turns TLS on)")
	flag.StringVar(&agents.ClientCertFile, "agent-client-cert-file", "", "client certificate for agents that require mutual TLS")
	flag.StringVar(&agents.ClientKeyFile, "agent-client-key-file", "", "private key for --agent-client-cert-file")
	flag.StringVar(&agents.ServerName, "agent-server-name", "", "name to verify in the agents' certificates (they are dialled by node IP)")
	flag.DurationVar(&agents.Timeout, "agent-timeout", agentclient.DefaultCallTimeout, "deadline for each call to an agent")
	flag.Parse()

	logger := applog.New().With("component", "controller")

	k8s, err := buildK8sClient(*kubeconfig)
	if err != nil {
		logger.Error("build k8s client", "err", err)
		os.Exit(1)
	}

	poolOpts, err := agents.poolOptions(logger)
	if err != nil {
		logger.Error("agent connection settings", "err", err)
		os.Exit(1)
	}
	pool := agentclient.NewPool(*agentPort, poolOpts...)
	defer pool.Close()

	var st *store
	if ns := os.Getenv("CONTROLLER_NAMESPACE"); ns != "" {
		st = newStore(k8s, ns)
	}

	ctrl := newController(k8s, pool, st, logger)
	ctrl.concurrency = *concurrency

	handler, err := api.wrap(ctrl.routes(), logger)
	if err != nil {
		logger.Error("configuration", "err", err)
		os.Exit(1)
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: k8s.CoreV1().Events(""),
	})
	ctrl.setRecorder(broadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "epochd-controller"},
	))
	defer broadcaster.Shutdown()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl.restore(ctx)
	ctrl.startSweeper(ctx, *sweepInterval)
	ctrl.startPodWatcher(ctx)

	// The timeouts keep a slow or stalled client from holding a connection (and a
	// goroutine) open indefinitely. The write timeout is generous because creating
	// a timeshift over many pods is legitimately slow.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("shutdown error", "err", err)
		}
	}()

	logger.Info("listening", "addr", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}

func buildK8sClient(kubeconfigPath string) (kubernetes.Interface, error) {
	if kubeconfigPath != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(cfg)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
