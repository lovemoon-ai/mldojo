package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
)

func req(method, target, origin, bearer string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "mldojo.example.com"
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestSameOrigin(t *testing.T) {
	s := &Server{Origins: []string{"https://app.example.com"}}
	cases := []struct {
		name, origin string
		want         bool
	}{
		{"no origin (CLI, agent, curl)", "", true},
		{"the API's own host", "https://mldojo.example.com", true},
		{"a configured app origin", "https://app.example.com", true},
		// The session cookie is SameSite=Lax, which treats every sibling of
		// the registrable domain as same-site. This is the case that matters.
		{"sibling host on the same site", "https://evil.example.com", false},
		{"unrelated site", "https://attacker.test", false},
		{"garbage", "not-a-url", false},
	}
	for _, c := range cases {
		if got := s.sameOrigin(req("POST", "/api/v1/runs", c.origin, "")); got != c.want {
			t.Errorf("%s: sameOrigin(%q) = %v, want %v", c.name, c.origin, got, c.want)
		}
	}
}

func TestCSRFGuard(t *testing.T) {
	s := &Server{Origins: []string{"https://app.example.com"}}
	reached := false
	h := s.csrfGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	cases := []struct {
		name, method, origin, token string
		wantStatus                  int
	}{
		{"cookie POST from a sibling host", "POST", "https://evil.example.com", "", 403},
		{"cookie DELETE from a sibling host", "DELETE", "https://evil.example.com", "", 403},
		{"cookie POST from the app", "POST", "https://app.example.com", "", 200},
		// A cross-site page cannot read the bearer token, so there is nothing
		// to forge -- blocking these would break legitimate API clients.
		{"token POST from anywhere", "POST", "https://evil.example.com", "mld_x", 200},
		{"GET is not a state change", "GET", "https://evil.example.com", "", 200},
	}
	for _, c := range cases {
		reached = false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req(c.method, "/api/v1/runs", c.origin, c.token))
		if w.Code != c.wantStatus {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.wantStatus)
		}
		if got := reached; got != (c.wantStatus == 200) {
			t.Errorf("%s: handler reached = %v, want %v", c.name, got, c.wantStatus == 200)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(60, 3) // 1/s, burst 3
	for i := range 3 {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d was refused inside the burst", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Error("the burst was not enforced")
	}
	if !l.allow("5.6.7.8") {
		t.Error("one client's burst blocked another's first request")
	}
	// Tokens refill over time.
	l.mu.Lock()
	l.buckets["1.2.3.4"].seen = time.Now().Add(-2 * time.Second)
	l.mu.Unlock()
	if !l.allow("1.2.3.4") {
		t.Error("tokens did not refill")
	}
}

func TestPrincipalOwns(t *testing.T) {
	admin := &auth.Principal{Kind: "user", Name: "alice", Role: auth.RoleAdmin}
	member := &auth.Principal{Kind: "user", Name: "bob", Role: auth.RoleMember}
	cases := []struct {
		name  string
		p     *auth.Principal
		owner string
		want  bool
	}{
		{"admin takes anyone's", admin, "bob", true},
		{"member takes their own", member, "bob", true},
		{"member refused someone else's", member, "alice", false},
		// Everything created before ownership was recorded has no owner and
		// must stay deletable, or old data gets stuck forever.
		{"unowned is free", member, "", true},
		{"nil principal on unowned", nil, "", true},
		{"nil principal on owned", nil, "alice", false},
	}
	for _, c := range cases {
		if got := c.p.Owns(c.owner); got != c.want {
			t.Errorf("%s: Owns(%q) = %v, want %v", c.name, c.owner, got, c.want)
		}
	}
}

func TestAuditAction(t *testing.T) {
	cases := []struct{ method, path, action, target string }{
		{"DELETE", "/api/v1/runs/abc123", "run.delete", "abc123"},
		{"POST", "/api/v1/runs", "run.create", ""},
		{"POST", "/api/v1/runs/abc123/cancel", "run.cancel", "abc123"},
		{"POST", "/api/v1/nodes", "node.create", ""},
		{"POST", "/api/v1/nodes/gpu-a/upgrade", "node.upgrade", "gpu-a"},
		{"POST", "/api/v1/secrets/unlock", "secret.unlock", "unlock"},
		{"DELETE", "/api/v1/projects/demo", "project.delete", "demo"},
	}
	for _, c := range cases {
		action, target := auditAction(req(c.method, c.path, "", ""))
		if action != c.action || target != c.target {
			t.Errorf("%s %s -> (%q, %q), want (%q, %q)", c.method, c.path, action, target, c.action, c.target)
		}
	}
}

func TestSkillNeedsLogin(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"index.html", "SKILL.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sso := auth.Config{BaseURL: "https://sso.test", ClientID: "c", ClientSecret: "s", AppBaseURL: "https://app.test", Allow: []string{"a"}}
	s := &Server{Token: "mld_x", WebDir: dir, Auth: &auth.Manager{Cfg: sso}}
	cases := []struct {
		name, path, token, accept string
		want                      int
	}{
		{"curl without a token", "/SKILL.md", "", "*/*", 401},
		{"a trailing slash names the same file", "/SKILL.md/", "", "*/*", 401},
		{"browser without a session goes to the login", "/SKILL.md", "", "text/html,application/xhtml+xml", 302},
		{"API token", "/SKILL.md", "mld_x", "*/*", 200},
		{"the rest of the web app stays public", "/", "", "text/html", 200},
	}
	for _, c := range cases {
		r := req("GET", c.path, "", c.token)
		r.Header.Set("Accept", c.accept)
		w := httptest.NewRecorder()
		s.static().ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, w.Code, c.want)
		}
	}
}
