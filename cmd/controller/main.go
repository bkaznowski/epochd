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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"epochd/pkg/agentclient"

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

	k8s, err := buildK8sClient(*kubeconfig)
	if err != nil {
		log.Fatalf("controller: build k8s client: %v", err)
	}

	pool := agentclient.NewPool(*agentPort)
	defer pool.Close()

	ctrl := newController(k8s, pool)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl.startSweeper(ctx, *sweepInterval)

	srv := &http.Server{
		Addr:    *listen,
		Handler: ctrl.routes(),
	}

	go func() {
		<-ctx.Done()
		log.Printf("controller: shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("controller: shutdown: %v", err)
		}
	}()

	log.Printf("controller: listening on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("controller: serve: %v", err)
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
