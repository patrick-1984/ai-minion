// "Send without asking" (v0.9.1, A19; one confirmation per visit, v0.9.3 A26):
// a choice of this browser, per session
// and per Worker (all its sessions). When it is on, text, keys and the task
// are sent with force=1, so a shell pane, an open dialog or a busy agent no
// longer answer with "Send anyway" and a second confirmation. A dialog answer
// keeps its own check: a changed dialog is never forced. The choice is kept
// in localStorage; nothing is stored on the Hub.
(function () {
  'use strict';
  // Two namespaces: "w." for a whole Worker, "s." for one session, so no
  // session name (not even "*") can stand for the Worker.
  var PREFIX = 'minion.unasked.';
  function key(worker, session) { return session === null ? PREFIX + 'w.' + worker : PREFIX + 's.' + worker + '|' + session; }
  function get(k) { try { return window.localStorage.getItem(k) === '1'; } catch (e) { return false; } }
  function put(k, on) {
    try { if (on) { window.localStorage.setItem(k, '1'); } else { window.localStorage.removeItem(k); } } catch (e) { /* not kept */ }
  }
  function sessionKeys(worker) {
    var out = [];
    try {
      for (var i = 0; i < window.localStorage.length; i++) {
        var k = window.localStorage.key(i);
        if (k && k.indexOf(PREFIX + 's.' + worker + '|') === 0) { out.push(k); }
      }
    } catch (e) { /* none */ }
    return out;
  }
  function active(worker, session) { return get(key(worker, null)) || get(key(worker, session)); }

  // ---- the session page (also inside the Worker page's inspector frame) ----
  // Two ways to stop the question: the checkbox (kept in this browser) and one
  // "Send anyway" on this page, which allows sending until the page is left or
  // reloaded (A26): one confirmation, on the screen, never a pop-up.
  var view = document.getElementById('session-view');
  var box = document.getElementById('unasked-session');
  if (view && box) {
    var worker = view.getAttribute('data-worker-id');
    var session = view.getAttribute('data-session');
    var note = document.getElementById('unasked-note');
    var visit = false;
    var showSession = function () {
      var all = get(key(worker, null));
      var saved = active(worker, session);
      box.checked = saved;
      box.disabled = all;
      if (!note) { return; }
      if (all) {
        note.textContent = 'Always allowed for every session of this Worker (setting of this browser; change it on the Worker page)';
      } else if (saved) {
        note.textContent = 'Always allowed (setting of this browser)';
      } else if (visit) {
        note.textContent = 'Allowed until you leave this page';
      } else {
        note.textContent = '';
      }
    };
    box.closest('[data-unasked]').hidden = false;
    box.addEventListener('change', function () { put(key(worker, session), box.checked); showSession(); });
    window.addEventListener('storage', showSession);
    // Hidden in the inspector (closed or another session chosen) counts as
    // leaving the page: the next send asks again.
    document.addEventListener('minion:view-suspended', function () { visit = false; showSession(); });
    showSession();
    document.body.addEventListener('htmx:configRequest', function (ev) {
      var d = ev.detail || {};
      var elt = d.elt;
      if (elt && elt.closest && elt.closest('form[data-force]')) {
        // The one confirmation of this visit.
        visit = true;
        showSession();
        return;
      }
      var path = String(d.path || '').split('?')[0];
      if (!/\/(text|keys|task)$/.test(path) || !(visit || active(worker, session))) { return; }
      if (d.formData && typeof d.formData.set === 'function') { d.formData.set('force', '1'); }
      if (d.parameters) {
        if (typeof d.parameters.set === 'function') { d.parameters.set('force', '1'); } else { d.parameters.force = '1'; }
      }
    });
  }

  // ---- the Worker page: all sessions at once, partial when some are on ----
  var panel = document.querySelector('[data-unasked-worker]');
  if (panel) {
    var wid = panel.getAttribute('data-unasked-worker');
    var all = document.getElementById('unasked-worker');
    var status = document.getElementById('unasked-worker-status');
    var showWorker = function () {
      var on = get(key(wid, null));
      var some = sessionKeys(wid).length;
      all.checked = on;
      all.indeterminate = !on && some > 0;
      status.textContent = on ? 'On for every session of this Worker.' :
        some > 0 ? 'On for ' + some + (some === 1 ? ' session' : ' sessions') + ' only (partial). Tick to turn it on for all.' :
        'Off: a shell, an open dialog or a busy agent asks before text or keys are sent.';
    };
    panel.hidden = false;
    all.addEventListener('change', function () {
      // All or nothing: on covers every session; off clears every session too.
      sessionKeys(wid).forEach(function (k) { put(k, false); });
      put(key(wid, null), all.checked);
      showWorker();
    });
    window.addEventListener('storage', showWorker);
    showWorker();
  }
})();
