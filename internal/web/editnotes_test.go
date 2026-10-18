package web

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode"

	"pgregory.net/rapid"

	"github.com/wccomps/battleship/internal/pods"
	"github.com/wccomps/battleship/internal/store"
)

// outsideDetails is the rule for what a details-only write leaves exactly
// as it was, written apart from pods: every line of desc but those of a
// details block (a line reading **Details**, less format characters and
// spaces, and the lines after it up to a blank or battleship line), and
// the one blank line after each block.
func outsideDetails(desc string) string {
	read := func(line string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, line))
	}
	lines := strings.Split(desc, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		if read(lines[i]) != "**Details**" {
			out = append(out, lines[i])
			continue
		}
		for i+1 < len(lines) && read(lines[i+1]) != "" && !strings.HasPrefix(read(lines[i+1]), "battleship:") && read(lines[i+1]) != "**Details**" {
			i++
		}
		if i+1 < len(lines) && read(lines[i+1]) == "" {
			i++
		}
	}
	return strings.Join(out, "\n")
}

// notesPieces are pieces of notes people may have written in Proxmox,
// hostile to a form and to Edit notes's rule for free text.
var notesPieces = []string{
	"Patched monthly.", "\n", "\n\n", "\r\n", "\r", "\t", " ", "  ", "\u00a0", "\u3000", "\u2007", "\u2028", "\u2029", "\u0085",
	"\u200b", "\u202e", "\u2066", "\ufeff", "\x00", "\x07", "\x1b", "\x7f", "\ue000", "\U0001F468\u200d\U0001F469", "é",
	"</textarea><script>x()</script>", "&amp;", "<b>", "\"'", "\nbattleship: ip=10.50.0.9 os=linux", "\n  battleship: source=1",
	"\n**Details**\n- Colour: blue\n", "- OS: Debian 12",
}

// Whatever notes an image has, its Edit notes form sent back unchanged but
// for a detail is accepted, and leaves the rest of the notes exactly as
// they were: the form posts nothing its own planner refuses for the way it
// was loaded.
func TestPropEditNotesFormSentBackUnchanged(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	rapid.Check(t, func(rt *rapid.T) {
		piece := rapid.OneOf(rapid.SampledFrom(notesPieces), rapid.SampledFrom(notesPieces), rapid.String())
		free := strings.Join(rapid.SliceOfN(piece, 0, 8).Draw(rt, "free pieces"), "")
		if rapid.IntRange(0, 4).Draw(rt, "long") == 0 {
			free += strings.Repeat("x", pods.MaxFreeNotes+1)
		}
		head := rapid.SampledFrom([]string{"", winBlock + "\n\n", "intro\n\n**Details**\n- Colour: blue\n\n"}).Draw(rt, "head")
		desc := head + free + rapid.SampledFrom([]string{"", "\nbattleship: source=160"}).Draw(rt, "line")
		h.api.Mu.Lock()
		h.api.VMs[8010].Config["description"] = desc
		h.api.Mu.Unlock()

		action, form := browserForm(rt, h.get(&sess, notesHref("win2022")).Body.String())
		form.Set(fieldOS, "Changed OS "+rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "os"))
		rec := h.post(&sess, action, form)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/images/details/confirm") {
			rt.Fatalf("notes %q: preview = %d\n%s", desc, rec.Code, rec.Body)
		}
		confirm, fields := confirmOf(h.t, rec.Body.String())
		if rec := h.post(&sess, confirm, fields); rec.Code != http.StatusSeeOther {
			rt.Fatalf("confirm = %d\n%s", rec.Code, rec.Body)
		}
		js := h.jobsInStore()
		newest := slices.MaxFunc(js, func(a, b store.Job) int { return int(a.ID - b.ID) })
		if res := h.runPlan(newest); len(res.Succeeded) != 1 {
			rt.Fatalf("result = %+v", res)
		}
		// Notes with nothing but spaces beside the block may lose them.
		if got, was := outsideDetails(h.notesOf(8010)), outsideDetails(desc); got != was && (got != "" || strings.TrimSpace(was) != "") {
			rt.Fatalf("notes %q became %q", desc, h.notesOf(8010))
		}
	})
}

// drawNotes draws notes an image may have: a details block or not, free
// text from notesPieces or anything, and a battleship line or not.
func drawNotes(rt *rapid.T, label string) string {
	piece := rapid.OneOf(rapid.SampledFrom(notesPieces), rapid.SampledFrom(notesPieces), rapid.String())
	free := strings.Join(rapid.SliceOfN(piece, 0, 6).Draw(rt, label+" free pieces"), "")
	head := rapid.SampledFrom([]string{"", winBlock + "\n\n", "intro\n\n**Details**\n- Colour: blue\n\n"}).Draw(rt, label+" head")
	return head + free + rapid.SampledFrom([]string{"", "\nbattleship: source=160"}).Draw(rt, label+" line")
}

// An Edit notes form remembers the notes it opened on: whatever notes A it
// opened with, and notes B written after it opened, its preview is refused
// when B isn't A, and offers a confirm that carries the mark through when
// it is.
func TestPropEditNotesFormRefusesNotesChangedSince(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	set := func(desc string) {
		h.api.Mu.Lock()
		h.api.VMs[8010].Config["description"] = desc
		h.api.Mu.Unlock()
	}
	rapid.Check(t, func(rt *rapid.T) {
		a := drawNotes(rt, "A")
		b := a
		if rapid.Bool().Draw(rt, "written since") {
			b = drawNotes(rt, "B")
		}
		set(a)
		action, form := browserForm(rt, h.get(&sess, notesHref("win2022")).Body.String())
		set(b)
		form.Set(fieldOS, "Changed OS "+rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "os"))
		rec := h.post(&sess, action, form)
		body := rec.Body.String()
		if a != b {
			if rec.Code != http.StatusOK || !strings.Contains(body, "changed since you opened this form") || strings.Contains(body, "/images/details/confirm") {
				rt.Fatalf("notes %q, then %q: preview = %d\n%s", a, b, rec.Code, body)
			}
			return
		}
		if rec.Code != http.StatusOK || !strings.Contains(body, "/images/details/confirm") {
			rt.Fatalf("notes %q, unchanged: preview = %d\n%s", a, rec.Code, body)
		}
		if _, fields := confirmOf(h.t, body); form.Get(fieldNotesFrom) == "" || fields.Get(fieldNotesFrom) != form.Get(fieldNotesFrom) {
			rt.Fatalf("the form's mark %q, the confirm's %q", form.Get(fieldNotesFrom), fields.Get(fieldNotesFrom))
		}
	})
}

// An Edit notes form that couldn't read the notes it would replace, or
// has no family to read, says why and offers no save: it can't post empty
// fields over notes it never showed.
func TestEditNotesFormWithoutNotesOffersNoSave(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	h.api.setReadErr(8010, errors.New("config read refused"))
	sess := h.login(asLead)
	for target, why := range map[string]string{
		"/images/details":    "Choose the base image whose notes to edit",
		notesHref("nope"):    "has no version that isn&#39;t deprecated",
		notesHref("win2022"): "Couldn&#39;t read the notes to edit",
	} {
		rec := h.get(&sess, target)
		body := rec.Body.String()
		contains(t, target, body, `<p class="warn" role="alert">`, why)
		lacks(t, target, body, `<button type="submit" class="btn pri">`, `name="notes"`, `name="detail_os"`, `name="details"`)
	}
	rec := h.post(&sess, "/images/details/preview", notesForm("win2022", pods.ImageDetails{}, ""))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("preview over unread notes = %d", rec.Code)
	}
	lacks(t, "the refused preview", rec.Body.String(), `<button type="submit" class="btn pri">`, "/images/details/confirm")
	h.noJobs("unread notes")
}

// The Edit notes form, and its review's confirm, mark that they post free
// text (fieldDetails is detailsWithNotes), so a server that reads only "yes"
// reads no details and refuses them rather than run details alone; and a
// confirm whose free text its preview didn't have is refused.
func TestEditNotesMarksFreeText(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	_, form := browserForm(t, h.get(&sess, notesHref("win2022")).Body.String())
	if form.Get(fieldDetails) != detailsWithNotes {
		t.Errorf("the form posts details=%q", form.Get(fieldDetails))
	}
	form.Set(fieldNotes, "Patched monthly.\r\nRebooted.")
	_, fields, _ := h.preview(&sess, "/images/details", form)
	if fields.Get(fieldDetails) != detailsWithNotes || fields.Get(fieldNotes) != "Patched monthly.\nRebooted." {
		t.Errorf("the confirm posts %v", fields)
	}

	_, fields, _ = h.preview(&sess, "/images/details", detailsForm("win2022", pods.ImageDetails{OS: "Debian 12"}))
	fields.Set(fieldDetails, detailsWithNotes)
	fields.Set(fieldNotes, "Typed after the review.")
	if rec := h.post(&sess, "/images/details/confirm", fields); rec.Code != http.StatusBadRequest {
		t.Errorf("a confirm with free text its preview lacked = %d", rec.Code)
	}
	h.noJobs("free text the preview lacked")
}

// The Edit notes form reads the cluster's VMs and the one version's
// config it edits: not every environment's.
func TestEditNotesFormReadsOnlyItsVersion(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	sess := h.login(asLead)
	h.api.Mu.Lock()
	h.api.Calls = nil
	h.api.Mu.Unlock()
	h.get(&sess, notesHref("win2022"))
	if reads, own := h.api.Called("config:"), h.api.Called("config:8010"); reads != 1 || own != 1 {
		t.Errorf("the form read %d configs, %d of them win2022-v1's", reads, own)
	}
}

// The Images list asks Proxmox for the user's privileges once, however
// many families' rows it checks for Edit.
func TestImagesAsksPrivilegesOnce(t *testing.T) {
	h := newHarness(t)
	addDetailed(h)
	viewer := h.loginWith("images-viewer", []int{8010}, "VM.Config.Options")
	_, _, before := h.pve.Counts()
	h.get(&viewer, "/images")
	_, _, after := h.pve.Counts()
	if after-before > 1 {
		t.Errorf("the Images list read the privileges %d times", after-before)
	}
}
