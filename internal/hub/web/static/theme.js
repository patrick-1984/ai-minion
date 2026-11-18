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

  var scheme = document.querySelector('meta[name="color-scheme"]');
  var initialTheme = root.getAttribute('data-theme');
  if (scheme && initialTheme) { scheme.content = initialTheme; }

  function markThemeChoice() {
    var choice = root.getAttribute('data-theme') || 'system';
    var buttons = document.querySelectorAll('[data-theme-choice]');
    for (var index = 0; index < buttons.length; index++) {
      var button = buttons[index];
      var pressed = button.getAttribute('data-theme-choice') === choice ? 'true' : 'false';
      if (button.getAttribute('aria-pressed') !== pressed) { button.setAttribute('aria-pressed', pressed); }
    }
  }

  if (typeof MutationObserver === 'function') {
    var controls = new MutationObserver(markThemeChoice);
    controls.observe(root, { childList: true, subtree: true });
    document.addEventListener('readystatechange', function () {
      controls.disconnect();
      markThemeChoice();
    }, { once: true });
  }

  // A theme chosen in another document of this origin (the page that hosts a
  // session inspector frame) reaches this one through the storage event.
  window.addEventListener('storage', function (ev) { if (!ev || ev.key === 'minion-theme' || ev.key === null) { apply(); } });
})();
