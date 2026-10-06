package web

import (
	"strings"
	"testing"

	"github.com/chromedp/cdproto/security"

	"github.com/wccomps/battleship/internal/auth"
	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// A session without a Proxmox ticket goes through the Proxmox OpenID
// sign-in in the browser: the grid sends it to /auth/proxmox, Proxmox's
// auth-url to the identity provider (the fake Proxmox's, which signs the
// user in without a form), and that back to /auth/proxmox/callback, which
// stores the ticket and returns to the grid.
func TestBrowserProxmoxSignIn(t *testing.T) {
	h, _ := bigHarness(t)
	user := "test-signin@auth.example.org"
	h.pve.AddUser(user, nil)
	h.pve.Grant(user, "/", personaPrivileges[asOperator]...)
	h.pve.SignIn(user)
	sess := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-signin", At: h.clock.Now(), NoProxmox: true})
	b := newBrowser(t, h, &sess, 1366, 768, false)
	b.run("trusting the fake Proxmox's certificate", security.SetIgnoreCertificateErrors(true))
	b.open("/")
	b.waitFor("the grid after the Proxmox sign-in", `location.pathname === "/" && !!document.getElementById("grid-form")`)
	stored, err := h.st.Session(t.Context(), auth.HashToken(sess.Cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	if stored.PVE.User != user {
		t.Fatalf("the session's Proxmox user is %q, want %q", stored.PVE.User, user)
	}
	if logins, _, _ := h.pve.Counts(); logins != 1 {
		t.Fatalf("Proxmox saw %d logins", logins)
	}
	if got := b.text(".whoami .role"); !strings.HasPrefix(got, user+" · ") {
		t.Errorf("account menu says %q", got)
	}
	b.clean()
}

// A preview shows what the user's privileges don't allow as blocked rows,
// in the blocked style, naming the privilege and path; a job whose
// submitter's authorization lapsed says so on its page.
func TestBrowserBlockedByPrivilegesAndLapsed(t *testing.T) {
	h, _ := bigHarness(t)
	cred := h.ticketWith("test-powerer", []string{"VM.Audit"})
	h.pve.Grant(cred.User, "/vms/10101", "VM.Audit", "VM.PowerMgmt")
	pw := authtest.Login(t, h.st, h.cfg, authtest.User{Subject: "test-powerer", At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			h.t = t
			b := newBrowser(t, h, &pw, size.width, size.height, size.dark)
			b.open("/power?teams=1&action=stop")
			b.click(`form.sheet button[type=submit]`)
			b.waitFor("the preview", `!!document.querySelector("#pv-title")`)
			if !b.is(`[...document.querySelectorAll("tr.blk")].some(r => r.textContent.includes("you don't have VM.PowerMgmt on /vms/10102"))`) {
				t.Errorf("no blocked row names VM.PowerMgmt on /vms/10102: %q", b.text(`tr.blk`))
			}
			if b.is(`[...document.querySelectorAll("tr.blk")].some(r => r.textContent.includes("team01-dc"))`) {
				t.Error("team01-dc, which the user may power, is blocked")
			}
			b.noSideways("preview with blocked rows")
			b.shot("preview-blocked-by-privilege-" + size.name)
			b.clean()
		})
	}

	h.t = t
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byOperator)
	h.start(id)
	h.finish(id, store.Outcome{Status: store.StatusInterrupted,
		Error: "authorization lapsed: Proxmox no longer accepts the submitter's login, so the job stopped; whoever re-runs it acts as themselves"})
	op := h.login(asOperator)
	b := newBrowser(t, h, &op, 1366, 768, false)
	b.open("/logs/" + itoa(id))
	if got := b.text("#job-live"); !strings.Contains(got, "authorization lapsed") {
		t.Errorf("job page = %q", got)
	}
	b.shot("job-authorization-lapsed")
	b.clean()
}
