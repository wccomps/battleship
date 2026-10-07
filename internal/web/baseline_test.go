package web

import (
	"net/url"
	"testing"
)

// VMs the old deploy tool made have fresh_clone_<timestamp> baselines and
// no "initial". A reset that names no snapshot rolls each back to its own.
func TestOldToolBaselines(t *testing.T) {
	h := newHarness(t)
	h.api.setSnapshots(10101, "fresh_clone_20261001000000", "fresh_clone_20261002034615", "before-scoring")
	h.api.setSnapshots(10102, "fresh_clone_20260901000000")
	h.poll()
	op := h.login(asOperator)

	body := h.get(&op, "/vm/01/dc").Body.String()
	contains(t, "cell page", body,
		`<tr><td><span class="vm">fresh_clone_20261002034615</span></td><td class="muted">baseline</td></tr>`,
		"Reset to snapshot…</button>")
	lacks(t, "cell page", body, `class="reasons`, "missing</td>")

	body = h.get(&op, "/reset?teams=1").Body.String()
	contains(t, "reset picker", body,
		`<input type="radio" name="snapshot" value="" checked><span>baseline (fresh_clone_20261002034615)</span>`,
		`<input type="radio" name="snapshot" value="fresh_clone_20261001000000"><span>fresh_clone_20261001000000</span>`)
	lacks(t, "reset picker", body, "has no initial snapshot", `value="fresh_clone_20261002034615"`)

	_, form, body := h.preview(&op, "/reset", url.Values{"teams": {"1"}})
	contains(t, "reset preview", body,
		"2 will run</span>", "<dt>Snapshot</dt><dd>baseline</dd>",
		"<td>stop · rollback to baseline (fresh_clone_20261002034615) · start</td>",
		"<td>stop · rollback to baseline (fresh_clone_20260901000000) · start</td>")
	if form.Get("snapshot") != "" {
		t.Errorf("confirm sends snapshot %q, want none: each VM's baseline", form.Get("snapshot"))
	}

	// An explicit snapshot keeps its exact name.
	_, _, body = h.preview(&op, "/reset", url.Values{"teams": {"1"}, "snapshot": {"fresh_clone_20261002034615"}})
	contains(t, "explicit reset preview", body,
		"<td>stop · rollback to fresh_clone_20261002034615 · start</td>",
		`no snapshot &#34;fresh_clone_20261002034615&#34; (has: fresh_clone_20260901000000)`)
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
