package authtest_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox/pvetest"
	"github.com/wccomps/battleship/internal/store/storetest"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

// statusPage draws auth's pages as their status and title.
func statusPage(w http.ResponseWriter, _ *http.Request, p auth.Page) {
	w.WriteHeader(p.Status)
	_, _ = io.WriteString(w, p.Title)
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func TestProviderSignsInThroughTheRealFlow(t *testing.T) {
	clock := &testClock{t: time.Now()}
	var h http.Handler
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(app.Close)

	cfg := config.Default()
	cfg.Web.BaseURL = app.URL
	p := authtest.NewProvider(t, &cfg)
	p.Now = clock.Now
	if err := cfg.RequireWeb(); err != nil {
		t.Fatalf("RequireWeb: %v", err)
	}
	st := storetest.New(t)
	pve := pvetest.New(t)
	pve.SetClock(clock.Now)
	pve.AddUser("lena@auth.example.org", nil)
	pve.SignIn("lena@auth.example.org")
	svc, err := auth.NewService(context.Background(), cfg, st, auth.Options{Now: clock.Now, Logf: t.Logf, Proxmox: pve.Client()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.SetRender(statusPage)
	mux := http.NewServeMux()
	svc.Routes(mux)
	mux.Handle("GET /whoami", svc.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		sess, err := st.Session(r.Context(), auth.SessionID(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "%s %s %s %s", u.Subject, u.Name, u.Email, strings.Join(sess.Groups, ","))
	})))
	h = mux

	jar, _ := cookiejar.New(nil)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the fake Proxmox's certificate
	browser := &http.Client{Jar: jar, Transport: tr}
	whoami := func() (int, string) {
		t.Helper()
		resp, err := browser.Get(app.URL + "/whoami")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	p.SignIn(authtest.User{Subject: "sub-lena", Name: "Lena", Groups: []string{"leads"}})
	if code, body := whoami(); code != http.StatusOK || body != "sub-lena Lena sub-lena@example.org leads" {
		t.Fatalf("after signing in: %d %q, want Lena in leads", code, body)
	}
	if p.Logins() != 1 {
		t.Errorf("logins = %d, want 1", p.Logins())
	}

	// The provider refreshes sessions too: a change of groups there reaches
	// the app at the next refresh.
	p.SignIn(authtest.User{Subject: "sub-lena", Name: "Lena", Groups: []string{"operators"}})
	clock.Advance(cfg.Web.SessionRefresh)
	if code, body := whoami(); code != http.StatusOK || !strings.HasSuffix(body, " operators") {
		t.Errorf("after a refresh: %d %q, want operators", code, body)
	}
	if p.Logins() != 1 {
		t.Errorf("logins = %d, want still 1 (a refresh isn't a login)", p.Logins())
	}
}
