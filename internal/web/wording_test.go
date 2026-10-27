package web

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/jobs"
	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/proxmox"
	"github.com/wccomps/battleship/internal/store"
)

var (
	scriptRE = regexp.MustCompile(`(?s)<(script|style)\b.*?</(script|style)>`)
	// shownAttrRE are the attributes people read: hover text, screen
	// readers' names, placeholders.
	shownAttrRE = regexp.MustCompile(`\b(?:aria-label|title|placeholder|alt)="([^"]*)"`)
	tagRE       = regexp.MustCompile(`<[^>]*>`)
	// vmNameRE is a VM name from naming.vm_name (team{team}-{host}): config,
	// not wording.
	vmNameRE = regexp.MustCompile(`\bteam\d{2}-[\w.-]*`)
	teamRE   = regexp.MustCompile(`(?i)\bteams?\b`)
	// competitorRE are the phrases where "team" means the competitors, as
	// it should: they read pod VMs' notes and reach it by VPN.
	competitorRE = regexp.MustCompile(`which team members can read|the team VPN`)
)

// shownText is what a person reads on a page: its text, and the
// attributes that are shown or spoken.
func shownText(body string) string {
	body = scriptRE.ReplaceAllString(body, " ")
	var b strings.Builder
	for _, m := range shownAttrRE.FindAllStringSubmatch(body, -1) {
		b.WriteString(m[1] + "\n")
	}
	b.WriteString(tagRE.ReplaceAllString(body, " "))
	return vmNameRE.ReplaceAllString(html.UnescapeString(b.String()), "VM")
}

// What people see calls a team's VMs a pod: no page says "team" or
// "teams", whatever the form fields, URLs and VM names are called, except
// where it means the competitors (competitorRE), which pages do say.
func TestPagesSayPods(t *testing.T) {
	h := newHarness(t)
	tagSets(h)
	addImages(h)
	h.api.add(proxmox.VM{VMID: 5001, Name: "fern.kilo.alpha", Node: "n1", Status: "stopped", Tags: "dev"},
		map[string]string{"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0"})
	h.api.Mu.Lock()
	h.api.VMs[10101].Config["description"] = "kilo.alpha's dc" // drift: notes
	h.api.Mu.Unlock()
	h.poll()
	h.srv.as = func(proxmox.Credential) pods.API { return resourcesAPI{h.api, capacityResources(60)} } // a deploy preview's capacity
	lead := h.login(asLead)
	failed := h.submitJob(jobs.Inputs{Kind: pods.KindTeardown, Teams: "1", Set: "kilo.alpha"}, byLead)
	h.start(failed)
	h.finish(failed, store.Outcome{Status: store.StatusFailed, Error: "planning failed"})

	pages := map[string]string{}
	// The VM page reads the notes first, so the grid after it shows them.
	for _, path := range []string{"/vm/01/dc", "/", "/deploy", "/teardown", "/teardown?teams=1-3", "/teardown?teams=x", "/reset", "/reset?teams=1",
		"/power", "/snapshot", "/logs", "/logs/" + itoa(failed), "/vm/09/dc"} {
		pages["GET "+path] = h.get(&lead, path).Body.String()
	}
	for name, form := range map[string]url.Values{
		"/teardown/preview set":        {"teams": {"1-3"}, "set": {"kilo.alpha"}},
		"/teardown/preview everything": {"teams": {"1-3"}, "set": {""}},
		"/deploy/preview":              {"teams": {"4"}, "pattern": {"*.kilo.alpha"}, "baseline": {"yes"}},
		"/environments/new/preview":    {"domain": {"kilo.alpha"}, "router": {"gw=gw"}, "env_hosts": {"teak=debian-12:3"}},
		"/teardown/preview none":       {"teams": {"1"}, "hosts": {"nope"}},
		"/teardown/preview bad":        {"teams": {"1-x"}},
		"/power/preview":               {"teams": {"1-3"}, "action": {"start"}},
		"/power/preview bad VM":        {"teams": {"1"}, "action": {"start"}, "vms": {"team03-dc"}},
		"/reset/preview":               {"teams": {"1"}},
	} {
		path, _, _ := strings.Cut(name, " ")
		pages["POST "+name] = h.post(&lead, path, form).Body.String()
	}
	competitors := map[string]bool{}
	for name, body := range pages {
		text := shownText(body)
		for _, line := range strings.Split(text, "\n") {
			for _, m := range competitorRE.FindAllString(line, -1) {
				competitors[m] = true
			}
			if line = competitorRE.ReplaceAllString(line, ""); teamRE.MatchString(line) {
				t.Errorf("%s says %q", name, strings.Join(strings.Fields(line), " "))
			}
		}
	}
	if len(competitors) != 2 {
		t.Errorf("the pages show the competitors' team only as %v", competitors)
	}
	// The checks themselves see what they should.
	if !teamRE.MatchString(shownText(`<p>Deploy teams</p>`)) || !teamRE.MatchString(shownText(`<a aria-label="Team operations">`)) ||
		teamRE.MatchString(shownText(`<input name="teams" value="1"><p>team01-dc</p>`)) {
		t.Error("shownText misreads")
	}
	contains(t, "the header", pages["GET /"], ">Deploy pods</a>", ">Tear down pods</a>", `aria-label="Pod operations"`)
}
