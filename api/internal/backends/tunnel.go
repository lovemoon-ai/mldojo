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
	"github.com/lovemoon-ai/mldojo/adapters/stdio_relay"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// tunnelKeeper maintains SSH reverse port-forwards (connection.reverse_tunnel)
// so agents on nodes that cannot reach the API (e.g. behind a bastion)
// dial 127.0.0.1:<port> on their own host, which the server forwards back.
// Where sshd forbids forwarding, reverse_tunnel_mode: stdio carries the same
// port over a plain exec session instead (adapters/stdio_relay).
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
	client, l, err := k.listen(ctx, nodeID, conn)
	if err != nil && !stdioRelay(conn) {
		return "", err
	}
	// A stdio relay runs the agent binary, which a first `node add` has not
	// deployed yet: keep retrying in the background, the agent retries too.
	tctx, cancel := context.WithCancel(context.Background())
	t := &revTunnel{cancel: cancel, url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	k.mu.Lock()
	k.t[nodeID] = t
	k.mu.Unlock()
	go k.serve(tctx, nodeID, conn, client, l, local.Host)
	if err == nil {
		slog.Info("reverse tunnel up", "node", nodeID, "remote", t.url, "route", client.Route, "mode", conn.ReverseTunnelMode)
	}
	return t.url, nil
}

func stdioRelay(conn v1.NodeConnection) bool { return conn.ReverseTunnelMode == "stdio" }

// relayCmd runs the node end of a stdio relay from the deployed agent binary.
func relayCmd(port int) string {
	return fmt.Sprintf(`exec "$HOME/.mldojo/agent/mldojo-agent" relay --listen 127.0.0.1:%d`, port)
}

// listen opens the node-side port that the agent dials, by -R or by relay.
func (k *tunnelKeeper) listen(ctx context.Context, nodeID string, conn v1.NodeConnection) (*node_ssh.Client, net.Listener, error) {
	port := remotePort(nodeID)
	client, err := k.b.Dialer.Dial(ctx, nodeID, conn)
	if err != nil {
		return nil, nil, err
	}
	var l net.Listener
	if stdioRelay(conn) {
		l, err = stdio_relay.ListenExec(client.Client, relayCmd(port))
	} else {
		l, err = client.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("remote listen on %s:127.0.0.1:%d: %w", nodeID, port, err)
	}
	return client, l, nil
}

func (k *tunnelKeeper) serve(ctx context.Context, nodeID string, conn v1.NodeConnection, client *node_ssh.Client, l net.Listener, localAddr string) {
	backoff, maxBackoff := time.Second, time.Minute
	if stdioRelay(conn) {
		maxBackoff = 10 * time.Second // the agent gives up on hello after 60s
	}
	for l == nil { // stdio relay whose first attempt failed
		if client, l = k.redial(ctx, nodeID, conn, &backoff, maxBackoff); client == nil {
			return
		}
	}
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
		if client, l = k.redial(ctx, nodeID, conn, &backoff, maxBackoff); client == nil {
			return
		}
	}
}

// redial retries listen with backoff until it works (and resets the backoff)
// or ctx ends (nil client).
func (k *tunnelKeeper) redial(ctx context.Context, nodeID string, conn v1.NodeConnection, backoff *time.Duration, maxBackoff time.Duration) (*node_ssh.Client, net.Listener) {
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(*backoff):
		}
		if *backoff < maxBackoff {
			*backoff = min(*backoff*2, maxBackoff)
		}
		c, l, err := k.listen(ctx, nodeID, conn)
		if err != nil {
			slog.Debug("reverse tunnel retry", "node", nodeID, "err", err)
			continue
		}
		slog.Info("reverse tunnel up", "node", nodeID, "route", c.Route, "mode", conn.ReverseTunnelMode)
		*backoff = time.Second
		return c, l
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
