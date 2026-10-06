package web

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wccomps/battleship/internal/auth"
)

// A preview reads the cluster as the signed-in user, and the job a confirm
// stores carries that user's ticket, sealed for the job.
func TestConfirmedJobCarriesTheUsersTicket(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	before := len(h.usedCreds())
	action, fields, _ := h.preview(&lead, "/power", url.Values{"teams": {"1"}, "action": {"stop"}})
	for _, c := range h.usedCreds()[before:] {
		if c.User != "test-lead@auth.example.org" || c.Ticket == "" {
			t.Fatalf("the preview read the cluster as %v", c)
		}
	}
	if rec := h.post(&lead, action, fields); rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
	}
	js := h.jobsInStore()
	if len(js) != 1 {
		t.Fatalf("%d jobs", len(js))
	}
	jc, err := h.st.JobCredential(t.Context(), js[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.creds.Open(jc)
	if err != nil {
		t.Fatal(err)
	}
	if cred.User != "test-lead@auth.example.org" || !h.pve.Accepts(cred) {
		t.Fatalf("the job carries %v", cred)
	}
}

// Without a ticket, nothing reads the cluster: the user is sent through
// the Proxmox sign-in first.
func TestNoProxmoxCallWithoutATicket(t *testing.T) {
	h := newHarness(t)
	h.poll()
	before := len(h.usedCreds())
	sess := h.login(asLead)
	if err := h.st.ClearSessionTicket(t.Context(), auth.HashToken(sess.Cookie.Value), time.Time{}); err != nil {
		t.Fatal(err)
	}
	rec := h.post(&sess, "/power/preview", url.Values{"teams": {"1"}, "action": {"stop"}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("preview without a ticket = %d", rec.Code)
	}
	for _, c := range h.usedCreds()[before:] {
		t.Fatalf("read the cluster as %v without a ticket", c)
	}
}

// Each user's grid is their own, read with their ticket: a student who
// opens the grid after a lead has sees none of the VMs the lead sees.
func TestGridViewsArePerUser(t *testing.T) {
	h := newHarness(t)
	h.poll()
	lead := h.login(asLead)
	contains(t, "lead's grid", h.get(&lead, "/").Body.String(), `id="cell-01-dc"`)
	student := h.loginStudent()
	lacks(t, "student's grid", h.get(&student, "/").Body.String(), `id="cell-01-dc"`, "team01-dc")
	cell := h.get(&student, "/vm/01/dc")
	if cell.Code != http.StatusNotFound {
		t.Errorf("student's page of a VM the lead sees = %d, want 404", cell.Code)
	}
}
