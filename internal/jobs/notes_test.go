package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
	"github.com/wccomps/battleship/internal/store/storetest"
)

// Register and Edit details jobs stored before free text keep their
// inputs and fingerprints: these were taken from the code before it.
func TestDetailsJobsKeepTheirFingerprints(t *testing.T) {
	d := pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: "Debian 12", User: "debian", Password: "pw", Built: "2026-10-01",
		Generalized: "cloud-init clean", Installed: "agent", Licence: "free"}}
	reg := &pods.Plan{Kind: pods.KindImageRegister, Items: []pods.Item{{Name: "debian-12-v2", VMID: 8002,
		Steps: []pods.Step{pods.StepClone, pods.StepLabel, pods.StepDetails}, Details: &d, Notes: &pods.Notes{Source: "150"}}},
		Locks: []string{"family:debian-12"}}
	edit := &pods.Plan{Kind: pods.KindImageDetails, Items: []pods.Item{{Name: "debian-12-v1", VMID: 8001, Steps: []pods.Step{pods.StepDetails},
		Details: &pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: "Debian 12"}}, DetailsWas: &pods.DetailsWas{Details: d.ImageDetails}}},
		Locks: []string{"family:debian-12"}}
	if got := Fingerprint(reg); got != "5cacaa78573740281622ba6d3bdf54cb1e7d13529165496e3acf2db4a91e4ef5" {
		t.Errorf("register fingerprint = %s", got)
	}
	if got := Fingerprint(edit); got != "ac841120574a0d6a2a7148b0f9296fc5b02e2c2f4b1634ffcff84179969db156" {
		t.Errorf("Edit details fingerprint = %s", got)
	}
	in, _ := json.Marshal(Inputs{Kind: pods.KindImageDetails, Family: "debian-12", Details: &d})
	if want := `{"kind":"image-details","teams":"","family":"debian-12","details":{"os":"Debian 12","user":"debian","password":"pw",` +
		`"built":"2026-10-01","generalized":"cloud-init clean","installed":"agent","licence":"free"}}`; string(in) != want {
		t.Errorf("inputs = %s", in)
	}
}

// notesFamily is a cluster with family debian-12, whose one version's
// notes are desc.
func notesFamily(desc string) *fakeAPI {
	f := newFake()
	f.Add(proxmox.VM{VMID: 8001, Name: "debian-12-v1", Node: "n1", Template: true, Tags: "base;linux"}, map[string]string{"description": desc})
	return f
}

// editNotes is an Edit notes on debian-12 writing free text.
func editNotes(os, free string) Inputs {
	return Inputs{Kind: pods.KindImageDetails, Family: "debian-12", Details: &pods.ImageNotes{ImageDetails: pods.ImageDetails{OS: os}, Free: &free}}
}

const notesBefore = "**Details**\n- OS: Debian 12\n\nPatched.\nbattleship: source=150"

// An Edit notes writing free text replaces all of it, so its fingerprint
// covers the notes it replaces: a change to them since the preview,
// visible or in an unreadable block, makes the plan differ.
func TestFingerprintCoversNotesReplaced(t *testing.T) {
	in := editNotes("Debian 12", "Patched.\nRebooted.")
	for _, after := range []string{
		"**Details**\n- OS: Debian 12\n\nPatched.\nDO NOT DEPLOY: AV broken\nbattleship: source=150",
		"**Details**\n- OS: Debian 13\n\nPatched.\nbattleship: source=150",
	} {
		f := notesFamily(notesBefore)
		before, err := BuildPlan(bg, pods.NewPlanner(f, testCfg()), in)
		if err != nil {
			t.Fatal(err)
		}
		f.VMs[8001].Config["description"] = after
		now, _ := BuildPlan(bg, pods.NewPlanner(f, testCfg()), in)
		if Fingerprint(before) == Fingerprint(now) {
			t.Errorf("notes changed to %q under the preview; the fingerprint didn't", after)
		}
	}
	odd := "**Details**\n- Colour: blue\n\nPatched."
	f := notesFamily(odd)
	before, _ := BuildPlan(bg, pods.NewPlanner(f, testCfg()), in)
	f.VMs[8001].Config["description"] = strings.Replace(odd, "blue", "red", 1)
	if now, _ := BuildPlan(bg, pods.NewPlanner(f, testCfg()), in); Fingerprint(before) == Fingerprint(now) {
		t.Error("an unreadable block changed under the preview; the fingerprint didn't")
	}
}

// Notes changed between an Edit notes's preview and its run end the job
// stale, the change kept: by another Edit notes on the family, whichever
// runs first, or in Proxmox.
func TestWorkerStalesNotesChangedSincePreview(t *testing.T) {
	alice, bob := editNotes("Debian 12", "Patched.\nRebooted (Alice)."), editNotes("Debian 12", "Patched.\nAV broken (Bob).")
	for _, order := range [][2]Inputs{{alice, bob}, {bob, alice}} {
		st := storetest.New(t)
		f := notesFamily(notesBefore)
		first, second := submit(t, st, f, order[0]), submit(t, st, f, order[1])
		w := newWorker(st, f)
		w.runJob(bg, claim(t, st))
		w.runJob(bg, claim(t, st))
		if j := job(t, st, first); j.Status != store.StatusSucceeded {
			t.Fatalf("first job = %s: %s", j.Status, j.Error)
		}
		if j := job(t, st, second); j.Status != store.StatusStale {
			t.Errorf("second job = %s: %s, want stale", j.Status, j.Error)
		}
		if got, want := f.VMs[8001].Config["description"], "**Details**\n- OS: Debian 12\n\n"+*order[0].Details.Free+"\n\nbattleship: source=150"; got != want {
			t.Errorf("notes = %q, want the first job's %q", got, want)
		}
	}

	st := storetest.New(t)
	f := notesFamily(notesBefore)
	id := submit(t, st, f, alice)
	edited := "**Details**\n- OS: Debian 12\n\nPatched.\nDO NOT DEPLOY\nbattleship: source=150"
	f.Mu.Lock()
	f.VMs[8001].Config["description"] = edited
	f.Mu.Unlock()
	newWorker(st, f).runJob(bg, claim(t, st))
	if j := job(t, st, id); j.Status != store.StatusStale || !strings.Contains(j.Error, "no longer matches the preview") {
		t.Errorf("job = %s: %s, want stale", j.Status, j.Error)
	}
	if got := f.VMs[8001].Config["description"]; got != edited {
		t.Errorf("notes = %q, want the edit in Proxmox kept", got)
	}
}
