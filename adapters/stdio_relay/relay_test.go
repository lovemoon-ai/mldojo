package stdio_relay

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type duplex struct {
	io.Reader
	io.WriteCloser
}

// pair is two ends of a stream; banner is written ahead of the relay end's
// output, as a chatty login shell would. OS pipes, not io.Pipe: both SSH ends
// send their version before reading, which needs a buffer, as real stdio has.
func pair(banner string) (server, relay io.ReadWriteCloser) {
	r1, w1, _ := os.Pipe() // relay -> server
	r2, w2, _ := os.Pipe() // server -> relay
	w1.Write([]byte(banner))
	return duplex{r1, w2}, duplex{r2, w1}
}

func echoAll(t *testing.T, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); io.Copy(c, c) }()
	}
}

func setup(t *testing.T, banner string) (local net.Listener, relayed net.Listener, served chan error) {
	t.Helper()
	srv, rel := pair(banner)
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served = make(chan error, 1)
	go func() { served <- Serve(rel, local) }()
	relayed, err = Listen(srv)
	if err != nil {
		t.Fatal(err)
	}
	return local, relayed, served
}

func TestRelayCarriesConcurrentConnections(t *testing.T) {
	local, relayed, _ := setup(t, "Welcome to the node!\nlast login: never\n")
	defer relayed.Close()
	go echoAll(t, relayed)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", local.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			rd := bufio.NewReader(c)
			for j := 0; j < 50; j++ {
				msg := fmt.Sprintf("conn %d line %d\n", i, j)
				if _, err := c.Write([]byte(msg)); err != nil {
					t.Error(err)
					return
				}
				got, err := rd.ReadString('\n')
				if err != nil || got != msg {
					t.Errorf("conn %d: got %q, %v; want %q", i, got, err, msg)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestRelayLargeTransfer(t *testing.T) {
	local, relayed, _ := setup(t, "")
	defer relayed.Close()
	const n = 8 << 20
	go func() {
		c, err := relayed.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.CopyN(c, zeroes{}, n)
	}()
	c, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := io.Copy(io.Discard, c)
	if err != nil || got != n {
		t.Fatalf("got %d bytes, %v; want %d", got, err, n)
	}
}

func TestClosingListenerStopsServe(t *testing.T) {
	local, relayed, served := setup(t, "")
	relayed.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the server end closed")
	}
	if _, err := net.DialTimeout("tcp", local.Addr().String(), time.Second); err == nil {
		t.Fatal("relay port still open after Serve returned")
	}
	if _, err := relayed.Accept(); err == nil {
		t.Fatal("Accept after Close should fail")
	}
}

func TestServerEndSeesRelayGoAway(t *testing.T) {
	srv, rel := pair("")
	local, _ := net.Listen("tcp", "127.0.0.1:0")
	go Serve(rel, local)
	relayed, err := Listen(srv)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() { _, err := relayed.Accept(); accepted <- err }()
	rel.Close() // the node end's stdout goes away
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("Accept succeeded after the relay went away")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not notice the relay going away")
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
