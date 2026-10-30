package web

import (
	"net/url"
	"testing"
)

// An unfinished snapshot (its task died) is not a reset target: the picker
// does not offer it, and a reset to it is blocked.
func TestUnfinishedSnapshotNotOffered(t *testing.T) {
	h := newHarness(t)
	h.api.setSnapshots(10101, "initial", "round1", "before-scoring")
	h.api.Mu.Lock()
	h.api.VMs[10101].SnapStates = map[string]string{"initial": "prepare", "before-scoring": "delete"}
	h.api.Mu.Unlock()
	h.poll()
	op := h.login(asOperator)

	body := h.get(&op, "/reset?teams=1&hosts=dc").Body.String()
	contains(t, "reset picker", body,
		`<input type="radio" name="snapshot" value="initial" required checked><span>initial</span>`,
		`<input type="radio" name="snapshot" value="round1" required><span>round1</span>`,
		"<span class=\"name\">team01-dc</span> has no initial snapshot.")
	lacks(t, "reset picker", body, `value="before-scoring"`)

	body = h.post(&op, "/reset/preview", url.Values{"teams": {"1"}, "hosts": {"dc"}, "snapshot": {"before-scoring"}}).Body.String()
	contains(t, "explicit reset preview", body, "Every VM is blocked",
		`no snapshot &#34;<span class="name">before-scoring</span>&#34; (has: round1)`)
}

// Proxmox leaves pool out when the user can't audit pools: the page says
// it is unknown, and it isn't drift.
func TestCellPageUnknownPool(t *testing.T) {
	h := newHarness(t)
	h.api.Mu.Lock()
	h.api.VMs[10301].Pool = ""
	h.api.Mu.Unlock()
	h.poll()
	op := h.login(asOperator)
	body := h.get(&op, "/vm/03/dc").Body.String()
	contains(t, "cell page", body, `<dt>Pool</dt><dd><span class="muted" title="your Proxmox user can't read pools">unknown</span></dd>`, `<span class="chip is-running">`)
	lacks(t, "cell page", body, `class="reasons`)
}
