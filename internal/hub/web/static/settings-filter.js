// Filters the rows of the Hub settings page by key, environment variable and
// description, and hides a section whose rows are all filtered out.
(function () {
  "use strict";
  var input = document.getElementById("settings-filter");
  var status = document.getElementById("settings-filter-status");
  if (!input) { return; }
  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    var shown = 0;
    document.querySelectorAll("#settings-sections section").forEach(function (sec) {
      var visible = 0;
      sec.querySelectorAll("li[data-filter]").forEach(function (li) {
        var match = q === "" || li.getAttribute("data-filter").toLowerCase().indexOf(q) !== -1;
        li.hidden = !match;
        if (match) { visible++; }
      });
      sec.hidden = visible === 0;
      shown += visible;
    });
    if (status) { status.textContent = shown === 0 ? "No setting matches the filter." : (q === "" ? "" : shown + (shown === 1 ? " setting matches." : " settings match.")); }
  });
})();
