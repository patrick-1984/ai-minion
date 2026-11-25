// Access > Access (grants): the filter box and the two checkboxes, the Add
// dialog (the form moves into the dialog; without a dialog it shows in its
// details block), the session lists that follow the chosen Worker, the target
// choice that is never made for the user, focus after a row swap, and the
// rows that wait for a list reload.
(function () {
  "use strict";
  var filter = document.getElementById("grant-filter");
  var between = document.getElementById("grant-only-between");
  var off = document.getElementById("grant-only-off");
  function apply() {
    var q = filter ? filter.value.trim().toLowerCase() : "";
    var shown = 0, rows = document.querySelectorAll("#grant-rows tr[data-filter]");
    rows.forEach(function (tr) {
      var text = tr.getAttribute("data-filter").toLowerCase();
      tr.hidden = (q !== "" && text.indexOf(q) === -1) ||
        (between && between.checked && !tr.hasAttribute("data-between")) ||
        (off && off.checked && !tr.hasAttribute("data-off"));
      if (!tr.hidden) { shown++; }
    });
    var none = document.getElementById("grant-none");
    if (none) { none.hidden = rows.length === 0 || shown > 0; }
  }
  [filter, between, off].forEach(function (el) {
    if (el) { el.addEventListener(el.type === "checkbox" ? "change" : "input", apply); }
  });

  // Focus: every control of a row and of the Add form has a stable id. A swap
  // replaces the focused control, so the last focused id is kept and focus
  // goes back to it; when that control is gone, to the confirmation or the
  // status line of the same row.
  var lastFocus = "";
  document.addEventListener("focusin", function (e) {
    var t = e.target;
    if (t && t.id && t.closest && t.closest("#grant-panel, #grant-add-form")) { lastFocus = t.id; }
  });
  function usable(el) { return el && !el.disabled && !el.hidden && !el.closest("[inert]"); }
  function restoreFocus() {
    var active = document.activeElement;
    if (!lastFocus || (active && active !== document.body && document.contains(active) && !active.disabled)) { return; }
    var el = document.getElementById(lastFocus);
    if (!usable(el)) {
      var m = /^(grant-.+)-(level|save|approve|revoke|confirm|cancel)$/.exec(lastFocus);
      el = null;
      if (m) {
        ["-confirm", "-level", "-status", "-revoke"].some(function (suffix) {
          var c = document.getElementById(m[1] + suffix);
          if (usable(c)) { el = c; }
          return el !== null;
        });
      }
    }
    if (el) { el.focus(); }
  }

  var status = document.getElementById("grant-status");
  // While the list is replaced no row action can start: a write from a row
  // that is about to go would answer into nothing.
  document.addEventListener("htmx:beforeRequest", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-rows") { t.inert = true; t.setAttribute("aria-busy", "true"); }
  });
  document.addEventListener("htmx:afterRequest", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-rows" && document.contains(t)) { t.inert = false; t.removeAttribute("aria-busy"); }
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-rows" && status) { status.textContent = ""; }
    apply();
    wire();
    confirmShown();
    restoreFocus();
  });
  // A change was sent and the list could not be read back: the row may be
  // stale, so its controls stay off until the list is loaded again.
  document.body.addEventListener("grant-unconfirmed", function (e) {
    var id = e.detail && e.detail.id;
    var tr = id ? document.getElementById("grant-" + id) : null;
    if (!tr) { return; }
    tr.setAttribute("data-unconfirmed", "1");
    var cell = tr.querySelector(".grant-actions");
    if (cell) { cell.inert = true; }
  });

  // The Add dialog.
  var dialog = document.getElementById("grant-dialog");
  var fallback = document.getElementById("grant-add");
  var form = document.getElementById("grant-add-form");
  var modal = dialog && form && typeof dialog.showModal === "function";
  if (modal) {
    dialog.appendChild(form);
    form.querySelectorAll("[data-grant-cancel]").forEach(function (b) { b.hidden = false; });
  } else if (fallback) {
    fallback.hidden = false;
  }
  function wire() {
    if (!modal) { return; }
    document.querySelectorAll("[data-grant-add]").forEach(function (b) { b.hidden = false; });
  }
  wire();
  function clearResult() {
    var result = document.getElementById("grant-add-result");
    if (result) { result.textContent = ""; }
    confirmShown();
  }
  // With the confirmation on screen its own button is the only way to save.
  function confirmShown() {
    var submit = document.getElementById("grant-add-submit");
    if (submit) { submit.hidden = document.getElementById("grant-add-confirm") !== null; }
  }
  function close() {
    if (!form) { return; }
    form.reset();
    clearResult();
    syncSessions();
    if (modal && dialog.open) { dialog.close(); }
  }
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!t.closest) { return; }
    if (t.closest("[data-grant-reload]") && window.htmx) { window.htmx.trigger(document.body, "grants-changed"); return; }
    if (!modal) { return; }
    if (t.closest("[data-grant-add]")) { dialog.showModal(); }
    if (t.closest("[data-grant-cancel]")) { close(); }
  });
  // The Hub saved the new grant: the list reloads itself, the dialog closes.
  document.body.addEventListener("grants-changed", function () {
    if (!modal || dialog.open) { close(); }
  });

  // A session list shows the sessions of the chosen Worker only. The target
  // is never chosen for the user: a change of the target Worker clears the
  // session and the choice between one session and every shared session.
  function syncSessions(changed) {
    if (!form) { return; }
    form.querySelectorAll("select[data-grant-worker]").forEach(function (sel) {
      var name = sel.getAttribute("data-grant-worker");
      var list = form.elements[name];
      if (!list) { return; }
      if (changed === sel) {
        list.value = "";
        if (name === "target_session") {
          form.querySelectorAll("input[name=target_scope]").forEach(function (r) { r.checked = false; });
        }
      }
      Array.prototype.forEach.call(list.options, function (o) {
        var w = o.getAttribute("data-worker");
        if (w === null) { return; }
        var other = sel.value === "" || w !== sel.value;
        o.hidden = other;
        o.disabled = other;
        if (other && o.selected) { list.value = ""; }
      });
    });
    // The session list of the target is used only for "one session".
    var one = document.getElementById("grant-add-scope-one");
    var target = form.elements.target_session;
    if (one && target) {
      target.disabled = !one.checked;
      target.required = one.checked;
      if (!one.checked) { target.value = ""; }
    }
  }
  if (form) {
    form.addEventListener("change", function (e) {
      var t = e.target;
      var worker = t.hasAttribute && t.hasAttribute("data-grant-worker");
      if (worker || t.name === "target_scope") { syncSessions(worker ? t : null); }
      // A confirmation belongs to the values it was shown for.
      clearResult();
    });
    form.addEventListener("input", function (e) {
      if (e.target.name !== "note") { clearResult(); }
    });
    syncSessions();
    // ?source=<worker>|<session> preselected the source: the form opens.
    if (form.hasAttribute("data-grant-open")) {
      if (modal) {
        dialog.showModal();
      } else if (fallback) {
        fallback.open = true;
      }
    }
  }
}());
