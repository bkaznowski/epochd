package main

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentclient"
	"github.com/bkaznowski/epochd/pkg/auth"
)

// apiConfig is how the controller's HTTP API is protected. The API can change
// the clock of any pod in the cluster it can list, so it is not left open by
// default.
type apiConfig struct {
	// TokenFile holds the bearer token API clients must present.
	TokenFile string
	// AllowUnauthenticated opts out of requiring one. For local development only.
	AllowUnauthenticated bool
}

// publicPaths are served without a token: Kubernetes probes and Prometheus
// cannot present one, and neither exposes anything that can change a clock.
var publicPaths = []string{"/healthz", "/metrics"}

// wrap returns h behind the configured authentication, or an error if the
// configuration would leave the API open by accident.
func (c apiConfig) wrap(h http.Handler, log *slog.Logger) (http.Handler, error) {
	switch {
	case c.TokenFile != "":
		token, err := auth.LoadToken(c.TokenFile)
		if err != nil {
			return nil, err
		}
		return auth.HTTPMiddleware(token, publicPaths...)(h), nil
	case c.AllowUnauthenticated:
		log.Warn("the API is running WITHOUT authentication: anything that can reach it can change the clock of any pod it can see")
		return h, nil
	default:
		return nil, errors.New("refusing to start without authentication: set --auth-token-file, " +
			"or pass --allow-unauthenticated for local development")
	}
}

// agentConfig is how the controller authenticates to, and secures its
// connections with, the node agents.
type agentConfig struct {
	// TokenFile holds the bearer token the agents require.
	TokenFile string
	// CAFile is the CA that signed the agents' TLS certificates; setting any TLS
	// option turns TLS on.
	CAFile string
	// ClientCertFile and ClientKeyFile present a client certificate to agents
	// that require mutual TLS.
	ClientCertFile, ClientKeyFile string
	// ServerName overrides the name checked against the agents' certificates,
	// which are usually issued for a name, not for the node IP they are dialled by.
	ServerName string
	// Timeout bounds each call to an agent.
	Timeout time.Duration
}

// tlsRequested reports whether any TLS setting was given.
func (c agentConfig) tlsRequested() bool {
	return c.CAFile != "" || c.ClientCertFile != "" || c.ClientKeyFile != "" || c.ServerName != ""
}

// poolOptions turns the configuration into options for the agent connection pool.
func (c agentConfig) poolOptions(log *slog.Logger) ([]agentclient.PoolOption, error) {
	opts := []agentclient.PoolOption{agentclient.WithCallTimeout(c.Timeout)}

	tlsOn := c.tlsRequested()
	if tlsOn {
		cfg, err := auth.ClientTLS(c.CAFile, c.ClientCertFile, c.ClientKeyFile, c.ServerName)
		if err != nil {
			return nil, err
		}
		opts = append(opts, agentclient.WithTLS(cfg))
	}
	if c.TokenFile != "" {
		token, err := auth.LoadToken(c.TokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, agentclient.WithToken(token))
		if !tlsOn {
			log.Warn("the agent bearer token travels in the clear: set --agent-ca-file to use TLS")
		}
	}
	if c.TokenFile == "" && c.ClientCertFile == "" {
		log.Warn("calling the agents without credentials; they must be running with --allow-unauthenticated")
	}
	return opts, nil
}
