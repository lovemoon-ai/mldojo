package backends

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/lovemoon-ai/mldojo/adapters/node_ssh"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// tunnelKeeper maintains SSH reverse port-forwards (connection.reverse_tunnel)
// so agents on nodes that cannot reach the API (e.g. behind a bastion)
// dial 127.0.0.1:<port> on their own host, which the server forwards back.
type tunnelKeeper struct {
	b  *NodeBackend
	mu sync.Mutex
	t  map[string]*revTunnel
}

type revTunnel struct {
	cancel context.CancelFunc
	url    string
}

func newTunnelKeeper(b *NodeBackend) *tunnelKeeper {
	return &tunnelKeeper{b: b, t: map[string]*revTunnel{}}
}

// remotePort is stable per node so the agent config stays valid across
// server restarts.
func remotePort(nodeID string) int {
	h := fnv.New32a()
	h.Write([]byte(nodeID))
	return 23000 + int(h.Sum32()%2000)
}

func (k *tunnelKeeper) start(ctx context.Context, nodeID string, conn v1.NodeConnection) (string, error) {
	k.mu.Lock()
	if t, ok := k.t[nodeID]; ok {
		k.mu.Unlock()
		return t.url, nil
	}
	k.mu.Unlock()
	local, err := url.Parse(k.b.rt.Cfg.LocalURL)
	if err != nil || local.Host == "" {
		return "", fmt.Errorf("server local URL is not configured")
	}
	port := remotePort(nodeID)
	client, err := k.b.Dialer.Dial(ctx, nodeID, conn)
	if err != nil {
		return "", err
	}
	l, err := client.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		client.Close()
		return "", fmt.Errorf("remote listen on %s:127.0.0.1:%d: %w", nodeID, port, err)
	}
	tctx, cancel := context.WithCancel(context.Background())
	t := &revTunnel{cancel: cancel, url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	k.mu.Lock()
	k.t[nodeID] = t
	k.mu.Unlock()
	go k.serve(tctx, nodeID, conn, client, l, local.Host)
	slog.Info("reverse tunnel up", "node", nodeID, "remote", t.url, "route", client.Route)
	return t.url, nil
}

func (k *tunnelKeeper) serve(ctx context.Context, nodeID string, conn v1.NodeConnection, client *node_ssh.Client, l net.Listener, localAddr string) {
	backoff := time.Second
	for {
		errc := make(chan error, 1)
		go func() {
			for {
				rc, err := l.Accept()
				if err != nil {
					errc <- err
					return
				}
				go func() {
					defer rc.Close()
					lc, err := net.DialTimeout("tcp", localAddr, 10*time.Second)
					if err != nil {
						return
					}
					defer lc.Close()
					go io.Copy(lc, rc)
					io.Copy(rc, lc)
				}()
			}
		}()
		select {
		case <-ctx.Done():
			l.Close()
			client.Close()
			return
		case err := <-errc:
			slog.Warn("reverse tunnel dropped; reconnecting", "node", nodeID, "err", err)
			l.Close()
			client.Close()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			c, err := k.b.Dialer.Dial(ctx, nodeID, conn)
			if err != nil {
				continue
			}
			nl, err := c.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort(nodeID)))
			if err != nil {
				c.Close()
				continue
			}
			client, l, backoff = c, nl, time.Second
			break
		}
	}
}

func (k *tunnelKeeper) stop(nodeID string) {
	k.mu.Lock()
	t := k.t[nodeID]
	delete(k.t, nodeID)
	k.mu.Unlock()
	if t != nil {
		t.cancel()
	}
}
