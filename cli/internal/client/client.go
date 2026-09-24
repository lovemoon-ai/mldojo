// Package client is the Go API client used by the mldojo CLI.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// Exit codes: 0 ok, 2 user error, 3 unreachable, 4 backend error, 5 conflict.
const (
	ExitOK          = 0
	ExitUser        = 2
	ExitUnreachable = 3
	ExitBackend     = 4
	ExitConflict    = 5
)

// Error is an API error with its CLI exit code.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) ExitCode() int {
	switch e.Code {
	// forbidden belongs here, not under backend_error: retrying a
	// permission failure never helps, and an agent that reads it as a
	// transient backend fault will retry forever.
	case "user_error", "not_found", "unauthorized", "forbidden":
		return ExitUser
	case "unreachable":
		return ExitUnreachable
	case "conflict":
		return ExitConflict
	}
	return ExitBackend
}

// ExitCode maps any error to a CLI exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	var ne net.Error
	var oe *net.OpError
	if errors.As(err, &ne) || errors.As(err, &oe) || strings.Contains(err.Error(), "connection refused") {
		return ExitUnreachable
	}
	return ExitUser
}

type Client struct {
	Server string
	Token  string
	HTTP   *http.Client
}

func New(server, token string) *Client {
	return &Client{Server: strings.TrimRight(server, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

func (c *Client) url(p string) string { return c.Server + "/api/v1" + p }

const maxRateLimitRetries = 3

// retryAfter reads the server's Retry-After, clamped so a bad header cannot
// park the CLI for hours.
func retryAfter(resp *http.Response) time.Duration {
	d := 5 * time.Second
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// rewind prepares a request body for a retry, and reports whether a retry is
// possible at all: a one-shot stream cannot be replayed.
func rewind(body io.Reader) bool {
	if body == nil {
		return true
	}
	s, ok := body.(io.Seeker)
	if !ok {
		return false
	}
	_, err := s.Seek(0, io.SeekStart)
	return err == nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) req(ctx context.Context, method, path string, body io.Reader, ctype string, attempt int) (*http.Response, error) {
	if c.Server == "" {
		return nil, &Error{Code: "user_error", Msg: "no server configured: run `mldojo login --server URL --token TOKEN` or set MLDOJO_SERVER"}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("X-Include-Summary", "true")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Code: "unreachable", Msg: fmt.Sprintf("cannot reach %s: %v", c.Server, err)}
	}
	if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateLimitRetries && rewind(body) {
		resp.Body.Close()
		// The server says when to come back; honour it rather than handing
		// the caller an error it would only have to retry itself.
		if err := sleepCtx(ctx, retryAfter(resp)); err != nil {
			return nil, err
		}
		return c.req(ctx, method, path, body, ctype, attempt+1)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e v1.Error
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
			if e.Error == "" {
				e.Error = resp.Status
			}
		}
		if e.Code == "" {
			switch {
			case resp.StatusCode == 404:
				e.Code = "not_found"
			case resp.StatusCode == 409:
				e.Code = "conflict"
			case resp.StatusCode >= 500:
				e.Code = "backend_error"
			default:
				e.Code = "user_error"
			}
		}
		return nil, &Error{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}
	}
	return resp, nil
}

// Do sends JSON and decodes a JSON response into out (if non-nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.req(ctx, method, path, body, ctype, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		b, err := io.ReadAll(resp.Body)
		*raw = b
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// Raw returns the response body stream (caller closes).
func (c *Client) Raw(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	return c.req(ctx, method, path, body, ctype, 0)
}

// UploadBlob stores bytes on the server and returns blob://sha.
func (c *Client) UploadBlob(ctx context.Context, r io.Reader) (*v1.BlobRef, error) {
	resp, err := c.req(ctx, http.MethodPost, "/blobs", r, "application/octet-stream", 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ref v1.BlobRef
	return &ref, json.NewDecoder(resp.Body).Decode(&ref)
}

// WS opens a WebSocket endpoint (path under /api/v1, with query).
func (c *Client) WS(ctx context.Context, path string, q url.Values) (*websocket.Conn, error) {
	u := c.url(path)
	if strings.HasPrefix(u, "https://") {
		u = "wss://" + strings.TrimPrefix(u, "https://")
	} else {
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.Token)
	ws, resp, err := websocket.DefaultDialer.DialContext(ctx, u, h)
	if err != nil {
		if resp != nil && resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			var e v1.Error
			json.Unmarshal(b, &e)
			if e.Error == "" {
				e.Error = resp.Status
			}
			code := e.Code
			if code == "" {
				code = "user_error"
			}
			return nil, &Error{Status: resp.StatusCode, Code: code, Msg: e.Error}
		}
		return nil, &Error{Code: "unreachable", Msg: fmt.Sprintf("websocket %s: %v", path, err)}
	}
	return ws, nil
}

// PathEscape escapes each segment but keeps slashes (queue ids).
func PathEscape(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
