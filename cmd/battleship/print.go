package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// printResult prints how a direct run without a database ended, as a
// stored job's summary prints.
func printResult(w io.Writer, res pods.Result) { printSummary(w, jobs.Summarize(res)) }

// printSummary prints how a run ended: what failed, what a stop cut off, and
// what cleanup did.
func printSummary(w io.Writer, sum jobs.Summary) {
	if len(sum.Failed) > 0 {
		printFailures(w, sum.Failed)
	}
	printInterrupted(w, sum.Interrupted)
	printCleanup(w, sum)
}

// printInterrupted lists the VMs a cancel or stop cut off, if any.
func printInterrupted(w io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	vms := "VMs"
	if len(names) == 1 {
		vms = "VM"
	}
	fmt.Fprintf(w, "\n%d %s interrupted (job cancelled or stopped early): %s; re-run the same command to finish.\n",
		len(names), vms, strings.Join(names, ", "))
}

// printCleanup summarizes what the executor did to leave the cluster clean.
func printCleanup(w io.Writer, sum jobs.Summary) {
	if len(sum.Completed) > 0 {
		fmt.Fprintf(w, "\nCompleted: %s\n", strings.Join(sum.Completed, ", "))
	}
	if len(sum.Removed) > 0 {
		fmt.Fprintf(w, "\nRemoved half-built VMs: %s\n", strings.Join(sum.Removed, ", "))
	}
	if len(sum.AlreadyGone) > 0 {
		fmt.Fprintf(w, "\nAlready gone: %s\n", strings.Join(sum.AlreadyGone, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(sum.CleanupFailed)) {
		fmt.Fprintln(w, sum.CleanupFailed[name]) // already advice
	}
}

func printFailures(w io.Writer, failed map[string]string) {
	fmt.Fprintf(w, "\n%d failed:\n", len(failed))
	for _, name := range slices.Sorted(maps.Keys(failed)) {
		fmt.Fprintf(w, "  %s: %s\n", name, strings.ReplaceAll(failed[name], "\n", "\n      "))
	}
}

func printPlan(w io.Writer, naming pods.Naming, plan *pods.Plan) {
	fmt.Fprintf(w, "Plan: %s, teams %s\n", plan.Kind, strings.Join(plan.Teams, ","))
	if len(plan.Templates) > 0 {
		fmt.Fprintln(w, "\nTemplates:")
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, t := range plan.Templates {
			state := "create"
			switch {
			case t.Blocked != "":
				state = "BLOCKED: " + t.Blocked
			case t.Rebuild:
				state = "rebuild"
			case t.Exists:
				state = "reuse"
			}
			if t.WillStopMaster {
				state += " (stops master " + t.MasterName + " while building)"
			}
			gpu := ""
			if t.GPU {
				gpu = " (GPU)"
			}
			fmt.Fprintf(tw, "  %s\t%d\t%s%s\t%s\n", t.Name, t.VMID, t.Node, gpu, state)
		}
		tw.Flush()
	}
	fmt.Fprintf(w, "\nVMs (%d, %d runnable):\n", len(plan.Items), len(plan.Runnable()))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, it := range plan.Items {
		detail := joinSteps(it.Steps)
		switch {
		case plan.Kind == pods.KindSnapshot:
			detail += " " + it.Snapshot
			if it.VMState {
				detail += " with RAM"
			}
		case it.Snapshot != "":
			detail += " (to " + pods.SnapshotLabel(it) + ")"
		}
		if it.Blocked != "" {
			detail = "BLOCKED: " + it.Blocked
		}
		if plan.Kind == pods.KindDeploy {
			pool := "-"
			if it.Team != "" {
				pool = naming.Pool(it.Team)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", it.Name, orDash(it.VMID), orDashStr(it.Node), pool, detail)
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", it.Name, orDash(it.VMID), orDashStr(it.Node), detail)
	}
	tw.Flush()
}

// printCapacity prints what the plan needs against what the cluster has
// free, and the same warnings the web preview shows.
func printCapacity(w io.Writer, c *pods.Capacity) {
	if c == nil || len(c.Usage)+len(c.Notes) == 0 {
		return
	}
	fmt.Fprintln(w, "\nCapacity:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, u := range c.Usage {
		label := u.Label()
		if u.Kind == pods.UsageMemory {
			label += " memory"
		}
		fmt.Fprintf(tw, "  %s\t%s used of %s, needs %s, %s free\n", label, pods.GiB(u.Used), pods.GiB(u.Total), pods.GiB(u.Needed), pods.GiB(u.Free()))
	}
	tw.Flush()
	for _, warn := range c.Warnings() {
		if warn.Over {
			fmt.Fprintln(w, "WARNING: "+warn.Text)
		} else {
			fmt.Fprintln(w, "Note: "+warn.Text)
		}
	}
	for _, n := range c.Notes {
		fmt.Fprintln(w, "Note: "+n)
	}
}

// orDash shows "-" for a VM that doesn't exist yet and has no VMID.
func orDash(vmid int) string {
	if vmid == 0 {
		return "-"
	}
	return strconv.Itoa(vmid)
}

func orDashStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinSteps(steps []pods.Step) string {
	s := make([]string, len(steps))
	for i, st := range steps {
		s[i] = string(st)
	}
	return strings.Join(s, " > ")
}

func printEvent(w io.Writer, e pods.Event) {
	if e.Status == pods.EventSkipped && e.Message == "" {
		return // already converged; not worth a line
	}
	item := e.Item
	if item == "" {
		item = "job"
	}
	line := fmt.Sprintf("%s %-8s %s", e.Time.Format("15:04:05"), e.Status, item)
	if e.Step != "" {
		line += " " + string(e.Step)
	}
	if e.Message != "" {
		line += ": " + e.Message
	}
	fmt.Fprintln(w, line)
}
