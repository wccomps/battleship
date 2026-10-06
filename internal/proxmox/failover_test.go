package proxmox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pveNode is one fake cluster node: an HTTPS server that records every
// request that reaches its handler, then runs handle.
type pveNode struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   []string
	handle atomic.Value // http.HandlerFunc
}

func (n *pveNode) set(h http.HandlerFunc) { n.handle.Store(h) }

func (n *pveNode) count(prefix string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, h := range n.hits {
		if strings.HasPrefix(h, prefix) {
			c++
		}
	}
	return c
}

func (n *pveNode) host() string { return strings.TrimPrefix(n.srv.URL, "https://") }

func newNode(t *testing.T, h http.HandlerFunc) *pveNode {
	t.Helper()
	n := &pveNode{}
	n.set(h)
	n.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		n.mu.Lock()
		n.hits = append(n.hits, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api2/json")+" "+string(body))
		n.mu.Unlock()
		n.handle.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// okNode answers every request like a healthy Proxmox: a clone UPID for
// POSTs, an empty list for GETs, and a stopped-OK task status.
func okNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/status") && strings.Contains(r.URL.Path, "/tasks/"):
		_, _ = io.WriteString(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
	case r.Method != http.MethodGet:
		_, _ = io.WriteString(w, `{"data":"UPID:cedar:0001:0002:0003:qmclone:9005:battleship@pve!app:"}`)
	default:
		_, _ = io.WriteString(w, `{"data":[]}`)
	}
}

func statusNode(code int, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, text)
	}
}

// hangNode reads the whole request, so a write has certainly arrived, then
// never answers.
func hangNode(w http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

// refusedURL is an https URL nothing listens on.
func refusedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "https://" + addr
}

func failoverClient(t *testing.T, urls ...string) (*Client, *logLines) {
	t.Helper()
	c := New(Options{URLs: urls, InsecureSkipVerify: true}).As(TokenCredential("battleship@pve!app", "secret"))
	c.http.Timeout = 500 * time.Millisecond
	l := &logLines{}
	c.SetLogf(l.logf)
	return c, l
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

var cloneReq = CloneRequest{SourceNode: "cedar", SourceVMID: 9005, NewVMID: 10105, Name: "team01-teak", TargetNode: "cedar"}

func TestNewWithOneURLHasOneEndpoint(t *testing.T) {
	c := New(Options{URLs: []string{"https://192.0.2.123:8006/"}}).As(TokenCredential("battleship@pve!app", "secret"))
	if got := activeEndpoint(c); got != "192.0.2.123:8006" {
		t.Errorf("ActiveEndpoint = %q", got)
	}
	c = New(Options{URLs: []string{"https://10.1.1.1:8006", "https://10.1.1.2:8006"}}).As(TokenCredential("battleship@pve!app", "secret"))
	if got := c.Endpoints(); len(got) != 2 || got[0] != "10.1.1.1:8006" || got[1] != "10.1.1.2:8006" {
		t.Errorf("Endpoints = %v", got)
	}
	if got := activeEndpoint(c); got != "10.1.1.1:8006" {
		t.Errorf("ActiveEndpoint = %q, want the first", got)
	}
}

// Refused: nothing got through, so reads and writes both move on, and the
// new endpoint sticks for later calls.
func TestFailoverOnConnectionRefused(t *testing.T) {
	down := refusedURL(t)
	b := newNode(t, okNode)
	c, logs := failoverClient(t, down, b.srv.URL)

	if _, err := c.ClusterVMs(context.Background()); err != nil {
		t.Fatalf("ClusterVMs: %v", err)
	}
	if activeEndpoint(c) != b.host() {
		t.Errorf("active = %s, want %s", activeEndpoint(c), b.host())
	}
	if _, err := c.Clone(context.Background(), cloneReq); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if n := b.count("POST /nodes/cedar/qemu/9005/clone"); n != 1 {
		t.Errorf("clone reached b %d times, want 1", n)
	}
	lines := logs.get()
	if len(lines) != 1 {
		t.Fatalf("log lines = %q, want exactly one failover", lines)
	}
	from := strings.TrimPrefix(down, "https://")
	if l := lines[0]; !strings.Contains(l, from) || !strings.Contains(l, b.host()) || !strings.Contains(l, "refused") || strings.Contains(l, "secret") {
		t.Errorf("log line = %q, want from %s to %s with the reason and no token", l, from, b.host())
	}
}

func TestWriteRefusedFailsOver(t *testing.T) {
	b := newNode(t, okNode)
	c, _ := failoverClient(t, refusedURL(t), b.srv.URL)
	if _, err := c.Power(context.Background(), "cedar", 10105, "start"); err != nil {
		t.Fatalf("Power: %v", err)
	}
	if n := b.count("POST"); n != 1 {
		t.Errorf("b got %d POSTs, want 1", n)
	}
}

// A write that timed out after the node read it may have happened, so it
// is never sent to another node.
func TestWriteTimeoutAfterSendNeverFailsOver(t *testing.T) {
	a := newNode(t, hangNode)
	b := newNode(t, okNode)
	c, logs := failoverClient(t, a.srv.URL, b.srv.URL)

	_, err := c.Clone(context.Background(), cloneReq)
	if err == nil {
		t.Fatal("Clone succeeded, want the timeout")
	}
	if n := a.count("POST"); n != 1 {
		t.Errorf("a got %d POSTs, want 1", n)
	}
	if n := b.count(""); n != 0 {
		t.Errorf("b got %d requests, want none: the clone would be duplicated", n)
	}
	if activeEndpoint(c) != a.host() {
		t.Errorf("active moved to %s", activeEndpoint(c))
	}
	if len(logs.get()) != 0 {
		t.Errorf("logged %q", logs.get())
	}
	for _, del := range []func() error{
		func() error { _, err := c.DeleteVM(context.Background(), "cedar", 10105); return err },
		func() error {
			return c.SetVMConfig(context.Background(), "cedar", 10105, map[string]string{"net0": "x"})
		},
		func() error { _, err := c.Rollback(context.Background(), "cedar", 10105, "initial"); return err },
	} {
		if err := del(); err == nil {
			t.Error("write succeeded, want the timeout")
		}
	}
	if n := b.count(""); n != 0 {
		t.Errorf("b got %d requests after DELETE/PUT/POST timeouts, want none", n)
	}
}

// A read that timed out is safe to send again.
func TestReadTimeoutFailsOver(t *testing.T) {
	a := newNode(t, hangNode)
	b := newNode(t, okNode)
	c, _ := failoverClient(t, a.srv.URL, b.srv.URL)
	if _, err := c.ClusterVMs(context.Background()); err != nil {
		t.Fatalf("ClusterVMs: %v", err)
	}
	if a.count("GET") != 1 || b.count("GET") != 1 {
		t.Errorf("a %d, b %d GETs, want 1 each", a.count("GET"), b.count("GET"))
	}
	if activeEndpoint(c) != b.host() {
		t.Errorf("active = %s", activeEndpoint(c))
	}
}

// A TLS handshake failure happens before the request is sent, so even a
// write moves on.
func TestTLSFailureFailsOverWrites(t *testing.T) {
	bad := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request %s %s reached the node with the untrusted certificate", r.Method, r.URL.Path)
	}))
	bad.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	bad.StartTLS()
	t.Cleanup(bad.Close)
	b := newNode(t, okNode)
	pool := x509.NewCertPool()
	pool.AddCert(b.srv.Certificate())
	c := New(Options{URLs: []string{bad.URL, b.srv.URL}, RootCAs: pool}).As(TokenCredential("battleship@pve!app", "secret"))

	if _, err := c.Clone(context.Background(), cloneReq); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if n := b.count("POST"); n != 1 {
		t.Errorf("b got %d POSTs, want 1", n)
	}
}

func TestHTTP5xx(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		method     string
		wantOnB    int
		wantStatus int
	}{
		{"GET 502 from the proxy", 502, "GET", 1, 0},
		{"GET 596 node unreachable", 596, "GET", 1, 0},
		{"GET 503", 503, "GET", 1, 0},
		{"GET 500 is Proxmox's own answer", 500, "GET", 0, 500},
		{"POST 502 may have reached the node", 502, "POST", 0, 502},
		{"POST 596", 596, "POST", 0, 596},
		{"DELETE 503", 503, "DELETE", 0, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newNode(t, statusNode(tc.code, ""))
			b := newNode(t, okNode)
			c, _ := failoverClient(t, a.srv.URL, b.srv.URL)
			var err error
			switch tc.method {
			case "GET":
				_, err = c.ClusterVMs(context.Background())
			case "POST":
				_, err = c.Clone(context.Background(), cloneReq)
			case "DELETE":
				_, err = c.DeleteVM(context.Background(), "cedar", 10105)
			}
			if a.count(tc.method) != 1 {
				t.Errorf("a got %d, want 1", a.count(tc.method))
			}
			if got := b.count(""); got != tc.wantOnB {
				t.Errorf("b got %d requests, want %d", got, tc.wantOnB)
			}
			var apiErr *APIError
			switch {
			case tc.wantStatus == 0 && err != nil:
				t.Errorf("err = %v, want success on b", err)
			case tc.wantStatus != 0 && (!errors.As(err, &apiErr) || apiErr.Status != tc.wantStatus):
				t.Errorf("err = %v, want a %d APIError", err, tc.wantStatus)
			}
		})
	}
}

// When every endpoint fails, each is tried once and the error names them
// all, without the token; it stays transient for the retrier.
func TestAllEndpointsFail(t *testing.T) {
	a := newNode(t, statusNode(502, ""))
	b := newNode(t, statusNode(596, ""))
	down := refusedURL(t)
	c, logs := failoverClient(t, a.srv.URL, b.srv.URL, down)

	_, err := c.ClusterVMs(context.Background())
	if err == nil {
		t.Fatal("ClusterVMs succeeded")
	}
	if a.count("GET") != 1 || b.count("GET") != 1 {
		t.Errorf("a %d, b %d GETs, want each tried once", a.count("GET"), b.count("GET"))
	}
	msg := err.Error()
	for _, h := range []string{a.host(), b.host(), strings.TrimPrefix(down, "https://")} {
		if !strings.Contains(msg, h) {
			t.Errorf("error %q doesn't name %s", msg, h)
		}
	}
	if strings.Contains(msg, "secret") || strings.Contains(msg, "PVEAPIToken") {
		t.Errorf("error leaks the token: %q", msg)
	}
	if NewClassifier(nil).Classify(err) != Transient {
		t.Errorf("all-endpoints error is not transient")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("errors.As APIError failed on %v", err)
	}
	if activeEndpoint(c) != a.host() {
		t.Errorf("active moved to %s though nothing answered", activeEndpoint(c))
	}
	if len(logs.get()) != 0 {
		t.Errorf("logged a failover though none happened: %q", logs.get())
	}
}

// The next endpoint after the active one comes first, wrapping around.
func TestFailoverWrapsAround(t *testing.T) {
	var aDown, cDown atomic.Bool
	pick := func(down *atomic.Bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if down.Load() {
				statusNode(502, "")(w, r)
				return
			}
			okNode(w, r)
		}
	}
	a := newNode(t, pick(&aDown))
	b := newNode(t, statusNode(502, ""))
	cn := newNode(t, pick(&cDown))
	c, logs := failoverClient(t, a.srv.URL, b.srv.URL, cn.srv.URL)

	aDown.Store(true)
	if _, err := c.ClusterVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if activeEndpoint(c) != cn.host() {
		t.Fatalf("active = %s, want c", activeEndpoint(c))
	}
	aDown.Store(false)
	cDown.Store(true)
	if _, err := c.ClusterVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if activeEndpoint(c) != a.host() {
		t.Errorf("active = %s, want a after wrapping", activeEndpoint(c))
	}
	if b.count("GET") != 1 {
		t.Errorf("b tried %d times, want once (only on the first call)", b.count("GET"))
	}
	if n := len(logs.get()); n != 2 {
		t.Errorf("logged %d failovers, want 2: %q", n, logs.get())
	}
}

// Many calls failing at once log the failover once.
func TestConcurrentFailoverLogsOnce(t *testing.T) {
	b := newNode(t, okNode)
	c, logs := failoverClient(t, refusedURL(t), b.srv.URL)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ClusterVMs(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(logs.get()); n != 1 {
		t.Errorf("logged %d failovers, want 1: %q", n, logs.get())
	}
}

// A task started on one endpoint can be waited for through another: the
// status path names the task's node, which any cluster node can answer for.
func TestWaitTaskContinuesAfterFailover(t *testing.T) {
	a := newNode(t, okNode)
	b := newNode(t, okNode)
	c, _ := failoverClient(t, a.srv.URL, b.srv.URL)
	upid, err := c.Clone(context.Background(), cloneReq)
	if err != nil {
		t.Fatal(err)
	}
	a.srv.Close() // a goes down after starting the task
	if err := c.WaitTask(context.Background(), upid, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
	if n := b.count("GET /nodes/cedar/tasks/"); n != 1 {
		t.Errorf("b got %d status polls, want 1", n)
	}
	if n := b.count("POST"); n != 0 {
		t.Errorf("the clone was sent again to b")
	}
}

// The retrier retries a whole failover pass once all endpoints failed.
func TestRetrierComposesWithFailover(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	a := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			statusNode(502, "")(w, r)
			return
		}
		okNode(w, r)
	})
	b := newNode(t, statusNode(502, ""))
	c, _ := failoverClient(t, a.srv.URL, b.srv.URL)
	r := Retrier{Retryable: transient(), Attempts: 3, Initial: time.Millisecond, Max: time.Millisecond,
		Sleep: func(context.Context, time.Duration) error { down.Store(false); return nil }}
	err := r.Do(context.Background(), func() error { _, err := c.ClusterVMs(context.Background()); return err })
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if a.count("GET") != 2 || b.count("GET") != 1 {
		t.Errorf("a %d, b %d GETs, want 2 and 1", a.count("GET"), b.count("GET"))
	}
}

// A cancelled call doesn't move on to the next endpoint.
func TestCancelledCallDoesNotFailOver(t *testing.T) {
	a := newNode(t, hangNode)
	b := newNode(t, okNode)
	c, _ := failoverClient(t, a.srv.URL, b.srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ClusterVMs(ctx); err == nil {
		t.Fatal("want an error")
	}
	if b.count("") != 0 {
		t.Errorf("b got a request after the caller gave up")
	}
}

func TestFailoverReasonClasses(t *testing.T) {
	dial := &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}
	dns := &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "x"}}}
	read := &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}}
	tlsErr := &url.Error{Op: "Post", URL: "https://x", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}
	for _, tc := range []struct {
		name   string
		method string
		err    error
		want   bool
	}{
		{"POST dial refused", "POST", dial, true},
		{"PUT DNS failure", "PUT", dns, true},
		{"DELETE TLS", "DELETE", tlsErr, true},
		{"POST read reset", "POST", read, false},
		{"GET read reset", "GET", read, true},
		{"POST 502", "POST", &APIError{Status: 502}, false},
		{"GET 502", "GET", &APIError{Status: 502}, true},
		{"GET 500", "GET", &APIError{Status: 500}, false},
		{"GET 404", "GET", &APIError{Status: 404}, false},
	} {
		if _, got := failoverReason(tc.method, tc.err, true); got != tc.want {
			t.Errorf("%s: eligible = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// selfSigned is a certificate for 127.0.0.1 that no pool trusts.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "untrusted"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// activeEndpoint is the host:port c's calls go to first.
func activeEndpoint(c *Client) string { return c.endpoints[c.active.Load()].host }
