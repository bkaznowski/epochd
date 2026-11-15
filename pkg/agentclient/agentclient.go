// Package agentclient provides a connection pool for the epochd node agent's
// gRPC API. One gRPC connection is maintained per node IP and reused across
// calls. Connections are created lazily on first use and closed by Close.
package agentclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentpb"
	"github.com/bkaznowski/epochd/pkg/api"
	"github.com/bkaznowski/epochd/pkg/auth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultCallTimeout bounds each RPC that arrives without a deadline of its own.
// Without it a node that is down or partitioned holds the caller for as long as
// the transport takes to give up, and the controller makes these calls one
// container at a time.
const DefaultCallTimeout = 15 * time.Second

// Pool manages gRPC connections to node agents, keyed by "nodeIP:port".
type Pool struct {
	mu          sync.Mutex
	conns       map[string]*grpc.ClientConn
	port        string
	token       string
	tlsConfig   *tls.Config
	callTimeout time.Duration
}

// PoolOption configures a Pool.
type PoolOption func(*Pool)

// WithToken makes the pool send token as a bearer credential on every call. The
// agent rejects calls without it. Pair it with WithTLS: over a plaintext
// connection the token can be read off the wire.
func WithToken(token string) PoolOption {
	return func(p *Pool) { p.token = token }
}

// WithTLS makes the pool speak TLS to agents using cfg: the CA that must have
// signed their certificates, a client certificate if they require one (mutual
// TLS), and the server name to verify when agents are addressed by node IP. See
// auth.ClientTLS.
func WithTLS(cfg *tls.Config) PoolOption {
	return func(p *Pool) { p.tlsConfig = cfg }
}

// WithCallTimeout sets the deadline applied to calls whose context has none.
// Zero or negative disables it. The default is DefaultCallTimeout.
func WithCallTimeout(d time.Duration) PoolOption {
	return func(p *Pool) { p.callTimeout = d }
}

// NewPool creates a Pool that dials agents on agentPort (e.g. "9433").
func NewPool(agentPort string, opts ...PoolOption) *Pool {
	p := &Pool{
		conns:       make(map[string]*grpc.ClientConn),
		port:        agentPort,
		callTimeout: DefaultCallTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ctxFor applies the pool's call timeout to ctx unless ctx already has a deadline.
func (p *Pool) ctxFor(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, has := ctx.Deadline(); has || p.callTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.callTimeout)
}

// Inject calls the agent's Inject RPC on the node at nodeIP.
// When freeze is true the agent starts the clock in freeze mode.
func (p *Pool) Inject(ctx context.Context, nodeIP, containerID string, target time.Time, freeze bool) (string, error) {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return "", err
	}
	ctx, cancel := p.ctxFor(ctx)
	defer cancel()
	resp, err := c.Inject(ctx, &agentpb.InjectRequest{
		ContainerId: containerID,
		TargetTime:  timestamppb.New(target),
		Freeze:      freeze,
	})
	if err != nil {
		return "", fmt.Errorf("agentclient: Inject on %s: %w", nodeIP, err)
	}
	return resp.HandleId, nil
}

// SetTime calls the agent's SetTime RPC on the node at nodeIP.
// When freeze is true the agent switches the clock to freeze mode at target.
func (p *Pool) SetTime(ctx context.Context, nodeIP, handleID string, target time.Time, freeze bool) error {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return err
	}
	ctx, cancel := p.ctxFor(ctx)
	defer cancel()
	_, err = c.SetTime(ctx, &agentpb.SetTimeRequest{
		HandleId:   handleID,
		TargetTime: timestamppb.New(target),
		Freeze:     freeze,
	})
	if err != nil {
		return fmt.Errorf("agentclient: SetTime on %s: %w", nodeIP, err)
	}
	return nil
}

// Reset calls the agent's Reset RPC on the node at nodeIP.
func (p *Pool) Reset(ctx context.Context, nodeIP, handleID string) error {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return err
	}
	ctx, cancel := p.ctxFor(ctx)
	defer cancel()
	_, err = c.Reset(ctx, &agentpb.ResetRequest{HandleId: handleID})
	if err != nil {
		return fmt.Errorf("agentclient: Reset on %s: %w", nodeIP, err)
	}
	return nil
}

// Release calls the agent's Release RPC on the node at nodeIP: the hook is taken
// out of the container's processes and the handle is forgotten.
func (p *Pool) Release(ctx context.Context, nodeIP, handleID string) error {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return err
	}
	ctx, cancel := p.ctxFor(ctx)
	defer cancel()
	_, err = c.Release(ctx, &agentpb.ReleaseRequest{HandleId: handleID})
	if err != nil {
		return fmt.Errorf("agentclient: Release on %s: %w", nodeIP, err)
	}
	return nil
}

// GetStatus calls the agent's Status RPC and returns the live injection state.
func (p *Pool) GetStatus(ctx context.Context, nodeIP, handleID string) (*api.HandleStatus, error) {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return nil, err
	}
	ctx, cancel := p.ctxFor(ctx)
	defer cancel()
	resp, err := c.Status(ctx, &agentpb.StatusRequest{HandleId: handleID})
	if err != nil {
		return nil, fmt.Errorf("agentclient: GetStatus on %s: %w", nodeIP, err)
	}
	return &api.HandleStatus{
		Generation: resp.Generation,
		LastTarget: resp.LastTargetTime.AsTime().UTC().Format(time.RFC3339),
		StateAddr:  resp.StateAddr,
		PID:        resp.Pid,
		PIDs:       resp.Pids,
	}, nil
}

// Close closes all pooled connections.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		conn.Close()
	}
	p.conns = make(map[string]*grpc.ClientConn)
}

func (p *Pool) clientFor(nodeIP string) (agentpb.AgentServiceClient, error) {
	addr := nodeIP + ":" + p.port
	p.mu.Lock()
	conn, ok := p.conns[addr]
	if !ok {
		var err error
		conn, err = grpc.NewClient(addr, p.dialOptions()...)
		if err != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("agentclient: dial %s: %w", addr, err)
		}
		p.conns[addr] = conn
	}
	p.mu.Unlock()
	return agentpb.NewAgentServiceClient(conn), nil
}

// dialOptions builds the transport and call credentials for the pool's settings.
func (p *Pool) dialOptions() []grpc.DialOption {
	var opts []grpc.DialOption
	if p.tlsConfig != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(p.tlsConfig)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if p.token != "" {
		// Insist on TLS for the token only when TLS was configured, so a
		// deliberately plaintext development setup still works.
		opts = append(opts, grpc.WithPerRPCCredentials(auth.PerRPCCredentials(p.token, p.tlsConfig != nil)))
	}
	return opts
}
