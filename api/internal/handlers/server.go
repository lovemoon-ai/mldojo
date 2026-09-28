// Package handlers exposes the REST + WebSocket API and serves
// the static web app.
package handlers

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/ai"
	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/core"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/api/internal/obs"
	"github.com/lovemoon-ai/mldojo/internal/version"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

type Server struct {
	Token  string
	WebDir string
	// Origins are the app URLs browsers may call this API from (public_url,
	// web_url). Used for CORS and the CSRF origin check.
	Origins []string
	Auth    *auth.Manager
	RT      *backends.Runtime
	Svc     *core.Service
	Node    *backends.NodeBackend
	Queues  map[string]*backends.SidecarBackend // queue plugins by name
	AI      *ai.AI
	// MetricsPublic serves /metrics without authentication (api.metrics_public).
	MetricsPublic bool

	limiter *rateLimiter
	gauges  gaugeCache
	devices deviceLogins
}

// Init prepares the per-server state the middlewares need.
func (s *Server) Init() {
	if s.limiter == nil {
		// Well above interactive CLI and web use; this is for runaway
		// scripts and credential stuffing, not for shaping normal traffic.
		s.limiter = newRateLimiter(1200, 200)
	}
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// Routes builds the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	api := func(pattern string, h handlerFunc) {
		mux.Handle(pattern, s.userAuth(h))
	}
	// adminAPI additionally requires the admin role.
	adminAPI := func(pattern string, h handlerFunc) {
		mux.Handle(pattern, s.userAuth(requireAdmin(h)))
	}
	mux.Handle("GET /api/v1/health", wrap(s.health))
	// Prometheus scrapes with no credentials, so the endpoint can be opened
	// deliberately; it stays behind the admin role otherwise.
	if s.MetricsPublic {
		mux.Handle("GET /api/v1/metrics", wrap(s.metrics))
	} else {
		adminAPI("GET /api/v1/metrics", s.metrics)
	}
	// SSO: unauthenticated by design (they are the way in).
	mux.Handle("GET /api/v1/auth/config", wrap(s.authConfig))
	mux.Handle("GET /api/v1/auth/login", wrap(s.authLogin))
	mux.Handle("GET /api/v1/auth/callback", wrap(s.authCallback))
	mux.Handle("GET /api/v1/auth/me", wrap(s.authMe))
	// POST only: a GET that changes state can be fired by any page's <img>.
	mux.Handle("POST /api/v1/auth/logout", wrap(s.authLogout))
	mux.Handle("POST /api/v1/auth/device/start", wrap(s.deviceStart))
	mux.Handle("POST /api/v1/auth/device/poll", wrap(s.devicePoll))
	mux.HandleFunc("GET /activate", s.activate)
	mux.HandleFunc("POST /activate", s.activate)
	mux.HandleFunc("GET /dl/{file}", s.download)
	mux.HandleFunc("GET /install.sh", s.installSh)

	api("GET /api/v1/projects", s.listProjects)
	api("POST /api/v1/projects", s.createProject)
	api("GET /api/v1/projects/{name}", s.getProject)
	api("DELETE /api/v1/projects/{name}", s.deleteProject)
	adminAPI("PATCH /api/v1/projects/{name}/limit", s.setProjectLimit)
	api("GET /api/v1/projects/{p}/experiments", s.listExperiments)
	api("POST /api/v1/projects/{p}/experiments", s.createExperiment)
	api("GET /api/v1/projects/{p}/experiments/{e}", s.getExperiment)
	api("DELETE /api/v1/projects/{p}/experiments/{e}", s.deleteExperiment)

	api("GET /api/v1/runs", s.listRuns)
	api("POST /api/v1/runs", s.submitRun)
	api("POST /api/v1/runs/external", s.startExternal)
	api("GET /api/v1/runs/{id}", s.getRun)
	api("DELETE /api/v1/runs/{id}", s.deleteRun)
	api("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	api("POST /api/v1/runs/{id}/rerun", s.rerunRun)
	api("GET /api/v1/runs/{id}/events", s.runEvents)
	api("GET /api/v1/runs/{id}/logs", s.runLogs)
	api("GET /api/v1/runs/{id}/logs/ws", s.runLogsWS)
	mux.Handle("POST /api/v1/runs/{id}/finish", wrap(s.finishExternal))
	mux.Handle("POST /api/v1/runs/{id}/config", wrap(s.putRunConfig))
	api("GET /api/v1/runs/{id}/metrics", s.runMetrics)
	api("GET /api/v1/runs/{id}/metrics/ws", s.runMetricsWS)
	mux.Handle("POST /api/v1/runs/{id}/metrics", wrap(s.ingestMetrics)) // user or run token
	api("GET /api/v1/runs/{id}/artifacts", s.listArtifacts)
	api("POST /api/v1/runs/{id}/artifacts/refresh", s.refreshArtifacts)
	api("GET /api/v1/runs/{id}/artifacts/raw", s.rawArtifact)
	api("GET /api/v1/runs/{id}/code", s.runCode)
	api("GET /api/v1/runs/{id}/gpu", s.runGPU)
	api("GET /api/v1/runs/{id}/models", s.runModels)
	api("GET /api/v1/runs/{id}/episodes", s.runEpisodes)
	api("GET /api/v1/compare/episodes", s.compareEpisodes)
	api("GET /api/v1/compare", s.compare)
	api("GET /api/v1/compare/many", s.compareMany)

	api("POST /api/v1/sweeps", s.createSweep)
	api("GET /api/v1/sweeps", s.listSweeps)
	api("GET /api/v1/sweeps/{p}/{name}", s.getSweep)
	api("POST /api/v1/sweeps/{p}/{name}/stop", s.stopSweep)

	api("GET /api/v1/models", s.listModels)
	api("GET /api/v1/models/{p}/{name}", s.getModel)
	api("POST /api/v1/models/{p}/{name}/versions", s.registerModelVersion)
	api("POST /api/v1/models/{p}/{name}/versions/{v}/stage", s.setModelStage)

	api("GET /api/v1/nodes", s.listNodes)
	adminAPI("POST /api/v1/nodes", s.addNode)
	api("GET /api/v1/nodes/{id}", s.getNode)
	adminAPI("DELETE /api/v1/nodes/{id}", s.deleteNode)
	adminAPI("PATCH /api/v1/nodes/{id}/limit", s.setNodeLimit)
	api("POST /api/v1/nodes/{id}/test", s.testNode)
	adminAPI("POST /api/v1/nodes/{id}/upgrade", s.upgradeNode)
	api("GET /api/v1/nodes/{id}/gpu", s.nodeGPU)
	api("GET /api/v1/nodes/{id}/gpu/ws", s.nodeGPUWS)
	api("GET /api/v1/nodes/{id}/gpu/history", s.nodeGPUHistory)
	api("GET /api/v1/nodes/{id}/disk", s.nodeDisk)

	api("GET /api/v1/usage", s.usage)
	api("GET /api/v1/gpus/idle", s.idleGPUs)

	api("GET /api/v1/queues", s.listQueues)
	api("GET /api/v1/queues/resources", s.queueResources)
	adminAPI("POST /api/v1/queues", s.addQueue)
	api("GET /api/v1/queues/{id...}", s.getQueue)
	adminAPI("DELETE /api/v1/queues/{id...}", s.deleteQueue)

	api("GET /api/v1/datasets", s.listDatasets)
	api("POST /api/v1/datasets", s.registerDataset)
	api("GET /api/v1/datasets/{ref}", s.getDataset)
	adminAPI("DELETE /api/v1/datasets/{ref}", s.deleteDataset)
	api("POST /api/v1/datasets/{ref}/push", s.pushDataset)

	api("GET /api/v1/secrets", s.listSecrets)
	api("GET /api/v1/secrets/status", s.secretStatus)
	adminAPI("POST /api/v1/secrets/unlock", s.unlockSecrets)
	adminAPI("POST /api/v1/secrets/{ns}/{name}", s.setSecret)
	adminAPI("DELETE /api/v1/secrets/{ns}/{name}", s.deleteSecret)

	adminAPI("GET /api/v1/tokens", s.listTokens)
	adminAPI("POST /api/v1/tokens", s.createToken)
	adminAPI("DELETE /api/v1/tokens/{id}", s.revokeToken)

	adminAPI("GET /api/v1/audit", s.listAudit)
	adminAPI("GET /api/v1/users", s.listUsers)

	api("POST /api/v1/blobs", s.putBlob)
	api("GET /api/v1/blobs/{sha}", s.getBlob)

	api("GET /api/v1/ai/runs/{id}/brief", s.aiBrief)
	api("GET /api/v1/ai/nodes/free", s.aiFreeNodes)
	api("POST /api/v1/ai/runs", s.aiSubmit)
	api("GET /api/v1/ai/experiments/{p}/{e}/summary", s.aiExpSummary)
	api("POST /api/v1/ai/anomaly-check/{id}", s.aiAnomaly)

	// Agent (internal) endpoints authenticate with agent tokens.
	mux.HandleFunc("GET /api/v1/agent/connect", s.Node.Hub.HandleConnect)
	mux.Handle("GET /api/v1/agent/blobs/{sha}", s.agentAuth(s.agentBlob))
	mux.Handle("PUT /api/v1/agent/upload/{id}", s.agentAuth(func(w http.ResponseWriter, r *http.Request, node string) {
		s.Node.Relay.HandleUpload(w, r, node, r.PathValue("id"))
	}))
	relay := s.agentAuth(func(w http.ResponseWriter, r *http.Request, node string) {
		s.Node.Relay.HandleRelay(w, r, node, r.PathValue("id"))
	})
	mux.Handle("GET /api/v1/agent/relay/{id}", relay)
	mux.Handle("PUT /api/v1/agent/relay/{id}", relay)

	mux.Handle("/api/", wrap(func(w http.ResponseWriter, r *http.Request) error {
		return httpErr{404, "not_found", "no such endpoint: " + r.Method + " " + r.URL.Path}
	}))
	mux.Handle("/", s.static())
	s.Init()
	return recoverPanic(securityHeaders(logRequests(mux, s.rateLimit(s.cors(s.csrfGuard(mux))))))
}

// ---- plumbing -------------------------------------------------------------

type httpErr struct {
	status int
	code   string
	msg    string
}

func (e httpErr) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return httpErr{400, "user_error", fmt.Sprintf(format, a...)}
}

func wrap(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeErr(w, err)
		}
	})
}

func writeErr(w http.ResponseWriter, err error) {
	var he httpErr
	status, code := 0, ""
	if errors.As(err, &he) {
		status, code = he.status, he.code
	} else {
		status, code = core.ErrStatus(err)
	}
	if status >= 500 {
		slog.Warn("request failed", "status", status, "err", err)
	}
	writeJSON(w, status, v1.Error{Error: err.Error(), Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func ok(w http.ResponseWriter, v any) error { return writeJSON(w, 200, v) }

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func bearer(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return r.URL.Query().Get("token")
}

func (s *Server) tokenOK(r *http.Request) bool {
	return s.tokenPrincipal(r) != nil
}

// tokenPrincipal resolves a bearer token: either the instance token from the
// config (what every existing CLI, agent and script holds, so it stays an
// administrator) or a named, revocable token from api_tokens.
func (s *Server) tokenPrincipal(r *http.Request) *auth.Principal {
	t := bearer(r)
	if t == "" {
		return nil
	}
	if s.Token != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1 {
		return &auth.Principal{Kind: "token", ID: "api-token", Name: "api-token", Role: auth.RoleAdmin}
	}
	at, err := s.store().AuthAPIToken(r.Context(), t)
	if err != nil {
		return nil
	}
	return &auth.Principal{Kind: "token", ID: at.ID, Name: at.Name, Role: at.Role}
}

// userAuth accepts the API token (CLI, agents, scripts) or an SSO session
// cookie (browser). The signed-in user is put on the request context so
// submissions can record who made them.
func (s *Server) userAuth(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.tokenPrincipal(r)
		if p == nil {
			var u *v1.User
			if s.Auth != nil && s.Auth.Enabled() {
				u = s.Auth.User(r.Context(), auth.SessionCookieValue(r))
			}
			if u == nil {
				writeJSON(w, 401, v1.Error{Error: "not signed in (use the Conductor login, `mldojo login`, or MLDOJO_TOKEN)", Code: "unauthorized"})
				return
			}
			r = r.WithContext(auth.WithUser(r.Context(), u))
			p = auth.PrincipalOf(u)
		}
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		if err := h(rec, r); err != nil {
			writeErr(rec, err)
		}
		s.recordAudit(r, p, rec.status)
	})
}

// requireAdmin gates the operations that can reach outside the platform or
// hand out credentials: adding a node makes the server SSH somewhere, and
// secrets can be overwritten with values the server will then use.
func requireAdmin(h handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if !auth.PrincipalFrom(r.Context()).IsAdmin() {
			return httpErr{403, "forbidden", "this needs an administrator (an API token, or a user listed in sso.admins)"}
		}
		return h(w, r)
	}
}

// mayDelete refuses to let one member delete another's work. Unowned
// resources -- everything created before ownership was recorded -- stay
// deletable so existing data does not get stuck.
func mayDelete(ctx context.Context, kind, owner string) error {
	if auth.PrincipalFrom(ctx).Owns(owner) {
		return nil
	}
	return httpErr{403, "forbidden", fmt.Sprintf("this %s belongs to %s", kind, owner)}
}

// auditAction turns a request into "resource.verb" plus the thing acted on.
func auditAction(r *http.Request) (action, target string) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", ""
	}
	resource := strings.TrimSuffix(parts[0], "s")
	verb := strings.ToLower(r.Method)
	switch r.Method {
	case http.MethodPost:
		verb = "create"
	case http.MethodDelete:
		verb = "delete"
	case http.MethodPut, http.MethodPatch:
		verb = "update"
	}
	// A trailing segment that is not an id is the real verb: .../cancel.
	if n := len(parts); n > 1 {
		switch last := parts[n-1]; last {
		case "cancel", "refresh", "unlock", "upgrade", "push", "test", "rotate", "revoke":
			verb = last
		}
		target = parts[1]
	}
	return resource + "." + verb, target
}

// recordAudit logs state-changing requests. Reads are left out on purpose:
// they would bury the writes, which are what an audit trail is for.
func (s *Server) recordAudit(r *http.Request, p *auth.Principal, status int) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return
	}
	action, target := auditAction(r)
	if action == "" || strings.HasPrefix(r.URL.Path, "/api/v1/agent/") {
		return
	}
	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	s.store().AddAudit(r.Context(), models.AuditEntry{
		Actor: p.Name, ActorKind: p.Kind, Action: action, Target: target,
		Status: status, IP: ip, RequestID: ReqID(r.Context()),
	})
}

func (s *Server) agentAuth(h func(w http.ResponseWriter, r *http.Request, node string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := r.Header.Get("X-Mldojo-Node")
		if _, ok := s.Node.Hub.AuthAgent(r.Context(), node, backends.AgentToken(r)); !ok {
			writeJSON(w, 401, v1.Error{Error: "invalid agent credentials", Code: "unauthorized"})
			return
		}
		h(w, r, node)
	})
}

// cors answers only origins this instance actually serves. Reflecting any
// Origin was safe only because credentials were never allowed with it; one
// line elsewhere would have turned it into a full cross-site read.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && s.sameOrigin(r) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Include-Summary, Range")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "X-Log-Size, Content-Range, X-Total-Count, X-Request-Id")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Hijack is needed for WebSocket upgrades through the recorder.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	return h.Hijack()
}

type ctxKeyReqID struct{}

// ReqID returns the request id attached by logRequests, for log correlation
// across the CLI -> API -> hub -> agent path.
func ReqID(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyReqID{}).(string)
	return s
}

func newReqID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// logRequests logs and measures every /api/ request. mux is consulted only
// for the route pattern the request matched, which is what the metrics are
// labelled with.
func logRequests(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newReqID()
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyReqID{}, id))
		route := routePattern(mux, r)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		obs.HTTPInFlight.Inc()
		next.ServeHTTP(rec, r)
		obs.HTTPInFlight.Add(-1)
		obs.HTTPRequests.With(r.Method, route, strconv.Itoa(rec.status)).Inc()
		// Streaming endpoints last as long as the client watches; their
		// duration would say nothing about the server's latency.
		if strings.HasSuffix(r.URL.Path, "/ws") || strings.HasPrefix(r.URL.Path, "/api/v1/agent/") {
			return
		}
		obs.HTTPLatency.With(r.Method, route).Observe(time.Since(start).Seconds())
		slog.Info("http", "id", id, "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

// recoverPanic keeps one bad request from taking the whole API down: without
// it a panic in any handler goroutine kills the process and drops every open
// log stream.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v) // the server's own "drop this connection" signal
			}
			slog.Error("panic in handler", "id", ReqID(r.Context()), "method", r.Method,
				"path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			writeJSON(w, 500, v1.Error{Error: "internal error", Code: "internal"})
		}()
		next.ServeHTTP(w, r)
	})
}

// static serves the exported Next.js app: /x -> x.html, /x/ -> x/index.html.
func (s *Server) static() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		root := s.WebDir
		if st, err := os.Stat(filepath.Join(root, "index.html")); root == "" || err != nil || st.IsDir() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<h1>MLDojo API %s</h1><p>The web app is not built (set api.web_dir / MLDOJO_WEB_DIR to web/out). API: <a href=\"/api/v1/health\">/api/v1/health</a></p>", version.Version)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if p == "/SKILL.md" { // checked after Clean, so /SKILL.md/ cannot slip past
			s.skill(w, r, filepath.Join(root, "SKILL.md"))
			return
		}
		try := []string{p, p + ".html", path.Join(p, "index.html")}
		for _, c := range try {
			f := filepath.Join(root, filepath.FromSlash(c))
			if st, err := os.Stat(f); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "/_next/static/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else if strings.HasSuffix(f, ".html") || strings.HasSuffix(f, "sw.js") {
					w.Header().Set("Cache-Control", "no-cache")
				}
				http.ServeFile(w, r, f)
				return
			}
		}
		w.WriteHeader(404)
		if b, err := os.ReadFile(filepath.Join(root, "404.html")); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(b)
			return
		}
		fmt.Fprint(w, "not found")
	})
}

// skill serves the CLI guide install.sh puts in the web root. It names
// internal hosts and paths, so it takes the same login as the API; a browser
// without a session goes through the Conductor login and comes back here.
func (s *Server) skill(w http.ResponseWriter, r *http.Request, file string) {
	sso := s.Auth != nil && s.Auth.Enabled()
	switch {
	case s.tokenOK(r) || sso && s.Auth.User(r.Context(), auth.SessionCookieValue(r)) != nil:
		w.Header().Set("Cache-Control", "private, no-cache")
		http.ServeFile(w, r, file)
	case sso && strings.Contains(r.Header.Get("Accept"), "text/html"):
		http.Redirect(w, r, "/api/v1/auth/login?next=/SKILL.md", http.StatusFound)
	default:
		http.Error(w, `not signed in: use the Conductor login, or curl -H "Authorization: Bearer $MLDOJO_TOKEN"`, http.StatusUnauthorized)
	}
}

var cliFile = regexp.MustCompile(`^mldojo-(linux|darwin)-(amd64|arm64)$`)

// download serves the CLI binaries from the agent dist dir. Public on
// purpose: a fresh machine has no credentials yet, and the binary holds none.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	f := r.PathValue("file")
	if !cliFile.MatchString(f) || s.RT == nil || s.RT.Cfg.AgentDist == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=mldojo")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filepath.Join(s.RT.Cfg.AgentDist, f))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	h := v1.Health{OK: true, Version: version.Version, DB: "ok", Secrets: "locked", AgentsOn: s.Node.Hub.Online()}
	if err := s.RT.Store.DB.Ping(ctx); err != nil {
		h.OK, h.DB = false, err.Error()
	}
	if s.RT.Secrets.Status().Unlocked {
		h.Secrets = "unlocked"
	}
	if len(s.Queues) > 0 {
		h.QueuePlugins = map[string]bool{}
		for name, q := range s.Queues {
			h.QueuePlugins[name] = q.Ready(ctx)
		}
	}
	return ok(w, h)
}
