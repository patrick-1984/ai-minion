// session-ui.js is the interface script of the session workspace
// (/s/<worker>/<session>). It never sends anything and never reads the
// terminal on its own: every write is a form that the person submits. The
// stream, drafts and result handling live in terminal-input.js.
//
//  - Input dock: Text / Keys / Task tabs with the established tab keyboard
//    pattern (Left, Right, Home, End). Without this script all three sections
//    are shown one below the other.
//  - Right pane: a toggle with aria-expanded. From 1280px it is docked and its
//    open or closed state is a display preference kept in localStorage (nothing
//    about the session is). Below 1280px it is a modal drawer: scrim, focus kept
//    inside, Escape or Close returns the focus to Details; it is never stored.
//  - Screen: Copy screen copies the text that is shown (on a click only), Text
//    view shows it as plain wrapped text.
//  - "Send anyway" asks first, in a modal dialog that names the target and puts
//    the focus on Cancel; only its own confirm button sends.
//  - Task: shows "Unsaved changes" until the task was saved.
//  - Dialog answers: a chosen option and a typed answer exclude each other.
(function () {
  'use strict';

  var view = document.getElementById('session-view');
  if (!view) { return; }

  function byId(id) { return document.getElementById(id); }

  // ---- tabs -------------------------------------------------------------------
  function initTabs(list) {
    var tabs = Array.prototype.slice.call(list.querySelectorAll('[role="tab"]'));
    if (!tabs.length) { return; }
    function select(tab, focus) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
        var panel = byId(t.getAttribute('aria-controls'));
        if (panel) { panel.hidden = !on; }
      });
      if (focus) { tab.focus(); }
    }
    list.addEventListener('click', function (ev) {
      var t = ev.target && ev.target.closest ? ev.target.closest('[role="tab"]') : null;
      if (t && tabs.indexOf(t) >= 0) { select(t, false); }
    });
    list.addEventListener('keydown', function (ev) {
      var i = tabs.indexOf(document.activeElement);
      if (i < 0) { return; }
      var next = null;
      if (ev.key === 'ArrowRight') { next = tabs[(i + 1) % tabs.length]; }
      else if (ev.key === 'ArrowLeft') { next = tabs[(i + tabs.length - 1) % tabs.length]; }
      else if (ev.key === 'Home') { next = tabs[0]; }
      else if (ev.key === 'End') { next = tabs[tabs.length - 1]; }
      if (next) { ev.preventDefault(); select(next, true); }
    });
    var first = tabs.filter(function (t) { return t.getAttribute('aria-selected') === 'true'; })[0] || tabs[0];
    select(first, false);
  }

  var dockTabs = byId('dock-tabs');
  if (dockTabs) {
    dockTabs.hidden = false;
    var dock = byId('session-controls');
    if (dock) { dock.classList.add('tabbed'); }
    initTabs(dockTabs);
  }
  var paneTabs = document.querySelector('.pane-tabs');
  if (paneTabs) { initTabs(paneTabs); }

  // ---- right pane -------------------------------------------------------------
  // Wide (1280px and up) the pane is docked beside the screen and its open or
  // closed state is a display preference. Narrower it is a modal drawer over the
  // page: a scrim behind it, the focus moves into it and stays there, Escape, the
  // scrim and Close shut it and the focus returns to Details. A drawer is never a
  // preference (a phone must not open a full-screen panel because a desktop left
  // it open), and opening or closing it only moves the same DOM: the draft, the
  // screen and the answer form are not touched, nothing is sent, and Escape is
  // not forwarded to the session.
  var toggle = byId('pane-toggle');
  var pane = byId('session-pane');
  var grid = byId('session-grid');
  var closeBtn = byId('pane-close');
  var paneScrim = byId('pane-scrim');
  var STORE = 'minion-pane';

  function drawer() {
    return typeof window.matchMedia === 'function' && window.matchMedia('(max-width: 1279px)').matches;
  }
  function paneOpen() { return !!pane && pane.classList.contains('open'); }
  function storedOpen() {
    try { return window.localStorage.getItem(STORE) === 'open'; } catch (e) { return false; }
  }
  function visible(el) {
    return !el.hidden && !el.disabled && el.tabIndex !== -1 && !(el.getClientRects && el.getClientRects().length === 0);
  }
  function paneFocusables() {
    var list = pane.querySelectorAll('a[href], button, input, select, textarea, summary, [tabindex]');
    var out = [];
    for (var k = 0; k < list.length; k++) { if (visible(list[k])) { out.push(list[k]); } }
    return out;
  }
  // render shows or hides the pane; it never stores anything.
  function render(open, focus) {
    var modal = open && drawer();
    var inside = !!document.activeElement && pane.contains(document.activeElement);
    pane.classList.toggle('open', open);
    if (grid) { grid.classList.toggle('pane-open', open); }
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (modal) {
      pane.setAttribute('role', 'dialog');
      pane.setAttribute('aria-modal', 'true');
    } else {
      pane.removeAttribute('role');
      pane.removeAttribute('aria-modal');
    }
    if (paneScrim) { paneScrim.classList.toggle('open', modal); }
    if (focus) {
      if (modal && closeBtn) { closeBtn.focus(); } else if (!open) { toggle.focus(); }
    } else if (!open && inside) {
      toggle.focus(); // the pane went away under the focus
    }
  }
  function setPane(open, focus) {
    if (!pane || !toggle) { return; }
    render(open, focus);
    if (!drawer()) {
      try { window.localStorage.setItem(STORE, open ? 'open' : 'closed'); } catch (e) { /* not stored */ }
    }
  }
  if (toggle && pane) {
    toggle.hidden = false;
    if (closeBtn) { closeBtn.hidden = false; closeBtn.addEventListener('click', function () { setPane(false, true); }); }
    if (paneScrim) { paneScrim.addEventListener('click', function () { setPane(false, true); }); }
    render(!drawer() && storedOpen(), false);
    toggle.addEventListener('click', function () { setPane(!paneOpen(), true); });
    document.addEventListener('keydown', function (ev) {
      if (!paneOpen() || !drawer()) { return; }
      if (document.querySelector('dialog[open]')) { return; }
      var rail = byId('site-rail');
      if (rail && rail.classList.contains('open')) { return; }
      if (ev.key === 'Escape') {
        ev.preventDefault();
        setPane(false, true);
        return;
      }
      if (ev.key === 'Tab') {
        var list = paneFocusables();
        if (!list.length) { ev.preventDefault(); return; }
        var first = list[0];
        var last = list[list.length - 1];
        var at = document.activeElement;
        if (ev.shiftKey && (at === first || !pane.contains(at))) { ev.preventDefault(); last.focus(); }
        else if (!ev.shiftKey && (at === last || !pane.contains(at))) { ev.preventDefault(); first.focus(); }
      }
    });
    // Crossing the breakpoint changes what the pane is: a drawer closes, a docked
    // pane returns to the stored preference.
    if (typeof window.matchMedia === 'function') {
      var narrow = window.matchMedia('(max-width: 1279px)');
      var onChange = function () { render(!drawer() && storedOpen(), false); };
      if (typeof narrow.addEventListener === 'function') { narrow.addEventListener('change', onChange); }
    }
  }

  // ---- screen: copy and text view ---------------------------------------------
  var copy = byId('copy-screen');
  if (copy) {
    copy.hidden = false;
    copy.addEventListener('click', function () {
      var screen = byId('screen');
      if (!screen) { return; }
      var label = copy.querySelector('span');
      var original = label ? label.textContent : '';
      var done = function (text) {
        if (!label) { return; }
        label.textContent = text;
        window.setTimeout(function () { label.textContent = original; }, 2000);
      };
      var range = document.createRange();
      range.selectNodeContents(screen);
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(screen.textContent).then(function () { done('Copied'); }, function () { done('Press Ctrl+C to copy'); });
      } else {
        done('Press Ctrl+C to copy');
      }
    });
  }
  var textView = byId('text-view');
  if (textView) {
    textView.hidden = false;
    textView.addEventListener('click', function () {
      var on = view.getAttribute('data-view') !== 'text';
      if (on) { view.setAttribute('data-view', 'text'); } else { view.removeAttribute('data-view'); }
      textView.setAttribute('aria-pressed', on ? 'true' : 'false');
      var label = textView.querySelector('span');
      if (label) { label.textContent = on ? 'Terminal view' : 'Text view'; }
    });
  }

  // ---- "Send anyway" confirmation ---------------------------------------------
  document.addEventListener('click', function (ev) {
    var t = ev.target;
    if (!t || !t.closest) { return; }
    var cancel = t.closest('[data-dialog-cancel]');
    if (cancel) {
      var dlg = cancel.closest('dialog');
      if (dlg && typeof dlg.close === 'function') { dlg.close(); }
      return;
    }
    var open = t.closest('form[data-force] > button');
    if (!open) { return; }
    var d = open.form ? open.form.querySelector('dialog') : null;
    if (!d || typeof d.showModal !== 'function') { return; } // no dialog support: the button sends, as before
    ev.preventDefault();
    d.showModal();
  });
  // The confirm button submits the form; the dialog closes with it.
  document.addEventListener('submit', function (ev) {
    var d = ev.target && ev.target.querySelector ? ev.target.querySelector('dialog[open]') : null;
    if (d && typeof d.close === 'function') { d.close(); }
  });

  // ---- task: unsaved changes --------------------------------------------------
  //
  // "Unsaved changes" shows while the field differs from the last task the Worker
  // acknowledged. The acknowledgement (HX-Trigger minion-task-saved) names the task
  // THAT REQUEST posted: it is read from the request's own parameters when the request
  // is configured and kept per request config (the object every later event of the
  // request carries as requestConfig). The field's current value is never used: a
  // "Send anyway" form posts the task of the refused save (A) while the person may
  // have edited the field to B since, and B was not saved. The response is read in
  // htmx:beforeOnLoad on the body and, for a request whose form was replaced by another
  // result (its events never reach document.body), by one-time listeners on the form
  // that issued it; one acknowledgement per request.
  var task = byId('task-text');
  var taskState = byId('task-state');
  if (task && taskState) {
    var TASK_PATH = /\/task$/;
    var posted = new WeakMap();
    var acked = new WeakSet();
    var refresh = function () { taskState.hidden = task.value === task.getAttribute('data-saved'); };
    task.addEventListener('input', refresh);
    var postedTask = function (d) {
      if (d.formData && typeof d.formData.get === 'function') { return d.formData.get('task'); }
      return d.parameters ? d.parameters.task : undefined;
    };
    var acknowledge = function (detail) {
      var cfg = detail && detail.requestConfig;
      if (!cfg || acked.has(cfg) || !posted.has(cfg)) { return; }
      var xhr = detail.xhr;
      var header = xhr && typeof xhr.getResponseHeader === 'function' ? xhr.getResponseHeader('HX-Trigger') : null;
      var saved = false;
      if (header) {
        try { saved = Object.prototype.hasOwnProperty.call(JSON.parse(header) || {}, 'minion-task-saved'); } catch (e) { saved = false; }
      }
      acked.add(cfg);
      var text = posted.get(cfg);
      // Without a readable posted task nothing is claimed as saved.
      if (saved && typeof text === 'string') {
        task.setAttribute('data-saved', text);
        refresh();
      }
    };
    document.body.addEventListener('htmx:configRequest', function (ev) {
      var d = ev.detail || {};
      if (TASK_PATH.test(String(d.path || '').split('?')[0])) { posted.set(d, postedTask(d)); }
    });
    document.body.addEventListener('htmx:beforeOnLoad', function (ev) { acknowledge(ev.detail); });
    document.body.addEventListener('htmx:beforeSend', function (ev) {
      var d = ev.detail || {};
      var cfg = d.requestConfig;
      var form = d.elt;
      if (!cfg || !posted.has(cfg) || !form || typeof form.addEventListener !== 'function') { return; }
      var onLoad = function (e) { acknowledge(e && e.detail); };
      var end = function (e) {
        if (!e || !e.detail || e.detail.requestConfig !== cfg) { return; }
        form.removeEventListener('htmx:beforeOnLoad', onLoad);
        form.removeEventListener('htmx:afterRequest', end);
      };
      form.addEventListener('htmx:beforeOnLoad', onLoad);
      form.addEventListener('htmx:afterRequest', end);
    });
  }

  // ---- a chosen option and a typed answer exclude each other ------------------
  document.body.addEventListener('input', function (ev) {
    var t = ev.target;
    if (!t || t.id !== 'answer-choice' || !t.value || !t.form) { return; }
    var radios = t.form.querySelectorAll('input[type="radio"]');
    for (var i = 0; i < radios.length; i++) { radios[i].checked = false; }
  });
  document.body.addEventListener('change', function (ev) {
    var t = ev.target;
    if (!t || t.type !== 'radio' || !t.form || !t.form.hasAttribute('data-answer-form')) { return; }
    var box = t.form.querySelector('#answer-choice');
    if (box && box.value) {
      box.value = '';
      box.dispatchEvent(new Event('input', { bubbles: true })); // counts as an edit of the draft
    }
  });
})();
