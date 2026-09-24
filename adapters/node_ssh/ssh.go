// Package node_ssh connects to SSH nodes: direct, through ProxyJump chains
// with ordered fallbacks (`via`), @-nested bastion usernames, keys or
// passwords resolved from secret:// references.
package node_ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Resolver gives the dialer access to other nodes (for via) and secrets.
type Resolver interface {
	NodeConnection(ctx context.Context, id string) (v1.NodeConnection, error)
	Secret(ctx context.Context, ref string) ([]byte, error)
}

// Dialer opens SSH clients.
type Dialer struct {
	Resolver       Resolver
	KnownHostsFile string        // TOFU store, e.g. ~/.mldojo/known_hosts
	Timeout        time.Duration // per hop
	mu             sync.Mutex
}

// Client is an SSH client plus the jump clients it depends on.
type Client struct {
	*ssh.Client
	Route string // human readable, e.g. "local -> gpu-a -> bastion-a"
	hops  []*ssh.Client
}

func (c *Client) Close() error {
	err := c.Client.Close()
	for i := len(c.hops) - 1; i >= 0; i-- {
		c.hops[i].Close()
	}
	return err
}

// Dial connects to conn, trying each `via` in order.
func (d *Dialer) Dial(ctx context.Context, name string, conn v1.NodeConnection) (*Client, error) {
	return d.dial(ctx, name, conn, 0)
}

func (d *Dialer) dial(ctx context.Context, name string, conn v1.NodeConnection, depth int) (*Client, error) {
	if depth > 4 {
		return nil, errors.New("via chain too deep (cycle?)")
	}
	if conn.Type == "local" {
		return nil, fmt.Errorf("node %s is local; nothing to dial", name)
	}
	if conn.Host == "" {
		return nil, fmt.Errorf("node %s: connection.host is empty", name)
	}
	if len(conn.Via) == 0 {
		return d.hop(ctx, nil, name, conn)
	}
	var errs []string
	for _, via := range conn.Via {
		if via.Node == "" || via.Node == "local" || via.Node == "direct" {
			c, err := d.hop(ctx, nil, name, conn)
			if err == nil {
				return c, nil
			}
			errs = append(errs, fmt.Sprintf("via local: %v", err))
			continue
		}
		vc, err := d.Resolver.NodeConnection(ctx, via.Node)
		if err != nil {
			errs = append(errs, fmt.Sprintf("via %s: %v", via.Node, err))
			continue
		}
		var jump *Client
		if vc.Type == "local" {
			c, err := d.hop(ctx, nil, name, conn)
			if err == nil {
				return c, nil
			}
			errs = append(errs, fmt.Sprintf("via %s(local): %v", via.Node, err))
			continue
		}
		jump, err = d.dial(ctx, via.Node, vc, depth+1)
		if err != nil {
			errs = append(errs, fmt.Sprintf("via %s: %v", via.Node, err))
			continue
		}
		c, err := d.hop(ctx, jump, name, conn)
		if err != nil {
			jump.Close()
			errs = append(errs, fmt.Sprintf("via %s: %v", via.Node, err))
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf("all routes to %s failed: %s", name, strings.Join(errs, "; "))
}

func (d *Dialer) hop(ctx context.Context, jump *Client, name string, conn v1.NodeConnection) (*Client, error) {
	port := conn.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(conn.Host, strconv.Itoa(port))
	auths, err := d.auth(ctx, conn)
	if err != nil {
		return nil, err
	}
	user := conn.User
	if user == "" {
		user = os.Getenv("USER")
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            user, // may contain '@' (bastion asset routing); passed verbatim
		Auth:            auths,
		HostKeyCallback: d.hostKeyCallback(),
		Timeout:         timeout,
	}
	var nc net.Conn
	if jump == nil {
		dl := net.Dialer{Timeout: timeout}
		nc, err = dl.DialContext(ctx, "tcp", addr)
	} else {
		nc, err = dialViaWithTimeout(jump.Client, addr, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	nc.SetDeadline(time.Now().Add(timeout))
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ssh %s@%s: %w", user, addr, err)
	}
	nc.SetDeadline(time.Time{})
	c := &Client{Client: ssh.NewClient(sc, chans, reqs), Route: name}
	if jump != nil {
		c.hops = append(append([]*ssh.Client{}, jump.hops...), jump.Client)
		c.Route = jump.Route + " -> " + name
	} else {
		c.Route = "local -> " + name
	}
	return c, nil
}

func dialViaWithTimeout(c *ssh.Client, addr string, timeout time.Duration) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		nc, err := c.Dial("tcp", addr)
		ch <- res{nc, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout")
	}
}

func (d *Dialer) auth(ctx context.Context, conn v1.NodeConnection) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if conn.Identity != "" {
		var pem []byte
		var err error
		if strings.HasPrefix(conn.Identity, "secret://") {
			pem, err = d.Resolver.Secret(ctx, conn.Identity)
		} else {
			pem, err = os.ReadFile(expandHome(conn.Identity))
		}
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w", conn.Identity, err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w", conn.Identity, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if conn.Password != "" {
		pw := conn.Password
		if strings.HasPrefix(pw, "secret://") {
			b, err := d.Resolver.Secret(ctx, pw)
			if err != nil {
				return nil, fmt.Errorf("password %s: %w", conn.Password, err)
			}
			pw = strings.TrimRight(string(b), "\r\n")
		}
		methods = append(methods, ssh.Password(pw), ssh.KeyboardInteractive(
			func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range qs {
					ans[i] = pw
				}
				return ans, nil
			}))
	}
	if conn.Identity == "" {
		// Fall back to the server user's default keys, then to an ssh-agent.
		// Files come first on purpose: a gpg-agent bound to SSH_AUTH_SOCK
		// offers its own auth subkeys and can exhaust the server's
		// MaxAuthTries before the real key is ever tried.
		var signers []ssh.Signer
		for _, f := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
			b, err := os.ReadFile(expandHome("~/.ssh/" + f))
			if err != nil {
				continue
			}
			if s, err := ssh.ParsePrivateKey(b); err == nil {
				signers = append(signers, s)
			}
		}
		if len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
		}
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if ac, err := net.Dial("unix", sock); err == nil {
				methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(ac).Signers))
			}
		}
	}
	if len(methods) == 0 {
		return nil, errors.New("no ssh auth method: set identity or password")
	}
	return methods, nil
}

// hostKeyCallback implements trust-on-first-use against KnownHostsFile.
func (d *Dialer) hostKeyCallback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if d.KnownHostsFile == "" {
			return nil
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		os.MkdirAll(filepath.Dir(d.KnownHostsFile), 0o700)
		f, err := os.OpenFile(d.KnownHostsFile, os.O_CREATE|os.O_RDONLY, 0o600)
		if err == nil {
			f.Close()
		}
		cb, err := knownhosts.New(d.KnownHostsFile)
		if err != nil {
			return err
		}
		err = cb(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
			af, err := os.OpenFile(d.KnownHostsFile, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer af.Close()
			_, err = af.WriteString(line + "\n")
			return err
		}
		if errors.As(err, &ke) {
			return fmt.Errorf("host key for %s changed (edit %s if expected): %w", hostname, d.KnownHostsFile, err)
		}
		return err
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// Exec runs a command over SSH. It implements node_local.Executor.
type Exec struct{ C *Client }

func (e Exec) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	sess, err := e.C.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	// x/crypto/ssh copies stdout and stderr in separate goroutines; a shared
	// bytes.Buffer needs a lock (login shells often print to stderr).
	out := &lockedBuffer{}
	sess.Stdout, sess.Stderr = out, out
	if stdin != nil {
		sess.Stdin = stdin
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run("bash -lc " + shellQuote(cmd)) }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("%w: %s", err, tail(out.String(), 800))
		}
		return out.String(), nil
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		return out.String(), ctx.Err()
	}
}

func (e Exec) Describe() string { return e.C.Route }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
