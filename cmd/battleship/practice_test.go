package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

const practiceTestPrincipal = "practice@pve"

// withPractice turns the session interface on, served on ln.
func withPractice(ln net.Listener, key string) serveOption {
	return func(s *serveSetup) {
		s.cfg.Practice = config.Practice{Pods: "51-59", Principal: practiceTestPrincipal, Listen: ln.Addr().String(), Key: key}
		web := s.deps.listen
		s.deps.listen = func(network, addr string) (net.Listener, error) {
			if network == "tcp" && addr == ln.Addr().String() {
				return ln, nil
			}
			return web(network, addr)
		}
	}
}

// The session interface is served on practice.listen and nowhere else, and
// stops with serve. Without [practice], serve opens no second listener:
// startServe's listen refuses any address but web.listen.
func TestServeSessionInterfaceHasItsOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", config.MinPracticeKey)
	e := startServe(t, withPractice(ln, key))
	e.pve.AddUser(practiceTestPrincipal, nil)
	e.pve.Grant(practiceTestPrincipal, "/", "VM.Audit")
	e.api.Mu.Lock()
	e.api.Tags = proxmox.TagAccess{Registered: []string{"approved"}}
	e.api.Mu.Unlock()

	sets := func(base string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, base+"/v1/sets", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Proxmox-Ticket", e.pve.Ticket(practiceTestPrincipal).Ticket)
		c := *e.http
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	own := "http://" + ln.Addr().String()
	if code, body := sets(own); code != http.StatusOK || strings.TrimSpace(body) != `{"sets":[]}` {
		t.Errorf("the session interface answers %d %q, want 200 and no sets", code, body)
	}
	if code, body := sets(e.url); code == http.StatusOK || strings.Contains(body, `"sets"`) {
		t.Errorf("the volunteer pages' listener serves the session interface: %d %q", code, body)
	}
	if !strings.Contains(e.logs.String(), "serving the session interface") {
		t.Errorf("serve doesn't log the session interface's listener:\n%s", e.logs.String())
	}

	if err, _ := e.shutdown(20 * time.Second); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if code, body := sets(own); code != 0 {
		t.Errorf("the session interface still answers after serve stopped: %d %q", code, body)
	}
}
