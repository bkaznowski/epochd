// Package auth is the access control the controller and the node agent share.
//
// The agent is privileged (hostPID and ptrace) and the controller drives it, so
// neither may be reachable by anything that can merely open a TCP connection to
// it. Callers authenticate with a shared bearer token, sent as
//
//	Authorization: Bearer <token>
//
// (an HTTP header, or gRPC metadata of the same name), and the transport can be
// wrapped in TLS, optionally mutual. A bearer token on a plaintext connection can
// be read off the wire, so the helpers here make that combination visible
// rather than silent.
package auth

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// LoadToken reads a bearer token from path, as mounted from a Kubernetes
// Secret: surrounding whitespace (including the trailing newline most tools
// add) is dropped, and an empty token is an error, never "no authentication".
func LoadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("auth: reading token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("auth: token file %s is empty", path)
	}
	return token, nil
}

// bearer extracts the credential from an Authorization header value.
func bearer(header string) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// Valid reports whether an Authorization header value carries want. The
// comparison takes the same time whatever the two tokens' contents.
func Valid(header, want string) bool {
	got, ok := bearer(header)
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// HTTPMiddleware rejects, with 401 and a Bearer challenge, every request whose
// Authorization header does not carry token. Paths listed in exempt are served
// without one: liveness probes and metrics scrapers cannot present a token and
// expose nothing sensitive.
func HTTPMiddleware(token string, exempt ...string) func(http.Handler) http.Handler {
	skip := make(map[string]bool, len(exempt))
	for _, p := range exempt {
		skip[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !skip[r.URL.Path] && !Valid(r.Header.Get("Authorization"), token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="epochd"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authenticate checks the Authorization metadata of an incoming RPC.
func authenticate(ctx context.Context, token string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		for _, h := range md.Get("authorization") {
			if Valid(h, token) {
				return nil
			}
		}
	}
	return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
}

// UnaryServerInterceptor rejects unary RPCs that do not carry token.
func UnaryServerInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authenticate(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor rejects streaming RPCs (the reflection service, for
// one) that do not carry token.
func StreamServerInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authenticate(ss.Context(), token); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// tokenCredentials attaches a bearer token to every outgoing RPC.
type tokenCredentials struct {
	token      string
	needSecure bool
}

func (c tokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

func (c tokenCredentials) RequireTransportSecurity() bool { return c.needSecure }

// PerRPCCredentials returns gRPC call credentials that send token. With
// secureTransport true grpc refuses to send it over a connection that is not
// TLS; pass false only for a deliberately plaintext connection.
func PerRPCCredentials(token string, secureTransport bool) credentials.PerRPCCredentials {
	return tokenCredentials{token: token, needSecure: secureTransport}
}

// ServerTLS builds the TLS configuration for a server from a certificate and
// key. If clientCAFile is set the server also requires and verifies a client
// certificate signed by that CA (mutual TLS).
func ServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("auth: loading server certificate: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if clientCAFile != "" {
		pool, err := loadCAPool(clientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ClientTLS builds the TLS configuration for a client. caFile (if set) is the
// CA that must have signed the server's certificate; the system roots are used
// otherwise. certFile and keyFile (both or neither) present a client
// certificate for servers that require one. serverName overrides the name the
// server certificate is checked against, which matters when agents are
// addressed by node IP.
func ClientTLS(caFile, certFile, keyFile, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		pool, err := loadCAPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("auth: a client certificate needs both a certificate file and a key file")
	}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("auth: loading client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: reading CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("auth: no certificates found in %s", path)
	}
	return pool, nil
}
