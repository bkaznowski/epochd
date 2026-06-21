// Command controller is the epochd HTTP+JSON service that resolves pods and
// orchestrates time injection via per-node gRPC agents.
//
// Usage:
//
//	controller [--listen=:8080] [--agent-port=9100] [--sweep-interval=30s] [--kubeconfig=PATH]
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

	"epochd/pkg/agentclient"
	applog "epochd/pkg/log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	agentPort := flag.String("agent-port", "9100", "gRPC port of node agents")
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig (empty = in-cluster)")
	sweepInterval := flag.Duration("sweep-interval", 30*time.Second, "how often to sweep expired skews")
	flag.Parse()

	logger := applog.New().With("component", "controller")

	k8s, err := buildK8sClient(*kubeconfig)
	if err != nil {
		logger.Error("build k8s client", "err", err)
		os.Exit(1)
	}

	pool := agentclient.NewPool(*agentPort)
	defer pool.Close()

	var st *store
	if ns := os.Getenv("CONTROLLER_NAMESPACE"); ns != "" {
		st = newStore(k8s, ns)
	}

	ctrl := newController(k8s, pool, st, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl.restore(ctx)
	ctrl.startSweeper(ctx, *sweepInterval)
	ctrl.startPodWatcher(ctx)

	srv := &http.Server{
		Addr:    *listen,
		Handler: ctrl.routes(),
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
