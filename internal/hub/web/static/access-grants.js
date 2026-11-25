// Access > Access (grants): the filter box and the two checkboxes, the Add
// dialog (the form lives in a details block without scripts and moves into
// the dialog with them), and the session lists that follow the chosen Worker.
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
  document.addEventListener("htmx:afterSwap", function () { apply(); wire(); });

  // The Add dialog.
  var dialog = document.getElementById("grant-dialog");
  var fallback = document.getElementById("grant-add");
  var form = document.getElementById("grant-add-form");
  var modal = dialog && form && typeof dialog.showModal === "function";
  if (modal) {
    dialog.appendChild(form);
    fallback.hidden = true;
    form.querySelectorAll("[data-grant-cancel]").forEach(function (b) { b.hidden = false; });
  }
  function wire() {
    if (!modal) { return; }
    document.querySelectorAll("[data-grant-add]").forEach(function (b) { b.hidden = false; });
  }
  wire();
  function close() {
    if (!modal) { return; }
    form.reset();
    var result = document.getElementById("grant-add-result");
    if (result) { result.textContent = ""; }
    syncSessions();
    if (dialog.open) { dialog.close(); }
  }
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!t.closest || !modal) { return; }
    if (t.closest("[data-grant-add]")) { dialog.showModal(); }
    if (t.closest("[data-grant-cancel]")) { close(); }
  });
  // The Hub saved the new grant: the list reloads itself, the dialog closes.
  document.body.addEventListener("grants-changed", close);

  // A session list shows the sessions of the chosen Worker only.
  function syncSessions() {
    if (!form) { return; }
    form.querySelectorAll("select[data-grant-worker]").forEach(function (sel) {
      var list = form.elements[sel.getAttribute("data-grant-worker")];
      if (!list) { return; }
      Array.prototype.forEach.call(list.options, function (o) {
        var w = o.getAttribute("data-worker");
        if (w === null) { return; }
        var other = sel.value === "" || w !== sel.value;
        o.hidden = other;
        o.disabled = other;
        if (other && o.selected) { list.value = ""; }
      });
    });
  }
  if (form) {
    form.addEventListener("change", function (e) {
      if (e.target.hasAttribute && e.target.hasAttribute("data-grant-worker")) { syncSessions(); }
    });
    syncSessions();
  }
}());
