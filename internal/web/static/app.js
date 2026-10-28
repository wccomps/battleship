// battleship's one script; every page works without it. It adds live
// updates (data-events), grid selection with the action bar, and the side
// panel (pages fetched with X-Battleship-Panel, rendered without layout).
(function () {
  "use strict";
  if (!window.fetch || !window.URLSearchParams) return;

  function each(list, fn) { Array.prototype.forEach.call(list, fn); }
  function $(id) { return document.getElementById(id); }

  var app = $("app");

  // parse turns server markup into nodes. Each event piece comes in its own
  // <template> (piece, sse.go), parsed in its first tag's context; markup
  // outside a <template> (a panel page) is one piece.
  function parse(html) {
    var tpl = document.createElement("template");
    tpl.innerHTML = html;
    var out = document.createDocumentFragment();
    Array.prototype.slice.call(tpl.content.childNodes).forEach(function (n) {
      out.appendChild(n.nodeName === "TEMPLATE" ? n.content : n);
    });
    return out;
  }

  // swap puts fresh, a piece, in for its id's element, keeping a tick and
  // focus.
  function swap(fresh) {
    var cur = $(fresh.id);
    if (!cur || cur.outerHTML === fresh.outerHTML) return;
    var box = cur.querySelector("input[type=checkbox]"), freshBox = fresh.querySelector("input[type=checkbox]");
    if (box && freshBox) freshBox.checked = box.checked;
    var focused = cur.contains(document.activeElement);
    var idx = focused ? Array.prototype.indexOf.call(cur.querySelectorAll("a, button, input"), document.activeElement) : -1;
    cur.replaceWith(fresh);
    if (focused) {
      var controls = fresh.querySelectorAll("a, button, input");
      var target = controls[idx] || controls[0];
      if (target) target.focus();
    }
  }

  // -- The live dot

  // The dot says "off" while an event stream is down; otherwise the grid's
  // own state (live, or stale while Proxmox doesn't answer), or live.
  var down = 0, shown = app && app.getAttribute("data-live");
  function liveState() {
    if (down > 0) return "off";
    var st = $("grid-status");
    return (st && st.getAttribute("data-live")) || shown || "live";
  }
  function showLive() {
    if (!app || !app.hasAttribute("data-live")) return;
    var state = liveState();
    app.setAttribute("data-live", state);
    var sum = document.querySelector("details.live > summary");
    var words = { live: "Live", stale: "Stale: no fresh data", off: "Disconnected, reconnecting" }[state];
    if (sum) {
      sum.setAttribute("aria-label", words);
      sum.title = words;
    }
    // Screen readers hear changes, not the state the page loaded with.
    var note = $("live-note");
    if (note && state !== shown) note.textContent = words;
    shown = state;
    updateBar();
  }

  // The pop-over says how long ago the stale grid was read.
  function ages() {
    each(document.querySelectorAll("[data-since]"), function (el) {
      var out = el.querySelector("[data-age]");
      if (!out) return;
      var s = Math.max(0, Math.round((Date.now() - Number(el.getAttribute("data-since"))) / 1000));
      var text = s < 120 ? s + " s ago" : Math.round(s / 60) + " min ago";
      if (out.textContent !== text) out.textContent = text;
      if (el.id === "grid-status" && liveState() === "stale") {
        var sum = document.querySelector("details.live > summary"), label = "Stale: no fresh data (" + text + ")";
        if (sum) { sum.setAttribute("aria-label", label); sum.title = label; }
      }
    });
  }
  ages();

  // A job's running times: data-tick is when each began (Unix ms), reckoned
  // on the server's clock, which data-now gave when the job was drawn.
  function pad(n) { return n < 10 ? "0" + n : String(n); }
  function ticks() {
    each(document.querySelectorAll("[data-now]"), function (box) {
      if (box.skew === undefined) box.skew = Number(box.getAttribute("data-now")) - Date.now();
      each(box.querySelectorAll("[data-tick]"), function (el) {
        var s = Math.max(0, Math.floor((Date.now() + box.skew - Number(el.getAttribute("data-tick"))) / 1000));
        var text = (s >= 3600 ? Math.floor(s / 3600) + ":" + pad(Math.floor(s / 60) % 60) : Math.floor(s / 60)) + ":" + pad(s % 60);
        if (el.textContent !== text) el.textContent = text;
      });
    });
  }
  setInterval(function () { ages(); ticks(); }, 1000);

  // -- Live updates

  // follow applies live's data-events stream; onChange runs after it
  // changes the page, and onEnd when the stream ends. It returns a function
  // that stops it.
  function follow(live, onChange, onEnd) {
    if (!window.EventSource) return function () {};
    var url = live.getAttribute("data-events");
    var source = null, failures = 0, errors = 0, pending = null, retry = null, stopped = false, isDown = false;

    function setDown(on, tries) {
      clearTimeout(pending);
      if (on !== isDown) {
        isDown = on;
        down += on ? 1 : -1;
        showLive();
      }
      each(document.querySelectorAll("[data-retry]"), function (el) {
        el.textContent = tries ? "Reconnecting · try " + tries : "Reconnecting";
      });
    }

    // The grid's shape: its classes, header and cell ids.
    function shape(table) {
      var ids = Array.prototype.map.call(table.querySelectorAll(".gc[id]"), function (c) { return c.id; });
      return table.className + "|" + table.querySelector(".hr").textContent + "|" + ids.join(",");
    }

    // "grid" is the whole live grid plus header pieces. If the shape is
    // unchanged, only differing cells are swapped, keeping focus, scroll and ticks.
    function grid(html) {
      var next = parse(html);
      Array.prototype.slice.call(next.children).forEach(function (el) {
        var cur = el.id && $(el.id);
        if (cur && !live.contains(cur)) {
          if (cur.outerHTML === el.outerHTML) el.remove();
          else swap(el);
        }
      });
      var oldTable = $("grid-table"), newTable = next.querySelector("#grid-table");
      if (!oldTable || !newTable || shape(oldTable) !== shape(newTable)) {
        var ticked = {};
        each(live.querySelectorAll("input[name=vms]:checked"), function (b) { ticked[b.value] = true; });
        each(next.querySelectorAll("input[name=vms]"), function (b) { b.checked = !!ticked[b.value]; });
        live.replaceChildren(next);
      } else {
        Array.prototype.slice.call(newTable.querySelectorAll(".gc[id]")).forEach(swap);
      }
      changed();
    }

    // "patch" is the pieces that changed.
    function patch(html) {
      Array.prototype.slice.call(parse(html).children).forEach(swap);
      changed();
    }

    // New lines count in the log's fold, and a failure unfolds it.
    function log(html) {
      var list = live.querySelector("#job-log"), sum = live.querySelector("#job-log-n");
      if (!list) return;
      Array.prototype.slice.call(parse(html).children).forEach(function (li) {
        if (li.id && $(li.id)) return;
        list.appendChild(li);
        if (li.querySelector(".err") && list.parentNode.matches("details")) list.parentNode.open = true;
      });
      var n = list.children.length;
      if (sum) sum.textContent = "Log · " + n + (n === 1 ? " line" : " lines");
    }

    function changed() {
      showLive();
      ages();
      ticks();
      if (onChange) onChange();
    }

    function end() {
      source.close();
      setDown(false);
      if (onEnd) onEnd();
    }

    function received(apply) {
      return function (e) {
        failures = 0;
        errors = 0;
        setDown(false);
        apply(e.data);
      };
    }

    function connect() {
      if (stopped) return;
      source = new EventSource(url);
      source.addEventListener("grid", received(grid));
      source.addEventListener("patch", received(patch));
      source.addEventListener("log", received(log));
      source.addEventListener("end", received(end));
      source.onopen = function () { errors = 0; setDown(false); };
      source.onerror = function () {
        // The browser reconnects by itself, unless refused (401); after
        // 3 errors in a row, check the login.
        if (source.readyState === EventSource.CONNECTING && ++errors < 3) {
          clearTimeout(pending);
          pending = setTimeout(function () { setDown(true, errors); }, 5000);
          return;
        }
        errors = 0;
        source.close();
        check();
      };
    }

    // HEAD the stream: on 401 or 403, reload for the login page; if
    // battleship can't be reached, retry with backoff.
    function check() {
      fetch(url, { method: "HEAD", credentials: "same-origin", redirect: "manual", cache: "no-store",
        headers: { Accept: "text/event-stream" } })
        .then(function (r) {
          if (stopped) return;
          if (r.type === "opaqueredirect" || r.status === 401 || r.status === 403) {
            location.reload();
          } else if (r.ok) {
            connect();
          } else {
            later();
          }
        }, later);
    }

    function later() {
      if (stopped) return;
      failures = Math.min(failures + 1, 5);
      setDown(true, failures);
      retry = setTimeout(connect, 2000 * Math.pow(2, failures - 1));
    }

    connect();
    return function () {
      stopped = true;
      clearTimeout(pending);
      clearTimeout(retry);
      if (source) source.close();
      setDown(false);
    };
  }

  // -- The grid: selecting VMs

  var form = $("grid-form");
  var bar = $("actionbar");
  var updateBar = function () {};

  if (form && bar) {
    var count = $("sel-count"), why = $("ab-why"), whyText = $("ab-why-text");
    var details = $("ab-details"), detailsSep = $("ab-details-sep");
    var last = null; // the last box clicked, where a shift-click range starts

    var boxes = function () { return Array.prototype.slice.call(form.querySelectorAll('input[name="vms"]')); };

    updateBar = function () {
      var all = boxes(), on = all.filter(function (b) { return b.checked; });
      var n = on.length;
      var state = app ? app.getAttribute("data-live") : "live";
      var locked = state === "stale" || state === "off";
      // A data-needs button is enabled only if some ticked VM holds one of its
      // privileges (not all in data-lacks). A hint: Proxmox and the preview decide.
      var allows = function (b, needs) {
        var lacks = (b.getAttribute("data-lacks") || "").split(" ");
        return needs.some(function (p) { return lacks.indexOf(p) < 0; });
      };
      var denied = false;
      each(bar.querySelectorAll(".ab-ops button"), function (b) {
        var needs = (b.getAttribute("data-needs") || "").split(" ").filter(Boolean);
        var no = n > 0 && needs.length > 0 && !on.some(function (x) { return allows(x, needs); });
        if (no) denied = true;
        b.disabled = n === 0 || locked || no;
        b.title = no ? "You don't have " + needs.join(" or ") + " on any selected VM" : "";
      });
      var text = locked ? "Waiting for live data" : denied ? "Some actions aren't permitted on these VMs" : "";
      if (count.textContent !== String(n)) count.textContent = n;
      why.hidden = !text;
      if (whyText.textContent !== text) whyText.textContent = text;
      var pick = last && last.checked && form.contains(last) ? last : on[on.length - 1];
      var vmHref = pick && "/vm/" + pick.dataset.team + "/" + pick.dataset.host;
      details.hidden = detailsSep.hidden = !vmHref;
      if (vmHref) {
        details.setAttribute("href", vmHref);
        details.title = n > 1 ? pick.value : "";
      }
      // Row and column headers show whether all of theirs are selected.
      var tot = {}, chk = {};
      all.forEach(function (b) {
        var t = "team:" + b.dataset.team, h = "host:" + b.dataset.host;
        tot[t] = (tot[t] || 0) + 1;
        tot[h] = (tot[h] || 0) + 1;
        if (b.checked) {
          chk[t] = (chk[t] || 0) + 1;
          chk[h] = (chk[h] || 0) + 1;
        }
      });
      each(form.querySelectorAll(".hb[data-select]"), function (h) {
        var k = h.getAttribute("data-select");
        h.setAttribute("aria-pressed", tot[k] > 0 && chk[k] === tot[k] ? "true" : "false");
      });
      var corner = form.querySelector('.all input[data-select="all"]');
      if (corner) {
        corner.checked = n > 0 && n === all.length;
        corner.indeterminate = n > 0 && n < all.length;
      }
    };

    // select ticks what a row, column or "all" names, or unticks it if
    // all ticked; "none" unticks all.
    var select = function (what) {
      var kind = what.split(":")[0], value = what.slice(kind.length + 1);
      var hit = boxes().filter(function (b) {
        return kind === "all" || kind === "none" || (kind === "team" && b.dataset.team === value) || (kind === "host" && b.dataset.host === value);
      });
      var on = what !== "none" && !hit.every(function (b) { return b.checked; });
      hit.forEach(function (b) { b.checked = on; });
      updateBar();
    };

    document.addEventListener("click", function (e) {
      var t = e.target;
      if (!t.closest || !form.contains(t) && !bar.contains(t)) return;
      if (t.matches('input[name="vms"]')) {
        if (e.shiftKey && last && last !== t && form.contains(last)) {
          var list = boxes(), a = list.indexOf(last), b = list.indexOf(t);
          list.slice(Math.min(a, b), Math.max(a, b) + 1).forEach(function (x) { x.checked = t.checked; });
        }
        last = t;
        updateBar();
        return;
      }
      var sel = t.closest("[data-select]");
      if (sel) {
        if (!sel.matches("input")) e.preventDefault(); // the corner box shows what select leaves
        select(sel.getAttribute("data-select"));
      }
    });
    form.addEventListener("change", updateBar);
    form.addEventListener("dblclick", function (e) {
      var cell = panel && e.target.closest && e.target.closest("label.cell");
      var box = cell && cell.querySelector('input[name="vms"]');
      if (!box) return;
      var href = "/vm/" + box.dataset.team + "/" + box.dataset.host;
      opener = box;
      load(href, { method: "GET" }).catch(function () { location.href = href; });
    });
  }

  // -- Forms

  function allChecked(boxes) {
    return boxes.length > 0 && Array.prototype.every.call(boxes, function (b) { return b.checked; });
  }

  function updateHostToggle(f) {
    if (!f) return;
    var btn = f.querySelector("[data-hosts-toggle]");
    if (!btn) return;
    var hosts = f.querySelectorAll('input[type=checkbox][name="hosts"]');
    btn.hidden = hosts.length === 0;
    var any = Array.prototype.some.call(hosts, function (b) { return b.checked; });
    if (hosts.length > 0) {
      hosts[0].required = !any;
      each(hosts, function (b, i) { b.setCustomValidity(i === 0 && !any ? "Tick at least one host." : ""); });
    }
    btn.textContent = allChecked(hosts) ? "Deselect all" : "Select all";
  }
  each(document.querySelectorAll("form"), updateHostToggle);

  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-hosts-toggle]");
    if (!btn || !btn.form) return;
    var hosts = btn.form.querySelectorAll('input[type=checkbox][name="hosts"]');
    var all = allChecked(hosts);
    each(hosts, function (b) { b.checked = !all; });
    updateHostToggle(btn.form);
  });

  document.addEventListener("change", function (e) {
    var t = e.target, f = t.form;
    if (f && t.name === "hosts") {
      updateHostToggle(f);
      return;
    }
    if (f && t.name === "pattern" && t.hasAttribute("data-hosts")) {
      var opts = f.querySelector("#host-opts");
      if (!opts) return;
      opts.replaceChildren();
      t.getAttribute("data-hosts").split(" ").filter(Boolean).forEach(function (h) {
        var label = document.createElement("label"), box = document.createElement("input"), span = document.createElement("span");
        label.className = "opt";
        box.type = "checkbox";
        box.name = "hosts";
        box.value = h;
        box.checked = true;
        span.textContent = h;
        label.append(box, span);
        opts.appendChild(label);
      });
      updateHostToggle(f);
    }
  });

  // The typed confirm: the button wakes once the required name or range is typed.
  function typedConfirm(root) {
    each(root.querySelectorAll("input[data-typed]"), function (input) {
      var button = input.form && input.form.querySelector("button[type=submit]:not([formnovalidate])");
      if (!button) return;
      var check = function () { button.disabled = input.value.trim() !== input.getAttribute("data-typed"); };
      input.addEventListener("input", check);
      check();
    });
  }
  typedConfirm(document);

  // -- The side panel

  var panel = $("panel");
  var stopPanel = null, opener = null;

  // Refresh the page behind the panel when a job in the panel finishes on a
  // page without its own event stream (Environments or Images).
  function refreshPage() {
    var page = document.querySelector("main > .page");
    if (!page || page.hasAttribute("data-events") || page.querySelector("[data-events]")) return;
    fetch(location.href, { credentials: "same-origin", cache: "no-store" }).then(function (r) {
      if (!r.ok || r.redirected) return;
      return r.text();
    }).then(function (html) {
      if (!html) return;
      var next = parse(html).querySelector("main > .page");
      var cur = document.querySelector("main > .page");
      if (!next || !cur) return;
      each(cur.querySelectorAll(".env[id] > details"), function (d) {
        var n = next.querySelector('[id="' + d.parentNode.id + '"] > details');
        if (n) n.open = d.open;
      });
      var href = opener && opener.getAttribute("href");
      cur.replaceWith(next);
      if (href) opener = next.querySelector('a[href="' + href + '"]');
      passwords(next);
      each(next.querySelectorAll("[data-fill]"), fill);
    }, function () {});
  }

  function showPanel(html) {
    if (stopPanel) stopPanel();
    stopPanel = null;
    var content = parse(html);
    var sheet = content.querySelector(".sheet");
    if (!sheet) throw new Error("not a sheet");
    var flash = content.querySelector(".flash");
    if (flash) sheet.querySelector(".pb, .ph").after(flash);
    panel.replaceChildren(sheet);
    var label = sheet.getAttribute("aria-labelledby");
    if (label) {
      panel.removeAttribute("aria-label");
      panel.setAttribute("aria-labelledby", label);
    }
    if (!panel.open) panel.showModal();
    var live = sheet.matches("[data-events]") ? sheet : sheet.querySelector("[data-events]");
    if (live) stopPanel = follow(live, null, refreshPage);
    else if (sheet.id === "job-live") refreshPage();
    ticks();
    typedConfirm(sheet);
    if (sheet.tagName === "FORM") updateHostToggle(sheet);
    each(sheet.querySelectorAll("form"), updateHostToggle);
    machineRows(sheet);
    passwords(sheet);
    each(sheet.querySelectorAll("[data-fill]"), fill);
    var typed = sheet.querySelector("input[data-typed], input[type=text], input[type=number]");
    var pb = sheet.querySelector(".pb");
    if (typed) typed.focus({ preventScroll: true });
    if (pb) pb.scrollTop = 0;
  }

  function closePanel() {
    if (panel && panel.open) panel.close();
  }

  // native submits the form without the script when the panel can't show the
  // answer. Safe even if the fetch got through: a confirm's nonce is
  // single-use, and previews and pickers change nothing.
  function native(f, action, sub) {
    if (sub && sub.name) {
      var h = document.createElement("input");
      h.type = "hidden";
      h.name = sub.name;
      h.value = sub.value;
      f.appendChild(h);
    }
    f.setAttribute("action", action);
    HTMLFormElement.prototype.submit.call(f);
  }

  function load(url, init) {
    init.credentials = "same-origin";
    init.headers = { "X-Battleship-Panel": "1" };
    return fetch(url, init).then(function (r) {
      if (r.headers.get("X-Battleship-Panel") !== "1") throw new Error("not a panel page");
      return r.text().then(showPanel);
    });
  }

  if (panel) {
    panel.addEventListener("close", function () {
      if (stopPanel && !panel.querySelector("#job-live[data-events]")) {
        stopPanel();
        stopPanel = null;
      }
      panel.replaceChildren();
      var back = opener && document.contains(opener) ? opener : bar && bar.querySelector("button:not(:disabled)");
      if (back) back.focus();
    });
    panel.addEventListener("click", function (e) {
      if (e.target === panel) closePanel();
    });
  }

  // The submitting button where e.submitter is missing (older Safari);
  // else Reset would post to the form's /power/preview.
  var clicked = null;
  document.addEventListener("click", function (e) {
    clicked = e.target.closest ? e.target.closest("button[type=submit]") : null;
  }, true);

  document.addEventListener("submit", function (e) {
    var f = e.target, sub = e.submitter || (clicked && (clicked.form === f) ? clicked : null);
    clicked = null;
    var which = f.id && /-pick$/.test(f.id) && $(f.id.replace(/-pick$/, "-which"));
    if (which) {
      if (!which.checkValidity()) {
        e.preventDefault();
        which.reportValidity();
        return;
      }
      var wt = which.querySelector('input[name="teams"]'), pt = f.querySelector('input[name="teams"]');
      if (wt && pt) pt.value = wt.value;
      var wv = which.querySelector('input[name="vms"]'), pv = f.querySelector('input[name="vms"]');
      if (wv && pv) pv.value = wv.value;
      var boxes = which.querySelectorAll('input[name="hosts"]');
      var wh = allChecked(boxes) ? "" : Array.prototype.map.call(which.querySelectorAll('input[name="hosts"]:checked'), function (b) { return b.value; }).join(",");
      var ph = f.querySelector('input[name="hosts"]');
      if (wh && !ph) {
        ph = document.createElement("input");
        ph.type = "hidden";
        ph.name = "hosts";
        f.appendChild(ph);
      }
      if (ph) {
        if (wh) ph.value = wh;
        else ph.remove();
      }
    }
    var hosts = f.querySelectorAll('input[type=checkbox][name="hosts"]');
    var omitHosts = allChecked(hosts);
    var method = ((sub && sub.getAttribute("formmethod")) || f.getAttribute("method") || "get").toLowerCase();
    if (!panel || !f.closest("[data-panel], #panel")) {
      if (omitHosts) {
        each(hosts, function (b) { b.disabled = true; });
        setTimeout(function () { each(hosts, function (b) { b.disabled = false; }); }, 0);
      }
      if (sub && method === "post") setTimeout(function () { sub.disabled = true; }, 0);
      return;
    }
    var action = (sub && sub.getAttribute("formaction")) || f.getAttribute("action") || location.pathname;
    var data = new URLSearchParams(new FormData(f));
    if (omitHosts) data.delete("hosts");
    if (sub && sub.name) data.append(sub.name, sub.value);
    e.preventDefault();
    if (f === form) opener = sub;
    if (sub) sub.disabled = true; // one click, one request
    var req = method === "post" ? load(action, { method: "POST", body: data }) : load(action + "?" + data.toString(), { method: "GET" });
    req.catch(function () {
      if (method === "post") native(f, action, sub);
      else location.href = action + "?" + data.toString();
    }).then(function () {
      if (sub && document.contains(sub)) sub.disabled = false;
      updateBar();
    });
  });

  document.addEventListener("click", function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || !e.target.closest) return;
    if (e.target.closest("summary .acts") && !e.target.closest("a")) {
      e.preventDefault();
      return;
    }
    var close = e.target.closest("#panel [data-close]");
    if (close) {
      e.preventDefault();
      closePanel();
      return;
    }
    var link = panel && e.target.closest("a[data-panel-link], a.cell");
    if (!link) return;
    e.preventDefault();
    if (!panel.contains(link)) opener = link;
    var menu = link.closest("header details");
    if (menu) menu.open = false;
    var href = link.getAttribute("href");
    load(href, { method: "GET" }).catch(function () { location.href = href; });
  });

  // -- Menus, the live dot and Escape

  document.addEventListener("click", function (e) {
    each(document.querySelectorAll("header details[open]"), function (d) {
      if (!d.contains(e.target)) d.open = false;
    });
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === " " && e.target.closest) {
      var hb = e.target.closest('a[role="button"][data-select]');
      if (hb && form && form.contains(hb)) {
        e.preventDefault();
        hb.click();
        return;
      }
    }
    if (e.key === "Enter" && form && bar && e.target.matches && e.target.matches('input[name="vms"]:checked')) {
      var act = bar.querySelector(".ab-ops button:not(:disabled)");
      if (act) {
        e.preventDefault();
        act.focus();
        return;
      }
    }
    if ((e.key === "ArrowUp" || e.key === "ArrowDown" || e.key === "ArrowLeft" || e.key === "ArrowRight") && e.target.closest) {
      var cell = e.target.closest("#grid-table .gc, #grid-table .gh");
      if (cell) {
        var row = cell.parentNode, rows = Array.prototype.slice.call(row.parentNode.children);
        var r = rows.indexOf(row), c = Array.prototype.indexOf.call(row.children, cell);
        var dr = e.key === "ArrowDown" ? 1 : e.key === "ArrowUp" ? -1 : 0;
        var dc = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
        for (var nr = r + dr, nc = c + dc; rows[nr] && rows[nr].children[nc]; nr += dr, nc += dc) {
          var next = rows[nr].children[nc].querySelector("input, a");
          if (next) {
            e.preventDefault();
            next.focus();
            break;
          }
        }
        return;
      }
    }
    if (e.key !== "Escape") return;
    var menu = document.querySelector("header details[open]");
    if (menu) {
      menu.open = false;
      menu.querySelector("summary").focus();
    } else if (form && !(panel && panel.open) && !e.target.matches("input[type=text], select, textarea")) {
      each(form.querySelectorAll('input[name="vms"]:checked'), function (b) { b.checked = false; });
      updateBar();
    }
  });

  // -- Environment machine rows

  // Add host appends an empty copy of the last host row; without this script
  // it is a submit that asks the server for more rows, and the remove buttons
  // stay hidden (a cleared row is ignored). The last host row stays.
  function machineRows(root) {
    each(root.querySelectorAll("[data-rows]"), function (box) {
      var rows = box.querySelectorAll("[data-row]");
      each(rows, function (r, i) {
        r.querySelector("[data-remove-row]").hidden = rows.length < 2;
        each(r.querySelectorAll("[aria-label]"), function (el) {
          el.setAttribute("aria-label", el.getAttribute("aria-label").replace(/\d+/, i + 1));
        });
      });
    });
  }
  machineRows(document);
  document.addEventListener("click", function (e) {
    var add = e.target.closest("[data-add-row]"), remove = e.target.closest("[data-remove-row]");
    var box = add ? add.form && add.form.querySelector("[data-rows]") : remove && remove.closest("[data-rows]");
    if (!box) return;
    e.preventDefault();
    if (remove) {
      var gone = remove.closest(".mrow"), next = gone.nextElementSibling || gone.previousElementSibling;
      gone.remove();
      (next && next.matches(".mrow") ? next.querySelector("input") : box.closest("fieldset").querySelector("[data-add-row]")).focus();
    } else {
      var rows = box.querySelectorAll("[data-row]"), row = rows[rows.length - 1].cloneNode(true);
      each(row.querySelectorAll("input"), function (i) { i.value = ""; });
      each(row.querySelectorAll("[data-stale], .ferr"), function (x) { x.remove(); });
      each(row.querySelectorAll("select"), function (s) { s.selectedIndex = 0; });
      each(row.querySelectorAll("[aria-invalid]"), function (x) { x.removeAttribute("aria-invalid"); x.removeAttribute("aria-describedby"); });
      box.appendChild(row);
      row.querySelector("input").focus();
    }
    machineRows(box.parentNode);
  });

  // -- Passwords

  // A password shows when its toggle opens; without this script that is
  // all. Where the clipboard can be written, its Copy button shows and
  // copies it, shown or not.
  function passwords(root) {
    if (!navigator.clipboard) return;
    each(root.querySelectorAll(".pw [data-copy]"), function (b) { b.hidden = false; });
  }
  passwords(document);
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest(".pw [data-copy]");
    if (!b) return;
    var say = b.querySelector("span");
    navigator.clipboard.writeText(b.closest(".pw").querySelector("code").textContent).then(function () {
      say.textContent = "Copied";
    }, function () {
      say.textContent = "Couldn't copy";
    }).then(function () {
      setTimeout(function () { say.textContent = "Copy"; }, 2000);
    });
  });

  // -- Live addresses

  function fill(line) {
    var say = line.querySelector("[data-fill-say]"), was = say ? say.textContent : "";
    if (say) say.textContent = "Reading…";
    line.setAttribute("aria-busy", "true");
    var undo = function () {
      if (say) say.textContent = was;
      line.removeAttribute("aria-busy");
    };
    fetch(line.getAttribute("data-fill"), { credentials: "same-origin", cache: "no-store" }).then(function (r) {
      if (!r.ok || r.headers.get("X-Battleship-Pieces") !== "1") throw new Error("no addresses");
      return r.text();
    }).then(function (html) {
      Array.prototype.slice.call(parse(html).children).forEach(swap);
    }).catch(undo);
  }
  each(document.querySelectorAll("main [data-fill]"), fill);

  // -- Start

  window.addEventListener("pageshow", function (e) {
    if (e.persisted) each(document.querySelectorAll("button[type=submit]:disabled"), function (b) { b.disabled = false; });
    if (form) updateBar();
  });
  var live = document.querySelector("main [data-events]");
  if (live) follow(live, form ? function () { updateBar(); } : null);
  showLive();
  ticks();
  if (form) updateBar();
})();
