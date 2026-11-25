// Access matrix (no inline script, no storage): the Print button, the live
// row filter, the toolbar that applies itself, "Updated, n changes" after a
// Refresh with a mark on the changed cells, and the warning tone of an "As
// of" that is older than five minutes. Everything also works without this
// file: the toolbar is a plain GET form.
(function () {
  var STALE_MS = 5 * 60 * 1000;
  var previous = null; // pair -> state of the last matrix seen, in memory only
  var typed = null; // what the user typed into the filter since the page loaded
  var hadFocus = "";

  function root() { return document.getElementById("access-matrix"); }

  function applyFilter(q) {
    var m = root();
    if (!m) return;
    q = q.trim().toLowerCase();
    m.querySelectorAll("[data-filter]").forEach(function (el) {
      el.hidden = q !== "" && el.getAttribute("data-filter").toLowerCase().indexOf(q) === -1;
    });
  }

  // snapshot reads every pair (table cells and the long list name the same
  // pair by the same key) and, apart, the aggregates of collapsed groups.
  function snapshot() {
    var s = { pairs: {}, aggs: {} }, m = root();
    if (!m) return s;
    m.querySelectorAll("[data-pair]").forEach(function (el) { s.pairs[el.getAttribute("data-pair")] = el.getAttribute("data-state"); });
    m.querySelectorAll("[data-agg]").forEach(function (el) { s.aggs[el.getAttribute("data-agg")] = el.getAttribute("data-state"); });
    return s;
  }

  function mark(el) {
    el.classList.add("access-changed");
    var note = document.createElement("span");
    note.className = "sr-only";
    note.textContent = " changed";
    el.appendChild(note);
  }

  // compare counts the pairs whose state differs from the previous matrix (a
  // pair that is not listed counts as "none") and marks their cells.
  function compare(before, now) {
    var changed = {}, n = 0, m = root();
    Object.keys(before.pairs).concat(Object.keys(now.pairs)).forEach(function (k) {
      if (changed[k] === undefined) changed[k] = (before.pairs[k] || "none") !== (now.pairs[k] || "none");
    });
    Object.keys(changed).forEach(function (k) { if (changed[k]) n++; });
    m.querySelectorAll("[data-pair]").forEach(function (el) { if (changed[el.getAttribute("data-pair")]) mark(el); });
    m.querySelectorAll("[data-agg]").forEach(function (el) {
      var k = el.getAttribute("data-agg");
      if (before.aggs[k] !== undefined && before.aggs[k] !== now.aggs[k]) mark(el);
    });
    return n;
  }

  function checkAge() {
    var m = root(), asof = m && m.querySelector(".matrix-asof"), t = asof && asof.querySelector("time[datetime]");
    if (!t) return;
    var at = Date.parse(t.getAttribute("datetime"));
    var stale = !isNaN(at) && Date.now() - at > STALE_MS;
    asof.classList.toggle("access-stale", stale);
    var note = asof.querySelector(".access-stale-note");
    if (note) note.hidden = !stale;
  }

  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-access=print]")) window.print();
  });
  document.addEventListener("input", function (e) {
    if (e.target.id !== "access-filter") return;
    typed = e.target.value;
    applyFilter(typed);
  });
  // The select, the checkbox and the direction apply themselves.
  document.addEventListener("change", function (e) {
    var form = e.target.closest && e.target.closest("#access-form");
    if (!form || e.target.id === "access-filter") return;
    if (form.requestSubmit) form.requestSubmit(); else form.submit();
  });
  document.addEventListener("htmx:beforeRequest", function (e) {
    if (!e.detail || !e.detail.elt || e.detail.elt.id !== "access-refresh") return;
    hadFocus = document.activeElement && document.activeElement.id || "";
    if (previous === null) previous = snapshot();
  });
  document.addEventListener("htmx:afterSwap", function () {
    var m = root();
    if (!m || previous === null) return;
    var filter = document.getElementById("access-filter");
    if (filter && typed !== null) { filter.value = typed; applyFilter(typed); }
    var now = snapshot(), n = compare(previous, now);
    previous = now;
    var status = document.getElementById("access-status");
    if (status) status.textContent = "Updated, " + n + (n === 1 ? " change" : " changes");
    var focus = hadFocus && document.getElementById(hadFocus);
    if (focus && document.activeElement !== focus) focus.focus();
    checkAge();
  });

  previous = snapshot();
  checkAge();
  setInterval(checkAge, 15000);
})();
