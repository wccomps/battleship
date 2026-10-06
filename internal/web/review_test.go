package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wccomps/battleship/internal/auth/authtest"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// loginWith signs in subject, whose Proxmox user holds VM.Audit on / and
// privs on each of vmids.
func (h *harness) loginWith(subject string, vmids []int, privs ...string) authtest.Session {
	h.t.Helper()
	cred := h.ticketWith(subject, []string{"VM.Audit"})
	for _, id := range vmids {
		h.pve.Grant(cred.User, "/vms/"+strconv.Itoa(id), append([]string{"VM.Audit"}, privs...)...)
	}
	sess := authtest.Login(h.t, h.st, h.cfg, authtest.User{Subject: subject, At: h.clock.Now(), Proxmox: cred})
	h.openView(cred)
	return sess
}

// Item 18: cancelling a job takes its operation's privilege on the job's
// own VMs, not just somewhere (or having started it).
func TestCancelNeedsThePrivilegeOnTheJobsVMs(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byLead) // 10101, 10102
	other := h.loginWith("test-team2", []int{10201, 10202}, "VM.PowerMgmt")
	if rec := h.post(&other, "/logs/"+itoa(id)+"/cancel", url.Values{}); rec.Code != http.StatusForbidden {
		t.Fatalf("cancel by someone who may power only team 02 = %d, want 403", rec.Code)
	}
	if j, _ := h.st.Job(context.Background(), id); j.CancelRequested {
		t.Fatal("cancelled by someone without the privilege on its VMs")
	}
	contains(t, "team 02 user's job page", h.get(&other, "/logs/"+itoa(id)).Body.String(), "Cancel · not permitted")
	mine := h.loginWith("test-team1", []int{10101, 10102}, "VM.PowerMgmt")
	if rec := h.post(&mine, "/logs/"+itoa(id)+"/cancel", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel by someone who may power its VMs = %d", rec.Code)
	}
}

// Item 19: when the job can't be read, a cancel is refused (500), never
// let through without the privilege check.
func TestCancelFailsClosedWhenTheJobCantBeRead(t *testing.T) {
	h := newHarness(t)
	h.poll()
	id := h.submitJob(jobs.Inputs{Kind: pods.KindPower, Teams: "1", Action: "start"}, byLead)
	nobody := h.login(asNobody)
	conn, err := pgx.Connect(context.Background(), h.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	// Reading the job fails; cancelling it still would work.
	if _, err := conn.Exec(context.Background(), `ALTER TABLE jobs RENAME COLUMN created_as TO created_as_gone`); err != nil {
		t.Fatal(err)
	}
	rec := h.post(&nobody, "/logs/"+itoa(id)+"/cancel", url.Values{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("cancel with the job unreadable = %d, want 500", rec.Code)
	}
	var requested bool
	if err := conn.QueryRow(context.Background(), `SELECT cancel_requested FROM jobs WHERE id = $1`, id).Scan(&requested); err != nil || requested {
		t.Fatalf("cancel requested = %v (%v) by a user without privileges", requested, err)
	}
	if rec := h.post(&nobody, "/logs/999999/cancel", url.Values{}); rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("cancel of a missing job = %d", rec.Code)
	}
}
