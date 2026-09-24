// sshcheck opens one SSH connection (same code path as mldojo-api) and runs
// several sequential sessions, to diagnose bastions that limit sessions.
//
//	go run ./internal/tools/sshcheck -host bastion.example.com -port 2222 -user 'a@b@10.x' -key ~/.ssh/id_rsa -n 8
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/adapters/node_ssh"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

type res struct{}

func (res) NodeConnection(context.Context, string) (v1.NodeConnection, error) {
	return v1.NodeConnection{}, fmt.Errorf("no via support")
}
func (res) Secret(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("no secrets") }

func main() {
	host := flag.String("host", "", "host")
	port := flag.Int("port", 22, "port")
	user := flag.String("user", os.Getenv("USER"), "user")
	key := flag.String("key", "", "private key path")
	n := flag.Int("n", 8, "sessions")
	script := flag.String("script", "", "run this script file once instead of the session test")
	flag.Parse()
	d := &node_ssh.Dialer{Resolver: res{}, Timeout: 15 * time.Second}
	c, err := d.Dial(context.Background(), "target", v1.NodeConnection{Type: "ssh", Host: *host, Port: *port, User: *user, Identity: *key})
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	ex := node_ssh.Exec{C: c}
	if *script != "" {
		b, _ := os.ReadFile(*script)
		out, err := ex.Run(context.Background(), string(b), nil)
		fmt.Printf("err=%v\noutput:\n%s\n", err, out)
		return
	}
	for i := 1; i <= *n; i++ {
		start := time.Now()
		out, err := ex.Run(context.Background(), fmt.Sprintf("echo session-%d; echo multi\necho line", i), strings.NewReader("stdin"))
		fmt.Printf("session %d (%s): err=%v out=%q\n", i, time.Since(start).Round(time.Millisecond), err, strings.TrimSpace(out))
	}
}
