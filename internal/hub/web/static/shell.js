// shell.js is the small script of the page shell (every page).
//
//  - Theme: every [data-theme-select] offers System, Light and Dark. The choice
//    is stored as "minion-theme" in localStorage (a display preference); System
//    removes it. theme.js applies it before the first paint.
//  - Menu: below 960px the left rail is a Menu overlay. The Menu button keeps
//    aria-expanded, opening moves the focus to the first link, Tab stays inside
//    the open menu, Escape or the scrim or "Close menu" closes it and returns the
//    focus to the button. Escape is never forwarded to anything else.
//  - Heading focus: a page reached from another page of this Hub puts the focus
//    on its heading, so that a screen reader announces the new location.
//  - Drafts: a field marked data-draft that holds text is an unsent draft. Signing
//    out asks first, and leaving the page asks the browser to confirm.
(function () {
  'use strict';

  var root = document.documentElement;
  var STORE = 'minion-theme';

  // ---- theme ----------------------------------------------------------------
  function storedTheme() {
    try { return window.localStorage.getItem(STORE) || 'system'; } catch (e) { return 'system'; }
  }
  function applyTheme(v) {
    if (v === 'light' || v === 'dark') { root.setAttribute('data-theme', v); } else { root.removeAttribute('data-theme'); }
  }
  var selects = document.querySelectorAll('[data-theme-select]');
  for (var i = 0; i < selects.length; i++) {
    (function (sel) {
      sel.value = storedTheme();
      sel.addEventListener('change', function () {
        var v = sel.value === 'light' || sel.value === 'dark' ? sel.value : 'system';
        try {
          if (v === 'system') { window.localStorage.removeItem(STORE); } else { window.localStorage.setItem(STORE, v); }
        } catch (e) { /* the choice then lasts for this page only */ }
        applyTheme(v);
        for (var j = 0; j < selects.length; j++) { selects[j].value = v; }
      });
    })(selects[i]);
  }

  // ---- menu -----------------------------------------------------------------
  var button = document.getElementById('menu-button');
  var rail = document.getElementById('site-rail');
  var scrim = document.getElementById('scrim');
  var closeBtn = document.getElementById('menu-close');
  var opener = null;

  function isOpen() { return !!rail && rail.classList.contains('open'); }

  function focusables() {
    var list = rail.querySelectorAll('a[href], button:not([disabled]), select, input:not([disabled])');
    var out = [];
    for (var k = 0; k < list.length; k++) { out.push(list[k]); }
    return out;
  }

  function openMenu() {
    if (!rail || !button) { return; }
    opener = document.activeElement || button;
    rail.classList.add('open');
    if (scrim) { scrim.classList.add('open'); }
    button.setAttribute('aria-expanded', 'true');
    var f = rail.querySelector('.nav-list a[aria-current="page"]') || rail.querySelector('.nav-list a');
    if (f) { f.focus(); }
  }

  function closeMenu(restore) {
    if (!rail || !button) { return; }
    rail.classList.remove('open');
    if (scrim) { scrim.classList.remove('open'); }
    button.setAttribute('aria-expanded', 'false');
    if (restore !== false) { (opener && opener.focus ? opener : button).focus(); }
    opener = null;
  }

  if (button && rail) {
    button.addEventListener('click', function () { if (isOpen()) { closeMenu(); } else { openMenu(); } });
    if (closeBtn) { closeBtn.addEventListener('click', function () { closeMenu(); }); }
    if (scrim) { scrim.addEventListener('click', function () { closeMenu(); }); }
    document.addEventListener('keydown', function (ev) {
      if (!isOpen()) { return; }
      if (ev.key === 'Escape' && !document.querySelector('dialog[open]')) {
        ev.preventDefault();
        closeMenu();
        return;
      }
      if (ev.key === 'Tab') {
        var list = focusables();
        if (!list.length) { return; }
        var first = list[0];
        var last = list[list.length - 1];
        if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
        else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
        else if (!rail.contains(document.activeElement)) { ev.preventDefault(); first.focus(); }
      }
    });
    if (typeof window.matchMedia === 'function') {
      var wide = window.matchMedia('(min-width: 960px)');
      var onWide = function (e) { if (e.matches && isOpen()) { closeMenu(false); } };
      if (typeof wide.addEventListener === 'function') { wide.addEventListener('change', onWide); }
    }
  }

  // ---- heading focus --------------------------------------------------------
  (function focusHeading() {
    var h = document.querySelector('#content h1');
    if (!h) { return; }
    // The compact view in the session inspector must not take the focus when its frame loads.
    if (document.body && document.body.classList && document.body.classList.contains('embed')) { return; }
    var a = document.activeElement;
    if (a && a !== document.body && a !== document.documentElement) { return; }
    if (document.querySelector('[autofocus]')) { return; }
    var ref = document.referrer || '';
    var here = window.location && window.location.origin ? window.location.origin : null;
    if (!here || ref.indexOf(here + '/') !== 0) { return; }
    h.setAttribute('tabindex', '-1');
    h.focus({ preventScroll: true });
  })();

  // ---- unsent drafts --------------------------------------------------------
  var leaving = false;

  // hasDraft: text that was typed and not sent, or a delivery that is still unknown
  // (terminal-input.js marks each with data-unresolved until it is dismissed), or a
  // frame of the session inspector that holds either (data-frame-dirty).
  function hasDraft() {
    if (document.querySelectorAll('[data-unresolved]').length > 0) { return true; }
    // The session inspector marks the page while a frame it keeps holds unsent input,
    // a request on its way or an unknown delivery.
    if (document.querySelectorAll('[data-frame-dirty]').length > 0) { return true; }
    var list = document.querySelectorAll('[data-draft]');
    for (var k = 0; k < list.length; k++) {
      if (typeof list[k].value === 'string' && list[k].value.trim() !== '') { return true; }
    }
    return false;
  }

  document.addEventListener('submit', function (ev) {
    var f = ev.target;
    if (!f || !f.getAttribute || f.getAttribute('data-json-action') !== '/auth/logout') { return; }
    if (!hasDraft()) { return; }
    if (!window.confirm('You have text that was not sent, or a delivery that is still unknown. Sign out and discard it?')) {
      ev.preventDefault();
      ev.stopImmediatePropagation();
      return;
    }
    leaving = true;
  }, true);

  window.addEventListener('beforeunload', function (ev) {
    if (leaving || !hasDraft()) { return; }
    ev.preventDefault();
    ev.returnValue = '';
  });
})();
