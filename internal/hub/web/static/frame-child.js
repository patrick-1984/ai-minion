// frame-child.js runs only in the compact session view that the session
// inspector shows in a frame (/s/<worker>/<session>?embed=1). It is the
// frame's half of one small, read-only conversation with the page that hosts it
// (static/inspector.js). It never sends terminal input, never reads the screen
// and never carries text.
//
// Messages (window.postMessage, always to and from this exact origin):
//   to the host   {minion:"inspector", v:1, type, target:{worker, session}, doc, ...}
//     ready          this document is built AND its session controller (terminal-input.js)
//                    answered; doc is a random id of THIS document, also set on the
//                    document element (data-inspector-doc) so that the host can tell
//                    which document it is looking at; ctx is the public, opaque
//                    identity context the server rendered into this document (see
//                    page_inspector_context.go; not a credential). A document that
//                    cannot say ready, such as an error page, or one whose controller
//                    did not load, is never trusted by the host and is never shown
//                    as operational.
//     state          flags and counts only: dirty (unsent input or an open local
//                    dialog), pending (writes on their way), unknown (deliveries
//                    that are still unknown), chat (the conversation's own share of
//                    those: a draft message, one on its way, unknown deliveries; flags
//                    and counts, never text; already included above), conn (connection label), suspended,
//                    ctrl (the controller answered; a state without it is never clean)
//     close-request  Escape was pressed here with no local dialog open
//   from the host {minion:"inspector", v:1, type, nonce, target, doc}
//     query          answer with the current state (echoing nonce)
//     suspend        close the read stream while this view is hidden
//     resume         read again, writes stay off until a fresh screen came back
// Everything else is ignored: a message is only accepted from the parent window
// itself (event.source), from this origin, for exactly this target, with one of
// those types, and (when it names one) for exactly this document. Nothing in a
// message can send, press a key, answer or force.
//
// Escape: when no local dialog (the "Send anyway" confirmation) is open, Escape
// asks the host to close the inspector. It is never forwarded to the session.
(function () {
  'use strict';

  var view = document.getElementById('session-view');
  if (!view || !window.parent || window.parent === window) { return; }

  var origin = window.location.origin;
  var worker = view.getAttribute('data-worker-id') || '';
  var session = view.getAttribute('data-session') || '';
  var FROM_HOST = { query: true, suspend: true, resume: true };
  var docId = Math.random().toString(36).slice(2, 12) + Date.now().toString(36);
  var ctx = view.getAttribute('data-context') || '';
  var last = '';

  function post(type, extra) {
    var msg = { minion: 'inspector', v: 1, type: type, target: { worker: worker, session: session }, doc: docId };
    if (extra) { Object.keys(extra).forEach(function (k) { msg[k] = extra[k]; }); }
    try { window.parent.postMessage(msg, origin); } catch (e) { /* no host to tell */ }
  }

  // control asks terminal-input.js, in this document, for the state (and may
  // suspend or resume it). The script answers with flags and counts only.
  function control(op) {
    var out = null;
    var ev = new CustomEvent('minion-frame-control', { detail: { op: op, reply: function (s) { out = s; } } });
    document.body.dispatchEvent(ev);
    return out;
  }

  // A missing controller (its script failed to load) is never "clean": the answer is
  // then an unknown state that keeps the frame, whatever the form holds.
  function stateOf(s) {
    if (!s) { return { dirty: true, pending: 0, unknown: 0, conn: '', suspended: false, ctrl: false }; }
    var out = { dirty: !!s.dirty, pending: s.pending | 0, unknown: s.unknown | 0, conn: String(s.conn || '').slice(0, 16), suspended: !!s.suspended, ctrl: true };
    // chat: the conversation's share of the same flags (a draft message, a message on its way, unknown deliveries). It is
    // included in dirty, pending and unknown above; it is passed on as flags and counts so the inspector can name it.
    if (s.chat && typeof s.chat === 'object') { out.chat = { draft: !!s.chat.draft, pending: s.chat.pending | 0, unknown: s.chat.unknown | 0 }; }
    return out;
  }

  function report(force) {
    var s = stateOf(control('state'));
    var key = JSON.stringify(s);
    if (!force && key === last) { return; }
    last = key;
    post('state', s);
  }

  // After the handlers of the event that changed something have run.
  function soon() { window.setTimeout(function () { report(false); }, 0); }

  window.addEventListener('message', function (ev) {
    if (ev.origin !== origin || ev.source !== window.parent) { return; }
    var d = ev.data;
    if (!d || typeof d !== 'object' || d.minion !== 'inspector' || d.v !== 1) { return; }
    if (typeof d.type !== 'string' || !Object.prototype.hasOwnProperty.call(FROM_HOST, d.type)) { return; }
    if (!d.target || d.target.worker !== worker || d.target.session !== session) { return; }
    if (d.doc !== undefined && d.doc !== docId) { return; } // meant for another document of this frame
    var s = stateOf(control(d.type === 'query' ? 'state' : d.type));
    last = JSON.stringify(s);
    post('state', Object.assign({ nonce: typeof d.nonce === 'string' ? d.nonce.slice(0, 64) : '' }, s));
  });

  ['input', 'change', 'click', 'close', 'htmx:beforeSend', 'htmx:afterRequest', 'htmx:sendError', 'htmx:timeout', 'htmx:sendAbort'].forEach(function (name) {
    document.body.addEventListener(name, soon);
  });

  document.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Escape' || ev.defaultPrevented || ev.isComposing) { return; }
    if (document.querySelector('dialog[open]')) { return; } // the local dialog closes first
    post('close-request');
  });

  // This document's identity, readable by the host (same origin) at its load event.
  document.documentElement.setAttribute('data-inspector-doc', docId);
  // Ready only when the controller answered: without it nothing can track a draft or
  // a request, and the host must not show this document as operational.
  var first = stateOf(control('state'));
  if (first.ctrl) {
    post('ready', Object.assign({ ctx: ctx }, first));
    last = JSON.stringify(first);
  }
})();
