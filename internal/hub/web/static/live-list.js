// live-list.js keeps a live list usable while the Hub updates it. A list page
// (control center, workers, sessions) receives the whole result fragment on
// every change. This script makes that update invisible to the person using the
// page:
//  - the focused row control is focused again after the swap, without scrolling.
//    It is matched by the row's data-key and the control's own identity
//    (data-control, else its id), never by its position: a row can gain or lose a
//    control (the session-count link of a worker row) and the neighbour that moves
//    into the old position is a different action. When the control is gone the
//    focus goes to the row itself (a non-mutating target, tabindex -1), and when the
//    row is gone to the page heading; no other control is ever substituted;
//  - the scroll position of the page and of every scrollable table container is
//    restored; a disclosure (details[data-keep-open], the sessions of a worker row)
//    that was open is open again;
//  - the filter form lives outside the swapped region and is never touched;
//  - a polite status line announces only the short summary ("3 of 7 workers"),
//    and only when it changed; the list itself is not a live region.
(function () {
  'use strict';

  var saved = null;
  var lastSummary = {};

  function isList(el) {
    return !!el && typeof el.hasAttribute === 'function' && el.hasAttribute('data-live-list');
  }

  var CONTROLS = 'a, button, input, select, textarea, summary';

  // identity is what stays the same for one control across updates.
  function identity(c) {
    if (!c || typeof c.getAttribute !== 'function') { return null; }
    return c.getAttribute('data-control') || null;
  }

  function focusKey(list, active) {
    if (!active || !list.contains || !list.contains(active)) { return null; }
    var row = active.closest ? active.closest('[data-key]') : null;
    return { key: row ? row.getAttribute('data-key') : null, control: identity(active), id: active.id || null };
  }

  function focusIt(el) {
    if (el && typeof el.focus === 'function') { el.focus({ preventScroll: true }); return true; }
    return false;
  }

  // makeFocusable lets a non-interactive element take the script's focus.
  function makeFocusable(el) {
    if (el && typeof el.setAttribute === 'function') { el.setAttribute('tabindex', '-1'); }
    return el;
  }

  function heading() {
    return document.querySelector('#content h1') || document.querySelector('h1');
  }

  // restoreFocus returns the control to focus, or falls back to a safe element.
  function restoreFocus(list, f) {
    if (!f.key) {
      return f.id ? document.getElementById(f.id) : null;
    }
    var rows = list.querySelectorAll('[data-key]');
    var row = null;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute('data-key') === f.key) { row = rows[i]; break; }
    }
    if (!row) { return makeFocusable(heading()); }
    if (f.control) {
      var controls = row.querySelectorAll(CONTROLS);
      for (var j = 0; j < controls.length; j++) {
        if (identity(controls[j]) === f.control) { return controls[j]; }
      }
    }
    // The control is gone (or never had an identity): the row takes the focus.
    return makeFocusable(row);
  }

  // openDetails lists the disclosures that are open, by their stable id.
  function openDetails(list) {
    var out = [];
    var found = list.querySelectorAll('details[data-keep-open]');
    for (var i = 0; i < found.length; i++) {
      if (found[i].hasAttribute('open')) { out.push(found[i].getAttribute('data-keep-open')); }
    }
    return out;
  }

  function reopen(list, ids) {
    if (!ids.length) { return; }
    var found = list.querySelectorAll('details[data-keep-open]');
    for (var i = 0; i < found.length; i++) {
      if (ids.indexOf(found[i].getAttribute('data-keep-open')) >= 0) { found[i].setAttribute('open', ''); }
    }
  }

  function wraps(list) {
    var out = [];
    var found = list.querySelectorAll('.table-wrap');
    for (var i = 0; i < found.length; i++) { out.push({ left: found[i].scrollLeft, top: found[i].scrollTop }); }
    return out;
  }

  document.body.addEventListener('htmx:sseBeforeMessage', function (ev) {
    var list = ev.target;
    if (!isList(list)) { return; }
    saved = {
      list: list,
      focus: focusKey(list, document.activeElement),
      open: openDetails(list),
      wraps: wraps(list),
      x: window.scrollX || 0,
      y: window.scrollY || 0
    };
  });

  document.body.addEventListener('htmx:sseMessage', function (ev) {
    var list = ev.target;
    if (!isList(list) || !saved || saved.list !== list) { return; }
    var s = saved;
    saved = null;
    reopen(list, s.open || []);
    if (s.focus) { focusIt(restoreFocus(list, s.focus)); }
    var now = list.querySelectorAll('.table-wrap');
    for (var j = 0; j < now.length && j < s.wraps.length; j++) {
      now[j].scrollLeft = s.wraps[j].left;
      now[j].scrollTop = s.wraps[j].top;
    }
    if (typeof window.scrollTo === 'function') { window.scrollTo(s.x, s.y); }

    var status = document.getElementById('list-announce');
    var summary = list.querySelector('[data-summary]');
    if (status && summary) {
      var text = summary.getAttribute('data-summary');
      var id = list.id || 'list';
      if (lastSummary[id] !== undefined && lastSummary[id] !== text) { status.textContent = 'Updated: ' + text + '.'; }
      lastSummary[id] = text;
    }
  });

  // The first fragment sets the baseline summary without announcing it.
  var first = document.querySelectorAll('[data-live-list]');
  for (var k = 0; k < first.length; k++) {
    var el = first[k].querySelector('[data-summary]');
    if (el) { lastSummary[first[k].id || 'list'] = el.getAttribute('data-summary'); }
  }
})();
