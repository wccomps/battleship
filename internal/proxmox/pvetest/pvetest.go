// Package pvetest is a fake Proxmox access layer on httptest, for tests of
// battleship's delegated authorization: users, login tickets that expire
// and renew, per-path ACLs answering /access/permissions, 401 for a
// ticket Proxmox no longer takes and 403 for a missing privilege, and the
// OpenID login (auth-url, the identity provider's redirect, login) a
// browser goes through. Its identity provider signs in, without a form,
// whoever SignIn named last.
//
// The real proxmox.Client talks to it; Client returns one.
package pvetest

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/proxmox"
)

// Realm is the OpenID realm the fake knows.
const Realm = "auth.example.org"

// Server is the fake. Its methods are safe for concurrent use.
type Server struct {
	srv *httptest.Server

	mu      sync.Mutex
	now     func() time.Time
	users   map[string]*user
	tickets map[string]ticket // by ticket
	states  map[string]loginState
	codes   map[string]string // authorization code -> user
	current string            // whom the identity provider signs in
	idpErr  string            // if set, the identity provider refuses with it
	logins  int
	renews  int
	perms   int // /access/permissions calls
}

type user struct {
	enabled bool
	acl     map[string]map[string]bool // path -> privilege -> propagates
}

type ticket struct {
	user   string
	issued time.Time
}

type loginState struct {
	redirect string
	created  time.Time
}

// New starts a fake that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{now: time.Now, users: map[string]*user{}, tickets: map[string]ticket{},
		states: map[string]loginState{}, codes: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/access/openid/auth-url", s.authURL)
	mux.HandleFunc("POST /api2/json/access/openid/login", s.login)
	mux.HandleFunc("POST /api2/json/access/ticket", s.renew)
	mux.HandleFunc("GET /api2/json/access/permissions", s.permissions)
	mux.HandleFunc("GET /idp/authorize", s.authorize)
	s.srv = httptest.NewTLSServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the fake's base URL, as proxmox.url.
func (s *Server) URL() string { return s.srv.URL }

// Endpoint is the fake's host:port, as the client names endpoints.
func (s *Server) Endpoint() string { return strings.TrimPrefix(s.srv.URL, "https://") }

// Client is a proxmox.Client for the fake, without a credential.
func (s *Server) Client() *proxmox.Client {
	return proxmox.New(proxmox.Options{URLs: []string{s.srv.URL}, InsecureSkipVerify: true})
}

// SetClock sets the fake's clock, for ticket ages. Default time.Now.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// AddUser creates an enabled user (e.g. alice@auth.example.org) with an
// ACL: path -> privilege -> whether it propagates to the paths below.
func (s *Server) AddUser(name string, acl map[string]map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acl == nil {
		acl = map[string]map[string]bool{}
	}
	s.users[name] = &user{enabled: true, acl: acl}
}

// Grant gives name privs on path, propagating.
func (s *Server) Grant(name, path string, privs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[name]
	if u.acl[path] == nil {
		u.acl[path] = map[string]bool{}
	}
	for _, p := range privs {
		u.acl[path][p] = true
	}
}

// Revoke removes every privilege name has on path.
func (s *Server) Revoke(name, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users[name].acl, path)
}

// Disable disables a user: Proxmox refuses their tickets at once (401).
func (s *Server) Disable(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[name].enabled = false
}

// SignIn makes the identity provider sign in name next.
func (s *Server) SignIn(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current, s.idpErr = name, ""
}

// RefuseSignIn makes the identity provider answer with an OAuth error.
func (s *Server) RefuseSignIn(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idpErr = code
}

// Ticket issues a ticket for name now, as a login would.
func (s *Server) Ticket(name string) proxmox.Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issue(name)
}

func (s *Server) issue(name string) proxmox.Credential {
	now := s.now()
	t := fmt.Sprintf("PVE:%s:%08X::%s", name, now.Unix(), random())
	s.tickets[t] = ticket{user: name, issued: now}
	c := proxmox.TicketCredential(name, t, fmt.Sprintf("%08X:%s", now.Unix(), random()), now)
	c.LoginAt = now
	return c
}

// Counts says how many logins, renewals and permission reads the fake
// answered.
func (s *Server) Counts() (logins, renews, perms int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins, s.renews, s.perms
}

// Accepts reports whether Proxmox would take cred now: a known ticket of
// an enabled user, not expired. Tokens are always taken. In-memory fakes
// use it to answer 401 like the real cluster.
func (s *Server) Accepts(cred proxmox.Credential) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cred.IsToken() {
		return true
	}
	_, ok := s.valid(cred.Ticket)
	return ok
}

// Allowed reports whether user holds priv on path, as Proxmox resolves it
// (see PermissionsAt).
func (s *Server) Allowed(name, path, priv string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.effective(name, path)[priv]
	return ok
}

func (s *Server) valid(t string) (string, bool) {
	tk, ok := s.tickets[t]
	if !ok {
		return "", false
	}
	u := s.users[tk.user]
	now := s.now()
	if u == nil || !u.enabled || !now.Before(tk.issued.Add(proxmox.TicketLifetime)) || now.Before(tk.issued.Add(-5*time.Minute)) {
		return "", false
	}
	return tk.user, true
}

// effective is name's privileges at path: the ACL at path, else what the
// nearest ancestor's ACL propagates. As in Proxmox, a deeper ACL replaces
// the inherited one rather than adding to it.
func (s *Server) effective(name, path string) map[string]bool {
	u := s.users[name]
	if u == nil {
		return nil
	}
	if privs, ok := u.acl[path]; ok {
		return privs
	}
	for p := parent(path); ; p = parent(p) {
		if privs, ok := u.acl[p]; ok {
			out := map[string]bool{}
			for k, prop := range privs {
				if prop {
					out[k] = true
				}
			}
			return out
		}
		if p == "/" {
			return map[string]bool{}
		}
	}
}

func parent(path string) string {
	i := strings.LastIndex(strings.TrimSuffix(path, "/"), "/")
	if i <= 0 {
		return "/"
	}
	return path[:i]
}

func random() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func reply(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// refuse answers like Proxmox: the reason in the status line, no body data.
func refuse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"data":null,"message":%q}`, msg)
}

func (s *Server) authURL(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("realm") != Realm {
		refuse(w, 400, "unknown realm")
		return
	}
	state := random()
	s.mu.Lock()
	s.states[state] = loginState{redirect: r.PostFormValue("redirect-url"), created: s.now()}
	s.mu.Unlock()
	q := url.Values{"state": {state}, "redirect_uri": {r.PostFormValue("redirect-url")}, "client_id": {"proxmox"}}
	reply(w, s.srv.URL+"/idp/authorize?"+q.Encode())
}

// authorize is the identity provider: it signs in whoever SignIn named and
// sends the browser back with a code.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || back.Host == "" {
		http.Error(w, "bad redirect_uri", 400)
		return
	}
	s.mu.Lock()
	who, refused := s.current, s.idpErr
	code := random()
	if who != "" && refused == "" {
		s.codes[code] = who
	}
	s.mu.Unlock()
	v := url.Values{"state": {q.Get("state")}}
	switch {
	case refused != "":
		v.Set("error", refused)
	case who == "":
		v.Set("error", "access_denied")
	default:
		v.Set("code", code)
	}
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// login answers every failure with a bare 401, as Proxmox does.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[r.PostFormValue("state")]
	delete(s.states, r.PostFormValue("state")) // single use
	who, known := s.codes[r.PostFormValue("code")]
	delete(s.codes, r.PostFormValue("code"))
	u := s.users[who]
	if !ok || !known || st.redirect != r.PostFormValue("redirect-url") || !s.now().Before(st.created.Add(10*time.Minute)) || u == nil || !u.enabled {
		refuse(w, 401, "authentication failure")
		return
	}
	s.logins++
	c := s.issue(who)
	reply(w, map[string]any{"username": who, "ticket": c.Ticket, "CSRFPreventionToken": c.CSRF, "cap": map[string]any{}})
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	who, ok := s.valid(r.PostFormValue("password"))
	if !ok || who != r.PostFormValue("username") {
		refuse(w, 401, "authentication failure")
		return
	}
	s.renews++
	c := s.issue(who)
	reply(w, map[string]any{"username": who, "ticket": c.Ticket, "CSRFPreventionToken": c.CSRF, "cap": map[string]any{}})
}

// caller is the user r's ticket belongs to, or "" (then it answered 401).
func (s *Server) caller(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie("PVEAuthCookie")
	var t string
	if err == nil {
		t, _ = url.QueryUnescape(c.Value)
	}
	who, ok := s.valid(t)
	if !ok {
		refuse(w, 401, "authentication failure")
		return ""
	}
	if r.Method != http.MethodGet {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("CSRFPreventionToken")), []byte("")) == 1 {
			refuse(w, 401, "permission denied - invalid csrf token")
			return ""
		}
	}
	return who
}

func (s *Server) permissions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	who := s.caller(w, r)
	if who == "" {
		return
	}
	s.perms++
	out := map[string]map[string]int{}
	add := func(path string, privs map[string]bool) {
		m := map[string]int{}
		for p, prop := range privs {
			m[p] = 0
			if prop {
				m[p] = 1
			}
		}
		out[path] = m
	}
	if path := r.URL.Query().Get("path"); path != "" {
		add(path, s.effective(who, path))
		reply(w, out)
		return
	}
	for path, privs := range s.users[who].acl {
		if len(privs) > 0 {
			add(path, privs)
		}
	}
	reply(w, out)
}

// Known reports whether the fake has a user called name.
func (s *Server) Known(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.users[name]
	return ok
}

// CertPool holds the fake's certificate, as proxmox.ca_file would hold the
// cluster CA.
func (s *Server) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	return pool
}
