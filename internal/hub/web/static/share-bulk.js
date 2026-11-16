// Share panel of the Worker page: All and None tick or untick every session,
// and any change of a session checkbox switches the mode to "Only the
// sessions I choose". Without JavaScript the form works as before.
(function () {
  "use strict";
  // With at least one session ticked the mode is "select"; with none it is
  // "none" (select with no names is refused).
  function syncMode(form) {
    var any = form.querySelector('input[type="checkbox"][name="name"]:checked');
    var r = form.querySelector('input[name="mode"][value="' + (any ? "select" : "none") + '"]');
    if (r) { r.checked = true; }
  }
  function reveal(root) {
    (root || document).querySelectorAll("[data-share-bulk-wrap]").forEach(function (w) { w.hidden = false; });
  }
  reveal();
  document.addEventListener("htmx:afterSwap", function (e) { reveal(e.target); });
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest("[data-share-bulk]") : null;
    if (!b) { return; }
    var form = b.closest("form");
    if (!form) { return; }
    var on = b.getAttribute("data-share-bulk") === "all";
    form.querySelectorAll('input[type="checkbox"][name="name"]').forEach(function (c) { c.checked = on; });
    syncMode(form);
  });
  document.addEventListener("change", function (e) {
    var c = e.target;
    if (c && c.type === "checkbox" && c.name === "name" && c.form) { syncMode(c.form); }
  });
})();
