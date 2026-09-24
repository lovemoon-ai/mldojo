// Package auth implements Conductor SSO (OAuth authorization-code) and the
// browser sessions it creates. The API token keeps working for the CLI and
// agents; SSO only adds a second way for humans to authenticate.
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lovemoon-ai/mldojo/api/internal/models"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

const (
	SessionCookie = "mldojo_session"
	stateCookie   = "mldojo_oauth_state"
	SessionTTL    = 30 * 24 * time.Hour
	stateTTL      = 10 * time.Minute
)

type Config struct {
	BaseURL      string   // https://conductor.example.com
	ClientID     string   // mldojo
	ClientSecret string   //
	AppBaseURL   string   // https://mldojo.example.com (never derived from the request: nginx hides it)
	Allow        []string // required allowlist of id/email/phone/name; empty disables SSO
	Admins       []string // subset of Allow that gets the admin role
}

// Configured reports whether the OAuth credentials are present. SSO also needs
// a non-empty allowlist to be Enabled: an empty one used to let every account
// of the provider in, which is not a safe default for a public deployment.
func (c Config) Configured() bool {
	return c.BaseURL != "" && c.ClientID != "" && c.ClientSecret != "" && c.AppBaseURL != ""
}

func (c Config) Enabled() bool { return c.Configured() && len(c.Allow) > 0 }

type Manager struct {
	Cfg   Config
	Store *models.Store
	HTTP  *http.Client
}

func New(cfg Config, store *models.Store) *Manager {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.AppBaseURL = strings.TrimRight(cfg.AppBaseURL, "/")
	return &Manager{Cfg: cfg, Store: store, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (m *Manager) Enabled() bool { return m.Cfg.Enabled() }

// CallbackURL must match the redirect_uri registered with the provider exactly.
func (m *Manager) CallbackURL() string { return m.Cfg.AppBaseURL + "/api/v1/auth/callback" }

func randToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// AuthorizeURL builds the browser redirect to the provider.
func (m *Manager) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {m.Cfg.ClientID},
		"redirect_uri":  {m.CallbackURL()},
		"response_type": {"code"},
		"state":         {state},
	}
	return m.Cfg.BaseURL + "/oauth/authorize?" + q.Encode()
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	User        struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Phone string `json:"phone"`
		Name  string `json:"name"` // always null today
	} `json:"user"`
	ConductorBaseURL string `json:"conductor_base_url"`
	// Errors: invalid_request | unsupported_grant_type | invalid_grant
	// (bad/expired code or redirect_uri mismatch) | invalid_client (401).
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Exchange trades the authorization code for the identity behind it.
func (m *Manager) Exchange(ctx context.Context, code string) (*v1.User, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     m.Cfg.ClientID,
		"client_secret": m.Cfg.ClientSecret,
		"code":          code,
		"redirect_uri":  m.CallbackURL(),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", m.Cfg.BaseURL+"/api/oauth/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenResponse
	json.Unmarshal(raw, &tr)
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(tr.Error + " " + tr.Message)
		if strings.TrimSpace(msg) == "" {
			msg = strings.TrimSpace(string(raw))
		}
		switch tr.Error {
		case "invalid_client":
			msg += " (client_id/client_secret do not match the ones registered with the SSO provider)"
		case "invalid_grant":
			msg += " (the code expired or was already used, or redirect_uri does not match the registered value byte for byte)"
		}
		return nil, fmt.Errorf("token exchange failed (HTTP %d): %s", resp.StatusCode, msg)
	}
	if tr.AccessToken == "" || tr.User.ID == "" || !strings.EqualFold(tr.TokenType, "bearer") {
		return nil, fmt.Errorf("token response missing access_token/user/token_type")
	}
	u := v1.User{ID: tr.User.ID, Provider: "conductor", Email: tr.User.Email, Phone: tr.User.Phone, Name: tr.User.Name}
	if !m.allowed(u) {
		return nil, fmt.Errorf("%s is not on this instance's allowlist", u.Display())
	}
	// The Conductor access token is not stored: MLDojo never calls Conductor
	// on the user's behalf, so there is nothing to keep at rest.
	saved, err := m.Store.UpsertUser(ctx, u)
	if err != nil {
		return nil, err
	}
	// Roles follow the config, so promoting someone is a config edit and a
	// re-login rather than a manual UPDATE.
	role := RoleMember
	if matches(u, m.Cfg.Admins) {
		role = RoleAdmin
	}
	if saved.Role != role {
		if err := m.Store.SetUserRole(ctx, saved.ID, role); err != nil {
			return nil, err
		}
		saved.Role = role
	}
	return saved, nil
}

// matches reports whether any of the user's identifiers is in list.
func matches(u v1.User, list []string) bool {
	for _, a := range list {
		a = strings.TrimSpace(strings.ToLower(a))
		if a == "" {
			continue
		}
		for _, v := range []string{u.ID, u.Email, u.Phone, u.Name} {
			if v != "" && strings.ToLower(v) == a {
				return true
			}
		}
	}
	return false
}

func (m *Manager) allowed(u v1.User) bool {
	if len(m.Cfg.Allow) == 0 {
		return false // no allowlist: deny (SSO is not Enabled in this state anyway)
	}
	return matches(u, m.Cfg.Allow)
}

// NewState returns the CSRF state and the cookie value carrying it plus the
// post-login destination.
func NewState(next string) (state, cookie string) {
	state = randToken(24)
	b, _ := json.Marshal(map[string]string{"s": state, "n": next})
	return state, base64.RawURLEncoding.EncodeToString(b)
}

// ParseState validates the state cookie against the query parameter.
func ParseState(cookie, state string) (next string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil {
		return "", false
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil || m["s"] == "" || m["s"] != state {
		return "", false
	}
	return m["n"], true
}

// SafeNext keeps redirects on this site (no open redirect).
func SafeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// StartSession creates a session and returns the cookie value.
func (m *Manager) StartSession(ctx context.Context, u *v1.User, userAgent string) (string, time.Time, error) {
	tok := randToken(32)
	exp := time.Now().Add(SessionTTL)
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	return tok, exp, m.Store.CreateSession(ctx, hash(tok), u.ID, userAgent, exp)
}

// User returns the session's user, or nil.
//
// The allowlist is re-checked here, not just at login: taking someone off it
// has to end their access now. Sessions last 30 days, so without this a
// removed user would keep working for a month.
func (m *Manager) User(ctx context.Context, cookie string) *v1.User {
	if cookie == "" {
		return nil
	}
	u, err := m.Store.Session(ctx, hash(cookie))
	if err != nil {
		return nil
	}
	if !m.allowed(*u) {
		m.Store.DeleteSession(ctx, hash(cookie))
		slog.Warn("session dropped: user is no longer on sso.allow", "user", u.Display())
		return nil
	}
	return u
}

func (m *Manager) EndSession(ctx context.Context, cookie string) {
	if cookie != "" {
		m.Store.DeleteSession(ctx, hash(cookie))
	}
}

// Cookies ---------------------------------------------------------------

func (m *Manager) secure() bool { return strings.HasPrefix(m.Cfg.AppBaseURL, "https://") }

func (m *Manager) SetSessionCookie(w http.ResponseWriter, value string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: value, Path: "/", Expires: exp,
		HttpOnly: true, Secure: m.secure(), SameSite: http.SameSiteLaxMode})
}

func (m *Manager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: m.secure(), SameSite: http.SameSiteLaxMode})
}

func (m *Manager) SetStateCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: value, Path: "/api/v1/auth", MaxAge: int(stateTTL.Seconds()),
		HttpOnly: true, Secure: m.secure(), SameSite: http.SameSiteLaxMode})
}

func (m *Manager) StateCookie(r *http.Request) string {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (m *Manager) ClearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/api/v1/auth", MaxAge: -1,
		HttpOnly: true, Secure: m.secure(), SameSite: http.SameSiteLaxMode})
}

func SessionCookieValue(r *http.Request) string {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// Request context --------------------------------------------------------

type ctxKey struct{}

func WithUser(ctx context.Context, u *v1.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the signed-in user, or nil for token auth.
func UserFrom(ctx context.Context) *v1.User {
	u, _ := ctx.Value(ctxKey{}).(*v1.User)
	return u
}

// Actor is a short label for who did something ("" for the API token).
func Actor(ctx context.Context) string {
	return UserFrom(ctx).Display()
}

// Authorization ----------------------------------------------------------

// Roles. Members read everything -- this is a team tool, not a silo -- but
// may only delete what they own, and may not touch infrastructure.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Principal is the authenticated caller: a signed-in user, or an API token.
type Principal struct {
	Kind string // "user" | "token"
	ID   string
	Name string // what lands in owner/submitter fields
	Role string
}

func (p *Principal) IsAdmin() bool { return p != nil && p.Role == RoleAdmin }

// Actor is the name to record as owner/submitter, nil-safe so unauthenticated
// paths do not panic.
func (p *Principal) Actor() string {
	if p == nil {
		return ""
	}
	return p.Name
}

// Owns reports whether p may act on something owned by owner. An unowned
// resource (created before ownership was recorded) is free for anyone, so
// existing data does not become undeletable.
func (p *Principal) Owns(owner string) bool {
	return p.IsAdmin() || owner == "" || (p != nil && p.Name != "" && owner == p.Name)
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller, or nil on an unauthenticated route.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// PrincipalOf builds the principal for a signed-in user.
func PrincipalOf(u *v1.User) *Principal {
	role := u.Role
	if role == "" {
		role = RoleMember
	}
	return &Principal{Kind: "user", ID: u.ID, Name: u.Display(), Role: role}
}
