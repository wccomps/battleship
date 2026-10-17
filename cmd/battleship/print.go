package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/wccomps/battleship/internal/apply"
	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
)

// printResult prints how a direct run without a database ended, as a
// stored job's summary prints.
func printResult(w io.Writer, res apply.Result) { printSummary(w, jobs.Summarize(res)) }

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
		len(names), vms, cleanJoin(names, ", "))
}

// printCleanup summarizes what the executor did to leave the cluster clean.
func printCleanup(w io.Writer, sum jobs.Summary) {
	if len(sum.Completed) > 0 {
		fmt.Fprintf(w, "\nCompleted: %s\n", cleanJoin(sum.Completed, ", "))
	}
	if len(sum.Removed) > 0 {
		fmt.Fprintf(w, "\nRemoved half-built VMs: %s\n", cleanJoin(sum.Removed, ", "))
	}
	if len(sum.AlreadyGone) > 0 {
		fmt.Fprintf(w, "\nAlready gone: %s\n", cleanJoin(sum.AlreadyGone, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(sum.CleanupFailed)) {
		fmt.Fprintln(w, clean(sum.CleanupFailed[name])) // already advice
	}
}

func printFailures(w io.Writer, failed map[string]string) {
	fmt.Fprintf(w, "\n%d failed:\n", len(failed))
	for _, name := range slices.Sorted(maps.Keys(failed)) {
		// A failure's own lines are indented under it.
		fmt.Fprintf(w, "  %s: %s\n", clean(name), cleanLines(failed[name], "      "))
	}
}

func printPlan(w io.Writer, naming pods.Naming, plan *pods.Plan) {
	if plan.Kind.Teamless() {
		fmt.Fprintf(w, "Plan: %s\n", plan.Kind)
	} else {
		fmt.Fprintf(w, "Plan: %s, teams %s\n", plan.Kind, cleanJoin(plan.Teams, ","))
	}
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
			fmt.Fprintf(tw, "  %s\t%d\t%s%s\t%s\n", clean(t.Name), t.VMID, clean(t.Node), gpu, clean(state))
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
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", clean(it.Name), orDash(it.VMID), clean(orDashStr(it.Node)), clean(pool), clean(detail))
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", clean(it.Name), orDash(it.VMID), clean(orDashStr(it.Node)), clean(detail))
	}
	tw.Flush()
	for _, n := range plan.Notices {
		fmt.Fprintln(w, "Note: "+clean(n))
	}
	for _, k := range plan.Kept {
		fmt.Fprintln(w, "Note: "+clean(k.String()))
	}
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
		fmt.Fprintf(tw, "  %s\t%s used of %s, needs %s, %s free\n", clean(label), pods.GiB(u.Used), pods.GiB(u.Total), pods.GiB(u.Needed), pods.GiB(u.Free()))
	}
	tw.Flush()
	for _, warn := range c.Warnings() {
		if warn.Over {
			fmt.Fprintln(w, "WARNING: "+clean(warn.Text))
		} else {
			fmt.Fprintln(w, "Note: "+clean(warn.Text))
		}
	}
	for _, n := range c.Notes {
		fmt.Fprintln(w, "Note: "+clean(n))
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

func printEvent(w io.Writer, e apply.Event) {
	if e.Status == apply.EventSkipped && e.Message == "" {
		return // already converged; not worth a line
	}
	item := e.Item
	if item == "" {
		item = "job"
	}
	line := fmt.Sprintf("%s %-8s %s", e.Time.Format("15:04:05"), clean(string(e.Status)), clean(item))
	if e.Step != "" {
		line += " " + clean(string(e.Step))
	}
	if e.Message != "" {
		line += ": " + cleanLines(e.Message, "    ")
	}
	fmt.Fprintln(w, line)
}

// clean makes untrusted text (names, inputs, Proxmox and guest output) safe
// to print: invalid UTF-8 and every non-graphic character, such as escape
// sequences, newlines, tabs and bidi overrides, become visible escapes like
// \x1b, \n and \u202e.
func clean(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, notGraphic) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case notGraphic(r):
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteRune(r)
		}
		i += n
	}
	return b.String()
}

func notGraphic(r rune) bool { return !unicode.IsGraphic(r) }

// cleanLines cleans each line of multi-line text (a task's log tail, a
// wrapped error), keeping its line breaks and indenting the lines after the
// first.
func cleanLines(s, indent string) string {
	return cleanJoin(strings.Split(s, "\n"), "\n"+indent)
}

// cleanJoin joins untrusted texts, each cleaned.
func cleanJoin(ss []string, sep string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = clean(s)
	}
	return strings.Join(out, sep)
}
