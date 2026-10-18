package web

import (
	"cmp"
	"context"
	"strconv"
	"strings"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
)

// everythingLabel is the choice that tears down every VM in the pods.
const everythingLabel = "Everything in these pods"

// allChoice is the label of the choice that tears down regardless of set:
// everything in the pods, or every VM matching a host or VM filter.
func allChoice(in jobs.Inputs) string {
	if len(in.Hosts) > 0 || len(in.VMs) > 0 {
		return "Every matching VM in these pods"
	}
	return everythingLabel
}

// podsText names pods in prose: "pod 07", "pods 01-03, 07".
func podsText(pods []string) string {
	word := "pods "
	if len(pods) == 1 {
		word = "pod "
	}
	return word + teamsText(strings.Join(pods, ","))
}

// deployedText says how much of a set the pods hold: "12 hosts in each of
// pods 01-32", "3 hosts in pod 07" or, when pods differ, "40 VMs in pods
// 01-04".
func deployedText(d pods.DeployedSet) string {
	list := d.PodList()
	if n := d.SameHosts(); n > 0 {
		hosts := strconv.Itoa(n) + byCount(n, " host", " hosts")
		if len(list) == 1 {
			return hosts + " in " + podsText(list)
		}
		return hosts + " in each of " + podsText(list)
	}
	return countVMs(d.VMs) + " in " + podsText(list)
}

// setStep is the teardown's step 2: what teams' pods hold among in's hosts,
// set by set, a choice per set, then everything. The choice is in.Set if
// chosen; else, when the pods hold one set alone, that set, and when they
// hold no set it can tell, everything. Otherwise the person chooses.
func (s *Server) setStep(ctx context.Context, in jobs.Inputs, teams []string, chosen bool) *stepTwo {
	step := &stepTwo{Legend: "Tear down", Button: "Preview teardown", Danger: true}
	held := &heldView{Heading: "In " + podsText(teams), Empty: "No VMs in " + podsText(teams) + "."}
	if len(in.Hosts) > 0 {
		held.Empty = "No VMs in " + podsText(teams) + " match these hosts."
	}
	step.Held = held
	vms, err := s.apiFor(ctx).ClusterVMs(ctx)
	if err != nil {
		held.Err = "Couldn't read what the pods hold: " + proxmox.Describe(err) + "."
	}
	deployed := s.naming.Deployed(vms, teams, in.Hosts)
	var sets []pods.Set
	unknown := 0
	for _, d := range deployed {
		held.Rows = append(held.Rows, heldRow{Label: cmp.Or(string(d.Set), pods.UnknownSet), Text: deployedText(d), Muted: d.Set == ""})
		if d.Set == "" {
			unknown = d.VMs
		} else {
			sets = append(sets, d.Set)
		}
	}
	all := allChoice(in)
	if unknown > 0 {
		step.Hints = append(step.Hints, "Battleship can't tell the set of "+countVMs(unknown)+", so only "+all+
			" removes "+byCount(unknown, "it", "them")+".")
	}
	set := in.Set
	switch {
	case chosen || err != nil:
	case len(deployed) == 1 && len(sets) == 1:
		set, chosen = sets[0], true
	case len(sets) == 0:
		set, chosen = "", true // everything: nothing else to choose
	}
	found := false
	for _, of := range sets {
		checked := chosen && of == set
		found = found || checked
		step.Options = append(step.Options, stepOpt{Value: string(of), Label: string(of), Checked: checked})
	}
	if chosen && set != "" && !found {
		// Not a set the pods hold, e.g. a retry's after its VMs went.
		step.Options = append(step.Options, stepOpt{Value: string(set), Label: string(set), Checked: true})
	}
	step.Options = append(step.Options, stepOpt{Label: all, Checked: chosen && set == "", Sans: true})
	return step
}
