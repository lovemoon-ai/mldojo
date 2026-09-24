// mldojo-api is the MLDojo API server.
//
//	mldojo-api [serve]          run the server (default)
//	mldojo-api migrate          apply database migrations and exit
//	mldojo-api token issue      print the API token (generates one if needed)
//	mldojo-api token rotate     replace the API token
//	mldojo-api version
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/ai"
	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/core"
	"github.com/lovemoon-ai/mldojo/api/internal/handlers"
	"github.com/lovemoon-ai/mldojo/api/internal/logstore"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/api/internal/obs"
	"github.com/lovemoon-ai/mldojo/api/internal/secrets"
	"github.com/lovemoon-ai/mldojo/internal/config"
	"github.com/lovemoon-ai/mldojo/internal/logging"
	"github.com/lovemoon-ai/mldojo/internal/version"
)

func main() {
	logging.Setup(os.Stderr)
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "migrate":
		err = withStore(func(ctx context.Context, st *models.Store) error { fmt.Println("migrations applied"); return nil })
	case "token":
		sub := "issue"
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "issue", "rotate":
			// The instance token: what install.sh prints and every existing
			// CLI holds. Kept as-is so upgrading does not log anyone out.
			err = withStore(func(ctx context.Context, st *models.Store) error {
				tok, err := apiToken(ctx, st, &config.API{}, sub == "rotate")
				if err == nil {
					fmt.Println(tok)
				}
				return err
			})
		case "create":
			err = withStore(func(ctx context.Context, st *models.Store) error {
				return createToken(ctx, st, args[2:])
			})
		case "ls":
			err = withStore(listTokens)
		case "revoke":
			if len(args) < 3 {
				err = fmt.Errorf("usage: mldojo-api token revoke <id>")
				break
			}
			err = withStore(func(ctx context.Context, st *models.Store) error {
				id, err := st.RevokeAPIToken(ctx, args[2])
				if err == nil {
					fmt.Println("revoked", id)
				}
				return err
			})
		default:
			err = fmt.Errorf("usage: mldojo-api token [issue|rotate|create --name N [--expire 720h] [--role member]|ls|revoke <id>]")
		}
	case "db":
		// db create|drop: manage the configured database (no psql needed).
		// db dump|restore: logical backup, because the embedded PostgreSQL
		// used by the native install ships no pg_dump.
		sub := ""
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "dump":
			err = withStore(func(ctx context.Context, st *models.Store) error { return st.Dump(ctx, os.Stdout) })
		case "restore":
			err = withStore(func(ctx context.Context, st *models.Store) error { return st.Restore(ctx, os.Stdin) })
		default:
			err = dbAdmin(sub)
		}
	case "version", "--version", "-v":
		fmt.Println("mldojo-api", version.Version)
	case "help", "-h", "--help":
		fmt.Println("usage: mldojo-api [serve|migrate|token issue|token rotate|token create|token ls|token revoke|db create|db drop|db dump|db restore|version]")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mldojo-api:", err)
		os.Exit(1)
	}
}

func loadConfig() (*config.API, error) {
	f, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", config.Path(), err)
	}
	c := f.API
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8765"
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(config.Dir(), "data")
	}
	c.DataDir = config.ExpandHome(c.DataDir)
	if c.DatabaseURL == "" {
		return nil, errors.New("database_url is not set (api.database_url in ~/.mldojo/config.yaml or MLDOJO_DATABASE_URL)")
	}
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	if c.WebDir == "" {
		for _, cand := range []string{filepath.Join(exeDir, "..", "web", "out"), filepath.Join(exeDir, "web"), "web/out"} {
			if _, err := os.Stat(filepath.Join(cand, "index.html")); err == nil {
				c.WebDir = cand
				break
			}
		}
	}
	c.WebDir = config.ExpandHome(c.WebDir)
	if c.AgentDist == "" {
		for _, cand := range []string{filepath.Join(exeDir, "..", "dist"), exeDir} {
			if m, _ := filepath.Glob(filepath.Join(cand, "mldojo-agent-*")); len(m) > 0 {
				c.AgentDist = cand
				break
			}
		}
	}
	c.AgentDist = config.ExpandHome(c.AgentDist)
	if c.MasterKeyFile == "" {
		c.MasterKeyFile = filepath.Join(config.Dir(), "master.key")
	}
	c.MasterKeyFile = config.ExpandHome(c.MasterKeyFile)
	if c.QueuePollSec <= 0 {
		c.QueuePollSec = 5
	}
	return &c, nil
}

func withStore(f func(ctx context.Context, st *models.Store) error) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := models.Open(ctx, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	return f(ctx, st)
}

// dbAdmin creates or drops the database named in database_url by
// connecting to the server's "postgres" maintenance database.
func dbAdmin(sub string) error {
	if sub != "create" && sub != "drop" {
		return errors.New("usage: mldojo-api db create|drop")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(name) {
		return fmt.Errorf("unsupported database name %q", name)
	}
	u.Path = "/postgres"
	ctx := context.Background()
	st, err := models.Open(ctx, u.String())
	if err != nil {
		return err
	}
	defer st.Close()
	var exists bool
	if err := st.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists); err != nil {
		return err
	}
	switch {
	case sub == "create" && !exists:
		_, err = st.DB.Exec(ctx, `CREATE DATABASE `+name)
	case sub == "drop" && exists:
		_, err = st.DB.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`)
	}
	if err == nil {
		fmt.Printf("database %s: %s ok\n", name, sub)
	}
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// apiToken returns the configured token, or the one stored in the DB
// (generated on first use).
func apiToken(ctx context.Context, st *models.Store, c *config.API, rotate bool) (string, error) {
	if c.Token != "" && !rotate {
		return c.Token, nil
	}
	var tok string
	has, err := st.GetSetting(ctx, "api.token", &tok)
	if err != nil {
		return "", err
	}
	if !has || tok == "" || rotate {
		tok = "mld_" + randHex(24)
		if err := st.PutSetting(ctx, "api.token", tok); err != nil {
			return "", err
		}
	}
	return tok, nil
}

func outboundIP() string {
	conn, err := net.Dial("udp", "10.255.255.255:9")
	if err == nil {
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

func serve() error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := models.Open(ctx, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	// No agent is connected at boot.
	st.DB.Exec(ctx, `UPDATE nodes SET agent_status='offline'`)

	token, err := apiToken(ctx, st, c, false)
	if err != nil {
		return err
	}
	var runKey string
	if has, _ := st.GetSetting(ctx, "run_token_key", &runKey); !has || runKey == "" {
		runKey = randHex(32)
		st.PutSetting(ctx, "run_token_key", runKey)
	}
	logs, err := logstore.New(c.DataDir)
	if err != nil {
		return err
	}
	logs.OnSegment = func(runID, stream string, off int64, uri string) {
		st.AddLogIndex(context.Background(), runID, stream, off, uri)
	}
	sec := secrets.New(st, c.MasterKeyFile)
	if err := sec.AutoUnlock(ctx); err != nil {
		slog.Warn("secrets auto-unlock", "err", err)
	}
	// A locked OS keyring may become available later (e.g. after login).
	go func() {
		for !sec.Status().Unlocked {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				sec.AutoUnlock(ctx)
			}
		}
	}()

	_, portStr, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	port, _ := strconv.Atoi(portStr)
	localURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	publicURL := c.PublicURL
	if publicURL == "" {
		publicURL = fmt.Sprintf("http://%s:%d", outboundIP(), port)
	}
	// Alerting is wired before the backends: from here on a run failure or a
	// node dropping out reaches whoever configured the webhook.
	alerts := obs.NewNotifier(obs.AlertConfig{WebhookURL: c.Alerts.WebhookURL, On: c.Alerts.On,
		MinInterval: time.Duration(c.Alerts.MinIntervalSec) * time.Second, Instance: publicURL})
	obs.SetNotifier(alerts)
	alerts.Start(ctx)

	rt := &backends.Runtime{Store: st, Logs: logs, Secrets: sec, Cfg: backends.Config{
		PublicURL: strings.TrimRight(publicURL, "/"), LocalURL: localURL, AgentDist: c.AgentDist,
		KnownHostsFile: filepath.Join(config.Dir(), "known_hosts"), RunTokenKey: []byte(runKey),
		APIToken: token, QueuePoll: time.Duration(c.QueuePollSec) * time.Second,
	}}
	node := backends.NewNodeBackend(rt)
	queues := map[string]*backends.SidecarBackend{}
	reg := &backends.Registry{Node: node, External: backends.NewExternalBackend(rt), Queues: map[string]backends.Backend{}}
	for name, url := range c.QueuePlugins {
		queues[name] = backends.NewSidecarBackend(rt, name, url)
		reg.Queues[name] = queues[name]
	}
	svc := &core.Service{RT: rt, Backends: reg}
	appURL := c.WebURL
	if appURL == "" {
		appURL = publicURL
	}
	authMgr := auth.New(auth.Config{BaseURL: c.SSO.BaseURL, ClientID: c.SSO.ClientID,
		ClientSecret: c.SSO.ClientSecret, AppBaseURL: appURL, Allow: c.SSO.Allow, Admins: c.SSO.Admins}, st)
	if authMgr.Cfg.Configured() && !authMgr.Enabled() {
		slog.Error("Conductor SSO is configured but disabled: sso.allow is empty. " +
			"An empty allowlist would let every account of the provider in with full access. " +
			"Set sso.allow (or MLDOJO_SSO_ALLOW) to the ids/emails that may sign in.")
	}
	if authMgr.Enabled() {
		slog.Info("Conductor SSO enabled", "provider", c.SSO.BaseURL, "client_id", c.SSO.ClientID,
			"callback", authMgr.CallbackURL(), "allowlist", len(c.SSO.Allow), "admins", len(c.SSO.Admins))
		go func() { // drop expired sessions
			for {
				st.PurgeSessions(ctx)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Hour):
				}
			}
		}()
	}
	srv := &handlers.Server{Token: token, WebDir: c.WebDir, Auth: authMgr, RT: rt, Svc: svc, Node: node, Queues: queues,
		AI: &ai.AI{Svc: svc, Hub: node.Hub}, Origins: browserOrigins(publicURL, c.WebURL),
		MetricsPublic: c.MetricsPublic}

	for _, q := range queues {
		go q.Poll(ctx)
	}
	node.StartTunnels(ctx)
	node.StartReaper(ctx, backends.DefaultOrphanGrace, backends.DefaultStartingTimeout)
	node.StartScheduler(ctx)
	svc.StartSweepController(ctx)
	go collectGarbage(ctx, st, logs)
	go watchPlatform(ctx, logs, sec, c.Alerts.DiskLimitGB)

	// No ReadTimeout or WriteTimeout on purpose: this server streams logs and
	// metrics over long-lived WebSockets and accepts multi-gigabyte code
	// bundles, both of which a whole-request deadline would cut off.
	// ReadHeaderTimeout covers slowloris; IdleTimeout reaps dead keep-alives.
	hs := &http.Server{Addr: c.Listen, Handler: srv.Routes(),
		ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	tlsCfg, err := serverTLS(c)
	if err != nil {
		return err
	}
	hs.TLSConfig = tlsCfg
	errc := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			errc <- hs.ListenAndServeTLS(c.TLSCert, c.TLSKey)
			return
		}
		errc <- hs.ListenAndServe()
	}()
	scheme := "http"
	if tlsCfg != nil {
		scheme = "https"
	}
	slog.Info("mldojo-api listening", "version", version.Version, "listen", c.Listen, "scheme", scheme,
		"mtls", tlsCfg != nil && tlsCfg.ClientCAs != nil, "public_url", rt.Cfg.PublicURL,
		"web", c.WebDir, "agent_dist", c.AgentDist, "queue_plugins", c.QueuePlugins, "secrets", sec.Status().Source,
		"alerts", alerts.Enabled(), "metrics_public", c.MetricsPublic)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

// Retention for the data directory. Run logs live as long as their run, so
// deleting a run (or a project) is what frees them; these two cover the
// caches, which are rebuildable.
const (
	cacheRetention = 7 * 24 * time.Hour
	tmpRetention   = 24 * time.Hour
	// Telemetry is the one table that grows on its own, with no run to
	// delete alongside it. A month is long enough to answer "was last
	// week worse than this one" and short enough to stay small.
	telemetryRetention = 30 * 24 * time.Hour
	gcInterval         = time.Hour
	gcFirstRun         = 5 * time.Minute
)

// collectGarbage reclaims disk nothing refers to any more. Without it the
// data directory only grows: log directories of deleted runs, code bundles of
// runs that are gone, and artifact caches all stayed forever.
func collectGarbage(ctx context.Context, st *models.Store, logs *logstore.Store) {
	t := time.NewTimer(gcFirstRun)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(gcInterval)
		runs, err := st.RunIDSet(ctx)
		if err != nil {
			slog.Warn("gc: run ids", "err", err)
			continue
		}
		blobs, err := st.ReferencedBlobs(ctx)
		if err != nil {
			slog.Warn("gc: referenced blobs", "err", err)
			continue
		}
		stats, err := logs.GC(func(id string) bool { return runs[id] },
			func(sha string) bool { return blobs[sha] }, cacheRetention, tmpRetention)
		if err != nil {
			slog.Warn("gc", "err", err)
			continue
		}
		if n, err := st.PruneTelemetry(ctx, telemetryRetention); err != nil {
			slog.Warn("gc: prune telemetry", "err", err)
		} else if n > 0 {
			slog.Info("gc pruned telemetry", "samples", n)
		}
		if stats.Runs+stats.Blobs+stats.Cache+stats.Tmp > 0 {
			slog.Info("gc reclaimed", "runs", stats.Runs, "blobs", stats.Blobs,
				"cache", stats.Cache, "tmp", stats.Tmp, "mb", stats.Bytes>>20)
		}
	}
}

// Sampling interval for the two platform-health numbers nobody else looks
// at. Both are cheap except the data directory walk, which is why this is
// minutes and not seconds.
const watchInterval = 5 * time.Minute

// watchPlatform feeds mldojo_data_dir_bytes and mldojo_secrets_unlocked, and
// raises the two alerts that have no natural event to hang off: the disk
// filling up and the secrets manager staying locked (every run that needs a
// secret fails while it is).
func watchPlatform(ctx context.Context, logs *logstore.Store, sec *secrets.Manager, diskLimitGB int) {
	unlockedAt := time.Now()
	t := time.NewTimer(gcFirstRun)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(watchInterval)
		used := logs.Usage()
		obs.DataDirBytes.Set(float64(used))
		if limit := int64(diskLimitGB) << 30; limit > 0 && used > limit {
			obs.Alert(obs.Event{Type: obs.EventDiskHigh, Level: "warning",
				Title: fmt.Sprintf("data dir is over its limit: %d GiB of %d GiB", used>>30, diskLimitGB),
				Text:  "run logs, code bundles and caches live here; a full disk takes the whole platform down",
				Fields: map[string]string{"used_gb": strconv.FormatInt(used>>30, 10),
					"limit_gb": strconv.Itoa(diskLimitGB)}})
		}
		if sec.Status().Unlocked {
			unlockedAt = time.Now()
			continue
		}
		if locked := time.Since(unlockedAt); locked > time.Hour {
			obs.Alert(obs.Event{Type: obs.EventSecretsLocked, Level: "warning",
				Title:  "secrets have been locked for " + locked.Round(time.Minute).String(),
				Text:   "every run that needs a secret fails until `mldojo secrets unlock` runs",
				Fields: map[string]string{"source": sec.Status().Source}})
		}
	}
}

// browserOrigins is the set of app URLs a browser may call the API from,
// reduced to scheme://host.
func browserOrigins(urls ...string) []string {
	var out []string
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return out
}

// createToken issues a named, revocable token and prints it once.
func createToken(ctx context.Context, st *models.Store, args []string) error {
	name, role, expire := "", auth.RoleAdmin, time.Duration(0)
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "--name":
			name = next()
		case "--role":
			role = next()
		case "--expire":
			d, err := time.ParseDuration(next())
			if err != nil || d <= 0 {
				return fmt.Errorf("--expire wants a positive Go duration, e.g. 720h")
			}
			expire = d
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if name == "" {
		return fmt.Errorf("--name is required, so the token can be told apart later")
	}
	if role != auth.RoleAdmin && role != auth.RoleMember {
		return fmt.Errorf("--role must be %q or %q", auth.RoleAdmin, auth.RoleMember)
	}
	plain, t, err := st.CreateAPIToken(ctx, name, role, "mldojo-api", expire)
	if err != nil {
		return err
	}
	fmt.Println(plain)
	fmt.Fprintf(os.Stderr, "id %s  name %s  role %s  expires %s\n", t.ID, t.Name, t.Role, expiry(t.ExpiresAt))
	fmt.Fprintln(os.Stderr, "this is the only time the token is shown")
	return nil
}

func listTokens(ctx context.Context, st *models.Store) error {
	ts, err := st.ListAPITokens(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tROLE\tSTATE\tEXPIRES\tLAST USED")
	for _, t := range ts {
		state := "active"
		switch {
		case t.RevokedAt != nil:
			state = "revoked"
		case !t.Active():
			state = "expired"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID[:8], t.Name, t.Role, state, expiry(t.ExpiresAt), expiry(t.LastUsedAt))
	}
	return w.Flush()
}

func expiry(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// serverTLS builds the listener's TLS config, or nil for plain HTTP.
//
// The public deployment terminates TLS in nginx, but the agent link is
// internal and was plain HTTP/WS: the agent token and every log line
// crossed the network in the clear. Terminating here closes that, and a
// client CA additionally demands a certificate from whoever connects.
func serverTLS(c *config.API) (*tls.Config, error) {
	if c.TLSCert == "" && c.TLSKey == "" {
		if c.TLSClientCA != "" {
			return nil, fmt.Errorf("tls_client_ca needs tls_cert and tls_key: mTLS cannot work without server TLS")
		}
		return nil, nil
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		return nil, fmt.Errorf("tls_cert and tls_key must be set together")
	}
	if _, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey); err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.TLSClientCA != "" {
		pem, err := os.ReadFile(c.TLSClientCA)
		if err != nil {
			return nil, fmt.Errorf("tls_client_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls_client_ca: %s has no certificates", c.TLSClientCA)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
