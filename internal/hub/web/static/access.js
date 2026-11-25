// Access matrix: the Print button and the source filter (no inline script).
document.addEventListener("click", function (e) {
  if (e.target.closest("[data-access=print]")) window.print();
});
document.addEventListener("input", function (e) {
  if (e.target.id !== "access-filter") return;
  var q = e.target.value.trim().toLowerCase();
  document.querySelectorAll("#access-matrix [data-filter]").forEach(function (el) {
    el.hidden = q !== "" && el.getAttribute("data-filter").toLowerCase().indexOf(q) === -1;
  });
});
