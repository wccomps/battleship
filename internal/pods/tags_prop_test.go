package pods

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/proxmox"
)

// tagSeparators are what Proxmox and people put between tags.
var tagSeparators = []string{";", ",", " ", "\t", "\n", "\r\n", ";;", "; ", " , ", ""}

// tagWords are tags and near misses of the ones Battleship reads.
var tagWords = []string{
	"base", "Base", "BASE", "windows", "Windows", "linux", "LINUX", "router", "other", "deprecated", "Deprecated",
	"bas", "base2", "basement", "xbase", "base-", "linux-x", "windows2", "dev", "x", "a;b", "k",
}

// splitTagsModel splits s at ; , and ASCII white space only, lower-casing
// ASCII letters, one byte at a time.
func splitTagsModel(s string) []string {
	var out []string
	cur := []byte{}
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = []byte{}
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ';' || c == ',' || c == ' ' || (c >= 9 && c <= 13):
			flush()
		case c >= 'A' && c <= 'Z':
			cur = append(cur, c+'a'-'A')
		default:
			cur = append(cur, c)
		}
	}
	flush()
	return out
}

func drawTags(t *rapid.T, label string) string {
	words := rapid.SliceOfN(rapid.SampledFrom(tagWords), 0, 6).Draw(t, label+" words")
	var b strings.Builder
	for i, w := range words {
		if i > 0 || rapid.Bool().Draw(t, label+" lead") {
			b.WriteString(rapid.SampledFrom(tagSeparators).Draw(t, label+" sep"))
		}
		b.WriteString(w)
	}
	if rapid.Bool().Draw(t, label+" trail") {
		b.WriteString(rapid.SampledFrom(tagSeparators).Draw(t, label+" sep"))
	}
	return b.String()
}

func checkTags(t interface{ Fatalf(string, ...any) }, tags string) {
	got := Tags(tags)
	for _, g := range got {
		if g == "" || strings.ContainsAny(g, ";, \t\n\r\v\f") {
			t.Fatalf("Tags(%q) has %q", tags, g)
		}
	}
	want := splitTagsModel(tags)
	if !slices.Equal(got, want) {
		t.Fatalf("Tags(%q) = %q, want %q", tags, got, want)
	}
	for _, w := range tagWords {
		wantHas := slices.Contains(want, strings.ToLower(w)) && !strings.ContainsAny(w, ";,")
		if hasTag(tags, w) != wantHas {
			t.Fatalf("hasTag(%q, %q) = %v, want %v", tags, w, !wantHas, wantHas)
		}
	}
	// Whatever Tags finds, hasTag finds in any case; a blank asks for nothing.
	for _, g := range got {
		if !hasTag(tags, g) || !hasTag(tags, strings.ToUpper(g)) || !hasTag(tags, " "+g+" ") {
			t.Fatalf("hasTag(%q, %q) = false", tags, g)
		}
	}
	for _, blank := range []string{"", " ", "\t", "\n"} {
		if hasTag(tags, blank) {
			t.Fatalf("hasTag(%q, %q) = true", tags, blank)
		}
	}
}

// Tags and hasTag split a tag list the same way: at semicolons, commas and
// white space, ignoring case, never yielding an empty tag.
func TestPropTagsAndHasTagAgree(t *testing.T) {
	for _, tags := range []string{"", ";", ";;", " ", "base", "BASE;Linux", "base,linux", "base linux", "base\tlinux", "base\nlinux", ";base;", ", base ,",
		"base;;linux", "basement", "xbase", "a,b;c d"} {
		checkTags(t, tags)
	}
	rapid.Check(t, func(t *rapid.T) {
		checkTags(t, drawTags(t, "tags"))
	})
}

// BaseImages lists exactly the templates tagged base with one kind and a
// versioned name, by family then version.
func TestPropBaseImagesMatchModel(t *testing.T) {
	names := []string{}
	for _, f := range []string{"debian-12", "win2022", "gw", "x-v1", "Debian", "a_b", ""} {
		for _, v := range []string{"-v1", "-v2", "-v10", "-v9999", "-v10000", "-v0", "-v01", ""} {
			names = append(names, f+v)
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 12).Draw(t, "n")
		var vms []proxmox.VM
		for i := 0; i < n; i++ {
			vms = append(vms, proxmox.VM{
				VMID:     100 + i,
				Name:     rapid.SampledFrom(names).Draw(t, "name"),
				Node:     rapid.SampledFrom([]string{"pve1", "pve2"}).Draw(t, "node"),
				Template: rapid.Map(rapid.IntRange(0, 5), func(i int) bool { return i > 0 }).Draw(t, "template"),
				Tags:     drawBaseTags(t),
			})
		}
		var want []BaseImage
		for _, vm := range vms {
			tags := splitTagsModel(vm.Tags)
			kinds := 0
			var kind string
			for _, k := range []string{"router", "windows", "linux", "other"} {
				if slices.Contains(tags, k) {
					kinds++
					kind = k
				}
			}
			fam, ver, ok := parseImageNameModel(vm.Name)
			if !vm.Template || !slices.Contains(tags, "base") || kinds != 1 || !ok {
				continue
			}
			want = append(want, BaseImage{Name: vm.Name, Family: fam, Version: ver, Kind: kind, VMID: vm.VMID, Node: vm.Node,
				Deprecated: slices.Contains(tags, "deprecated")})
		}
		got := BaseImages(vms)
		order := func(a, b BaseImage) int {
			if c := strings.Compare(a.Family, b.Family); c != 0 {
				return c
			}
			if a.Version != b.Version {
				return a.Version - b.Version
			}
			return a.VMID - b.VMID
		}
		// Ties (same family and version) may come in either order.
		if !slices.IsSortedFunc(got, func(a, b BaseImage) int {
			return order(BaseImage{Family: a.Family, Version: a.Version}, BaseImage{Family: b.Family, Version: b.Version})
		}) {
			t.Fatalf("BaseImages out of order: %+v", got)
		}
		sorted := slices.Clone(got)
		slices.SortFunc(sorted, order)
		slices.SortFunc(want, order)
		if !slices.Equal(sorted, want) {
			t.Fatalf("BaseImages(%+v)\n= %+v\nwant %+v", vms, sorted, want)
		}
	})
}

// drawBaseTags draws a tag list that is often one valid for a base image
// and sometimes one slip away from it.
func drawBaseTags(t *rapid.T) string {
	if rapid.IntRange(0, 3).Draw(t, "random tags") == 0 {
		return drawTags(t, "tags")
	}
	words := []string{rapid.SampledFrom([]string{"base", "Base", "BASE"}).Draw(t, "base")}
	for _, k := range []string{"router", "windows", "linux", "other"} {
		if rapid.IntRange(0, 5).Draw(t, k) == 0 {
			words = append(words, rapid.SampledFrom([]string{k, strings.ToUpper(k), k + ";" + k}).Draw(t, k+" case"))
		}
	}
	if len(words) == 1 {
		words = append(words, rapid.SampledFrom([]string{"linux", "windows", "router", "other", "Linux"}).Draw(t, "kind"))
	}
	if rapid.Bool().Draw(t, "deprecated") {
		words = append(words, "deprecated")
	}
	return strings.Join(words, rapid.SampledFrom(tagSeparators[:6]).Draw(t, "sep"))
}
