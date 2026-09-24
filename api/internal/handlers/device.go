package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	ttemplate "text/template"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
)

// Device login lets `mldojo login` on a headless box get a token without
// anyone pasting one: the CLI shows a short code, the user approves it in a
// browser where they are signed in with SSO, and the CLI's poll picks up a
// named token issued to that user. Pending logins live in memory: they last
// ten minutes, and a restart only means running `mldojo login` again.

const (
	deviceTTL      = 10 * time.Minute
	deviceMaxOpen  = 200
	deviceTokenTTL = 90 * 24 * time.Hour
	userCodeChars  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I
)

type deviceLogin struct {
	userCode, host, ip string
	exp                time.Time
	token              string // set on approval, handed out once
	denied             bool
}

type deviceLogins struct {
	mu sync.Mutex
	m  map[string]*deviceLogin // by sha256(device_code)
}

func devKey(deviceCode string) string {
	h := sha256.Sum256([]byte(deviceCode))
	return hex.EncodeToString(h[:])
}

// byUserCode must be called with mu held; it also drops expired entries.
func (d *deviceLogins) byUserCode(code string) *deviceLogin {
	var hit *deviceLogin
	for k, l := range d.m {
		if time.Now().After(l.exp) {
			delete(d.m, k)
		} else if l.userCode == code {
			hit = l
		}
	}
	return hit
}

func newUserCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = userCodeChars[int(b[i])%len(userCodeChars)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func (s *Server) ssoOn() bool { return s.Auth != nil && s.Auth.Enabled() }

// deviceStart is unauthenticated: it is how a machine with no credentials begins.
func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request) error {
	if !s.ssoOn() {
		return badRequest("browser login needs SSO on the server; use `mldojo login --token` instead")
	}
	var req struct {
		Hostname string `json:"hostname"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	b := make([]byte, 32)
	rand.Read(b)
	deviceCode := hex.EncodeToString(b)
	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = r.RemoteAddr
	}
	d := &s.devices
	d.mu.Lock()
	if d.m == nil {
		d.m = map[string]*deviceLogin{}
	}
	code := newUserCode()
	for d.byUserCode(code) != nil {
		code = newUserCode()
	}
	if len(d.m) >= deviceMaxOpen {
		d.mu.Unlock()
		return httpErr{429, "rate_limited", "too many pending logins; try again in a few minutes"}
	}
	d.m[devKey(deviceCode)] = &deviceLogin{userCode: code, host: trunc(req.Hostname, 64), ip: trunc(ip, 64),
		exp: time.Now().Add(deviceTTL)}
	d.mu.Unlock()
	return ok(w, map[string]any{
		"device_code": deviceCode, "user_code": code,
		"verification_url": s.Auth.Cfg.AppBaseURL + "/activate?code=" + code,
		"expires_in":       int(deviceTTL.Seconds()), "interval": 3,
	})
}

// devicePoll hands the token out exactly once, to whoever holds the device code.
func (s *Server) devicePoll(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	d := &s.devices
	d.mu.Lock()
	defer d.mu.Unlock()
	k := devKey(req.DeviceCode)
	l := d.m[k]
	switch {
	case l == nil || time.Now().After(l.exp):
		delete(d.m, k)
		return httpErr{404, "not_found", "login request expired or unknown; run `mldojo login` again"}
	case l.denied:
		delete(d.m, k)
		return httpErr{403, "forbidden", "login request was denied in the browser"}
	case l.token == "":
		return ok(w, map[string]string{"status": "pending"})
	}
	delete(d.m, k)
	return ok(w, map[string]string{"status": "approved", "token": l.token})
}

var activatePage = template.Must(template.New("a").Parse(`<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.T.title}}</title>
<style>body{font:16px system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#222}
code{font-size:1.6rem;letter-spacing:.15em}button,input{font:inherit;padding:.4rem 1rem}.muted{color:#777;font-size:.9rem}</style>
<h2>{{.T.title}}</h2>
{{if .Msg}}<p>{{.Msg}}</p>{{else if .Host}}
<p>{{printf .T.request .User .Host .IP}}</p>
<p>{{.T.code}} <code>{{.Code}}</code></p>
<p class="muted">{{.T.warn}}</p>
<form method="post"><input type="hidden" name="code" value="{{.Code}}">
<button name="action" value="approve">{{.T.approve}}</button> <button name="action" value="deny">{{.T.deny}}</button></form>
{{else}}<form method="get"><p>{{.T.enter}}</p><input name="code" placeholder="XXXX-XXXX" autofocus> <button>{{.T.next}}</button></form>{{end}}`))

// activateText holds the device-login page strings per language; the page
// follows the browser's Accept-Language (Chinese or English).
var activateText = map[string]map[string]string{
	"en": {
		"title":    "MLDojo device login",
		"request":  "%s, the machine %s (%s) asks to sign in to the MLDojo CLI as you.",
		"code":     "Code:",
		"warn":     "Approve only if you just ran mldojo login yourself and your terminal shows the same code.",
		"approve":  "Approve",
		"deny":     "Deny",
		"enter":    "Enter the code shown in your terminal:",
		"next":     "Next",
		"unknown":  "The code does not exist or has expired (valid for 10 minutes); run mldojo login again.",
		"handled":  "This login request has already been handled.",
		"denied":   "Denied.",
		"approved": "Approved. Go back to your terminal; the CLI finishes signing in by itself.",
		"nohost":   "(unknown host)",
	},
	"zh": {
		"title":    "MLDojo 设备登录",
		"request":  "%s，机器 %s（%s）请求以你的身份登录 MLDojo 命令行。",
		"code":     "校验码：",
		"warn":     "只有在你本人刚刚执行了 mldojo login、且终端显示的校验码与上面一致时才批准。",
		"approve":  "批准",
		"deny":     "拒绝",
		"enter":    "输入终端上显示的校验码：",
		"next":     "下一步",
		"unknown":  "校验码不存在或已过期（10 分钟有效），请重新执行 mldojo login。",
		"handled":  "这个登录请求已经处理过了。",
		"denied":   "已拒绝。",
		"approved": "已批准，回到终端即可（命令行会自动完成登录）。",
		"nohost":   "(未知主机)",
	},
}

// pageLang picks "zh" when the browser's first preferred language is Chinese.
func pageLang(r *http.Request) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Accept-Language"))), "zh") {
		return "zh"
	}
	return "en"
}

// activate is the browser half: sign in with SSO, then approve the code.
func (s *Server) activate(w http.ResponseWriter, r *http.Request) {
	if !s.ssoOn() {
		http.Error(w, "SSO is not configured on this server", http.StatusNotFound)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	u := s.Auth.User(r.Context(), auth.SessionCookieValue(r))
	if u == nil {
		if r.Method != http.MethodGet {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		next := "/activate"
		if code != "" {
			next += "?code=" + url.QueryEscape(code)
		}
		http.Redirect(w, r, "/api/v1/auth/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	t := activateText[pageLang(r)]
	v := map[string]any{"User": u.Display(), "Code": code, "T": t}
	d := &s.devices
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.byUserCode(code)
	switch {
	case code == "":
	case l == nil:
		v["Msg"] = t["unknown"]
	case l.token != "" || l.denied:
		v["Msg"] = t["handled"]
	case r.Method == http.MethodPost && r.FormValue("action") == "deny":
		l.denied = true
		v["Msg"] = t["denied"]
	case r.Method == http.MethodPost:
		role := u.Role
		if role == "" {
			role = auth.RoleMember
		}
		// Named after the user, so runs the CLI submits are owned by them.
		plain, _, err := s.store().CreateAPIToken(r.Context(), u.Display(), role, "device login: "+l.host, deviceTokenTTL)
		if err != nil {
			http.Error(w, "could not issue a token", http.StatusInternalServerError)
			return
		}
		l.token = plain
		v["Msg"] = t["approved"]
	default:
		v["Host"], v["IP"] = l.host, l.ip
		if l.host == "" {
			v["Host"] = t["nohost"]
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	activatePage.Execute(w, v)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

//go:embed install.sh
var installSrc string
var installScript = ttemplate.Must(ttemplate.New("i").Parse(installSrc))

// installSh is public: it is how a machine with nothing installed gets the
// CLI. The URL baked in is the one the script was fetched from, so an
// intranet-only node downloads over the intranet. Behind the public proxy the
// Host is a loopback tunnel address, which maps back to the public base.
func (s *Server) installSh(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" {
		scheme = p
	}
	base := scheme + "://" + host
	public := ""
	if s.Auth != nil {
		public = s.Auth.Cfg.AppBaseURL
	}
	if h, _, err := net.SplitHostPort(host); public != "" && (host == "" || err == nil && net.ParseIP(h).IsLoopback() || h == "localhost") {
		base = public
	}
	login := "mldojo login"
	if base != public {
		login += " --server " + base
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	installScript.Execute(w, map[string]string{"Base": base, "Login": login})
}
