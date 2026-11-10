// inspector.js is the controller of the session inspector on the control center,
// Workers and Sessions pages: a plain click on a session link opens a contextual
// right pane with that session next to the list, without leaving or reloading the
// list. The pane shows the compact view of the existing session workspace
// (/s/<worker>/<session>?embed=1) in a same-origin frame, so the screen, live and
// pause, copy, text view, the Text composer (Enter is an opt-in checkbox), the keys,
// the task, reviewed-dialog answers, "Send anyway" and the unknown-delivery
// evidence are the ONE implementation of the session page, in the frame's own
// document with its own forms, request lifecycle and drafts. This script never
// sends anything: no form post, no key, no text, no answer, no force. Its one request is
// the guarded READ of the sign-in described under "Sign-in" (a plain GET).
//
// Rows. A plain click on a part of a session row that does nothing of its own opens the pane
// like the session link (see "whole rows" below); a Worker row, like its Worker link, goes to the
// Worker's page. The links stay the keyboard way in and the fallback; the row is not a button.
//
// Links. Only a link marked data-inspect is taken over, only for a plain primary
// click (Ctrl, Cmd, Shift, Alt, middle click and a link with a target keep the
// browser's own behaviour: the full session page). Its href must be a path of this
// origin, /s/<worker>/<session>; the frame address is that path plus embed=1.
// Without this script (or when the link cannot be used) the href is the answer.
// The session is keyed by the canonical worker id and the session name of its row
// (data-worker, data-session), never by the display name or a position.
//
// Frames. One frame is shown; hidden frames are kept (at most 4) so that a draft,
// an unknown delivery or a request on its way is never lost by looking at another
// session. A hidden frame is told to suspend (its read stream stops; this is not
// the operator's Pause) and is told to resume, with a fresh read, when it is shown
// again. A hidden frame is discarded to make room only when that frame itself
// answers, now, that it is clean (no draft, no request on its way, no unknown
// delivery) and nothing about it changed or showed it since the question was asked;
// the decision is taken in the same task as the answer, frames are asked one at a
// time (a decision is never kept while waiting for another frame), and the shown
// frame is never discarded on its own, because the person can still type into it.
// When nothing can be discarded, an explicit dialog names the frames and the person
// decides; nothing is discarded to make room on its own.
//
// Identity. A cookie is shared by every tab: another sign-in (another user, another act-as
// setting, a new login) in ANOTHER tab changes who the next request is made as, while this
// page and its kept frames still hold what the first person saw and typed. The server renders
// a public, opaque identity context into this page and into every frame (data-context; an
// HMAC of the authenticated principal under a key only the Hub has, no session id or token,
// not a credential, it grants nothing) and answers two read-only routes with it:
//   GET /inspector/principal           who is this request?
//   GET /s/<worker>/<session>/context  may this request read this session now?
// A kept frame is NEVER shown before the second one confirmed, just now, the same context and
// the right to read that session (no time window in which it is shown unchecked); while frames
// are kept the first one is asked every 30 seconds. A different context (another account, a
// new login, another act-as setting), a 401 or a redirect to sign-in clears EVERY kept frame
// with its draft and evidence at once and the links work as ordinary links again. The context is
// derived from persisted material only, so a restart of the Hub is NOT another sign-in: what the
// page holds survives it. A refusal of one session (403, 404) first asks who is signed in (the
// shown frame waits, hidden): another sign-in clears everything; the same sign-in removes a frame
// that holds nothing, and keeps (hidden, never shown) one that holds a draft, a request on its way
// or an unknown delivery, because the session may only be unreachable for now (a restarted Hub whose
// Worker is not back yet). A concealed frame comes back only after the sign-in AND this session were
// confirmed. Every read is bounded (8 s, then aborted):
// when an answer cannot be had the kept frames are concealed (hidden; drafts, evidence and
// requests on their way are untouched) and the check repeats every 10 s with a warning until
// it succeeds. Nothing is ever shown from the cache while the identity is unconfirmed.
// The cache lives in this page only: leaving the page ends it.
//
// Messages with a frame (window.postMessage, this origin only, see frame-child.js)
// carry flags and counts, never text. A message is accepted only from the
// contentWindow of a frame this script made, from this origin, for exactly that
// frame's target, with an allowed type (ready, state, close-request) and, after
// the first ready, from the document that said ready. Every loaded document must
// say ready itself, and only the document the frame holds right now can (it carries
// its id on its document element, read at the load event and at the message; the
// window of a frame stays the same across documents and says nothing). A reload to an
// error page at the same address never says ready; the frame is hidden and removed
// when the deadline passes. A frame is shown only after its controller answered.
//
// Keyboard. Escape closes the topmost local dialog first; otherwise it closes the
// inspector (as a drawer anywhere on the page, docked only when the focus is in it)
// and returns the focus to the row control that opened it, or to the page heading
// when that is gone. A drawer (narrower than 1280px) is modal: the rest of the page is
// inert, and Close, Escape and the scrim leave it. Escape is never sent to a session.
(function () {
  'use strict';

  var host = document.getElementById('inspector');
  var framesHost = document.getElementById('inspector-frames');
  if (!host || !framesHost) { return; }

  var origin = window.location.origin;
  var MAX_FRAMES = 4;
  var READY_MS = 8000;
  var QUERY_MS = 1500;
  var AUTH_MS = 30000;
  var GUARD_MS = 8000;
  var DBL_MS = 520; // a quiet row waits this long for a second click of a double click (slow system settings run to 500 ms)
  var RETRY_MS = 10000;
  var CONCEAL_TEXT = 'Your sign-in could not be confirmed, so the sessions kept here are not shown. What you typed and any request on its way are kept. The check is repeated; you can also open the session in full.';
  var PATH_RE = /^\/s\/[^\/?#]+\/[^\/?#]+$/;
  var SRC_RE = /^\/s\/[^\/?#]+\/[^\/?#]+\?embed=1$/;
  var FROM_FRAME = { ready: true, state: true, 'close-request': true };
  var has = function (o, k) { return Object.prototype.hasOwnProperty.call(o, k); };

  function byId(id) { return document.getElementById(id); }
  var title = byId('inspector-title');
  var openLink = byId('inspector-open');
  var closeBtn = byId('inspector-close');
  var status = byId('inspector-status');
  var scrim = byId('inspector-scrim');
  var kept = byId('inspector-kept');
  var keptList = byId('inspector-kept-list');
  var dlg = byId('inspector-capacity');
  var dlgList = byId('inspector-capacity-list');
  var dlgCancel = byId('inspector-capacity-cancel');
  var layout = byId('inspect-layout');
  var alertBox = byId('inspector-alert');

  var frames = []; // least recently shown first
  var active = null;
  var seq = 0; // a newer choice cancels an older one that is still waiting
  var nonceSeq = 0;
  var opener = null; // {key, control}: where the focus returns to
  var lost = false; // the sign-in is gone: the links work as plain links again
  var authTimer = null;
  var concealed = false; // the identity could not be confirmed: kept frames are not shown
  var verifying = 0; // checks of who this request is that are on their way after a refusal: the shown frame waits for them
  var pageCtx = host.getAttribute('data-context') || ''; // the identity context this page was rendered for
  var evictSeq = 0;

  // ---- links -----------------------------------------------------------------

  // infoOf validates the link and the row it belongs to. It returns null for
  // anything that is not a session of this origin with a known identity.
  function infoOf(a) {
    var href = a.getAttribute('href');
    if (!href) { return null; }
    var u;
    try { u = new URL(href, window.location.href); } catch (e) { return null; }
    if (u.origin !== origin || !PATH_RE.test(u.pathname) || u.search || u.hash) { return null; }
    var row = a.closest ? a.closest('[data-worker][data-session]') : null;
    if (!row) { return null; }
    var worker = row.getAttribute('data-worker');
    var session = row.getAttribute('data-session');
    if (!worker || !session) { return null; }
    var src = u.pathname + '?embed=1';
    if (!SRC_RE.test(src)) { return null; }
    return { key: worker + '\n' + session, worker: worker, session: session, label: a.getAttribute('data-inspect-label') || session, full: u.pathname, src: src };
  }

  function openerOf(a) {
    var row = a.closest ? a.closest('[data-key]') : null;
    return { key: row ? row.getAttribute('data-key') : null, control: a.getAttribute('data-control') };
  }

  // ---- whole rows ------------------------------------------------------------
  //
  // A row of the session table (tr[data-row-link="session"]) opens the pane like its
  // session link does, and a row of the Workers table (tr[data-row-link="worker"])
  // goes to the Worker's page like its Worker link does, when a plain primary click
  // lands on a part of the row that does nothing of its own. The link stays the
  // keyboard and assistive way in (the row is not a button and takes no focus; a
  // click on the link itself is handled as before, so nothing fires twice), and
  // Ctrl, Cmd, Shift, Alt and middle clicks keep the link's own behaviour. A click
  // that ends a text selection or a double click (which selects a word) opens
  // nothing, so a name or a task can be selected and copied.
  var INTERACTIVE_TAGS = { A: 1, BUTTON: 1, INPUT: 1, SELECT: 1, TEXTAREA: 1, OPTION: 1, SUMMARY: 1, DETAILS: 1, LABEL: 1, FORM: 1, DIALOG: 1, IFRAME: 1, VIDEO: 1, AUDIO: 1 };
  var INTERACTIVE_ROLES = /^(button|link|checkbox|radio|switch|tab|menuitem|option|combobox|textbox|slider)$/;

  // interactiveWithin: is there, between the click target and the row, something that
  // does its own thing (a control, a link, an editable or focusable element, an
  // element that opted out)? A focus fallback of the live list (tabindex -1) is not.
  function interactiveWithin(target, row) {
    for (var n = target; n && n !== row; n = n.parentNode) {
      if (!n.tagName || typeof n.getAttribute !== 'function') { continue; }
      if (has(INTERACTIVE_TAGS, String(n.tagName).toUpperCase())) { return true; }
      if (n.hasAttribute('data-no-row') || n.hasAttribute('contenteditable') || n.isContentEditable === true) { return true; }
      if (INTERACTIVE_ROLES.test(String(n.getAttribute('role') || ''))) { return true; }
      var ti = n.getAttribute('tabindex');
      if (ti !== null && ti !== '-1') { return true; }
    }
    return false;
  }

  function selecting() {
    var sel = typeof window.getSelection === 'function' ? window.getSelection() : null;
    return !!sel && (sel.isCollapsed === false || String(sel) !== '');
  }

  function workerHref(a) {
    var href = a && a.getAttribute('href');
    if (!href) { return null; }
    var u;
    try { u = new URL(href, window.location.href); } catch (e) { return null; }
    if (u.origin !== origin || !/^\/workers\/[^\/?#]+$/.test(u.pathname) || u.hash) { return null; }
    if (u.search && u.search !== '?scope=all') { return null; }
    return u.pathname + u.search;
  }

  // A double click that selects a word starts with an ordinary primary click (detail 1, nothing selected
  // yet), so the first click of a sequence cannot tell a click from the start of a double click. A quiet row
  // therefore waits a short moment (DBL_MS, about the double-click time of the system) and acts only if
  // nothing else happened: a second press, a second click of the sequence (detail above 1), a double click, a
  // text selection, a modified or other click, or the row being replaced by a live update all cancel it. The
  // links of a row, the keyboard way in, and a touch or a pen tap (no double click selects text there) act at once.
  var pendingRow = null;
  function cancelRow() {
    if (pendingRow) { window.clearTimeout(pendingRow.timer); pendingRow = null; }
  }
  function attached(n) {
    for (; n; n = n.parentNode) { if (n === document.body || n === document.documentElement) { return true; } }
    return false;
  }
  // rowByKey finds the row that stands for the same list entry now (a live update replaces the rows with new
  // elements of the same data-key), or null when that entry is gone.
  function rowByKey(key) {
    if (!key || typeof document.querySelectorAll !== 'function') { return null; }
    var list = document.querySelectorAll('tr[data-row-link]');
    for (var i = 0; i < list.length; i++) { if (list[i].getAttribute('data-key') === key) { return list[i]; } }
    return null;
  }
  // resolve(row) says what a click on that row stands for NOW: {id, act}, or null when it stands for nothing. When the time has
  // passed the row is looked up again by its stable data-key and the target is read from what is in the page now: the click goes
  // to the same entry (and the same session or Worker) or to nothing, never to another row.
  function afterDoubleClickTime(row, resolve, now) {
    cancelRow();
    var first = resolve(row);
    if (!first) { return; }
    if (now) { first.act(); return; }
    var key = row.getAttribute('data-key');
    var rec = { row: row, timer: null };
    rec.timer = window.setTimeout(function () {
      if (pendingRow !== rec) { return; }
      pendingRow = null;
      if (selecting()) { return; } // a word was selected
      var cur = attached(row) ? row : rowByKey(key); // replaced by a live update: the same entry, if it is still there
      if (!cur || cur.getAttribute('data-key') !== key) { return; }
      var again = resolve(cur);
      if (!again || again.id !== first.id) { return; } // gone, or it now stands for another session or Worker
      again.act();
    }, DBL_MS);
    pendingRow = rec;
  }
  document.addEventListener('mousedown', cancelRow);   // a second press: a double click or a new gesture
  document.addEventListener('dblclick', cancelRow);

  function rowClick(ev) {
    if (ev.button !== undefined && ev.button !== 0) { return; }
    if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.altKey) { return; }
    if (ev.detail !== undefined && ev.detail > 1) { return; }
    var row = ev.target && ev.target.closest ? ev.target.closest('tr[data-row-link]') : null;
    if (!row || interactiveWithin(ev.target, row) || selecting()) { return; }
    var immediate = ev.pointerType === 'touch' || ev.pointerType === 'pen';
    var kind = row.getAttribute('data-row-link');
    if (kind === 'session') {
      if (lost) { return; }
      afterDoubleClickTime(row, function (r) {
        if (lost) { return null; }
        var a = r.querySelector('a[data-inspect]');
        var info = a ? infoOf(a) : null;
        if (!info) { return null; }
        var from = openerOf(a);
        return { id: info.key, act: function () { if (!lost) { choose(info, from); } } };
      }, immediate);
    } else if (kind === 'worker') {
      afterDoubleClickTime(row, function (r) {
        var to = workerHref(r.querySelector('a[data-worker-link]'));
        return to ? { id: to, act: function () { window.location.assign(to); } } : null;
      }, immediate);
    }
  }

  document.addEventListener('click', function (ev) {
    cancelRow();
    if (ev.defaultPrevented) { return; }
    if (ev.target && ev.target.closest && !ev.target.closest('a[data-inspect]')) { rowClick(ev); return; }
    if (lost) { return; }
    if (ev.button !== undefined && ev.button !== 0) { return; }
    if (ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.altKey) { return; }
    var a = ev.target && ev.target.closest ? ev.target.closest('a[data-inspect]') : null;
    if (!a) { return; }
    var t = a.getAttribute('target');
    if ((t && t !== '_self') || a.hasAttribute('download')) { return; }
    var info = infoOf(a);
    if (!info) { return; } // the link works as it is
    ev.preventDefault();
    choose(info, openerOf(a));
  });

  // ---- frames ----------------------------------------------------------------

  function find(key) {
    for (var i = 0; i < frames.length; i++) { if (frames[i].key === key) { return frames[i]; } }
    return null;
  }

  function send(rec, type, nonce) {
    var w = rec.el.contentWindow;
    if (!w || typeof w.postMessage !== 'function') { return; }
    var msg = { minion: 'inspector', v: 1, type: type, target: { worker: rec.worker, session: rec.session } };
    if (rec.doc) { msg.doc = rec.doc; }
    if (nonce) { msg.nonce = nonce; }
    w.postMessage(msg, origin);
  }

  function create(info) {
    if (!SRC_RE.test(info.src)) { return null; }
    var el = document.createElement('iframe');
    el.className = 'inspector-frame';
    el.setAttribute('title', 'Session ' + info.label);
    el.setAttribute('referrerpolicy', 'same-origin');
    el.hidden = true;
    var rec = { key: info.key, worker: info.worker, session: info.session, label: info.label, full: info.full, src: info.src,
      el: el, ready: false, suspended: false, state: null, queries: {}, timer: null,
      doc: null, ctx: null, evicting: null };
    el.addEventListener('load', function () { onLoad(rec); });
    framesHost.appendChild(el);
    frames.push(rec);
    rec.timer = window.setTimeout(function () { fail(rec); }, READY_MS);
    el.setAttribute('src', info.src);
    watchAuth();
    return rec;
  }

  function destroy(rec) {
    var i = frames.indexOf(rec);
    if (i >= 0) { frames.splice(i, 1); }
    if (rec.timer !== null) { window.clearTimeout(rec.timer); rec.timer = null; }
    Object.keys(rec.queries).forEach(function (n) { var q = rec.queries[n]; if (q) { q(null); } });
    if (rec.el.parentNode) { rec.el.parentNode.removeChild(rec.el); }
    if (active === rec) { active = null; }
    renderKept();
    renderDirty();
  }

  // locate says where the frame is: still our page, blank (not loaded yet), or
  // somewhere else (blocked, redirected, signed out, another resource).
  function locate(rec) {
    try {
      var w = rec.el.contentWindow;
      var loc = w && w.location;
      if (!loc) { return 'moved'; }
      if (loc.href === 'about:blank') { return 'blank'; }
      if (loc.origin === origin && loc.pathname + loc.search === rec.src) { return 'ok'; }
    } catch (e) { /* cross-origin or refused: not our page */ }
    return 'moved';
  }

  // curDoc is the id of the document the frame holds RIGHT NOW: the child sets it on its
  // document element before it says ready (data-inspector-doc), and this page can read it
  // because the frame is of this origin. A document without a bridge (an error page, a page
  // whose script failed) has none. The window of a frame stays the same across documents, so
  // the window says nothing about which document is loaded; this does.
  function curDoc(rec) {
    try {
      var w = rec.el.contentWindow;
      var de = w && w.document && w.document.documentElement;
      var v = de && de.getAttribute('data-inspector-doc');
      return v ? String(v) : null;
    } catch (e) { return null; }
  }

  // visible: a frame is shown only while it is the active one, its document has said
  // ready (so its controller answered), and the identity is confirmed. Until then it is
  // hidden, so nothing that cannot be tracked can be typed into, and nothing cached is shown.
  function applyVisibility(rec) {
    rec.el.hidden = !(rec === active && rec.ready && !concealed && verifying === 0);
  }

  // onLoad is called for every document the frame loads. The document it holds must say
  // ready ITSELF: a ready that already came for this very document (a script may say it
  // before or after this event) keeps the frame ready; any other document, such as an
  // error page at the same address, makes it not ready, hides it and starts the deadline.
  // No count of earlier loads or readies is compared: only the current document counts.
  function onLoad(rec) {
    if (frames.indexOf(rec) < 0) { return; }
    var where = locate(rec);
    if (where === 'blank') { return; }
    if (where === 'moved') { fail(rec); return; }
    var cur = curDoc(rec);
    if (cur && cur === rec.doc && rec.ready) { return; }
    rec.ready = false;
    rec.doc = null;
    rec.state = null;
    if (rec.timer !== null) { window.clearTimeout(rec.timer); }
    rec.timer = window.setTimeout(function () { fail(rec); }, READY_MS);
    applyVisibility(rec);
    if (rec === active) { setStatus('info', 'Loading the session…'); }
  }

  // fail removes a frame that could not be shown (refused, moved, never became
  // ready). Nothing about the reason is shown: the full session page says it.
  function fail(rec) {
    if (frames.indexOf(rec) < 0) { return; }
    var wasActive = rec === active;
    destroy(rec);
    recheck(); // a refused session is ordinary; a lost sign-in is for the whole page
    if (wasActive) {
      header(rec);
      setStatus('warning', 'This session cannot be shown here. It may have been removed, or you may no longer be allowed to see it. Use Open full session to see what the Hub says.');
      markSelected(null);
    }
  }

  // park hides a frame and asks it to stop reading; wake asks it to read again.
  function park(rec) {
    rec.el.hidden = true;
    rec.suspended = true;
    if (rec.ready) { send(rec, 'suspend'); }
  }
  function wake(rec) {
    rec.suspended = false;
    if (rec.ready) { send(rec, 'resume'); }
  }

  // freshState asks the frame for its state now. null means no answer in time (or
  // a frame that is not ready), which never counts as clean.
  function freshState(rec, cb) {
    if (!rec.ready || !rec.el.contentWindow) { cb(null); return; }
    var nonce = 'q' + (++nonceSeq) + '.' + Math.random().toString(36).slice(2, 10);
    var done = false;
    var timer = window.setTimeout(function () { settle(null); }, QUERY_MS);
    function settle(s) {
      if (done) { return; }
      done = true;
      window.clearTimeout(timer);
      delete rec.queries[nonce];
      cb(s);
    }
    rec.queries[nonce] = settle;
    send(rec, 'query', nonce);
  }

  // A frame whose controller did not answer (ctrl false) is never clean: nothing tracks what it holds.
  // The conversation's share (chat) is part of dirty, pending and unknown; a frame that reports it as something while
  // reporting the totals as nothing is inconsistent, and an inconsistent frame is never clean.
  function chatHolds(s) { return !!s && !!s.chat && (s.chat.draft || s.chat.pending > 0 || s.chat.unknown > 0); }
  function clean(s) { return !!s && s.ctrl === true && s.dirty === false && s.pending === 0 && s.unknown === 0 && !chatHolds(s); }
  function unsettled(s) { return !!s && (!s.ctrl || s.dirty || s.pending > 0 || s.unknown > 0 || chatHolds(s)); }

  // tryEvict discards one HIDDEN frame if it answers clean now. The mark ties the
  // answer to this question: showing the frame, a state message from it (it changed)
  // or a newer choice clears it, and then the answer is not used. The decision is
  // taken here, in the task of the answer, and the frame is removed at once.
  function tryEvict(rec, token, done) {
    if (rec === active || !rec.ready) { done(false); return; }
    var mark = ++evictSeq;
    rec.evicting = mark;
    freshState(rec, function (s) {
      var ok = rec.evicting === mark && token === seq && rec !== active && frames.indexOf(rec) >= 0 && clean(s);
      if (rec.evicting === mark) { rec.evicting = null; }
      if (ok) { destroy(rec); }
      done(ok);
    });
  }

  // makeRoom discards one frame when the cache is full, asking the hidden frames one
  // at a time, the least recently shown first. It reports false when none can go;
  // the shown frame is never a candidate.
  function makeRoom(token, cb) {
    if (frames.length < MAX_FRAMES) { cb(true); return; }
    var order = frames.filter(function (r) { return r !== active; });
    (function next(i) {
      if (token !== seq) { return; }
      if (i >= order.length) { cb(false); return; }
      if (frames.indexOf(order[i]) < 0) { next(i + 1); return; }
      tryEvict(order[i], token, function (gone) {
        if (token !== seq) { return; }
        if (gone) { cb(true); } else { next(i + 1); }
      });
    })(0);
  }

  // collect drops the hidden frames that are clean now (after the inspector was closed).
  function collect() {
    var token = seq;
    frames.slice().forEach(function (rec) { tryEvict(rec, token, function () {}); });
  }

  // ---- choosing and showing --------------------------------------------------

  function choose(info, from) {
    var token = ++seq;
    if (from) { opener = from; }
    var rec = find(info.key);
    if (rec && rec === active) { show(rec, true); return; }
    if (rec) {
      // A kept frame is never shown before the server has confirmed, now, that this request is
      // made by the identity the page and the frame were rendered for, and that it may read this
      // session. There is no time window in which a kept frame is shown unchecked.
      checkFrame(rec, function (r) {
        if (token !== seq || frames.indexOf(rec) < 0) { return; }
        if (r === 'ok') { unconceal(); say('', ''); show(rec, true); return; }
        if (r === 'lost' || r === 'switched') { authLost(r); return; }
        if (r === 'refused') { refusedFrame(rec); return; }
        say('warning', 'Your sign-in could not be confirmed just now, so ' + rec.label + ' was not shown. What you typed there is kept. Try again, or open it in full.');
        armAuth(RETRY_MS);
      });
      return;
    }
    makeRoom(token, function (ok) {
      if (token !== seq) { return; }
      if (!ok) { askDiscard(info); return; }
      var made = create(info);
      if (made) { show(made, true); }
    });
  }

  function show(rec, user) {
    var prev = active;
    if (prev && prev !== rec) { park(prev); }
    active = rec;
    rec.evicting = null; // a question about discarding it is void once it is shown
    var i = frames.indexOf(rec);
    if (i >= 0) { frames.splice(i, 1); frames.push(rec); }
    if (rec.suspended) { wake(rec); }
    applyVisibility(rec);
    host.hidden = false;
    header(rec);
    if (concealed) { setStatus('warning', CONCEAL_TEXT); } else if (rec.ready) { hideStatus(); } else { setStatus('info', 'Loading the session…'); }
    markSelected(rec);
    renderMode();
    renderKept();
    renderDirty();
    if (user && title && typeof title.focus === 'function') { title.focus({ preventScroll: true }); }
  }

  function header(rec) {
    if (title) { title.textContent = rec.label; }
    if (openLink) { openLink.setAttribute('href', rec.full); }
  }

  function setStatus(kind, text) {
    if (!status) { return; }
    status.textContent = text;
    status.className = 'notice notice-' + kind + ' inspector-status';
    status.hidden = false;
  }
  function hideStatus() { if (status) { status.hidden = true; status.textContent = ''; } }

  function close(restore) {
    seq++; // a choice that is still waiting is cancelled
    var rec = active;
    active = null;
    if (rec) { park(rec); }
    host.hidden = true;
    hideStatus();
    markSelected(null);
    renderMode();
    collect();
    renderKept();
    if (restore) { restoreOpener(); }
  }

  // restoreOpener returns the focus to the link that opened the inspector when it is
  // still there and visible, else to its row (not a control), else to the page
  // heading. It never moves the focus to any other control, whatever a live update
  // did to the order of the rows.
  function restoreOpener() {
    var o = opener;
    opener = null;
    var done = false;
    if (o && o.key) {
      var rows = document.querySelectorAll('[data-key]');
      var row = null;
      for (var i = 0; i < rows.length; i++) { if (rows[i].getAttribute('data-key') === o.key) { row = rows[i]; break; } }
      if (row) {
        var links = row.querySelectorAll('a[data-inspect]');
        for (var j = 0; j < links.length && !done; j++) {
          if (o.control && links[j].getAttribute('data-control') === o.control) { links[j].focus(); done = document.activeElement === links[j]; }
        }
        if (!done) { row.setAttribute('tabindex', '-1'); row.focus(); done = document.activeElement === row; }
      }
    }
    if (!done) {
      var h = document.querySelector('#content h1') || document.querySelector('h1');
      if (h) { h.setAttribute('tabindex', '-1'); h.focus({ preventScroll: true }); }
    }
  }

  // markSelected shows which session is open: the row (or list item) of the
  // canonical worker id and session name, whatever its position.
  function markSelected(rec) {
    var list = document.querySelectorAll('[data-worker][data-session]');
    for (var i = 0; i < list.length; i++) {
      var el = list[i];
      var on = !!rec && el.getAttribute('data-worker') === rec.worker && el.getAttribute('data-session') === rec.session;
      if (on) { el.setAttribute('data-inspected', 'true'); } else { el.removeAttribute('data-inspected'); }
      var links = el.querySelectorAll('a[data-inspect]');
      for (var j = 0; j < links.length; j++) {
        if (on) { links[j].setAttribute('aria-current', 'true'); } else { links[j].removeAttribute('aria-current'); }
      }
    }
  }

  // ---- drawer or docked ------------------------------------------------------

  function drawer() {
    return typeof window.matchMedia === 'function' && window.matchMedia('(max-width: 1279px)').matches;
  }

  function renderMode() {
    var open = !host.hidden;
    var modal = open && drawer();
    if (modal) { host.setAttribute('role', 'dialog'); host.setAttribute('aria-modal', 'true'); }
    else { host.removeAttribute('role'); host.removeAttribute('aria-modal'); }
    if (scrim) { scrim.classList.toggle('open', modal); }
    if (layout) { layout.classList.toggle('inspect-open', open); }
    var rest = document.querySelectorAll('[data-modal-inert], .skip-link');
    for (var i = 0; i < rest.length; i++) {
      if (modal) { rest[i].setAttribute('inert', ''); } else { rest[i].removeAttribute('inert'); }
    }
  }

  // ---- sessions that keep unsent input ---------------------------------------

  function renderKept() {
    if (!kept || !keptList) { return; }
    keptList.textContent = '';
    var n = 0;
    frames.forEach(function (rec) {
      if (rec === active || !unsettled(rec.state)) { return; }
      n++;
      var li = document.createElement('li');
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn btn-secondary';
      b.textContent = rec.label;
      b.setAttribute('data-kept', rec.key);
      b.setAttribute('aria-label', 'Show ' + rec.label + ', which keeps ' + facts(rec.state));
      b.addEventListener('click', function () {
        choose({ key: rec.key, worker: rec.worker, session: rec.session, label: rec.label, full: rec.full, src: rec.src }, null);
      });
      li.appendChild(b);
      keptList.appendChild(li);
    });
    kept.hidden = n === 0;
  }

  function facts(s) {
    var out = [];
    var c = s && s.chat;
    if (s && (s.dirty || (c && c.draft))) { out.push(c && c.draft ? 'unsent input (a conversation message among it)' : 'unsent input'); }
    if (s && (s.pending > 0 || (c && c.pending > 0))) { out.push(c && c.pending > 0 ? 'a request on its way (a conversation message among it)' : 'a request on its way'); }
    if (s && (s.unknown > 0 || (c && c.unknown > 0))) { out.push(c && c.unknown > 0 ? 'an unknown delivery (a conversation message among it)' : 'an unknown delivery'); }
    return out.join(', ') || 'nothing';
  }

  // renderDirty marks the page while any frame (shown or kept) holds something that
  // closing would lose: shell.js asks before signing out or leaving the page.
  function renderDirty() {
    var any = frames.some(function (rec) { return unsettled(rec.state); });
    if (any) { host.setAttribute('data-frame-dirty', ''); } else { host.removeAttribute('data-frame-dirty'); }
  }

  // askDiscard is the explicit decision when the cache is full of frames that hold
  // something: the person picks which one to discard, or cancels. Nothing is
  // discarded without that choice.
  function askDiscard(info) {
    if (!dlg || !dlgList || typeof dlg.showModal !== 'function') {
      // No dialog support: the ordinary link is the way (the page asks before it drops unsent input).
      window.location.assign(info.full);
      return;
    }
    dlgList.textContent = '';
    frames.forEach(function (rec) {
      var li = document.createElement('li');
      var t = document.createElement('span');
      t.textContent = rec.label + ' keeps ' + facts(rec.state) + '. ';
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'btn-danger';
      b.textContent = 'Discard it and open ' + info.label;
      b.setAttribute('aria-label', 'Discard ' + rec.label + ' (' + facts(rec.state) + ') and open ' + info.label);
      b.addEventListener('click', function () {
        dlg.close();
        var token = ++seq;
        destroy(rec);
        var made = create(info);
        if (made && token === seq) { show(made, true); }
      });
      li.appendChild(t);
      li.appendChild(b);
      dlgList.appendChild(li);
    });
    dlg.showModal();
  }
  if (dlgCancel) { dlgCancel.addEventListener('click', function () { dlg.close(); }); }

  // ---- messages from the frames ----------------------------------------------

  function count(n) { return typeof n === 'number' && isFinite(n) && n >= 0 && Math.floor(n) === n ? Math.min(n, 1000) : 1; }
  // A field that is missing or malformed counts as "not clean": it can only keep a frame, never discard one.
  function readState(d) {
    var out = { dirty: d.dirty !== false, pending: count(d.pending), unknown: count(d.unknown), conn: typeof d.conn === 'string' ? d.conn.slice(0, 16) : '',
      suspended: d.suspended === true, ctrl: d.ctrl === true };
    // chat is optional (an older frame has none: the totals say it all). Present but malformed counts as holding something.
    if (has(d, 'chat')) {
      var c = d.chat;
      out.chat = c && typeof c === 'object' ? { draft: c.draft !== false, pending: count(c.pending), unknown: count(c.unknown) } : { draft: true, pending: 1, unknown: 1 };
    }
    return out;
  }

  window.addEventListener('message', function (ev) {
    if (ev.origin !== origin) { return; }
    var rec = null;
    for (var i = 0; i < frames.length; i++) {
      var w = frames[i].el.contentWindow;
      if (w && w === ev.source) { rec = frames[i]; break; }
    }
    if (!rec) { return; }
    var d = ev.data;
    if (!d || typeof d !== 'object' || d.minion !== 'inspector' || d.v !== 1) { return; }
    if (typeof d.type !== 'string' || !has(FROM_FRAME, d.type)) { return; }
    if (!d.target || d.target.worker !== rec.worker || d.target.session !== rec.session) { return; }
    if (typeof d.doc !== 'string' || d.doc === '') { return; } // every message names its document
    if (d.type !== 'ready' && d.doc !== rec.doc) { return; } // only the document that said ready speaks
    if (d.type === 'close-request') {
      if (rec !== active || document.querySelector('dialog[open]')) { return; }
      close(true);
      return;
    }
    var s = readState(d);
    if (d.type === 'ready') {
      if (d.doc === rec.doc) { return; } // one ready per document
      // Only the document the frame holds right now can say ready: a ready that was queued by
      // an older document (a reload happened since) names a document that is gone.
      var cur = curDoc(rec);
      if (!cur || cur !== d.doc || !s.ctrl) { return; }
      // The document was rendered for an identity; it must be the one this page was rendered for.
      if (typeof d.ctx !== 'string' || d.ctx === '' || d.ctx !== pageCtx) { authLost('switched'); return; }
      rec.doc = d.doc;
      rec.ctx = d.ctx;
      rec.state = s;
      rec.evicting = null;
      rec.ready = true;
      if (rec.timer !== null) { window.clearTimeout(rec.timer); rec.timer = null; }
      // A frame that loaded (or reloaded) while hidden starts reading: stop it again.
      if (rec.suspended) { send(rec, 'suspend'); }
      applyVisibility(rec);
      if (rec === active) { if (concealed) { setStatus('warning', CONCEAL_TEXT); } else { hideStatus(); } }
    } else if (typeof d.nonce === 'string' && has(rec.queries, d.nonce)) {
      var q = rec.queries[d.nonce];
      rec.state = s;
      if (q) { q(s); }
    } else {
      rec.state = s;
      rec.evicting = null; // it changed: an answer to an earlier question is not about this state
    }
    renderKept();
    renderDirty();
  });

  // ---- the identity of the page -------------------------------------------------

  // fetchJSON is a bounded READ: a credentialed GET with the real cookie, no body, redirects
  // not followed, aborted after GUARD_MS (a request that hangs cannot stop the checks). It
  // answers 'ok' (with the JSON), 'lost' (401 or a redirect to sign-in), 'refused' (403 or
  // 404: this resource, not the sign-in), or 'unknown' (a network error, a 5xx, a timeout, an
  // unreadable answer: it proves nothing either way).
  function fetchJSON(url, cb) {
    if (typeof window.fetch !== 'function') { cb('unknown'); return; }
    var ctl = typeof window.AbortController === 'function' ? new window.AbortController() : null;
    var done = false;
    var timer = window.setTimeout(function () { finish('unknown'); if (ctl) { ctl.abort(); } }, GUARD_MS);
    function finish(res, data) {
      if (done) { return; }
      done = true;
      window.clearTimeout(timer);
      cb(res, data);
    }
    window.fetch(url, { credentials: 'same-origin', cache: 'no-store', redirect: 'manual', headers: { Accept: 'application/json' }, signal: ctl ? ctl.signal : undefined }).then(function (r) {
      var st = r && typeof r.status === 'number' ? r.status : 0;
      if (r && (r.type === 'opaqueredirect' || st === 401)) { finish('lost'); return; }
      if (st === 403 || st === 404) { finish('refused'); return; }
      if (!r || !r.ok || typeof r.json !== 'function') { finish('unknown'); return; }
      r.json().then(function (j) { finish('ok', j); }, function () { finish('unknown'); });
    }, function () { finish('unknown'); });
  }

  // checkPrincipal asks the server who this request is (GET /inspector/principal). The answer is
  // a public, opaque context derived by the server from the principal its session layer
  // authenticated; it is compared with the one this page was rendered for. 'switched' means
  // another sign-in (another user, another act-as target, a new login of the same user) has
  // taken the cookie since, whatever that person may read: an empty 200 list proves nothing
  // about who the requester is, this does. The context is not a credential and grants nothing.
  function checkPrincipal(cb) {
    fetchJSON('/inspector/principal', function (r, j) {
      if (r !== 'ok') { cb(r === 'refused' ? 'unknown' : r); return; }
      cb(pageCtx !== '' && j && j.context === pageCtx ? 'ok' : 'switched');
    });
  }

  // checkFrame is the resource check before a kept frame is shown: the same context, and the
  // server's answer that this request may read this session now (GET <session>/context).
  function checkFrame(rec, cb) {
    fetchJSON(rec.full + '/context', function (r, j) {
      if (r !== 'ok') { cb(r); return; }
      // A frame that never said ready holds nothing to show; one that did carries the context it was rendered for.
      cb(pageCtx !== '' && j && j.context === pageCtx && (rec.ctx === null || rec.ctx === pageCtx) ? 'ok' : 'switched');
    });
  }

  function say(kind, text) {
    if (!alertBox) { return; }
    alertBox.textContent = text;
    alertBox.className = 'notice notice-' + kind;
    alertBox.hidden = !text;
  }

  // authLost removes EVERY kept frame, with its draft and evidence, and closes the pane:
  // nothing of it is shown again before the person signs in (or reloads as the new person).
  // The links are ordinary links from now on.
  function authLost(kind) {
    if (lost) { return; }
    lost = true;
    seq++;
    if (authTimer !== null) { window.clearTimeout(authTimer); authTimer = null; }
    frames.slice().forEach(function (r) { destroy(r); });
    active = null;
    opener = null;
    concealed = false;
    host.hidden = true;
    hideStatus();
    markSelected(null);
    renderMode();
    renderKept();
    renderDirty();
    say('danger', kind === 'switched'
      ? 'Another sign-in (another account, another act-as setting or a new login) took over this browser. The sessions kept in the inspector, with anything you had typed in them, were cleared. Reload the page to continue as the current account.'
      : 'You are signed out, or your session ended. The sessions kept in the inspector, with anything you had typed in them, were cleared. Sign in again to continue.');
  }

  // conceal hides every kept frame (their drafts, evidence and requests on their way stay as
  // they are, only the display and the way in are closed) while the identity is unconfirmed;
  // unconceal shows the active one again once the server confirmed it.
  function conceal() {
    if (concealed || lost) { return; }
    concealed = true;
    frames.forEach(applyVisibility);
    if (!host.hidden) { setStatus('warning', CONCEAL_TEXT); }
    say('warning', CONCEAL_TEXT);
  }
  function unconceal() {
    if (!concealed) { return; }
    concealed = false;
    frames.forEach(applyVisibility);
    if (status && /sign-in could not be confirmed/.test(status.textContent)) { if (active && !active.ready) { setStatus('info', 'Loading the session…'); } else { hideStatus(); } }
    say('', '');
  }

  // monitor is the periodic check while frames are kept. It always ends in a decision and
  // schedules the next one (a hung read is aborted), so the monitoring cannot stop: a good
  // answer repeats in AUTH_MS, an unknown one conceals the kept frames and repeats in RETRY_MS.
  function monitor() {
    authTimer = null;
    if (!frames.length || lost) { return; }
    checkPrincipal(function (r) {
      if (lost) { return; }
      if (r === 'lost' || r === 'switched') { authLost(r); return; }
      if (r === 'ok') {
        if (!concealed || !active) { unconceal(); armAuth(AUTH_MS); return; }
        // The frame that was concealed is not shown again on the strength of who is signed in alone:
        // the server must also say, now, that this request may read THIS session.
        var rec = active;
        checkFrame(rec, function (rr) {
          if (lost) { return; }
          if (rr === 'ok') { unconceal(); armAuth(AUTH_MS); return; }
          if (rr === 'lost' || rr === 'switched') { authLost(rr); return; }
          if (rr === 'refused') { refusedFrame(rec); unconceal(); armAuth(AUTH_MS); return; } // it stays hidden until the check of who is signed in answers
          armAuth(RETRY_MS); // still unconfirmed: stays concealed
        });
        return;
      }
      conceal();
      armAuth(RETRY_MS);
    });
  }

  function armAuth(ms) {
    if (lost) { return; }
    if (authTimer !== null) { window.clearTimeout(authTimer); }
    authTimer = window.setTimeout(monitor, ms);
  }

  // watchAuth keeps the monitor going while frames are kept: the cache is not an
  // authentication cache and ends within one interval of the sign-in ending.
  function watchAuth() {
    if (authTimer !== null || lost) { return; }
    armAuth(AUTH_MS);
  }

  // recheck asks once, for the whole page, after one frame failed: a refused session is
  // ordinary, a changed or lost sign-in is for every kept frame.
  function recheck() {
    if (lost) { return; }
    checkPrincipal(function (r) { if (r === 'lost' || r === 'switched') { authLost(r); } });
  }

  // refusedFrame is the answer to a refusal (403, 404) of ONE session. A refusal does not say
  // why: the session may be gone, or another sign-in may have taken the cookie in another tab (as
  // another user, a new login or another act-as setting: that person may not read what this one
  // did), or the Hub may have restarted and its Worker not be back yet. So it asks, once and
  // bounded, WHO this request is, before it decides anything, and the frame that is shown (if
  // any) waits, hidden, for that answer. Another or a lost sign-in clears EVERY kept frame. The
  // same sign-in makes it an ordinary refusal: a frame that holds nothing is removed; a frame
  // that holds a draft, a request on its way or an unknown delivery is kept, hidden and never
  // shown (nothing of it is lost, nothing is sent), and the person may choose it again to check
  // once more. There is no timer of its own: this is one read per refusal, and a refusal comes
  // from the person's own choice or from the periodic check.
  function refusedFrame(rec) {
    if (lost) { return; }
    verifying++;
    frames.forEach(applyVisibility);
    checkPrincipal(function (r) {
      verifying--;
      if (lost) { return; }
      if (r === 'lost' || r === 'switched') { authLost(r); return; }
      if (r !== 'ok') {
        // Who is asking could not be confirmed: fail closed like every other unconfirmed identity.
        conceal();
        frames.forEach(applyVisibility);
        say('warning', 'Your sign-in could not be confirmed just now, so ' + rec.label + ' was not shown. What you typed there is kept. The check is repeated; you can also open it in full.');
        armAuth(RETRY_MS);
        return;
      }
      resourceGone(rec); // first: a refused frame is parked or removed before visibility is applied, so it is never shown even for a task
      frames.forEach(applyVisibility);
    });
  }

  // resourceGone: the sign-in is the same and the server does not (now) give this session to
  // this request. Clean: nothing is lost by removing it. Not clean: it is kept, hidden.
  function resourceGone(rec) {
    if (frames.indexOf(rec) < 0) { return; }
    var wasActive = rec === active;
    if (clean(rec.state)) {
      destroy(rec);
      if (wasActive) { close(false); }
      say('warning', rec.label + ' is not available to you any more (it was removed or is no longer shared).');
      return;
    }
    if (wasActive) { close(false); } // parks it: hidden, suspended, kept
    renderKept();
    renderDirty();
    say('warning', rec.label + ' is not available to you right now (it may have been removed or unshared, or the Hub may be reconnecting to its Worker). What you typed there and any request on its way are kept, and it is not shown. Choose it from the kept sessions to check again.');
  }

  // ---- keyboard, buttons, list updates ---------------------------------------

  if (closeBtn) { closeBtn.addEventListener('click', function () { close(true); }); }
  if (scrim) { scrim.addEventListener('click', function () { close(true); }); }

  document.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Escape' || ev.defaultPrevented || host.hidden) { return; }
    if (document.querySelector('dialog[open]')) { return; } // the topmost local dialog goes first
    var rail = byId('site-rail');
    if (rail && rail.classList.contains('open')) { return; } // so does the menu
    var inside = !!ev.target && typeof host.contains === 'function' && host.contains(ev.target);
    if (!drawer() && !inside) { return; }
    ev.preventDefault();
    close(true);
  });

  if (typeof window.matchMedia === 'function') {
    var narrow = window.matchMedia('(max-width: 1279px)');
    if (typeof narrow.addEventListener === 'function') { narrow.addEventListener('change', renderMode); }
  }

  // A live update of the list swaps the rows: the selection follows the identity again.
  var reselect = function () { markSelected(active); };
  document.body.addEventListener('htmx:sseMessage', reselect);
  document.body.addEventListener('htmx:afterSwap', reselect);
})();
