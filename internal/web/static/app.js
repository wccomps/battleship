// battleship's one script. Every page works without it; it adds live
// updates (data-events) and the header's live dot, selecting rows, columns
// and ranges on the grid with its action bar, and the side panel, where
// previews, forms, VMs and jobs show (the server renders those pages
// without the layout for X-Battleship-Panel).
(function () {
  "use strict";
  if (!window.fetch || !window.URLSearchParams) return;

  function each(list, fn) { Array.prototype.forEach.call(list, fn); }
  function $(id) { return document.getElementById(id); }

  var app = $("app");

  // parse turns server markup into nodes: events (grid, job) and panel
  // pages. Each event piece comes in its own <template> (piece, sse.go),
  // whose content is parsed in the context of its first tag. Markup
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

  // -- The live dot

  // The dot says "off" while an event stream is down; otherwise the grid's
  // own state (live, or stale while Proxmox doesn't answer), or live.
  var down = 0, shown = app && app.getAttribute("data-live");
  function liveState() {
    if (down > 0) return "off";
    var st = $("grid-status");
    return (st && st.getAttribute("data-live")) || "live";
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
    });
  }
  setInterval(ages, 1000);
  ages();

  // -- Live updates

  // follow applies live's data-events stream; onChange runs after it
  // changes the page. It returns a function that stops it.
  function follow(live, onChange) {
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

    // Swap fresh in for its id's element, keeping a tick and focus.
    function swap(fresh) {
      var cur = $(fresh.id);
      if (!cur || cur.outerHTML === fresh.outerHTML) return;
      var box = cur.querySelector("input[type=checkbox]"), freshBox = fresh.querySelector("input[type=checkbox]");
      if (box && freshBox) freshBox.checked = box.checked;
      var focused = cur.contains(document.activeElement), onBox = box && document.activeElement === box;
      cur.replaceWith(fresh);
      var target = onBox ? freshBox : fresh.querySelector("a, button, input");
      if (focused && target) target.focus();
    }

    // The grid's shape: its classes, header and cell ids.
    function shape(table) {
      var ids = Array.prototype.map.call(table.querySelectorAll(".gc[id]"), function (c) { return c.id; });
      return table.className + "|" + table.querySelector(".hr").textContent + "|" + ids.join(",");
    }

    // "grid" is the whole live grid, then the header's pieces; for a grid
    // of the same shape, only cells that differ are swapped, so focus,
    // scrolling and ticks stay.
    function grid(html) {
      var next = parse(html);
      Array.prototype.slice.call(next.children).forEach(function (el) {
        var cur = el.id && $(el.id);
        if (cur && !live.contains(cur)) swap(el);
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

    // "patch" is the pieces that changed; "log", new log lines.
    function patch(html) {
      Array.prototype.slice.call(parse(html).children).forEach(swap);
      changed();
    }

    function log(html) {
      var list = live.querySelector("#job-log");
      if (!list) return;
      Array.prototype.slice.call(parse(html).children).forEach(function (li) {
        if (!li.id || !$(li.id)) list.appendChild(li);
      });
    }

    function changed() {
      showLive();
      ages();
      if (onChange) onChange();
    }

    function end() {
      source.close();
      setDown(false);
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
    var matches = function (what, b) {
      var kind = what.split(":")[0], value = what.slice(kind.length + 1);
      return kind === "all" || kind === "none" || (kind === "team" && b.dataset.team === value) || (kind === "host" && b.dataset.host === value);
    };

    updateBar = function () {
      var all = boxes(), on = all.filter(function (b) { return b.checked; });
      var n = on.length;
      var state = app ? app.getAttribute("data-live") : "live";
      var locked = state === "stale" || state === "off";
      // A button marked data-needs is offered only if some ticked VM
      // allows one of its privileges: its box doesn't list them all in
      // data-lacks. Proxmox decides; the preview says exactly.
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
      var one = n === 1 && "/vm/" + on[0].dataset.team + "/" + on[0].dataset.host;
      details.hidden = detailsSep.hidden = !one;
      if (one) details.setAttribute("href", one);
      // Row and column headers show whether all of theirs are selected.
      each(form.querySelectorAll(".hb[data-select]"), function (h) {
        var mine = all.filter(function (b) { return matches(h.getAttribute("data-select"), b); });
        var full = mine.length > 0 && mine.every(function (b) { return b.checked; });
        h.setAttribute("aria-pressed", full ? "true" : "false");
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
      var hit = boxes().filter(function (b) { return matches(what, b); });
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
  }

  // -- Forms

  // A deploy builds all of its set's hosts unless told otherwise: when
  // every host is ticked, none is sent. Choosing another set shows its
  // hosts.
  function deployHosts(f) {
    var hosts = f.querySelectorAll('input[name="hosts"]');
    var all = hosts.length > 0 && Array.prototype.every.call(hosts, function (b) { return b.checked; });
    each(hosts, function (b) { b.disabled = all; });
    return function () { each(hosts, function (b) { b.disabled = false; }); };
  }

  document.addEventListener("change", function (e) {
    var t = e.target, f = t.form;
    if (f && f.hasAttribute("data-deploy") && t.name === "pattern" && t.hasAttribute("data-hosts")) {
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
    }
  });

  // The typed confirm: the button wakes once the range is typed.
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
    if (live) stopPanel = follow(live);
    typedConfirm(sheet);
    var typed = sheet.querySelector("input[data-typed], input[type=text]");
    var pb = sheet.querySelector(".pb");
    if (pb) pb.scrollTop = 0;
    if (typed) typed.focus();
  }

  function closePanel() {
    if (panel && panel.open) panel.close();
  }

  // native submits the form as if there were no script, when the panel
  // couldn't show the answer. Safe even if the fetch got through: a
  // confirm's preview nonce is single-use, so a second post lands on the
  // job the first made; previews and pickers change nothing.
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
      if (stopPanel) stopPanel();
      stopPanel = null;
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

  // Posts from the grid and the panel show their answer in the panel, as
  // do the panel's own GET forms.
  document.addEventListener("submit", function (e) {
    var f = e.target, sub = e.submitter || (clicked && (clicked.form === f) ? clicked : null);
    clicked = null;
    var restore = f.hasAttribute("data-deploy") ? deployHosts(f) : null;
    if (!panel || !f.closest("[data-panel], #panel")) return;
    var method = ((sub && sub.getAttribute("formmethod")) || f.getAttribute("method") || "get").toLowerCase();
    var action = (sub && sub.getAttribute("formaction")) || f.getAttribute("action") || location.pathname;
    var data = new URLSearchParams(new FormData(f));
    if (restore) restore();
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
    var close = e.target.closest("#panel [data-close]");
    if (close) {
      e.preventDefault();
      closePanel();
      return;
    }
    var link = panel && e.target.closest("a[data-panel-link]");
    if (!link) return;
    e.preventDefault();
    if (!panel.contains(link)) opener = link;
    var menu = link.closest("details");
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

  // -- Start

  var live = document.querySelector("main [data-events]");
  if (live) follow(live, form ? function () { updateBar(); } : null);
  showLive();
  if (form) updateBar();
})();
