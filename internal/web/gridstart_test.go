package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/proxmox"
)

// emptyWithMasters is the app on a cluster with no team VMs, holding the
// masters of two template sets and two masters that are in no set. One of kilo.alpha's two masters is running.
func emptyWithMasters(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	h := newHarness(t, mut...)
	h.noTeamVMs()
	for i, m := range []struct{ name, status string }{
		{"web.kilo.alpha", "stopped"}, {"dc.kilo.alpha", "running"},
		{"bugs.looney.tunes", "stopped"}, {"y", "running"}, {"blank", "stopped"},
	} {
		h.api.add(proxmox.VM{VMID: 5001 + i, Name: m.name, Node: "n1", Status: m.status, Tags: "dev"},
			map[string]string{"net0": "virtio=BC:24:11:00:00:0" + itoa(int64(i)) + ",bridge=vmbr0"})
	}
	h.poll()
	return h
}

var setDeployRE = regexp.MustCompile(`<a class="btn set-deploy" href="([^"]+)" data-panel-link>`)

// deployLinks returns the template sets' deploy links in a page, parsed.
func deployLinks(t *testing.T, body string) []*url.URL {
	t.Helper()
	var out []*url.URL
	for _, m := range setDeployRE.FindAllStringSubmatch(body, -1) {
		u, err := url.Parse(html.UnescapeString(m[1]))
		if err != nil {
			t.Fatalf("deploy link %q: %v", m[1], err)
		}
		out = append(out, u)
	}
	return out
}

// checkDeployLink fails unless u opens the deploy form for pattern, with
// the baseline snapshot on, as the form has it by default, and no teams:
// those are typed.
func checkDeployLink(t *testing.T, u *url.URL, pattern string) {
	t.Helper()
	q := u.Query()
	if u.Path != "/deploy" || q.Get("pattern") != pattern || q.Get("teams") != "" || q.Get("baseline") != "yes" ||
		q.Has("rebuild") || q.Has("hosts") || q.Has("vms") {
		t.Errorf("deploy link %s, want /deploy with pattern %s, the baseline and no teams", u, pattern)
	}
}

// The empty grid lists the cluster's template sets as cards, by name, with
// their hosts and how many masters run, and gives a lead a button per set
// that opens the deploy form filled in for it.
func TestGridEmptyOffersTemplateSets(t *testing.T) {
	h := emptyWithMasters(t)
	lead := h.login(asLead)
	body := h.get(&lead, "/").Body.String()
	contains(t, "lead's empty grid", body,
		`<section class="start" id="grid-start" aria-labelledby="sets-h">`,
		`<h2 class="sub sets-h" id="sets-h">Template sets</h2>`,
		"<h3>kilo.alpha</h3>", `<ul class="hosts" aria-label="Hosts"><li>dc</li><li>web</li></ul>`,
		`<div class="masters" aria-label="1 of 2 master VMs running">`,
		`<span class="is-running dots" aria-hidden="true"><i class="g"></i></span><span class="is-stopped dots" aria-hidden="true"><i class="g"></i></span><b>1/2</b><span>masters</span>`,
		"<h3>looney.tunes</h3>", `<ul class="hosts" aria-label="Hosts"><li>bugs</li></ul>`, "<b>0/1</b>",
		`Not in a set: <span class="vm">blank</span>, <span class="vm">y</span>`,
	)
	if strings.Index(body, "<h3>kilo.alpha</h3>") > strings.Index(body, "<h3>looney.tunes</h3>") {
		t.Error("template sets aren't in name order")
	}
	links := deployLinks(t, body)
	if len(links) != 2 {
		t.Fatalf("lead's empty grid has %d set deploy buttons, want 2 (none for other masters)", len(links))
	}
	checkDeployLink(t, links[0], "*.kilo.alpha")
	checkDeployLink(t, links[1], "*.looney.tunes")

	op := h.login(asOperator)
	body = h.get(&op, "/").Body.String()
	contains(t, "operator's empty grid", body,
		`<section class="start" id="grid-start"`,
		"<h3>kilo.alpha</h3>", "<b>1/2</b>", "<h3>looney.tunes</h3>", "Not in a set:")
	lacks(t, "operator's empty grid", body, "set-deploy", `href="/deploy`)
}

// With no tagged masters at all, the empty grid says what's missing.
func TestGridEmptyWithoutMasters(t *testing.T) {
	h := newHarness(t)
	h.noTeamVMs()
	h.poll()
	lead := h.login(asLead)
	body := h.get(&lead, "/").Body.String()
	contains(t, "lead's empty grid", body, "No master VM is tagged <code>dev</code>.", `<a class="btn" href="/deploy" data-panel-link>`)
	lacks(t, "lead's empty grid", body, "set-deploy", "Not in a set", `class="card"`)
	op := h.login(asOperator)
	body = h.get(&op, "/").Body.String()
	contains(t, "operator's empty grid", body, "No master VM is tagged <code>dev</code>.")
	lacks(t, "operator's empty grid", body, `href="/deploy`)
}

// One viewer's live grid is rendered once for all their event streams.
// The empty grid's template sets are part of it, with Deploy links for a
// viewer who may deploy, and follow the cluster: a master starting
// patches them in by id.
func TestGridEmptySetsAreLive(t *testing.T) {
	h := emptyWithMasters(t)
	lead, op := h.login(asLead), h.login(asOperator)
	before := h.srv.gridRenders.Load()
	leadStream := h.openSSE(&lead, "/events/grid")
	a := leadStream.next()
	a2 := h.openSSE(&lead, "/events/grid").next()
	b := h.openSSE(&op, "/events/grid").next()
	if a.Data != a2.Data {
		t.Errorf("one viewer's two streams differ:\n%s\n---\n%s", a.Data, a2.Data)
	}
	if n := h.srv.gridRenders.Load() - before; n != 2 {
		t.Errorf("two viewers' three streams rendered the grid %d times, want once per viewer", n)
	}
	contains(t, "lead's empty grid event", a.Data, `<section class="start" id="grid-start"`, "set-deploy", "<b>1/2</b>")
	contains(t, "operator's empty grid event", b.Data, `<section class="start" id="grid-start"`, "<b>1/2</b>")
	lacks(t, "operator's empty grid event", b.Data, "/deploy", "set-deploy")

	h.api.setStatus(5001, "running")
	h.poll()
	p := leadStream.next()
	if p.Event != "patch" {
		t.Fatalf("after a master started: %s event, want a patch", p.Event)
	}
	contains(t, "patch after a master started", p.Data, `<template><section class="start" id="grid-start"`, "<b>2/2</b>", "set-deploy")
}

// formRE and inputRE read an operation's form as a browser would submit it.
var (
	opFormRE = regexp.MustCompile(`(?s)<form [^>]*?method="post" action="(/[a-z]+/preview)"[^>]*>(.*?)</form>`)
	inputRE  = regexp.MustCompile(`<input ([^>]*)>`)
	attrRE   = regexp.MustCompile(`([a-z]+)(?:="([^"]*)")?`)
)

// submitted is what posting the operation form in body sends, without
// the CSRF token, which post adds.
func submitted(t *testing.T, body string) (string, url.Values) {
	t.Helper()
	m := opFormRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no operation form in:\n%s", body)
	}
	form := url.Values{}
	for _, in := range inputRE.FindAllStringSubmatch(m[2], -1) {
		attrs := map[string]string{}
		for _, a := range attrRE.FindAllStringSubmatch(in[1], -1) {
			attrs[a[1]] = html.UnescapeString(a[2])
		}
		_, checked := attrs["checked"]
		switch typ := attrs["type"]; {
		case attrs["name"] == "" || attrs["name"] == "csrf":
		case typ == "checkbox" || typ == "radio":
			if checked {
				form.Add(attrs["name"], attrs["value"])
			}
		default:
			form.Add(attrs["name"], attrs["value"])
		}
	}
	return m[1], form
}

// A set's button opens the deploy form filled in but for the teams, which
// it asks for; with them typed, it previews a deploy of exactly that set
// to exactly those teams, through the usual preview and confirm.
func TestGridEmptyDeployClickThrough(t *testing.T) {
	h := emptyWithMasters(t)
	lead := h.login(asLead)
	links := deployLinks(t, h.get(&lead, "/").Body.String())
	if len(links) == 0 {
		t.Fatal("no deploy buttons on the lead's empty grid")
	}
	formPage := h.get(&lead, links[0].String())
	if formPage.Code != 200 {
		t.Fatalf("GET %s = %d", links[0], formPage.Code)
	}
	contains(t, "deploy form from the grid", formPage.Body.String(),
		`name="pattern" value="*.kilo.alpha"`, `name="teams" value="" required`,
		`<input type="checkbox" name="baseline" value="yes" checked>`,
		`<input type="checkbox" name="hosts" value="dc" checked>`, `<input type="checkbox" name="hosts" value="web" checked>`)
	action, form := submitted(t, formPage.Body.String())
	if action != "/deploy/preview" {
		t.Fatalf("the deploy form posts to %s", action)
	}
	if rec := h.post(&lead, action, form); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("the deploy form sent without teams = %d, want 422\n%s", rec.Code, rec.Body)
	} else {
		contains(t, "the deploy form sent without teams", rec.Body.String(), `name="teams" value="" required`, `name="pattern" value="*.kilo.alpha"`)
		lacks(t, "the deploy form sent without teams", rec.Body.String(), `action="/deploy/confirm"`)
	}
	form.Set("teams", "4-5")
	confirm, fields, body := h.preview(&lead, "/deploy", form)
	contains(t, "deploy preview from the grid", body,
		`Deploy <span class="vm">kilo.alpha</span></h1>`, "4 will run</span>",
		`<dt>Teams</dt><dd><span class="vm">04-05</span></dd>`,
		"team04-dc", "team04-web", "team05-dc", "team05-web",
		"Building its template stops master <span class=\"name\">dc.kilo.alpha</span>, which is running, until the copy finishes.")
	lacks(t, "deploy preview from the grid", body, "bugs", "looney")
	// Without the script, the form sends the set's hosts, all ticked: the
	// same VMs as none. (The script sends none when all are ticked.)
	if confirm != "/deploy/confirm" || fields.Get("pattern") != "*.kilo.alpha" || fields.Get("teams") != "4-5" ||
		fields.Get("baseline") != "yes" || fields.Has("rebuild") || fields.Get("hosts") != "dc,web" || fields.Has("vms") {
		t.Errorf("confirm %s fields %v", confirm, fields)
	}
	h.noJobs("after the preview")
}

// With web.templates set and nothing deployed, the grid is empty like any
// other with no team VMs, with the template sets; once a team has a VM,
// its row has a column per host of the set, missing where not deployed.
func TestGridTemplatesNothingDeployed(t *testing.T) {
	h := emptyWithMasters(t, func(c *config.Config) { c.Web.Templates = "*.kilo.alpha" })
	lead := h.login(asLead)
	body := h.get(&lead, "/").Body.String()
	contains(t, "lead's undeployed grid", body, `<section class="start" id="grid-start"`)
	lacks(t, "lead's undeployed grid", body, `id="grid-table"`)
	if regexp.MustCompile(`id="grid-start"[^>]*hidden`).MatchString(body) {
		t.Error("the template sets are hidden while nothing is deployed")
	}
	links := deployLinks(t, body)
	if len(links) != 2 {
		t.Fatalf("lead's undeployed grid has %d deploy buttons, want 2", len(links))
	}
	checkDeployLink(t, links[0], "*.kilo.alpha")

	op := h.login(asOperator)
	body = h.get(&op, "/").Body.String()
	contains(t, "operator's undeployed grid", body, `<section class="start" id="grid-start"`, "<h3>kilo.alpha</h3>")
	lacks(t, "operator's undeployed grid", body, "set-deploy", `href="/deploy`)

	// Once a team VM exists, the grid replaces the sets.
	h.api.add(teamVM("04", "dc", 10401), cleanConfig("04"), "initial")
	h.poll()
	body = h.get(&lead, "/").Body.String()
	contains(t, "lead's grid with team04-dc", body, `id="grid-table"`, `id="cell-04-web"><span class="cell is-missing" title="team04-web · missing">`)
	lacks(t, "lead's grid with team04-dc", body, `id="grid-start"`)
}
