// Access > Grants: the filter box and the two checkboxes, the Add dialog (the
// form moves into the dialog; without a dialog it shows in its details
// block), the From and To pickers of the chosen scope, the three boxes that
// require Control, Install apps for Android targets only, the saved rows
// marked after a save, focus after a row swap, and the rows that wait for a
// list reload.
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
  // The list (#grant-body) is in the page with rows and without: a reload
  // after the last revoke keeps its trigger for the next one.
  // While the list is replaced no row action can start: a write from a row
  // that is about to go would answer into nothing.
  document.addEventListener("htmx:beforeRequest", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-body") { t.inert = true; t.setAttribute("aria-busy", "true"); }
  });
  document.addEventListener("htmx:afterRequest", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-body" && document.contains(t)) { t.inert = false; t.removeAttribute("aria-busy"); }
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-body" && status) { status.textContent = ""; }
    apply();
    wire();
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
  }
  function close() {
    if (!form) { return; }
    form.reset();
    clearResult();
    syncForm();
    if (window.htmx) { window.htmx.trigger(form, "change"); }
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
  // The Hub saved the new grants: the list reloads itself and marks them,
  // the dialog closes.
  var saved = null;
  document.body.addEventListener("grants-changed", function (e) {
    if (e.detail && e.detail.ids) { saved = e.detail; }
    if (!modal || dialog.open) { close(); }
  });
  function markSaved() {
    if (!saved) { return; }
    var first = null;
    (saved.ids || []).forEach(function (id) {
      var tr = document.getElementById("grant-" + id);
      if (tr) { tr.setAttribute("data-saved", "1"); first = first || tr; }
    });
    if (status && saved.text) { status.textContent = saved.text; }
    if (first && first.scrollIntoView) { first.scrollIntoView({ block: "nearest" }); }
    saved = null;
  }
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === "grant-body") { markSaved(); }
  });

  // The three boxes beneath the level: Send files and Install apps require
  // Control, so below it they are off and say so.
  function syncBoxes(scope) {
    var level = scope.querySelector("select[name=level]") || scope.querySelector("input[name=level]:checked");
    var control = level && level.value === "control";
    scope.querySelectorAll("[data-needs-control]").forEach(function (box) {
      box.disabled = !control;
      if (!control) { box.checked = false; }
      var note = box.parentNode.querySelector(".grant-needs-control");
      if (note) { note.hidden = control; }
    });
  }
  document.addEventListener("change", function (e) {
    var f = e.target.closest && e.target.closest("form");
    if (f && f !== form && e.target.name === "level" && f.querySelector("[data-grant-boxes]")) { syncBoxes(f); }
  });

  // From and To: the picker of the chosen scope is the only one sent. Install
  // apps is offered only while every chosen target is on an Android Worker,
  // and names the device.
  function syncForm() {
    if (!form) { return; }
    form.querySelectorAll("[data-grant-for]").forEach(function (el) {
      var radio = document.getElementById(el.getAttribute("data-grant-for"));
      var on = !!(radio && radio.checked);
      el.hidden = !on;
      el.querySelectorAll("select").forEach(function (s) { s.disabled = !on; });
    });
    var devices = [], android = true, any = false;
    var scope = form.querySelector("input[name=target_scope]:checked");
    var picked = [];
    if (scope && scope.value === "sessions") {
      picked = Array.prototype.filter.call(form.elements.target_sessions.options, function (o) { return o.selected; });
    } else if (scope && scope.value === "worker") {
      var w = form.elements.target_worker_id;
      if (w.selectedIndex > 0) { picked = [w.options[w.selectedIndex]]; }
    }
    picked.forEach(function (o) {
      any = true;
      var d = o.getAttribute("data-android");
      if (d === null) { android = false; return; }
      if (devices.indexOf(d) === -1) { devices.push(d); }
    });
    var apps = form.querySelector("[data-grant-apps]");
    if (apps) {
      var show = any && android;
      apps.hidden = !show;
      var box = apps.querySelector("input");
      if (!show && box) { box.checked = false; box.disabled = true; }
      var name = apps.querySelector("[data-grant-device]");
      if (name) { name.textContent = devices.join(", "); }
    }
    syncBoxes(form);
    var appsBox = apps && apps.querySelector("input");
    if (apps && apps.hidden && appsBox) { appsBox.disabled = true; }
  }
  if (form) {
    form.addEventListener("change", function () {
      syncForm();
      clearResult();
    });
    syncForm();
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
