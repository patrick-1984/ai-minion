// Share panel of the Worker page: All and None tick or untick every session,
// and any change of a session checkbox switches the mode to "Only the
// sessions I choose". Without JavaScript the form works as before.
(function () {
  "use strict";
  function selectMode(form) {
    var r = form.querySelector('input[name="mode"][value="select"]');
    if (r) { r.checked = true; }
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest("[data-share-bulk]") : null;
    if (!b) { return; }
    var form = b.closest("form");
    if (!form) { return; }
    var on = b.getAttribute("data-share-bulk") === "all";
    form.querySelectorAll('input[type="checkbox"][name="name"]').forEach(function (c) { c.checked = on; });
    var mode = form.querySelector('input[name="mode"][value="' + (on ? "select" : "none") + '"]');
    if (mode) { mode.checked = true; }
  });
  document.addEventListener("change", function (e) {
    var c = e.target;
    if (c && c.type === "checkbox" && c.name === "name" && c.form) { selectMode(c.form); }
  });
})();
