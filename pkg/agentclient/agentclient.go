// Package agentclient provides a connection pool for the epochd node agent's
// gRPC API. One gRPC connection is maintained per node IP and reused across
// calls. Connections are created lazily on first use and closed by Close.
package agentclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bkaznowski/epochd/pkg/agentpb"
	"github.com/bkaznowski/epochd/pkg/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Pool manages gRPC connections to node agents, keyed by "nodeIP:port".
type Pool struct {
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
	port  string
}

// NewPool creates a Pool that dials agents on agentPort (e.g. "9100").
func NewPool(agentPort string) *Pool {
	return &Pool{
		conns: make(map[string]*grpc.ClientConn),
		port:  agentPort,
	}
}

// Inject calls the agent's Inject RPC on the node at nodeIP.
// When freeze is true the agent starts the clock in freeze mode.
func (p *Pool) Inject(ctx context.Context, nodeIP, containerID string, target time.Time, freeze bool) (string, error) {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return "", err
	}
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
	_, err = c.Reset(ctx, &agentpb.ResetRequest{HandleId: handleID})
	if err != nil {
		return fmt.Errorf("agentclient: Reset on %s: %w", nodeIP, err)
	}
	return nil
}

// GetStatus calls the agent's Status RPC and returns the live injection state.
func (p *Pool) GetStatus(ctx context.Context, nodeIP, handleID string) (*api.HandleStatus, error) {
	c, err := p.clientFor(nodeIP)
	if err != nil {
		return nil, err
	}
	resp, err := c.Status(ctx, &agentpb.StatusRequest{HandleId: handleID})
	if err != nil {
		return nil, fmt.Errorf("agentclient: GetStatus on %s: %w", nodeIP, err)
	}
	return &api.HandleStatus{
		Generation: resp.Generation,
		LastTarget: resp.LastTargetTime.AsTime().UTC().Format(time.RFC3339),
		StateAddr:  resp.StateAddr,
		PID:        resp.Pid,
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
		conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("agentclient: dial %s: %w", addr, err)
		}
		p.conns[addr] = conn
	}
	p.mu.Unlock()
	return agentpb.NewAgentServiceClient(conn), nil
}
