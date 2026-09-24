// Package sync moves bytes between the agent and the server: code bundles,
// git checkouts, tar streams for dataset relay and artifact uploads.
package sync

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to the API server with the agent's credentials.
type Client struct {
	Server string
	Token  string
	NodeID string
	HTTP   *http.Client
}

func (c *Client) URL(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	return strings.TrimRight(c.Server, "/") + p
}

func (c *Client) do(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL(url), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Mldojo-Node", c.NodeID)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// Download saves a URL to dest.
func (c *Client) Download(ctx context.Context, url, dest string) error {
	resp, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// FetchTar GETs a tar stream and extracts it into dest.
func (c *Client) FetchTar(ctx context.Context, url, dest string) error {
	resp, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return ExtractTar(resp.Body, dest, false)
}

// SendFile PUTs a file, or a tar of a directory, to url.
func (c *Client) SendFile(ctx context.Context, path, url string, asTar bool) error {
	var body io.Reader
	if asTar {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(WriteTar(pw, path)) }()
		body = pr
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		body = f
	}
	resp, err := c.do(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// WriteTar writes root (a file or directory) as an uncompressed tar.
func WriteTar(w io.Writer, root string) error {
	tw := tar.NewWriter(w)
	st, err := os.Stat(root)
	if err != nil {
		return err
	}
	base := root
	if !st.IsDir() {
		base = filepath.Dir(root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil // sockets etc.
		}
		h.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// ExtractTar unpacks a (optionally gzipped) tar into dest, rejecting paths
// that escape dest. Symlinks are created last so no entry is written
// through one.
func ExtractTar(r io.Reader, dest string, gz bool) error {
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	type link struct{ path, target string }
	var links []link
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes destination: %s", h.Name)
		}
		p := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode&0o777)|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			os.Chtimes(p, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			links = append(links, link{p, h.Linkname})
		}
	}
	for _, l := range links {
		os.MkdirAll(filepath.Dir(l.path), 0o755)
		os.Remove(l.path)
		if err := os.Symlink(l.target, l.path); err != nil {
			return err
		}
	}
	return nil
}

// GitCheckout clones repo at ref/commit into dest and returns HEAD.
func GitCheckout(ctx context.Context, repo, ref, commit, dest string, env []string) (string, error) {
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	os.RemoveAll(dest)
	if _, err := run("", "clone", "--quiet", "--recurse-submodules", repo, dest); err != nil {
		return "", err
	}
	target := commit
	if target == "" {
		target = ref
	}
	if target != "" {
		if _, err := run(dest, "checkout", "--quiet", target); err != nil {
			return "", err
		}
	}
	return run(dest, "rev-parse", "HEAD")
}

// GitApply applies a patch file inside dir.
func GitApply(ctx context.Context, dir, patch string) error {
	cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", patch)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// TLSOptions configures how the agent trusts the API. The agent link was
// plain HTTP/WS, so the node token and every shipped log line crossed the
// network in the clear; with a self-signed or internal CA the agent needs to
// be told what to trust.
type TLSOptions struct {
	CAFile     string // extra CA to trust, for an internal or self-signed cert
	ClientCert string // client certificate, when the API asks for mTLS
	ClientKey  string
}

// TLSConfig builds the agent's TLS settings. Empty options mean "the system
// roots", which is what a publicly trusted certificate needs.
func (o TLSOptions) TLSConfig() (*tls.Config, error) {
	if o.CAFile == "" && o.ClientCert == "" && o.ClientKey == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file: %s has no certificates", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	if (o.ClientCert == "") != (o.ClientKey == "") {
		return nil, fmt.Errorf("client_cert and client_key must be set together")
	}
	if o.ClientCert != "" {
		pair, err := tls.LoadX509KeyPair(o.ClientCert, o.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// NewHTTP is the default HTTP client for transfers (no overall timeout:
// datasets can be large; connections still time out on dial).
func NewHTTP(tlsCfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:               nil, // the API is reached directly, never via the node proxy
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}}
}
