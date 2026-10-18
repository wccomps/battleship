package pods

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/config"
	"github.com/wccomps/battleship/internal/pods/podstest"
	"github.com/wccomps/battleship/internal/proxmox"
)

// setsFake is two pods holding two sets and VMs whose set is unknown:
//
//	team01-dc   tagged tango.delta
//	team01-web  untagged, though web.tango.delta.tpl gives its VMID: unknown
//	team01-mail tagged kilo.alpha
//	team01-fs   untagged: unknown
//	team02-dc   tagged kilo.alpha, in capitals among other tags
//	team02-web  tagged tango.delta
//	team02-db   tagged kilo.alpha and kilo.bravo: unknown
//	team02-fw   tagged kilo.alpha and tango.delta: unknown
//
// plus disks a gone VM left at 10201 (dc.tango.delta's) and 10203
// (mail.kilo.alpha's), an out-of-range template 9103 whose CloneVMID mod 100
// collides with 9003's, and a half-deleted VM at 10105 (its tags are gone
// with its config).
func setsFake() *podstest.Fake {
	f := podstest.New("cedar")
	tpl := func(vmid int, name string) {
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "cedar", Template: true, Tags: "dev"}, nil)
	}
	tpl(9001, "dc.tango.delta.tpl")
	tpl(9002, "web.tango.delta.tpl")
	tpl(9003, "mail.kilo.alpha.tpl")
	tpl(9004, "dc.Kilo.Alpha.tpl")
	tpl(9005, "db.kilo.alpha.tpl")
	tpl(9006, "db.kilo.bravo.tpl")
	tpl(9103, "xmail.tango.delta.tpl")
	vm := func(vmid int, name, tags string) {
		f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "cedar", Status: "running", Pool: "pool-" + name[4:6], Tags: tags}, nil)
	}
	vm(10101, "team01-dc", "dev;set-tango.delta")
	vm(10102, "team01-web", "dev")
	vm(10103, "team01-mail", "set-kilo.alpha")
	vm(10150, "team01-fs", "")
	vm(10204, "team02-dc", "dev, SET-KILO.ALPHA tango.delta")
	vm(10202, "team02-web", "set-tango.delta")
	vm(10205, "team02-db", "set-kilo.bravo;set-kilo.alpha")
	vm(10260, "team02-fw", "set-kilo.alpha;dev;set-tango.delta")
	f.Add(halfDeleted(10105, "pool-01", ""))
	f.Mu.Lock()
	storage := config.Default().Deploy.Storage
	f.Vols[storage+":10201/vm-10201-disk-0.qcow2"] = 10201
	f.Vols[storage+":10203/vm-10203-disk-0.qcow2"] = 10203
	f.Mu.Unlock()
	return f
}

// One set tag is the VM's set; none is "not recorded"; several or an empty
// one is drift and "unknown set".
func TestSetOf(t *testing.T) {
	for _, tc := range []struct {
		tags    string
		want    Set
		several bool
		label   string
	}{
		{"", "", false, "not recorded"},
		{"dev;tango.delta", "", false, "not recorded"},
		{"sets-kilo.alpha;xset-kilo.a", "", false, "not recorded"},
		{"set-", "", true, "unknown set"},
		{"set-;set-kilo.alpha", "", true, "unknown set"},
		{"set-tango.delta", "tango.delta", false, "tango.delta"},
		{"dev,SET-Tango.Delta set-tango.delta", "tango.delta", false, "tango.delta"},
		{"set-kilo.bravo;set-kilo.alpha", "", true, "unknown set"},
		{"set-c;set-b;set-a", "", true, "unknown set"},
	} {
		got, ok := SetOf(proxmox.VM{Name: "team01-dc", VMID: 10101, Tags: tc.tags})
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("tags %q: set %q %t, want %q", tc.tags, got, ok, tc.want)
		}
		r := SetTags(tc.tags)
		if r.Several() != tc.several || r.Label() != tc.label {
			t.Errorf("tags %q: record %v several=%t label=%q, want several=%t label=%q",
				tc.tags, r, r.Several(), r.Label(), tc.several, tc.label)
		}
	}
}

func TestSplitMaster(t *testing.T) {
	n := NewNaming(config.Default().Naming)
	for name, want := range map[string][2]string{
		"teak.tango.delta": {"teak", "tango.delta"}, "teak.Tango.Delta.tpl": {"teak", "tango.delta"},
		"teak": {"teak", ""}, "teak.tpl": {"teak", ""}, "teak.": {"teak", ""}, ".kilo": {"", "kilo"},
	} {
		if host, set := n.SplitMaster(name); host != want[0] || string(set) != want[1] {
			t.Errorf("SplitMaster(%q) = %q, %q, want %q", name, host, set, want)
		}
	}
}

func TestParseSet(t *testing.T) {
	for in, want := range map[string]string{"": "", " Kilo.Alpha ": "kilo.alpha", "eu.corp.local": "eu.corp.local"} {
		if got, err := ParseSet(in); err != nil || string(got) != want {
			t.Errorf("ParseSet(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"*", "*.kilo.alpha", "kilo.*", "kilo alpha", "kilo;alpha", "kilo,alpha", "kilo\u00a0alpha"} {
		if got, err := ParseSet(in); err == nil {
			t.Errorf("ParseSet(%q) = %q, want an error", in, got)
		}
	}
	if s := Set("kilo.alpha"); s.Tag() != "set-kilo.alpha" || s.Pattern() != "*.kilo.alpha" {
		t.Errorf("tag %q, pattern %q", s.Tag(), s.Pattern())
	}
	for in, want := range map[string]Set{
		"*.kilo.alpha": "kilo.alpha", " *.Kilo.Alpha ": "kilo.alpha",
		"*": "", "*.*": "", "*.kilo.*": "", "teak.kilo.alpha": "", "": "",
	} {
		if got, ok := SetOfPattern(in); got != want || ok != (want != "") {
			t.Errorf("SetOfPattern(%q) = %q, %t, want %q", in, got, ok, want)
		}
	}
}

func TestSetTagChanges(t *testing.T) {
	for _, tc := range []struct {
		cur  string
		set  Set
		want string
	}{
		{"", "kilo.alpha", "set-kilo.alpha"},
		{"dev", "kilo.alpha", "dev;set-kilo.alpha"},
		{"dev;set-kilo.alpha", "kilo.alpha", ""},
		{"Set-Tango.Delta, dev keep", "kilo.alpha", "dev;keep;set-kilo.alpha"},
		{"set-kilo.alpha;dev", "", "dev"},
		{"set-kilo.alpha", "", "<delete>"},
	} {
		got := SetTagChanges(map[string]string{"tags": tc.cur}, tc.set)
		want := map[string]string{}
		switch tc.want {
		case "":
		case "<delete>":
			want["delete"] = "tags"
		default:
			want["tags"] = tc.want
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("SetTagChanges(%q, %q) = %v, want %v", tc.cur, tc.set, got, want)
		}
	}
}

func setsTeardown(t *testing.T, f *podstest.Fake, set Set, hosts ...string) *Plan {
	t.Helper()
	plan, err := testPlanner(f).Teardown(context.Background(), []string{"01", "02"}, hosts, set)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func keptNames(plan *Plan) map[string]string {
	out := map[string]string{}
	for _, k := range plan.Kept {
		out[k.Name] = k.Why
	}
	return out
}

// A teardown of a set takes only VMs tagged with it alone, and only its
// in-range templates' disks, while still reporting any half-deleted VM in
// the pods (whose tags Proxmox erased); everything else in the pods is
// kept, saying why.
func TestTeardownOfASet(t *testing.T) {
	f := setsFake()
	plan := setsTeardown(t, f, "tango.delta")
	// 9103 (xmail.tango.delta.tpl) is outside 9000..9099, so 10203's disks
	// (mail.kilo.alpha.tpl's) are not freed for tango.delta.
	if got, want := itemNames(plan), []string{"team01-dc", "team01-half-deleted-10105", "team02-disks-10201", "team02-web"}; !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	for _, it := range plan.Items {
		if it.Host != "" && it.Set != "tango.delta" {
			t.Errorf("%s: set %q", it.Name, it.Set)
		}
	}
	kept := keptNames(plan)
	// team01-web's VMID is web.tango.delta.tpl's clone VMID, but that
	// template's slot may have been another's: untagged, it is unknown.
	unknown := "of an unknown set, with no single set tag, which only a teardown that names no set removes"
	if len(kept) != 6 || kept["team01-mail"] != "of kilo.alpha" || kept["team02-dc"] != "of kilo.alpha" ||
		kept["team01-fs"] != unknown || kept["team01-web"] != unknown || kept["team02-db"] != unknown || kept["team02-fw"] != unknown {
		t.Errorf("kept = %q", kept)
	}

	// dc.Kilo.Alpha.tpl is kilo.alpha's; the half-deleted VM in pod 01 is
	// reported here too so the operator sees why VMID 10105 is still held.
	plan = setsTeardown(t, f, "kilo.alpha")
	if got, want := itemNames(plan), []string{"team01-half-deleted-10105", "team01-mail", "team02-dc", "team02-disks-10203"}; !slices.Equal(got, want) {
		t.Errorf("kilo.alpha items = %v, want %v", got, want)
	}
	// A host filter narrows a set's teardown, and plans no gone VM or disks.
	plan = setsTeardown(t, f, "tango.delta", "web")
	if got, want := itemNames(plan), []string{"team02-web"}; !slices.Equal(got, want) {
		t.Errorf("web items = %v, want %v", got, want)
	}
	// A set the pods don't hold keeps every named VM and still reports the
	// half-deleted VM.
	if plan = setsTeardown(t, f, "kilo"); !slices.Equal(itemNames(plan), []string{"team01-half-deleted-10105"}) || len(plan.Kept) != 8 {
		t.Errorf("kilo: items %v, kept %v", itemNames(plan), plan.Kept)
	}
}

// Everything (no set, as every teardown stored before sets) takes every
// VM in the pods, unknown ones too, and every gone VM and disks; it keeps
// none.
func TestTeardownOfEverything(t *testing.T) {
	plan := setsTeardown(t, setsFake(), "")
	want := []string{"team01-dc", "team01-fs", "team01-half-deleted-10105", "team01-mail", "team01-web",
		"team02-db", "team02-dc", "team02-disks-10201", "team02-disks-10203", "team02-fw", "team02-web"}
	if got := itemNames(plan); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	if len(plan.Kept) != 0 {
		t.Errorf("kept = %+v", plan.Kept)
	}
	sets := map[string]Set{}
	labels := map[string]string{}
	for _, it := range plan.Items {
		sets[it.Name] = it.Set
		labels[it.Name] = it.Sets.Label()
	}
	if sets["team02-web"] != "tango.delta" || sets["team02-dc"] != "kilo.alpha" || sets["team01-web"] != "" || sets["team02-db"] != "" {
		t.Errorf("sets = %v", sets)
	}
	if labels["team02-db"] != "unknown set" || labels["team01-web"] != "not recorded" {
		t.Errorf("labels = %v", labels)
	}
}

// A set whose name ends in another's (kilo.alpha, eu.corp.local) is a set
// of its own: a teardown of alpha or corp.local takes none of its VMs and
// frees none of its disks.
func TestSetTeardownLeavesSetsNamedLikeIt(t *testing.T) {
	cfg := config.Default()
	n := NewNaming(cfg.Naming)
	for _, tc := range []struct{ parent, child Set }{{"alpha", "kilo.alpha"}, {"corp.local", "eu.corp.local"}} {
		f := podstest.New("cedar")
		f.Add(proxmox.VM{VMID: 9001, Name: "dc." + string(tc.parent) + ".tpl", Node: "cedar", Template: true}, nil)
		f.Add(proxmox.VM{VMID: 9002, Name: "dc2." + string(tc.child) + ".tpl", Node: "cedar", Template: true}, nil)
		f.Add(proxmox.VM{VMID: 9003, Name: "web." + string(tc.child) + ".tpl", Node: "cedar", Template: true}, nil)
		f.Add(proxmox.VM{VMID: n.CloneVMID("01", 9001), Name: "team01-dc", Node: "cedar", Status: "running", Pool: n.Pool("01"), Tags: tc.parent.Tag()}, nil)
		f.Add(proxmox.VM{VMID: n.CloneVMID("01", 9002), Name: "team01-dc2", Node: "cedar", Status: "running", Pool: n.Pool("01"), Tags: tc.child.Tag()}, nil)
		gone := n.CloneVMID("01", 9003) // a gone VM of the child set left its disks
		f.Mu.Lock()
		f.Vols[cfg.Deploy.Storage+":"+strconv.Itoa(gone)+"/vm-"+strconv.Itoa(gone)+"-disk-0.qcow2"] = gone
		f.Mu.Unlock()

		plan, err := NewPlanner(f, cfg).Teardown(context.Background(), []string{"01"}, nil, tc.parent)
		if err != nil {
			t.Fatal(err)
		}
		if got := itemNames(plan); !slices.Equal(got, []string{"team01-dc"}) {
			t.Errorf("a teardown of %s plans %v, want only team01-dc", tc.parent, got)
		}
	}
}

func TestDeployed(t *testing.T) {
	f := setsFake()
	n := NewNaming(config.Default().Naming)
	vms, _ := f.ClusterVMs(context.Background())
	var got []string
	for _, d := range n.Deployed(vms, []string{"01", "02"}, nil) {
		got = append(got, fmt.Sprintf("%s:%d:%v:%d", cmp.Or(string(d.Set), UnknownSet), d.VMs, d.Pods, d.SameHosts()))
	}
	want := []string{
		"kilo.alpha:2:map[01:[mail] 02:[dc]]:1",
		"tango.delta:2:map[01:[dc] 02:[web]]:1",
		"unknown set:4:map[01:[fs web] 02:[db fw]]:2",
	}
	if !slices.Equal(got, want) {
		t.Errorf("deployed =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if d := n.Deployed(vms, []string{"02"}, []string{"web"}); len(d) != 1 || d[0].Set != "tango.delta" || d[0].VMs != 1 {
		t.Errorf("pod 02 web: %+v", d)
	}
}

func TestKeptLines(t *testing.T) {
	var kept []KeptVM
	for i := 1; i <= 6; i++ {
		kept = append(kept, KeptVM{Name: fmt.Sprintf("team%02d-dc", i), Why: "of kilo.alpha"})
	}
	kept = append(kept, KeptVM{Name: "team01-web", Why: "of tango.delta"}, KeptVM{Name: "team02-web", Why: "of tango.delta"},
		KeptVM{Name: "stray", Why: "outside dev-01"})
	want := []string{
		"6 VMs are of kilo.alpha: team01-dc, team02-dc, team03-dc and 3 more.",
		"team01-web and team02-web are of tango.delta.",
		"stray is outside dev-01.",
	}
	if got := KeptLines(kept); !slices.Equal(got, want) {
		t.Errorf("KeptLines = %q, want %q", got, want)
	}
	if got := KeptLines(kept[:4]); !slices.Equal(got, []string{"team01-dc, team02-dc, team03-dc and team04-dc are of kilo.alpha."}) {
		t.Errorf("four: %q", got)
	}
}

// setNames draws set names that nest in and end in one another: alpha and
// kilo.alpha, corp.local and eu.corp.local, and chains of labels before
// some of them.
func setNames(t *rapid.T) []string {
	sets := []string{"alpha", "kilo.alpha", "corp.local", "eu.corp.local"}
	for range rapid.IntRange(0, 3).Draw(t, "chains") {
		set := rapid.SampledFrom(append(slices.Clone(sets), "local")).Draw(t, "chain end")
		for range rapid.IntRange(1, 2).Draw(t, "labels") {
			set = rapid.StringMatching(`[a-z][a-z0-9]{0,2}`).Draw(t, "label") + "." + set
		}
		if !slices.Contains(sets, set) {
			sets = append(sets, set)
		}
	}
	return sets
}

// setTagsOf is what a tag list's set tags say, by the rule: each tag, split
// at ';', ',' or spaces and lower-cased, that starts "set-" is a set tag.
func setTagsOf(tags []string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tags {
		if t = strings.ToLower(t); strings.HasPrefix(t, "set-") {
			out[t] = true
		}
	}
	return out
}

// A teardown of a set plans the VMs tagged with that set's tag alone, every
// half-deleted VM in the pods (which lost its tags), and the disks gone VMs
// left at the clone VMIDs of that set's in-range templates, keeping every
// other VM in the pods. The sets nest and end in one another; the
// templates' names may be in capitals, gone, or outside the template VMID
// range; VMs may carry no set tag, another set's, several or an empty one.
// Everything plans every VM, gone VM and disks.
func TestPropTeardownTakesOnlyTheSet(t *testing.T) {
	cfg := config.Default()
	hosts := []string{"dc", "web", "mail"}
	teams := []string{"01", "02", "03"}
	storage := cfg.Deploy.Storage
	rapid.Check(t, func(t *rapid.T) {
		sets := setNames(t)
		f := podstest.New("cedar")
		n := NewNaming(cfg.Naming)
		type tpl struct {
			vmid int
			set  string
		}
		var tpls []tpl // the in-range templates that exist
		tplVMID := map[string]int{}
		for i, set := range sets {
			for j, host := range hosts {
				key := host + "." + set
				tplVMID[key] = cfg.Naming.TemplateVMIDBase + 1 + i*len(hosts) + j
				if rapid.IntRange(0, 4).Draw(t, key+" gone") == 0 {
					continue
				}
				name := host + "." + rapid.SampledFrom([]string{set, strings.ToUpper(set)}).Draw(t, key+" case") + cfg.Naming.TemplateSuffix
				f.Add(proxmox.VM{VMID: tplVMID[key], Name: name, Node: "cedar", Template: true}, nil)
				tpls = append(tpls, tpl{tplVMID[key], set})
			}
		}
		// Another set's template outside template_vmid_base..+99 whose clone
		// VMIDs modulo 100 are the same as one's: deploy blocks it, so a set
		// teardown must not free the in-range template's disks for it.
		for _, host := range hosts {
			if rapid.IntRange(0, 4).Draw(t, host+" twin") == 0 {
				a := rapid.SampledFrom(sets).Draw(t, host+" twin of")
				b := rapid.SampledFrom(sets).Filter(func(s string) bool { return s != a }).Draw(t, host+" twin set")
				vmid := tplVMID[host+"."+a] + 100
				f.Add(proxmox.VM{VMID: vmid, Name: "x" + host + "." + b + cfg.Naming.TemplateSuffix, Node: "cedar", Template: true}, nil)
			}
		}
		drawTags := func(label, own string) []string {
			tags := rapid.SliceOfN(rapid.SampledFrom([]string{"dev", "alpha", "kilo.alpha", "sets-x", "x-set-alpha"}), 0, 2).Draw(t, label+" other tags")
			for range rapid.IntRange(0, 3).Draw(t, label+" set tags") {
				set := rapid.SampledFrom(append([]string{own, own, ""}, sets...)).Draw(t, label+" set tag")
				tags = append(tags, rapid.SampledFrom([]string{"set-" + set, "SET-" + strings.ToUpper(set)}).Draw(t, label+" tag case"))
			}
			return tags
		}
		vmTags := map[string][]string{} // each VM in the pods, by its plan item's name
		taken := map[int]bool{}         // VMIDs a VM holds
		for _, team := range teams {
			for _, host := range hosts {
				if rapid.IntRange(0, 3).Draw(t, team+host+" none") == 0 {
					continue
				}
				own := rapid.SampledFrom(sets).Draw(t, team+host+" set")
				tags := drawTags(team+host, own)
				vmid := n.CloneVMID(team, tplVMID[host+"."+own])
				name := n.VMName(team, host)
				f.Add(proxmox.VM{VMID: vmid, Name: name, Node: "cedar", Status: "running", Pool: n.Pool(team), Tags: strings.Join(tags, rapid.SampledFrom([]string{";", ",", " "}).Draw(t, team+host+" sep"))}, nil)
				vmTags[name] = tags
				taken[vmid] = true
			}
		}
		// Half-deleted VMs and gone VMs' disks at clone VMIDs no VM holds.
		var disks []int
		for _, team := range teams {
			for _, key := range slices.Sorted(maps.Keys(tplVMID)) {
				vmid := n.CloneVMID(team, tplVMID[key])
				if taken[vmid] {
					continue
				}
				switch rapid.IntRange(0, 5).Draw(t, team+key+" left") {
				case 0:
					f.Add(halfDeleted(vmid, n.Pool(team), ""))
					vmTags[n.VMName(team, "half-deleted-"+strconv.Itoa(vmid))] = nil
					taken[vmid] = true
				case 1:
					f.Mu.Lock()
					f.Vols[storage+":"+strconv.Itoa(vmid)+"/vm-"+strconv.Itoa(vmid)+"-disk-0.qcow2"] = vmid
					f.Mu.Unlock()
					disks = append(disks, vmid)
				}
			}
		}

		asked := rapid.SliceOfNDistinct(rapid.SampledFrom(teams), 1, 3, rapid.ID).Draw(t, "pods")
		chosen := rapid.SampledFrom(sets).Draw(t, "chosen")
		inAsked := func(name string) bool { team, _, _ := n.ParseVMName(name); return slices.Contains(asked, team) }
		// want is what a teardown of set ("" for everything) must plan.
		want := func(set string) (items, kept []string) {
			for name, tags := range vmTags {
				if !inAsked(name) {
					continue
				}
				st := setTagsOf(tags)
				if set == "" || len(st) == 1 && st["set-"+set] || strings.Contains(name, "half-deleted") {
					items = append(items, name)
				} else {
					kept = append(kept, name)
				}
			}
			for _, vmid := range disks {
				team, _ := n.TeamOfCloneVMID(vmid)
				if slices.Contains(asked, team) && slices.ContainsFunc(tpls, func(tp tpl) bool {
					return (set == "" || tp.set == set) && n.CloneVMID(team, tp.vmid) == vmid
				}) {
					items = append(items, n.VMName(team, "disks-"+strconv.Itoa(vmid)))
				}
			}
			slices.Sort(items)
			slices.Sort(kept)
			return items, kept
		}
		p := NewPlanner(f, cfg)
		for _, set := range []string{chosen, ""} {
			plan, err := p.Teardown(context.Background(), asked, nil, Set(set))
			if err != nil {
				t.Fatal(err)
			}
			wantItems, wantKept := want(set)
			got := itemNames(plan)
			slices.Sort(got)
			var gotKept []string
			for _, k := range plan.Kept {
				gotKept = append(gotKept, k.Name)
			}
			slices.Sort(gotKept)
			if !slices.Equal(got, wantItems) || !slices.Equal(gotKept, wantKept) {
				t.Fatalf("a teardown of %q (sets %v) plans %v and keeps %v, want %v and %v", set, sets, got, gotKept, wantItems, wantKept)
			}
		}
	})
}
