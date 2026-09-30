// Package stdio_relay carries TCP connections over one byte stream --
// typically the stdin/stdout of a plain SSH exec session.
//
// It is for nodes that allow nothing but `ssh host cmd`: no outbound network,
// and an sshd with AllowTcpForwarding no, so neither a direct connection nor
// `ssh -R` can bring anything on the node back to the server. The server runs
// a small relay command on the node (Serve on its end); every connection made
// to the relay's local port comes out of the server's net.Listener (Listen on
// this end), the same shape ssh.Client.Listen gives for -R. Nothing here knows
// about MLDojo agents.
//
// The stream carries an SSH connection of its own (x/crypto/ssh), used purely
// as a multiplexer: its channels give per-connection flow control, and global
// requests give keepalives. The outer SSH session is what is authenticated;
// the inner one uses no client auth and a throwaway host key.
package stdio_relay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const chanType = "mldojo-relay@v1"

// KeepaliveInterval is how often each end checks the other is still there;
// a relay whose stream went silent is torn down after two missed replies.
var KeepaliveInterval = 30 * time.Second

// Serve is the relay end: every connection accepted on l is carried to the
// Listen end of rw. It returns when either rw or l fails, and closes both.
func Serve(rw io.ReadWriteCloser, l net.Listener) error {
	defer l.Close()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return err
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	conn, chans, reqs, err := ssh.NewServerConn(streamConn{rw}, cfg)
	if err != nil {
		rw.Close()
		return fmt.Errorf("relay handshake: %w", err)
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	go func() {
		for nc := range chans {
			nc.Reject(ssh.Prohibited, "connections only flow from the relay end")
		}
	}()
	go keepalive(conn)
	errc := make(chan error, 2)
	go func() { errc <- conn.Wait() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				errc <- err
				return
			}
			go func() {
				ch, reqs, err := conn.OpenChannel(chanType, nil)
				if err != nil {
					c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				pipe(c, chanConn{Channel: ch})
			}()
		}
	}()
	return <-errc
}

// Listen is the server end: the listener yields one connection for every
// connection the Serve end of rw accepted. Closing it closes rw.
func Listen(rw io.ReadWriteCloser) (net.Listener, error) {
	conn, chans, reqs, err := ssh.NewClientConn(streamConn{rw}, "relay", &ssh.ClientConfig{
		User: "relay",
		// The host key is thrown away after every run, and rw is already
		// an authenticated stream; there is nothing to verify.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	})
	if err != nil {
		rw.Close()
		return nil, fmt.Errorf("relay handshake: %w", err)
	}
	go ssh.DiscardRequests(reqs)
	go keepalive(conn)
	return &listener{conn: conn, chans: chans}, nil
}

// ListenExec runs cmd -- which must run Serve on its stdin/stdout -- over a
// new session on c and returns Listen on its stdio. Closing the listener ends
// the session, which the relay sees as EOF and exits.
func ListenExec(c *ssh.Client, cmd string) (net.Listener, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := s.StdinPipe()
	if err != nil {
		s.Close()
		return nil, err
	}
	stdout, err := s.StdoutPipe()
	if err != nil {
		s.Close()
		return nil, err
	}
	stderr := &tailBuffer{}
	s.Stderr = stderr
	if err := s.Start(cmd); err != nil {
		s.Close()
		return nil, err
	}
	l, err := Listen(sessionStream{Reader: stdout, WriteCloser: stdin, s: s})
	if err != nil {
		// The command usually failed to start (not installed, port taken);
		// its stderr says which.
		done := make(chan struct{})
		go func() { s.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
			return nil, fmt.Errorf("%w; relay: %s", err, msg)
		}
		return nil, err
	}
	return l, nil
}

func keepalive(conn ssh.Conn) {
	for {
		time.Sleep(KeepaliveInterval)
		t := time.AfterFunc(2*KeepaliveInterval, func() { conn.Close() })
		_, _, err := conn.SendRequest("keepalive@mldojo", true, nil)
		t.Stop()
		if err != nil {
			conn.Close()
			return
		}
	}
}

// pipe copies both ways and closes both ends once either direction ends.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

type listener struct {
	conn  ssh.Conn
	chans <-chan ssh.NewChannel
}

func (l *listener) Accept() (net.Conn, error) {
	for nc := range l.chans {
		if nc.ChannelType() != chanType {
			nc.Reject(ssh.UnknownChannelType, nc.ChannelType())
			continue
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(reqs)
		return chanConn{Channel: ch}, nil
	}
	return nil, net.ErrClosed
}

func (l *listener) Close() error   { return l.conn.Close() }
func (l *listener) Addr() net.Addr { return relayAddr{} }

// chanConn and streamConn dress an ssh.Channel and a plain stream up as
// net.Conn; deadlines are not supported and not needed by either user.
type chanConn struct{ ssh.Channel }

func (chanConn) LocalAddr() net.Addr                { return relayAddr{} }
func (chanConn) RemoteAddr() net.Addr               { return relayAddr{} }
func (chanConn) SetDeadline(t time.Time) error      { return nil }
func (chanConn) SetReadDeadline(t time.Time) error  { return nil }
func (chanConn) SetWriteDeadline(t time.Time) error { return nil }

type streamConn struct{ io.ReadWriteCloser }

func (streamConn) LocalAddr() net.Addr                { return relayAddr{} }
func (streamConn) RemoteAddr() net.Addr               { return relayAddr{} }
func (streamConn) SetDeadline(t time.Time) error      { return nil }
func (streamConn) SetReadDeadline(t time.Time) error  { return nil }
func (streamConn) SetWriteDeadline(t time.Time) error { return nil }

type relayAddr struct{}

func (relayAddr) Network() string { return "relay" }
func (relayAddr) String() string  { return "relay" }

// Stdio is the relay end's stream when it runs as a command: stdin/stdout.
type Stdio struct {
	io.Reader
	io.WriteCloser
}

type sessionStream struct {
	io.Reader
	io.WriteCloser
	s *ssh.Session
}

func (s sessionStream) Close() error {
	return errors.Join(s.WriteCloser.Close(), s.s.Close())
}

// tailBuffer keeps the last few KB of the relay's stderr for error messages.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.b...)
}
