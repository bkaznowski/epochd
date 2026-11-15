package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if got, err := LoadToken(write("ok", "s3cret\n")); err != nil || got != "s3cret" {
		t.Errorf("LoadToken = (%q, %v), want the token with its newline dropped", got, err)
	}
	if _, err := LoadToken(write("empty", "  \n")); err == nil {
		t.Error("an empty token file must be an error, not 'no authentication'")
	}
	if _, err := LoadToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing token file must be an error")
	}
}

func TestValid(t *testing.T) {
	for _, c := range []struct {
		header string
		want   bool
	}{
		{"Bearer tok", true},
		{"bearer tok", true},
		{"BEARER   tok  ", true},
		{"Bearer wrong", false},
		{"Bearer tok-and-more", false},
		{"Bearer", false},
		{"Bearer ", false},
		{"Basic tok", false},
		{"tok", false},
		{"", false},
	} {
		if got := Valid(c.header, "tok"); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestHTTPMiddleware(t *testing.T) {
	h := HTTPMiddleware("tok", "/healthz")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	do := func(path, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("/timeshifts", ""); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no credentials: %d %v, want 401 with a Bearer challenge", rec.Code, rec.Header())
	}
	if rec := do("/timeshifts", "Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", rec.Code)
	}
	if rec := do("/timeshifts", "Bearer tok"); rec.Code != http.StatusNoContent {
		t.Errorf("right token: %d, want it to reach the handler", rec.Code)
	}
	if rec := do("/healthz", ""); rec.Code != http.StatusNoContent {
		t.Errorf("exempt path without a token: %d, want it served", rec.Code)
	}
	if rec := do("/healthz/../timeshifts", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a path that only starts like an exempt one: %d, want 401", rec.Code)
	}
}

// startHealth serves the standard gRPC health service behind the interceptors.
func startHealth(t *testing.T, token string, extra ...grpc.ServerOption) (*bufconn.Listener, *grpc.Server) {
	t.Helper()
	opts := append([]grpc.ServerOption{
		grpc.UnaryInterceptor(UnaryServerInterceptor(token)),
		grpc.StreamInterceptor(StreamServerInterceptor(token)),
	}, extra...)
	srv := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis := bufconn.Listen(1 << 16)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)
	return lis, srv
}

func dialBuf(t *testing.T, lis *bufconn.Listener, opts ...grpc.DialOption) healthpb.HealthClient {
	t.Helper()
	opts = append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)
	conn, err := grpc.NewClient("passthrough://bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return healthpb.NewHealthClient(conn)
}

func TestGRPCInterceptors(t *testing.T) {
	lis, _ := startHealth(t, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	anon := dialBuf(t, lis)
	if _, err := anon.Check(ctx, &healthpb.HealthCheckRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("unary call with no token: %v, want Unauthenticated", err)
	}
	// Streaming RPCs (the reflection service is one) must not be a way around it.
	if stream, err := anon.Watch(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("stream with no token: %v, want Unauthenticated", err)
		}
	}

	wrong := dialBuf(t, lis, grpc.WithPerRPCCredentials(PerRPCCredentials("nope", false)))
	if _, err := wrong.Check(ctx, &healthpb.HealthCheckRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("unary call with the wrong token: %v, want Unauthenticated", err)
	}

	good := dialBuf(t, lis, grpc.WithPerRPCCredentials(PerRPCCredentials("tok", false)))
	if _, err := good.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Errorf("unary call with the right token: %v", err)
	}
	stream, err := good.Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Errorf("stream with the right token: %v", err)
	}
}

// Credentials that require a secure transport must refuse to go out in the
// clear, so a misconfigured client cannot leak the token. gRPC rejects the
// combination when the client is built.
func TestSecureCredentialsRefusePlaintext(t *testing.T) {
	conn, err := grpc.NewClient("passthrough://nowhere",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(PerRPCCredentials("tok", true)))
	if err == nil {
		conn.Close()
		t.Fatal("a token that requires TLS was allowed on a plaintext connection")
	}
}

// ---------------------------------------------------------------------------
// TLS and mutual TLS, over a real loopback listener
// ---------------------------------------------------------------------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue writes a leaf certificate and key signed by the CA into dir.
func (ca *testCA) issue(t *testing.T, dir, name string, usage x509.ExtKeyUsage) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile = filepath.Join(dir, name+".crt")
	keyFile = filepath.Join(dir, name+".key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)      //nolint:errcheck
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600) //nolint:errcheck
	return certFile, keyFile
}

func (ca *testCA) writePEM(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, ca.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMutualTLS(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t, "epochd-test-ca")
	otherCA := newTestCA(t, "someone-else")
	caFile := ca.writePEM(t, dir, "ca.pem")
	otherCAFile := otherCA.writePEM(t, dir, "other-ca.pem")
	srvCert, srvKey := ca.issue(t, dir, "server", x509.ExtKeyUsageServerAuth)
	cliCert, cliKey := ca.issue(t, dir, "client", x509.ExtKeyUsageClientAuth)
	strangerCert, strangerKey := otherCA.issue(t, dir, "stranger", x509.ExtKeyUsageClientAuth)

	serverCfg, err := ServerTLS(srvCert, srvKey, caFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverCfg)))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	// dial connects with the given client TLS settings and makes one call.
	dial := func(ca, cert, key string) error {
		cfg, err := ClientTLS(ca, cert, key, "")
		if err != nil {
			return err
		}
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	}

	if err := dial(caFile, cliCert, cliKey); err != nil {
		t.Errorf("a client with a certificate from the trusted CA was refused: %v", err)
	}
	if err := dial(caFile, "", ""); err == nil {
		t.Error("a client with no certificate was accepted although the server requires one")
	}
	if err := dial(caFile, strangerCert, strangerKey); err == nil {
		t.Error("a client certificate from an untrusted CA was accepted")
	}
	if err := dial(otherCAFile, cliCert, cliKey); err == nil {
		t.Error("the client trusted a server certificate signed by a different CA")
	}
}

func TestClientTLSRejectsHalfACertificate(t *testing.T) {
	if _, err := ClientTLS("", "only-a-cert.pem", "", ""); err == nil {
		t.Error("a client certificate without its key must be an error")
	}
	if _, err := ClientTLS("", "", "only-a-key.pem", ""); err == nil {
		t.Error("a client key without its certificate must be an error")
	}
	if _, err := ClientTLS(filepath.Join(t.TempDir(), "missing.pem"), "", "", ""); err == nil {
		t.Error("an unreadable CA file must be an error")
	}
}
