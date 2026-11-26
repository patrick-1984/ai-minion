// Access > Minion control: filter the Workers and the matrix, "only Workers
// with master sessions", filters inside the pickers, and a lost answer: the
// page does not guess what was saved, it reloads from the Hub.
(function () {
  "use strict";
  var filter = document.getElementById("control-filter");
  var only = document.getElementById("control-only-masters");
  var status = document.getElementById("control-status");
  function apply() {
    var q = filter ? filter.value.trim().toLowerCase() : "";
    var onlyMasters = only && only.checked;
    document.querySelectorAll(".control-table tbody tr, .control-matrix tbody tr").forEach(function (tr) {
      var text = (tr.getAttribute("data-filter") || "").toLowerCase();
      var masters = tr.hasAttribute("data-masters") ? parseInt(tr.getAttribute("data-masters"), 10) : 1;
      tr.hidden = (q !== "" && text.indexOf(q) === -1) || (onlyMasters && masters === 0);
    });
  }
  if (filter) { filter.addEventListener("input", apply); }
  if (only) { only.addEventListener("change", apply); }
  document.addEventListener("input", function (e) {
    var f = e.target;
    if (!f.classList || !f.classList.contains("pick-filter")) { return; }
    var q = f.value.trim().toLowerCase();
    f.parentNode.querySelectorAll("li[data-filter]").forEach(function (li) {
      li.hidden = q !== "" && li.getAttribute("data-filter").toLowerCase().indexOf(q) === -1;
    });
  });
  document.addEventListener("htmx:afterSwap", apply);
  // "Who can control this session?": choosing a session shows the answer.
  document.querySelectorAll("form[data-autosubmit] select").forEach(function (sel) {
    sel.addEventListener("change", function () { if (sel.value) { sel.form.submit(); } });
  });
  var answer = document.querySelector(".check-result");
  if (answer) { answer.scrollIntoView({ block: "nearest" }); }
  function lost() {
    if (status) { status.textContent = "The answer was lost: reloading what the Hub has saved."; }
    setTimeout(function () { window.location.reload(); }, 1200);
  }
  // A click whose row could not be read back: the Hub's words are already in
  // the status region; the page reloads the rows and checkboxes from the Hub.
  document.addEventListener("control-reload", function () {
    setTimeout(function () { window.location.reload(); }, 1200);
  });
  document.addEventListener("htmx:sendError", lost);
  document.addEventListener("htmx:timeout", lost);
  document.addEventListener("htmx:responseError", function (e) {
    if (e.detail && e.detail.xhr && e.detail.xhr.status >= 500) { lost(); }
  });
})();
