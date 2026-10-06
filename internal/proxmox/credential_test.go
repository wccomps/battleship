package proxmox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// authRecorder is a fake node that records the credential headers of every
// request and answers like okNode.
type authRecorder struct {
	mu   sync.Mutex
	seen []seenAuth
}

type seenAuth struct {
	method, path          string
	authorization, cookie string
	csrf                  string
}

func (a *authRecorder) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, seenAuth{
		method: r.Method, path: strings.TrimPrefix(r.URL.Path, "/api2/json"),
		authorization: r.Header.Get("Authorization"), cookie: r.Header.Get("Cookie"),
		csrf: r.Header.Get("CSRFPreventionToken"),
	})
	a.mu.Unlock()
	okNode(w, r)
}

func (a *authRecorder) all() []seenAuth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]seenAuth(nil), a.seen...)
}

func bareClient(t *testing.T, urls ...string) *Client {
	t.Helper()
	return New(Options{URLs: urls, InsecureSkipVerify: true})
}

var testTicket = func() Credential {
	c := TicketCredential("jdoe@auth.example.org", "PVE:jdoe@auth.example.org:6700AAAA::sig", "6700AAAA:csrf", time.Unix(1_700_000_000, 0))
	c.LoginAt = time.Unix(1_699_990_000, 0)
	return c
}()

func TestTicketCredentialSendsCookieAndCSRFOnWrites(t *testing.T) {
	rec := &authRecorder{}
	n := newNode(t, rec.handler)
	c := bareClient(t, n.srv.URL).As(testTicket)
	ctx := context.Background()
	if _, err := c.ClusterVMs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.SetVMConfig(ctx, "n1", 101, map[string]string{"net0": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteVM(ctx, "n1", 101); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Clone(ctx, cloneReq); err != nil {
		t.Fatal(err)
	}
	seen := rec.all()
	if len(seen) != 4 {
		t.Fatalf("saw %d requests, want 4", len(seen))
	}
	wantCookie := "PVEAuthCookie=" + url.QueryEscape(testTicket.Ticket)
	for _, s := range seen {
		if s.authorization != "" {
			t.Errorf("%s %s sent Authorization %q with a ticket", s.method, s.path, s.authorization)
		}
		if s.cookie != wantCookie {
			t.Errorf("%s %s sent Cookie %q, want %q", s.method, s.path, s.cookie, wantCookie)
		}
		wantCSRF := testTicket.CSRF
		if s.method == http.MethodGet {
			wantCSRF = ""
		}
		if s.csrf != wantCSRF {
			t.Errorf("%s %s sent CSRFPreventionToken %q, want %q", s.method, s.path, s.csrf, wantCSRF)
		}
	}
}

func TestTokenCredentialSendsAuthorizationOnly(t *testing.T) {
	rec := &authRecorder{}
	n := newNode(t, rec.handler)
	c := bareClient(t, n.srv.URL).As(TokenCredential("jdoe@auth.example.org!cli", "s3cret"))
	if _, err := c.ClusterVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Power(context.Background(), "n1", 101, "start"); err != nil {
		t.Fatal(err)
	}
	for _, s := range rec.all() {
		if s.authorization != "PVEAPIToken=jdoe@auth.example.org!cli=s3cret" {
			t.Errorf("%s %s sent Authorization %q", s.method, s.path, s.authorization)
		}
		if s.cookie != "" || s.csrf != "" {
			t.Errorf("%s %s sent a cookie %q or CSRF token %q with a token", s.method, s.path, s.cookie, s.csrf)
		}
	}
}

func TestClientWithoutCredentialSendsNothing(t *testing.T) {
	rec := &authRecorder{}
	n := newNode(t, rec.handler)
	c := bareClient(t, n.srv.URL)
	_, err := c.ClusterVMs(context.Background())
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("ClusterVMs without a credential = %v, want ErrNoCredential", err)
	}
	if got := len(rec.all()); got != 0 {
		t.Fatalf("%d requests reached Proxmox without a credential", got)
	}
	if _, err := c.As(Credential{}).ClusterVMs(context.Background()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("ClusterVMs with an empty credential = %v, want ErrNoCredential", err)
	}
}

func TestViewsShareFailover(t *testing.T) {
	rec := &authRecorder{}
	good := newNode(t, rec.handler)
	base := bareClient(t, refusedURL(t), good.srv.URL)
	a := base.As(testTicket)
	b := base.As(TokenCredential("x@pve!t", "s"))
	if _, err := a.ClusterVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := activeEndpoint(b), good.host(); got != want {
		t.Fatalf("after one view failed over, another's active endpoint is %s, want %s", got, want)
	}
	if got := activeEndpoint(base); got != good.host() {
		t.Fatalf("the client's active endpoint is %s, want %s", got, good.host())
	}
}

func TestAsSourceReadsTheCredentialPerCall(t *testing.T) {
	rec := &authRecorder{}
	n := newNode(t, rec.handler)
	var mu sync.Mutex
	cur := testTicket
	c := bareClient(t, n.srv.URL).AsSource(func() Credential { mu.Lock(); defer mu.Unlock(); return cur })
	ctx := context.Background()
	if _, err := c.ClusterVMs(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cur = TicketCredential(testTicket.User, "PVE:renewed", "csrf2", time.Now())
	mu.Unlock()
	if _, err := c.ClusterVMs(ctx); err != nil {
		t.Fatal(err)
	}
	seen := rec.all()
	if !strings.Contains(seen[1].cookie, "PVE%3Arenewed") {
		t.Fatalf("second call sent %q, want the renewed ticket", seen[1].cookie)
	}
}

func TestCredentialStringRedacts(t *testing.T) {
	for _, c := range []Credential{testTicket, TokenCredential("u@pve!t", "topsecret")} {
		for _, s := range []string{c.String(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
			for _, secret := range []string{c.Ticket, c.CSRF, c.TokenSecret} {
				if secret != "" && strings.Contains(s, secret) {
					t.Errorf("%q shows a secret", s)
				}
			}
			if !strings.Contains(s, c.User) {
				t.Errorf("%q doesn't name the user %s", s, c.User)
			}
		}
	}
}

func TestCredentialExpiry(t *testing.T) {
	issued := time.Unix(1_700_000_000, 0)
	c := TicketCredential("u@r", "t", "c", issued)
	if c.Expired(issued.Add(TicketLifetime - time.Second)) {
		t.Error("expired before its lifetime")
	}
	if !c.Expired(issued.Add(TicketLifetime)) {
		t.Error("not expired at the end of its lifetime")
	}
	if TokenCredential("u@r!t", "s").Expired(issued.Add(100 * TicketLifetime)) {
		t.Error("a token expired")
	}
}

func TestCredentialLapsed(t *testing.T) {
	login := time.Unix(1_700_000_000, 0)
	maxAge := 8 * time.Hour
	c := TicketCredential("u@r", "t", "c", login.Add(7*time.Hour))
	c.LoginAt = login
	for _, tc := range []struct {
		at     time.Time
		maxAge time.Duration
		want   bool
	}{
		{login.Add(maxAge - time.Second), maxAge, false},
		{login.Add(maxAge), maxAge, true}, // the login is too old
		{c.Issued.Add(TicketLifetime - time.Second), 24 * time.Hour, false},
		{c.Issued.Add(TicketLifetime), 24 * time.Hour, true}, // expired
	} {
		if got := c.Lapsed(tc.at, tc.maxAge); got != tc.want {
			t.Errorf("Lapsed %s after login, max age %s = %v, want %v", tc.at.Sub(login), tc.maxAge, got, tc.want)
		}
	}
	if TokenCredential("u@r!t", "s").Lapsed(login.Add(100*maxAge), maxAge) {
		t.Error("a token lapsed")
	}
}

// ticketServer answers POST /access/ticket like Proxmox: it renews a
// ticket it knows, and answers 401 otherwise.
func ticketServer(t *testing.T, valid map[string]bool) (*pveNode, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var posts []url.Values
	n := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access/ticket") {
			http.Error(w, "unexpected", 500)
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("renewal sent credentials in headers")
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		mu.Lock()
		posts = append(posts, form)
		mu.Unlock()
		if !valid[form.Get("password")] {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"data":null}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"username":%q,"ticket":"PVE:fresh","CSRFPreventionToken":"csrf-fresh","cap":{}}}`, form.Get("username"))
	})
	return n, &posts
}

func TestRenewTicketPostsTheTicketAsPassword(t *testing.T) {
	n, posts := ticketServer(t, map[string]bool{testTicket.Ticket: true})
	c := bareClient(t, n.srv.URL)
	before := time.Now()
	got, err := c.RenewTicket(context.Background(), testTicket, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Ticket != "PVE:fresh" || got.CSRF != "csrf-fresh" || got.User != testTicket.User {
		t.Fatalf("renewed = %v %q %q", got, got.Ticket, got.CSRF)
	}
	if got.Issued.Before(before) {
		t.Fatalf("renewed ticket issued %v, before the renewal at %v", got.Issued, before)
	}
	if !got.LoginAt.Equal(testTicket.LoginAt) {
		t.Fatalf("renewal changed the login time from %v to %v", testTicket.LoginAt, got.LoginAt)
	}
	p := (*posts)[0]
	if p.Get("username") != testTicket.User || p.Get("password") != testTicket.Ticket {
		t.Fatalf("renewal posted %v", p)
	}
}

func TestRenewRefusedIsLapsed(t *testing.T) {
	n, _ := ticketServer(t, map[string]bool{})
	_, err := bareClient(t, n.srv.URL).RenewTicket(context.Background(), testTicket, time.Now())
	if err == nil {
		t.Fatal("renewing an unknown ticket succeeded")
	}
	if !IsLapsed(err) {
		t.Fatalf("refused renewal %v is not lapsed", err)
	}
	if NewClassifier(nil).Classify(err) == Transient {
		t.Fatal("a refused renewal is transient")
	}
}

func TestRenewTokenIsAnError(t *testing.T) {
	_, err := bareClient(t, "https://127.0.0.1:1").RenewTicket(context.Background(), TokenCredential("a@b!c", "d"), time.Now())
	if err == nil {
		t.Fatal("renewing a token succeeded")
	}
}

func TestClassifyAuthorization(t *testing.T) {
	c := NewClassifier([]string{"permission", "authentication"})
	forbidden := &APIError{Status: 403, Message: "Permission check failed (/vms/101, VM.PowerMgmt)"}
	lapsed := &APIError{Status: 401, Message: "authentication failure"}
	if got := c.Classify(forbidden); got != Forbidden {
		t.Errorf("403 = %v, want Forbidden", got)
	}
	if got := c.Classify(fmt.Errorf("step: %w", forbidden)); got != Forbidden {
		t.Errorf("wrapped 403 = %v, want Forbidden", got)
	}
	if got := c.Classify(lapsed); got != Lapsed {
		t.Errorf("401 = %v, want Lapsed", got)
	}
	if !IsLapsed(fmt.Errorf("x: %w", lapsed)) || IsLapsed(forbidden) {
		t.Error("IsLapsed is wrong")
	}
	if !IsForbidden(forbidden) || IsForbidden(lapsed) {
		t.Error("IsForbidden is wrong")
	}
	// Through every endpoint failing, as with one node answering 401 and
	// another refusing connections.
	multi := &EndpointsError{Method: "GET", Path: "/x", Failures: []endpointFailure{{host: "a", reason: "401", err: lapsed}}}
	if got := c.Classify(multi); got != Lapsed {
		t.Errorf("401 inside EndpointsError = %v, want Lapsed", got)
	}
}

func TestForbiddenIsNeverRetried(t *testing.T) {
	calls := 0
	r := Retrier{Retryable: func(err error) bool { return NewClassifier([]string{"permission"}).Classify(err) == Transient }, Attempts: 5,
		Sleep: func(context.Context, time.Duration) error { return nil }}
	_ = r.Do(context.Background(), func() error {
		calls++
		return &APIError{Status: 403, Message: "Permission check failed (/vms/1, VM.Audit)"}
	})
	if calls != 1 {
		t.Fatalf("a 403 was tried %d times", calls)
	}
}

func TestOpenIDAuthURLAndLogin(t *testing.T) {
	var got []url.Values
	var mu sync.Mutex
	n := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("OpenID call sent credentials")
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		mu.Lock()
		got = append(got, form)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api2/json") {
		case "/access/openid/auth-url":
			_, _ = io.WriteString(w, `{"data":"https://idp.example/authorize?state=abc&client_id=pve"}`)
		case "/access/openid/login":
			_, _ = io.WriteString(w, `{"data":{"username":"jdoe@auth.example.org","ticket":"PVE:t","CSRFPreventionToken":"c","cap":{}}}`)
		default:
			http.Error(w, "unexpected", 500)
		}
	})
	c := bareClient(t, n.srv.URL)
	u, endpoint, err := c.OpenIDAuthURL(context.Background(), "auth.example.org", "https://rk.example/auth/proxmox/callback")
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://idp.example/authorize?state=abc&client_id=pve" || endpoint != n.host() {
		t.Fatalf("auth url = %q at %q", u, endpoint)
	}
	cred, err := c.OpenIDLogin(context.Background(), endpoint, "the-code", "abc", "https://rk.example/auth/proxmox/callback", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cred.User != "jdoe@auth.example.org" || cred.Ticket != "PVE:t" || cred.CSRF != "c" || cred.Issued.IsZero() || !cred.LoginAt.Equal(cred.Issued) {
		t.Fatalf("login credential = %v", cred)
	}
	if got[0].Get("realm") != "auth.example.org" || got[0].Get("redirect-url") != "https://rk.example/auth/proxmox/callback" {
		t.Fatalf("auth-url posted %v", got[0])
	}
	if got[1].Get("code") != "the-code" || got[1].Get("state") != "abc" || got[1].Get("redirect-url") != "https://rk.example/auth/proxmox/callback" {
		t.Fatalf("login posted %v", got[1])
	}
}

func TestPermissionsParses(t *testing.T) {
	n := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access/permissions") {
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"/":{"Sys.Audit":1},"/pool/pool-01":{"VM.PowerMgmt":1,"VM.Audit":0}}}`)
	})
	perms, err := bareClient(t, n.srv.URL).As(testTicket).Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if prop, ok := perms["/pool/pool-01"]["VM.PowerMgmt"]; !ok || !prop {
		t.Fatalf("perms = %v", perms)
	}
	if prop, ok := perms["/pool/pool-01"]["VM.Audit"]; !ok || prop {
		t.Fatalf("VM.Audit = %v %v, want held without propagation", prop, ok)
	}
}

// Proxmox keeps a login's state on the disk of the node that started it,
// so the login must finish there: never on another node, even if that one
// is down.
func TestOpenIDLoginStaysOnTheNodeThatStartedIt(t *testing.T) {
	answer := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/auth-url") {
			_, _ = io.WriteString(w, `{"data":"https://idp.example/a"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"username":"u@r","ticket":"PVE:t","CSRFPreventionToken":"c"}}`)
	}
	refused := refusedURL(t)
	a, b := newNode(t, answer), newNode(t, answer)
	c := bareClient(t, refused, a.srv.URL, b.srv.URL)
	_, endpoint, err := c.OpenIDAuthURL(context.Background(), "r", "https://rk/cb")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != a.host() {
		t.Fatalf("auth-url served by %q, want %q (the first that answered)", endpoint, a.host())
	}
	if _, err := c.OpenIDLogin(context.Background(), b.host(), "code", "s", "https://rk/cb", time.Now()); err != nil {
		t.Fatal(err)
	}
	if a.count("POST /access/openid/login") != 0 || b.count("POST /access/openid/login") != 1 {
		t.Fatalf("login went to a=%d b=%d, want only b", a.count("POST /access/openid/login"), b.count("POST /access/openid/login"))
	}
	strip := strings.TrimPrefix(refused, "https://")
	if _, err := c.OpenIDLogin(context.Background(), strip, "code", "s", "https://rk/cb", time.Now()); err == nil {
		t.Fatal("a login pinned to a node that is down succeeded elsewhere")
	}
	if a.count("POST /access/openid/login") != 0 || b.count("POST /access/openid/login") != 1 {
		t.Fatal("a login pinned to a node that is down failed over")
	}
	if _, err := c.OpenIDLogin(context.Background(), "10.9.9.9:8006", "code", "s", "https://rk/cb", time.Now()); err == nil {
		t.Fatal("a login pinned to an endpoint that isn't configured succeeded")
	}
}

func TestPermissionsAtAsksForOnePath(t *testing.T) {
	var query string
	n := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"/vms/10101":{"VM.PowerMgmt":0}}}`)
	})
	privs, err := bareClient(t, n.srv.URL).As(testTicket).PermissionsAt(context.Background(), "/vms/10101")
	if err != nil {
		t.Fatal(err)
	}
	if query != "path=%2Fvms%2F10101" {
		t.Fatalf("query = %q", query)
	}
	if _, ok := privs["VM.PowerMgmt"]; !ok || len(privs) != 1 {
		t.Fatalf("privs = %v", privs)
	}
}

func TestNodeTasksSince(t *testing.T) {
	var query, path string
	n := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		query, path = r.URL.RawQuery, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"upid":"UPID:n1:1:2:3:qmstart:10101:jdoe@auth.example.org:","node":"n1","type":"qmstart","id":"10101","user":"jdoe@auth.example.org","starttime":1700000100,"endtime":1700000105,"status":"OK"},
			{"upid":"UPID:n1:1:2:4:vncproxy:10102:x@pam:","node":"n1","type":"vncproxy","id":"10102","user":"x@pam","starttime":1700000200}]}`)
	})
	tasks, err := bareClient(t, n.srv.URL).As(testTicket).NodeTasks(context.Background(), "n1", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/nodes/n1/tasks") || !strings.Contains(query, "since=1700000000") || !strings.Contains(query, "source=all") {
		t.Fatalf("asked %s?%s", path, query)
	}
	if len(tasks) != 2 || tasks[0].Type != "qmstart" || tasks[0].VMID != 10101 || tasks[0].Start.Unix() != 1700000100 || tasks[0].Running {
		t.Fatalf("tasks = %+v", tasks)
	}
	if !tasks[1].Running {
		t.Fatalf("a task without an end time isn't running: %+v", tasks[1])
	}
}

// A ticket's age counts from when battleship asked for it, by its clock,
// not from the time Proxmox wrote in it: a clock difference between the
// hosts (here, years) can't make a fresh ticket look expired.
func TestTicketIssueTimeIsWhenAsked(t *testing.T) {
	n := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"username":"u@r","ticket":"PVE:u@r:6553F100::c2ln","CSRFPreventionToken":"6553F100:x"}}`)
	})
	asked := time.Now()
	got, err := bareClient(t, n.srv.URL).RenewTicket(context.Background(), testTicket, asked)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Issued.Equal(asked) || got.LoginAt != testTicket.LoginAt || got.Expired(time.Now()) {
		t.Fatalf("issued %v, login %v; want %v, the old login, and not expired", got.Issued, got.LoginAt, asked)
	}
}
