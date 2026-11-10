// pipe-chat.js is the read side of the pipe conversation component (#pipe-chat, page_pipe.go).
//
//  - It never writes. Sending is the form of the component, handled by terminal-input.js like every write of the
//    session view (one request, drop-not-queue, outcomes, unknown deliveries kept, "send anyway").
//  - It keeps the turn list (#pipe-turns, rendered by the server from the Hub's stored pipe turns) current: it reads
//    the list again (GET the component's read route, one request at a time, bursts coalesce) when the view's event
//    stream says this session changed, after a pipe send of this page (minion-pipe-refresh), when a hidden view is shown
//    again, and on a slow timer while the view is visible. A hidden (suspended) or paused view reads nothing; an answer
//    that arrives after it was hidden is dropped, never applied and never aborted. The page makes no turns up: what is
//    shown is exactly what the server rendered, as escaped text.
//  - Reading position: a new turn scrolls into view only while the reader is already at the bottom. A reader who scrolled
//    up keeps the position; the new turns are counted ("N new") on the Jump to latest button, announced politely, and
//    the button takes them to the latest turn (and to the list, which is then where the focus is: only on that click).
//    Nothing here ever moves the focus by itself. The position that counts is the one at the moment the answer is applied
//    (the reader may have scrolled while the read was on its way), and a view that was hidden when it first laid out is
//    taken to the newest turn when it first has a size (the layout callback), unless the reader already went up.
//  - A long history is read in steps (page_pipe.go readPipe): an answer that says it is behind (a marker with
//    data-pipe-behind) is followed by the next step at once, until the newest turns are shown. The page says so while it
//    is behind. Every answer is a whole, authorised read: a failed step changes nothing.
//  - Generations: a read belongs to the visibility generation it started in. Hiding the view ends the generation; an
//    answer of an earlier generation is never applied, even after the view is shown again (the resume asks a fresh read).
//  - Live: there is no stream of this component's own. The view's one event stream (terminal-input.js, GET /api/v1/events,
//    authorised by the Hub on every event and re-checked) carries a metadata-only pipe_changed event (Worker id and
//    session name) when a message or a reply of this session was committed, whoever sent it. The view passes it on as the
//    'pipe' signal; it wakes the read and nothing else: the list is always the authorised read of the server, and nothing of
//    the event is shown. While the view's stream is down the page says so in general words and the slow read stays.
(function () {
  'use strict';

  function byId(id) { return document.getElementById(id); }
  var log = byId('pipe-turns');
  var chat = byId('pipe-chat');
  if (!log || !chat || typeof htmx === 'undefined') { return; }

  var readURL = chat.getAttribute('data-pipe-read');
  var jump = byId('pipe-jump');
  var unreadEl = byId('pipe-unread');
  var live = byId('pipe-live');
  var statusEl = byId('pipe-status');
  var MIN_GAP_MS = 300;
  var RETRY_MS = 10000;
  var SLOW_MS = 30000;
  var CHAIN_MS = 50;
  var BOTTOM_PX = 24;

  var busy = false;
  var wanted = false;
  var timer = null;
  var slow = null;
  var stopped = false; // the session is gone or no longer allowed: reads end until the page is reloaded
  var unread = 0;
  var requests = new WeakMap(); // read config -> the visibility generation it started in
  var gen = 0; // bumped when the view is hidden: a read of an earlier generation is never applied
  var mark = null; // the ids the list had when the (first step of the) read started
  var cur = null; // where the reader is, sampled when the answer is about to be applied
  var pos = { bottom: true, top: 0 }; // the reader's last place while the list had a size
  var jumpPending = false; // Jump to latest was pressed: the answer that is on its way goes to the newest
  var walk = ''; // the cursor the next read starts from ('' is the plain read)
  var lastH = log.clientHeight || 0;
  var liveStateEl = byId('pipe-live-state');

  function sess() { return window.minionSession && typeof window.minionSession.status === 'function' ? window.minionSession.status() : null; }
  function reading() {
    var s = sess();
    if (stopped) { return false; }
    return !s || (!s.suspended && !s.resuming && !s.paused && s.stream !== 'offline');
  }
  function note(text) { if (statusEl) { statusEl.textContent = text; } }

  function laidOut() { return log.clientHeight > 0; }
  function atBottom() { return log.scrollHeight - log.scrollTop - log.clientHeight <= BOTTOM_PX; }
  function toBottom() { pos.bottom = true; if (laidOut()) { log.scrollTop = log.scrollHeight; } }
  // Where the reader is now. A list that has no size (its pane is hidden) has no reader position: what it was is kept.
  function sample() { return laidOut() ? { bottom: atBottom(), top: log.scrollTop } : { bottom: pos.bottom, top: pos.top }; }
  function isRead(path) { return typeof path === 'string' && (path === readURL || path.indexOf(readURL + '?') === 0); }
  function marker() { return typeof log.querySelector === 'function' ? log.querySelector('[data-pipe-walk]') : null; }
  function readMarker() {
    var m = marker();
    return { walk: m ? (m.getAttribute('data-pipe-walk') || '') : '', behind: !!(m && m.getAttribute('data-pipe-behind')) };
  }
  function turns() { return typeof log.querySelectorAll === 'function' ? log.querySelectorAll('[data-turn]') : []; }
  function idOf(n) { return n && typeof n.getAttribute === 'function' ? n.getAttribute('data-turn') : null; }

  function showUnread() {
    if (jump) { jump.hidden = unread === 0; }
    if (unreadEl) { unreadEl.textContent = unread > 0 ? '(' + unread + ' new)' : ''; }
  }

  // ---- reading ------------------------------------------------------------------

  function later(ms) {
    if (timer !== null) { return; }
    timer = window.setTimeout(function () { timer = null; refresh(); }, ms);
  }
  function want() { wanted = true; if (reading()) { later(MIN_GAP_MS); } }

  function refresh() {
    if (!wanted || busy || !reading() || !readURL) { return; }
    wanted = false;
    busy = true;
    if (!mark) { // the first step of a read; the following steps of a long history keep it
      var list = turns();
      mark = { ids: [] };
      for (var i = 0; i < list.length; i++) { mark.ids.push(idOf(list[i])); }
    }
    var done = function () { busy = false; if (wanted) { later(MIN_GAP_MS); } };
    var p = htmx.ajax('GET', walk ? readURL + '?walk=' + encodeURIComponent(walk) : readURL, { target: '#pipe-turns', swap: 'innerHTML' });
    if (p && typeof p.then === 'function') { p.then(done, done); } else { done(); }
  }

  document.body.addEventListener('htmx:configRequest', function (ev) {
    var d = ev.detail || {};
    if (String(d.verb || '').toLowerCase() === 'get' && isRead(d.path)) { requests.set(d, gen); }
  });

  // The swap is gated: a view that was hidden or paused while the read was on its way does not change, and neither does a
  // view that was hidden and shown again since (the answer belongs to an earlier generation). The reader's place is
  // sampled here, the last moment before the list is replaced.
  document.body.addEventListener('htmx:beforeSwap', function (ev) {
    var d = ev.detail || {};
    if (!d.requestConfig || !requests.has(d.requestConfig)) { return; }
    if (!reading() || requests.get(d.requestConfig) !== gen) { d.shouldSwap = false; wanted = true; return; }
    cur = sample();
  });

  document.body.addEventListener('htmx:afterSwap', function (ev) {
    var d = ev.detail || {};
    if (!d.requestConfig || !requests.has(d.requestConfig)) { return; }
    var at = cur || sample();
    cur = null;
    var force = jumpPending;
    jumpPending = false;
    var list = turns();
    var mk = readMarker();
    walk = mk.walk;
    var added = 0;
    if (!mk.behind && mark) {
      // The turns that are new: those whose ids were not in the list before the read (the first step of it).
      var known = {};
      for (var i = 0; i < mark.ids.length; i++) { known[mark.ids[i]] = true; }
      for (var j = 0; j < list.length; j++) { if (!known[idOf(list[j])]) { added++; } }
    }
    if (!mk.behind) { mark = null; }
    log.setAttribute('data-latest', list.length ? idOf(list[list.length - 1]) : '');
    log.setAttribute('aria-busy', mk.behind ? 'true' : 'false');
    note('');
    if (force || at.bottom) {
      toBottom();
      unread = 0;
    } else {
      pos = { bottom: false, top: at.top };
      if (laidOut()) { log.scrollTop = at.top; } // the reader's place is kept
      unread += added;
    }
    showUnread();
    if (added > 0 && live && !(force || at.bottom)) { live.textContent = unread + ' new ' + (unread === 1 ? 'turn' : 'turns') + ' in the conversation.'; }
    if (mk.behind) { wanted = true; later(CHAIN_MS); } // the newest turns are not shown yet: the next step
  });

  document.body.addEventListener('htmx:responseError', function (ev) {
    var d = ev.detail || {};
    if (!d.pathInfo || !isRead(d.pathInfo.requestPath)) { return; }
    if (d.requestConfig && requests.has(d.requestConfig) && requests.get(d.requestConfig) !== gen) { wanted = true; return; } // an earlier generation
    mark = null;
    walk = ''; // a failed step starts again from the plain read
    var st = d.xhr && d.xhr.status;
    if (st === 401 || st === 403 || st === 404) {
      stopped = true;
      note('The conversation can no longer be read: this session was removed or your access to it ended. Reload the page. What you typed is kept.');
      return;
    }
    note('The conversation could not be read just now. It is tried again. Messages already shown are unchanged.');
    wanted = true;
    later(RETRY_MS);
  });
  ['htmx:sendError', 'htmx:timeout'].forEach(function (name) {
    document.body.addEventListener(name, function (ev) {
      var d = ev.detail || {};
      if (!d.requestConfig || !requests.has(d.requestConfig)) { return; }
      mark = null;
      walk = '';
      note('The conversation could not be read just now. It is tried again. Messages already shown are unchanged.');
      wanted = true;
      later(RETRY_MS);
    });
  });

  // ---- what asks for a read -------------------------------------------------------

  // A send of this page (any outcome: the broker may have stored the message) and the view's own event stream.
  document.body.addEventListener('minion-pipe-refresh', function () { want(); });
  if (window.minionSession && typeof window.minionSession.subscribe === 'function') {
    window.minionSession.subscribe(function (kind) {
      if (kind === 'suspended') {
        gen++; // every read that is on its way now belongs to a generation that is over
        if (timer !== null) { window.clearTimeout(timer); timer = null; }
        wanted = false;
        walk = '';
        mark = null;
        cur = null;
        return;
      }
      liveSync();
      if (kind === 'resumed' || kind === 'events' || kind === 'pipe') { want(); }
    });
  }
  // A slow safety read while the view is visible (a turn can be committed without a screen event).
  if (typeof window.setInterval === 'function') {
    slow = window.setInterval(function () {
      var hidden = typeof document.visibilityState === 'string' && document.visibilityState === 'hidden';
      liveSync();
      if (reading() && !hidden) { want(); }
    }, SLOW_MS);
  }

  // ---- the reader's place ----------------------------------------------------------

  log.addEventListener('scroll', function () {
    if (laidOut()) { pos = { bottom: atBottom(), top: log.scrollTop }; }
    if (unread > 0 && atBottom()) { unread = 0; showUnread(); }
  });
  if (jump) {
    jump.addEventListener('click', function () {
      jumpPending = true; // an answer that is on its way goes to the newest too
      toBottom();
      unread = 0;
      showUnread();
      if (typeof log.focus === 'function') { log.focus(); } // the person asked for this: the button just went away
    });
  }
  // The list may start hidden (its pane is not the one shown), where it has no size and "the bottom" is nothing. When it
  // first has a size, the layout callback puts it where the reader left it: the newest turns unless they went up.
  var RO = window.ResizeObserver;
  if (typeof RO === 'function') {
    new RO(function () {
      var h = log.clientHeight || 0;
      var shown = lastH === 0 && h > 0;
      lastH = h;
      if (!shown) { return; }
      if (pos.bottom) { log.scrollTop = log.scrollHeight; } else { log.scrollTop = pos.top; }
    }).observe(log);
  }

  // ---- live: the view's event stream is the signal that a turn was committed ----------------------------------

  // The page says in general words (no reason is invented) when the view's stream is not live; the slow read stays.
  function liveSync() {
    if (!liveStateEl) { return; }
    var st = sess();
    var down = !!st && (st.stream === 'offline' || st.stream === 'reconnecting');
    liveStateEl.textContent = down ? 'Live updates are not available right now. The conversation is read again every 30 seconds.' : '';
  }

  var first = readMarker();
  walk = first.walk;
  if (first.behind) { wanted = true; if (reading()) { later(CHAIN_MS); } } // the first render stopped at its page bound
  toBottom(); // the newest turns first; nothing is focused
  showUnread();
  void slow;
})();
