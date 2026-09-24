package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	"github.com/lovemoon-ai/mldojo/api/internal/backends"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

var testSSO = auth.Config{BaseURL: "https://sso.test", ClientID: "c", ClientSecret: "s", AppBaseURL: "https://app.test", Allow: []string{"u1"}}

func call(t *testing.T, h func(http.ResponseWriter, *http.Request) error, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	if err := h(w, httptest.NewRequest("POST", "/", strings.NewReader(body))); err != nil {
		writeErr(w, err)
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func startDevice(t *testing.T, s *Server) (device, user string) {
	t.Helper()
	code, st := call(t, s.deviceStart, `{"hostname":"box1"}`)
	if code != 200 {
		t.Fatalf("start: %d %v", code, st)
	}
	device, user = st["device_code"].(string), st["user_code"].(string)
	if st["verification_url"] != "https://app.test/activate?code="+user {
		t.Errorf("verification_url = %v", st["verification_url"])
	}
	return device, user
}

func TestDeviceLoginMemory(t *testing.T) {
	s := &Server{Auth: &auth.Manager{Cfg: testSSO}}
	device, user := startDevice(t, s)

	if code, st := call(t, s.devicePoll, `{"device_code":"`+device+`"}`); code != 200 || st["status"] != "pending" {
		t.Fatalf("poll before approval: %d %v", code, st)
	}
	if code, _ := call(t, s.devicePoll, `{"device_code":"wrong"}`); code != 404 {
		t.Errorf("unknown device code: %d, want 404", code)
	}

	// Signed out: the page sends the browser through SSO and back with the code.
	w := httptest.NewRecorder()
	s.activate(w, httptest.NewRequest("GET", "/activate?code="+user, nil))
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != 302 || loc.Path != "/api/v1/auth/login" || loc.Query().Get("next") != "/activate?code="+user {
		t.Errorf("signed-out activate: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = httptest.NewRecorder()
	s.activate(w, httptest.NewRequest("POST", "/activate", strings.NewReader("code="+user)))
	if w.Code != 401 {
		t.Errorf("signed-out approve: %d, want 401", w.Code)
	}

	s.devices.mu.Lock()
	s.devices.byUserCode(user).token = "mld_t"
	s.devices.mu.Unlock()
	if code, st := call(t, s.devicePoll, `{"device_code":"`+device+`"}`); code != 200 || st["token"] != "mld_t" {
		t.Fatalf("poll after approval: %d %v", code, st)
	}
	if code, _ := call(t, s.devicePoll, `{"device_code":"`+device+`"}`); code != 404 {
		t.Errorf("token handed out twice: second poll %d, want 404", code)
	}

	if code, _ := call(t, (&Server{}).deviceStart, `{}`); code != 400 {
		t.Errorf("start without SSO: %d, want 400", code)
	}
}

// TestDeviceLoginApprove runs the browser half against a real database.
func TestDeviceLoginApprove(t *testing.T) {
	dbURL := os.Getenv("MLDOJO_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set MLDOJO_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	st, err := models.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	m := &auth.Manager{Cfg: testSSO, Store: st}
	s := &Server{Auth: m, RT: &backends.Runtime{Store: st}}
	u, err := st.UpsertUser(ctx, v1.User{ID: "u1", Name: "Device Tester"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, exp, err := m.StartSession(ctx, u, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.EndSession(context.Background(), cookie) })
	withSession := func(r *http.Request) *http.Request {
		w := httptest.NewRecorder()
		m.SetSessionCookie(w, cookie, exp)
		r.AddCookie(w.Result().Cookies()[0])
		return r
	}
	form := func(v string) *http.Request {
		r := httptest.NewRequest("POST", "/activate", strings.NewReader(v))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return withSession(r)
	}

	device, user := startDevice(t, s)
	w := httptest.NewRecorder()
	s.activate(w, withSession(httptest.NewRequest("GET", "/activate?code="+strings.ToLower(user), nil)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "box1") || !strings.Contains(w.Body.String(), user) {
		t.Fatalf("approval page: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.activate(w, form("action=approve&code="+user))
	if !strings.Contains(w.Body.String(), "Approved.") {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	_, got := call(t, s.devicePoll, `{"device_code":"`+device+`"}`)
	tok, _ := got["token"].(string)
	at, err := st.AuthAPIToken(ctx, tok)
	if err != nil {
		t.Fatalf("issued token does not authenticate: %v", err)
	}
	t.Cleanup(func() { st.RevokeAPIToken(context.Background(), at.ID) })
	if at.Name != "Device Tester" || at.Role != auth.RoleMember || at.ExpiresAt == nil {
		t.Errorf("token = %+v; want the user's name and role, with an expiry", at)
	}

	device, user = startDevice(t, s)
	s.activate(httptest.NewRecorder(), form("action=deny&code="+user))
	if code, _ := call(t, s.devicePoll, `{"device_code":"`+device+`"}`); code != 403 {
		t.Errorf("denied poll: %d, want 403", code)
	}
}

func TestDownloadServesOnlyTheCLI(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"mldojo-darwin-arm64", "mldojo-agent-linux-amd64"} {
		os.WriteFile(filepath.Join(dir, f), []byte("bin"), 0o755)
	}
	s := &Server{RT: &backends.Runtime{Cfg: backends.Config{AgentDist: dir}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dl/{file}", s.download)
	for path, want := range map[string]int{
		"/dl/mldojo-darwin-arm64":      200,
		"/dl/mldojo-agent-linux-amd64": 404,
		"/dl/..%2fconfig.yaml":         404,
		"/dl/mldojo-linux-amd64":       404, // allowed name, not built
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: %d, want %d", path, w.Code, want)
		}
	}
}

// install.sh points at the address it was fetched from; the public proxy
// reaches the API over a loopback tunnel, which maps back to the public URL.
func TestInstallScriptBase(t *testing.T) {
	s := &Server{Auth: &auth.Manager{Cfg: testSSO}}
	for host, want := range map[string]string{
		"127.0.0.1:18765": `BASE="https://app.test"`,
		"192.0.2.10:8765": `BASE="http://192.0.2.10:8765"`,
	} {
		r := httptest.NewRequest("GET", "/install.sh", nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.installSh(w, r)
		body := w.Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("host %s: script lacks %s", host, want)
		}
		if intranet := host != "127.0.0.1:18765"; intranet != strings.Contains(body, "mldojo login --server") {
			t.Errorf("host %s: login hint wrong:\n%s", host, body)
		}
	}
}

func TestPageLang(t *testing.T) {
	for al, want := range map[string]string{"": "en", "en-US,en;q=0.9": "en", "zh-CN,zh;q=0.9,en;q=0.8": "zh", "en;q=1, zh": "en"} {
		r := httptest.NewRequest("GET", "/activate", nil)
		r.Header.Set("Accept-Language", al)
		if got := pageLang(r); got != want {
			t.Errorf("Accept-Language %q -> %q, want %q", al, got, want)
		}
	}
}
