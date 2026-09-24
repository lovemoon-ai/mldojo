// Package config reads ~/.mldojo/config.yaml, shared by the CLI (top-level
// server/token) and mldojo-api (the `api:` section). Environment variables
// MLDOJO_* override file values.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type File struct {
	// Client settings (mldojo CLI).
	Server string `yaml:"server,omitempty"`
	Token  string `yaml:"token,omitempty"`
	// Server settings (mldojo-api).
	API API `yaml:"api,omitempty"`
}

type API struct {
	Listen      string `yaml:"listen,omitempty"`       // 0.0.0.0:8765
	DatabaseURL string `yaml:"database_url,omitempty"` // postgres://...
	DataDir     string `yaml:"data_dir,omitempty"`     // ~/.mldojo/data
	// TLS terminates in the API itself when both are set. The public
	// deployment terminates in nginx instead, but the agent link is
	// internal and was plain HTTP/WS -- tokens and logs in the clear.
	TLSCert       string `yaml:"tls_cert,omitempty"`        // or MLDOJO_TLS_CERT
	TLSKey        string `yaml:"tls_key,omitempty"`         // or MLDOJO_TLS_KEY
	TLSClientCA   string `yaml:"tls_client_ca,omitempty"`   // require client certs signed by this CA (mTLS)
	WebDir        string `yaml:"web_dir,omitempty"`         // web/out
	PublicURL     string `yaml:"public_url,omitempty"`      // URL agents dial (http://192.0.2.10:8765)
	AgentDist     string `yaml:"agent_dist,omitempty"`      // dir with mldojo-agent-<os>-<arch>
	Token         string `yaml:"token,omitempty"`           // API token (MLDOJO_TOKEN)
	MasterKeyFile string `yaml:"master_key_file,omitempty"` // fallback master key file
	// QueuePlugins maps a queue plugin name to its sidecar URL
	// ({myqueue: http://127.0.0.1:8766}); see docs/queue-plugins.md.
	// Or MLDOJO_QUEUE_PLUGINS="name=url,name2=url2".
	QueuePlugins map[string]string `yaml:"queue_plugins,omitempty"`
	QueuePollSec int               `yaml:"queue_poll_sec,omitempty"` // default 5
	// Deprecated spellings of queue_plugins: {aidi: <url>} and queue_poll_sec.
	AidiSidecarURL string `yaml:"aidi_sidecar_url,omitempty"`
	AidiPollSec    int    `yaml:"aidi_poll_sec,omitempty"`
	// WebURL is the public origin users reach (https://mldojo.example.com).
	// Redirects are built from it because a reverse proxy hides the real host.
	WebURL string `yaml:"web_url,omitempty"`
	SSO    SSO    `yaml:"sso,omitempty"`
	// MetricsPublic drops the admin requirement on GET /api/v1/metrics.
	// Off by default; Prometheus scrapes without credentials, so this is a
	// deliberate choice about who can reach the endpoint at all.
	MetricsPublic bool   `yaml:"metrics_public,omitempty"` // or MLDOJO_METRICS_PUBLIC=1
	Alerts        Alerts `yaml:"alerts,omitempty"`
}

// Alerts configures outgoing notifications (webhook only; a Feishu or Slack
// incoming-webhook URL works as-is).
type Alerts struct {
	WebhookURL     string   `yaml:"webhook_url,omitempty"`      // or MLDOJO_ALERTS_WEBHOOK_URL
	On             []string `yaml:"on,omitempty"`               // subscribed events; empty = all. Or MLDOJO_ALERTS_ON
	MinIntervalSec int      `yaml:"min_interval_sec,omitempty"` // per-event dedup window, default 600
	DiskLimitGB    int      `yaml:"disk_limit_gb,omitempty"`    // data_dir above this fires disk.high; 0 = off
}

// SSO configures Conductor single sign-on (OAuth authorization code).
type SSO struct {
	BaseURL      string   `yaml:"base_url,omitempty"`      // https://conductor.example.com
	ClientID     string   `yaml:"client_id,omitempty"`     // mldojo
	ClientSecret string   `yaml:"client_secret,omitempty"` // or MLDOJO_SSO_CLIENT_SECRET
	Allow        []string `yaml:"allow,omitempty"`         // required allowlist (id/email/phone/name); or MLDOJO_SSO_ALLOW (comma-separated)
	Admins       []string `yaml:"admins,omitempty"`        // subset of allow that gets the admin role; or MLDOJO_SSO_ADMINS
}

// Dir is the mldojo config directory (~/.mldojo or $MLDOJO_HOME).
func Dir() string {
	if d := os.Getenv("MLDOJO_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".mldojo")
}

// Path is the config file path ($MLDOJO_CONFIG or <Dir>/config.yaml).
func Path() string {
	if p := os.Getenv("MLDOJO_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.yaml")
}

// LoadRaw reads the config file without env overrides (for editing/saving).
func LoadRaw() (*File, error) {
	var f File
	b, err := os.ReadFile(Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err := yaml.Unmarshal(b, &f); err != nil {
			return nil, err
		}
	}
	return &f, nil
}

// Load reads the config file (missing file = empty config) and applies env.
func Load() (*File, error) {
	fp, err := LoadRaw()
	if err != nil {
		return nil, err
	}
	f := *fp
	env := func(k string, dst *string) {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
	// envList replaces a whole list from a comma-separated variable.
	envList := func(k string, dst *[]string) {
		v := os.Getenv(k)
		if v == "" {
			return
		}
		*dst = nil
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				*dst = append(*dst, a)
			}
		}
	}
	env("MLDOJO_SERVER", &f.Server)
	env("MLDOJO_TOKEN", &f.Token)
	env("MLDOJO_LISTEN", &f.API.Listen)
	env("MLDOJO_DATABASE_URL", &f.API.DatabaseURL)
	env("DATABASE_URL", &f.API.DatabaseURL)
	env("MLDOJO_DATA_DIR", &f.API.DataDir)
	env("MLDOJO_TLS_CERT", &f.API.TLSCert)
	env("MLDOJO_TLS_KEY", &f.API.TLSKey)
	env("MLDOJO_TLS_CLIENT_CA", &f.API.TLSClientCA)
	env("MLDOJO_WEB_DIR", &f.API.WebDir)
	env("MLDOJO_PUBLIC_URL", &f.API.PublicURL)
	env("MLDOJO_AGENT_DIST", &f.API.AgentDist)
	env("MLDOJO_API_TOKEN", &f.API.Token)
	env("MLDOJO_MASTER_KEY_FILE", &f.API.MasterKeyFile)
	env("MLDOJO_WEB_URL", &f.API.WebURL)
	env("MLDOJO_SSO_BASE_URL", &f.API.SSO.BaseURL)
	env("MLDOJO_SSO_CLIENT_ID", &f.API.SSO.ClientID)
	env("MLDOJO_SSO_CLIENT_SECRET", &f.API.SSO.ClientSecret)
	envList("MLDOJO_SSO_ALLOW", &f.API.SSO.Allow)
	envList("MLDOJO_SSO_ADMINS", &f.API.SSO.Admins)
	env("MLDOJO_ALERTS_WEBHOOK_URL", &f.API.Alerts.WebhookURL)
	envList("MLDOJO_ALERTS_ON", &f.API.Alerts.On)
	for _, kv := range strings.Split(os.Getenv("MLDOJO_QUEUE_PLUGINS"), ",") {
		if name, url, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && name != "" {
			if f.API.QueuePlugins == nil {
				f.API.QueuePlugins = map[string]string{}
			}
			f.API.QueuePlugins[name] = url
		}
	}
	if f.API.AidiSidecarURL != "" && f.API.QueuePlugins["aidi"] == "" {
		if f.API.QueuePlugins == nil {
			f.API.QueuePlugins = map[string]string{}
		}
		f.API.QueuePlugins["aidi"] = f.API.AidiSidecarURL
	}
	if f.API.QueuePollSec <= 0 {
		f.API.QueuePollSec = f.API.AidiPollSec
	}
	if v, err := strconv.Atoi(os.Getenv("MLDOJO_QUEUE_POLL_SEC")); err == nil && v > 0 {
		f.API.QueuePollSec = v
	}
	if v, err := strconv.Atoi(os.Getenv("MLDOJO_ALERTS_MIN_INTERVAL_SEC")); err == nil && v > 0 {
		f.API.Alerts.MinIntervalSec = v
	}
	if v, err := strconv.Atoi(os.Getenv("MLDOJO_ALERTS_DISK_LIMIT_GB")); err == nil && v > 0 {
		f.API.Alerts.DiskLimitGB = v
	}
	switch os.Getenv("MLDOJO_METRICS_PUBLIC") {
	case "1", "true", "yes":
		f.API.MetricsPublic = true
	}
	if f.API.Token == "" && os.Getenv("MLDOJO_TOKEN") != "" {
		f.API.Token = os.Getenv("MLDOJO_TOKEN")
	}
	return &f, nil
}

// Save writes the file with 0600 permissions (it holds tokens).
func Save(f *File) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// ExpandHome expands a leading ~/.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}
