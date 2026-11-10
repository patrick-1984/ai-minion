// theme.js runs in the head, before the first paint: it applies the operator's
// theme choice (System, Light or Dark) and marks the document as scripted, so
// that the stylesheet may use the menu and pane enhancements. The choice is a
// display preference, so it may live in localStorage; nothing else does.
(function () {
  'use strict';
  var root = document.documentElement;
  root.classList.add('js');
  function apply() {
    try {
      var t = window.localStorage.getItem('minion-theme');
      if (t === 'light' || t === 'dark') { root.setAttribute('data-theme', t); } else { root.removeAttribute('data-theme'); }
    } catch (e) { /* storage is blocked: the system theme stays */ }
  }
  apply();
  // A theme chosen in another document of this origin (the page that hosts a
  // session inspector frame) reaches this one through the storage event.
  window.addEventListener('storage', function (ev) { if (!ev || ev.key === 'minion-theme' || ev.key === null) { apply(); } });
})();
