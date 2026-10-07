package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/store"
)

// browser is a web client that follows redirects and keeps cookies, as a
// volunteer's browser does.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The fake Proxmox's identity provider has a self-signed certificate.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test servers
	return &browser{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: tr}}
}

// page is a response after redirects: where the browser ended up.
type page struct {
	status int
	path   string
	body   string
}

func (b *browser) read(resp *http.Response, err error) page {
	b.t.Helper()
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return page{status: resp.StatusCode, path: resp.Request.URL.RequestURI(), body: string(body)}
}

func (b *browser) get(path string) page {
	b.t.Helper()
	return b.read(b.c.Get(b.base + path))
}

func (b *browser) post(path string, form url.Values) page {
	b.t.Helper()
	return b.read(b.c.PostForm(b.base+path, form))
}

var (
	formRe   = regexp.MustCompile(`(?s)<form [^>]*?method="post" action="([^"]+)"[^>]*>(.*?)</form>`)
	hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)
)

// form returns the fields of the page's POST form to action, as the
// browser would submit them: its hidden inputs.
func (p page) form(t *testing.T, action string) url.Values {
	t.Helper()
	for _, m := range formRe.FindAllStringSubmatch(p.body, -1) {
		if m[1] != action {
			continue
		}
		fields := url.Values{}
		for _, h := range hiddenRe.FindAllStringSubmatch(m[2], -1) {
			fields.Add(h[1], html.UnescapeString(h[2]))
		}
		return fields
	}
	t.Fatalf("page %s has no form posting to %s:\n%s", p.path, action, p.body)
	return nil
}

// sseEvent is one server-sent event.
type sseEvent struct{ name, data string }

// events opens path as an event stream and returns its events, one at a
// time, until it ends.
func (b *browser) events(ctx context.Context, path string) <-chan sseEvent {
	b.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+path, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	stream := &http.Client{Jar: b.c.Jar} // no timeout: it streams
	resp, err := stream.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		b.t.Fatalf("%s = %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan sseEvent)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev sseEvent
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" || data != nil {
					ev.data = strings.Join(data, "\n")
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
				ev, data = sseEvent{}, nil
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return out
}

// next waits for the next event named one of names, skipping others.
func next(t *testing.T, events <-chan sseEvent, names ...string) sseEvent {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before a %v event", names)
			}
			for _, n := range names {
				if ev.name == n {
					return ev
				}
			}
		case <-timeout:
			t.Fatalf("no %v event within 20s", names)
		}
	}
}

// TestEndToEndPowerJob is a volunteer's whole journey over real HTTP: log
// in, preview and confirm a power job, and watch an embedded worker run it.
func TestEndToEndPowerJob(t *testing.T) {
	e := startServe(t, withLogin())
	e.idp.SignIn(authtest.User{Subject: "sub-olive", Name: "Olive Operator", Email: "olive@example.org", Groups: []string{"volunteers"}})
	e.pve.AddUser("olive@auth.example.org", nil)
	e.pve.Grant("olive@auth.example.org", "/vms", "VM.Audit", "VM.PowerMgmt")
	e.pve.SignIn("olive@auth.example.org")
	release := e.api.hold(false) // Proxmox tasks wait until the test lets them finish
	defer release()
	e.ready()
	b := newBrowser(t, e.url)

	// Log in: the grid sends the browser to the provider and back.
	grid := b.get("/")
	if grid.status != http.StatusOK || grid.path != "/" || e.idp.Logins() != 1 {
		t.Fatalf("grid after logging in = %d at %s (logins %d)", grid.status, grid.path, e.idp.Logins())
	}
	for _, want := range []string{"Olive Operator", `id="cell-01-dc"`, `id="cell-02-dc"`, `data-events="/events/grid"`} {
		if !strings.Contains(grid.body, want) {
			t.Errorf("grid doesn't contain %q", want)
		}
	}

	// Preview a start of team 01's dc.
	form := b.get("/power?teams=1&hosts=dc&action=start")
	if form.status != http.StatusOK {
		t.Fatalf("power form = %d", form.status)
	}
	fields := form.form(t, "/power/preview")
	fields.Set("teams", "1")
	fields.Set("hosts", "dc")
	fields.Set("action", "start")
	preview := b.post("/power/preview", fields)
	if preview.status != http.StatusOK || !strings.Contains(preview.body, "team01-dc") || strings.Contains(preview.body, "team01-web") {
		t.Fatalf("preview = %d, want team01-dc only:\n%s", preview.status, preview.body)
	}

	// Confirm: the browser lands on the new job's page.
	job := b.post("/power/confirm", preview.form(t, "/power/confirm"))
	if job.status != http.StatusOK || job.path != "/logs/1" {
		t.Fatalf("confirm ended at %d %s, want the page of job 1", job.status, job.path)
	}
	if !strings.Contains(job.body, `data-events="/events/jobs/1`) {
		t.Errorf("job page has no live stream:\n%s", job.body)
	}

	// Watch it run on the job page's stream: running while Proxmox works,
	// then the VM's log line, then the end.
	signalled(t, e.api.waiting, "a worker to wait for the power task")
	running := e.job(1)
	if running.Status != store.StatusRunning || !isWorkerID(running.ClaimedBy, "serve") {
		t.Fatalf("job while its task runs = %s, claimed by %q", running.Status, running.ClaimedBy)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := b.events(ctx, "/events/jobs/1")
	if head := next(t, events, "patch"); !strings.Contains(head.data, "running") {
		t.Errorf("first patch while the task runs doesn't say running:\n%s", head.data)
	}
	release()
	for {
		ev := next(t, events, "log")
		if strings.Contains(ev.data, `<span class="vm">team01-dc</span><span>power`) {
			if !strings.Contains(ev.data, "power done<") {
				t.Errorf("team01-dc's log line doesn't say done:\n%s", ev.data)
			}
			break
		}
	}
	next(t, events, "end")

	done := b.get("/logs/1")
	for _, want := range []string{`<span class="st b s-ok">`, "succeeded</span>", `<span title="olive@example.org">olive</span> · olive@auth.example.org`} {
		if !strings.Contains(done.body, want) {
			t.Errorf("finished job page doesn't contain %q:\n%s", want, done.body)
		}
	}
	j := e.job(1)
	if j.Status != store.StatusSucceeded || j.CreatedBy != "olive@example.org" || j.CreatedAs != "olive@auth.example.org" {
		t.Errorf("job = %s by %s as %s, want succeeded by olive@example.org as olive@auth.example.org", j.Status, j.CreatedBy, j.CreatedAs)
	}
	if got := signalled(t, e.api.powered, "a power call"); got != 10101 {
		t.Errorf("powered VM %d, want 10101 (team01-dc)", got)
	}

	// The access log named who did what, without the login code.
	var sawCallback, sawConfirm bool
	for _, r := range e.records("request") {
		switch r["path"] {
		case "/auth/callback":
			sawCallback = r["user"] == "sub-olive" && r["status"] == float64(http.StatusSeeOther)
		case "/power/confirm":
			sawConfirm = r["user"] == "sub-olive" && r["method"] == "POST" && r["status"] == float64(http.StatusSeeOther)
		}
	}
	if !sawCallback || !sawConfirm {
		t.Errorf("access log lacks the login or the confirm by sub-olive:\n%s", e.logs)
	}
	if strings.Contains(e.logs.String(), "code=") {
		t.Errorf("logs contain a login code:\n%s", e.logs)
	}
}
