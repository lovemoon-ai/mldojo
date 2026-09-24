package handlers

import (
	"net/http"
	"net/url"

	"github.com/lovemoon-ai/mldojo/api/internal/auth"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

// authConfig is unauthenticated: the login page asks what it can offer.
func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) error {
	c := v1.AuthConfig{TokenLogin: true}
	if s.Auth != nil && s.Auth.Enabled() {
		c.SSOEnabled, c.Provider, c.LoginURL = true, "conductor", "/api/v1/auth/login"
	}
	return ok(w, c)
}

// authLogin redirects the browser to the Conductor authorize page.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) error {
	if s.Auth == nil || !s.Auth.Enabled() {
		return badRequest("SSO is not configured on this server (api.sso in config.yaml)")
	}
	state, cookie := auth.NewState(auth.SafeNext(r.URL.Query().Get("next")))
	s.Auth.SetStateCookie(w, cookie)
	http.Redirect(w, r, s.Auth.AuthorizeURL(state), http.StatusFound)
	return nil
}

// authCallback finishes the code exchange and starts a session.
func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) error {
	if s.Auth == nil || !s.Auth.Enabled() {
		return badRequest("SSO is not configured")
	}
	q := r.URL.Query()
	// Redirects are built from the configured public base URL: behind nginx
	// r.Host is the internal proxy address.
	fail := func(reason string) error {
		s.Auth.ClearStateCookie(w)
		http.Redirect(w, r, s.Auth.Cfg.AppBaseURL+"/login?error="+url.QueryEscape(reason), http.StatusFound)
		return nil
	}
	if e := q.Get("error"); e != "" {
		return fail(e)
	}
	next, okState := auth.ParseState(s.Auth.StateCookie(r), q.Get("state"))
	if !okState {
		return fail("login state expired, please try again")
	}
	code := q.Get("code")
	if code == "" {
		return fail("missing code")
	}
	user, err := s.Auth.Exchange(r.Context(), code)
	if err != nil {
		return fail(err.Error())
	}
	value, exp, err := s.Auth.StartSession(r.Context(), user, r.UserAgent())
	if err != nil {
		return err
	}
	s.Auth.ClearStateCookie(w)
	s.Auth.SetSessionCookie(w, value, exp)
	http.Redirect(w, r, s.Auth.Cfg.AppBaseURL+auth.SafeNext(next), http.StatusFound)
	return nil
}

// authMe reports the current identity (session or token).
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) error {
	st := v1.AuthStatus{}
	if s.Auth != nil && s.Auth.Enabled() {
		if u := s.Auth.User(r.Context(), auth.SessionCookieValue(r)); u != nil {
			st = v1.AuthStatus{Authenticated: true, Mode: "sso", User: u}
			return ok(w, st)
		}
	}
	if s.tokenOK(r) {
		st = v1.AuthStatus{Authenticated: true, Mode: "token"}
	}
	return ok(w, st)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) error {
	if s.Auth != nil {
		s.Auth.EndSession(r.Context(), auth.SessionCookieValue(r))
		s.Auth.ClearSessionCookie(w)
	}
	if r.Method == http.MethodGet { // convenience for a plain link
		base := "/"
		if s.Auth != nil && s.Auth.Cfg.AppBaseURL != "" {
			base = s.Auth.Cfg.AppBaseURL + "/login"
		}
		http.Redirect(w, r, base, http.StatusFound)
		return nil
	}
	return ok(w, map[string]bool{"ok": true})
}
