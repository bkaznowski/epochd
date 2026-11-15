//go:build linux

package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/bkaznowski/epochd/pkg/auth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// config is how the agent's API is protected.
type config struct {
	// TokenFile holds the bearer token callers must present.
	TokenFile string
	// TLSCertFile and TLSKeyFile make the listener speak TLS.
	TLSCertFile, TLSKeyFile string
	// TLSClientCAFile additionally requires callers to present a client
	// certificate signed by this CA (mutual TLS). A verified client certificate
	// counts as authentication, so it can be used instead of a token.
	TLSClientCAFile string
	// AllowUnauthenticated opts out of requiring any credentials. The agent is
	// privileged and can change the clock of any process on the node, so this is
	// for local development only.
	AllowUnauthenticated bool
}

// serverOptions turns the configuration into gRPC server options, and refuses
// configurations that would leave a privileged daemon open by accident.
func (c config) serverOptions(log *slog.Logger) ([]grpc.ServerOption, error) {
	var opts []grpc.ServerOption

	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return nil, errors.New("--tls-cert-file and --tls-key-file must be given together")
	}
	if c.TLSClientCAFile != "" && c.TLSCertFile == "" {
		return nil, errors.New("--tls-client-ca-file needs --tls-cert-file and --tls-key-file")
	}
	tlsOn := c.TLSCertFile != ""

	if tlsOn {
		cfg, err := auth.ServerTLS(c.TLSCertFile, c.TLSKeyFile, c.TLSClientCAFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg)))
	}

	switch {
	case c.TokenFile != "":
		token, err := auth.LoadToken(c.TokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts,
			grpc.UnaryInterceptor(auth.UnaryServerInterceptor(token)),
			grpc.StreamInterceptor(auth.StreamServerInterceptor(token)))
		if !tlsOn {
			log.Warn("the bearer token travels in the clear: set --tls-cert-file and --tls-key-file")
		}
	case c.TLSClientCAFile != "":
		// Every connection already had to present a certificate the CA signed.
	case c.AllowUnauthenticated:
		log.Warn("running WITHOUT authentication: anything that can reach this port can change the clock of any process on the node")
	default:
		return nil, errors.New("refusing to start without authentication: set --auth-token-file " +
			"or --tls-client-ca-file, or pass --allow-unauthenticated for local development")
	}
	return opts, nil
}

func (c config) String() string {
	return fmt.Sprintf("token=%t tls=%t mtls=%t unauthenticated=%t",
		c.TokenFile != "", c.TLSCertFile != "", c.TLSClientCAFile != "", c.AllowUnauthenticated)
}
