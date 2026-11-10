// terminal-input.js is the small script of the session view (/s/<worker>/<session>).
//
//  - It listens to the Hub's event feed and fetches the screen panel again when
//    a screen_changed or state_changed event names this session. The panel is
//    rendered by the server (html/template); the script never builds markup
//    from event data.
//  - The page's Content-Security-Policy forbids inline styles, so the terminal
//    colours (classes t-fg-N, t-bg-N, t-fgx-rrggbb, t-bgx-rrggbb) are defined
//    here, in a constructed style sheet, one rule per colour that is on screen.
//  - Every SSE snapshot (the first one and those after a reconnect) refreshes the
//    panel; a page rendered while the Worker was offline probes the screen route
//    instead and rebuilds itself when the Worker is back.
//  - It reacts to the events that the write routes send in the HX-Trigger header:
//    minion-screen-refresh fetches the panel; minion-dialog-stale (a refused
//    answer) also drops the typed answer first; minion-input-sent (read per request
//    in htmx:beforeOnLoad) clears the text or answer box if it still holds exactly
//    the delivered, unedited text.
//
//  - Connection (UI only, never a write): the read stream may reconnect by itself;
//    while it is reconnecting or the view is paused the write buttons are
//    disabled and a write that is attempted anyway is refused here, with a
//    message, and never queued or replayed. A write whose answer never arrives
//    (network error, timeout, gateway error; text, key, answer or task save) and a write the Worker itself
//    reports as unknown or partly sent (HX-Trigger minion-input-unknown) are kept
//    as "Delivery is unknown" outcomes, persistently and never repeated by this
//    script. Each keeps the ORIGINAL submitted payload (text, key or answer), the
//    target, the action, an attempt number and the time, as escaped text in this
//    tab's DOM, until the operator dismisses that one outcome. Later edits, sends,
//    results and screen refreshes never touch it; the count is bounded (more
//    unresolved outcomes refuse further writes until one is dismissed); a reload
//    that would discard them waits. Nothing is stored (no web storage, no URL).
//  - Composer (the Text box): a plain Enter sends a non-empty draft ONCE, exactly as the Send
//    button does: the Send button is clicked (button.click(), so the vendored htmx 2.0.11 puts the
//    button's own enter=1 into the request; requestSubmit() would not carry the submitter) and the
//    Hub sends ONE send_text call with submit=true. Shift+Enter is a line break. An Enter that
//    confirms an IME composition, a held (repeating) Enter, an Enter while a text is on its way,
//    while the view is paused, hidden, suspended or reconnecting, or on a blank box sends
//    nothing and queues nothing (htmx would queue a second request of the same form: this script
//    never gives it one). "Paste without Enter" is the separate secondary button. Blank-box Enter
//    is never a remote Enter: that is the Send Enter key.
//  - Remote keys: the Keys tab has ONE explicit keyboard surface (#remote-keys). Only while it has
//    the focus, an arrow key, Enter, Space, Backspace, Delete or Ctrl+C presses the matching key
//    BUTTON of the Keys form (one named key per press, never a held key, never when a key is on its
//    way, never over a local dialog), so every guard of the button applies and nothing is forced or
//    replayed. Tab, Shift+Tab and Escape are never sent: they leave the surface (Escape and the
//    "Back to the page" button hand the focus back; Tab moves it on). No other key of the page and
//    no click on the screen is a remote control.
//  - Pause freezes the screen (no panel swap) without stopping the process.
//  - Suspend (session inspector, frame-child.js): a view that is hidden inside the
//    inspector closes its read stream and its timers and refuses writes ("Suspended").
//    It is NOT the operator's Pause: the two flags are separate, a suspended view
//    that is shown again stays paused if the operator had paused it. Resuming opens
//    the stream again and keeps writes off until ONE fresh read of the screen route,
//    started after the resume (a read of the current generation; an older read, from
//    before the view was hidden, can never unlock writes), has come back with a
//    success (it also re-checks that the session is still allowed and present). That
//    read is separate from the panel swap, so a typed or chosen dialog answer, which
//    holds the swap back, never holds the unlock back: the answer stays as typed,
//    with the reviewed-dialog tokens it was typed for (never re-bound to a newer dialog).
//    A request that is already on its way is not touched by either: its answer is
//    settled by its own form as always. frameState() reports counts and flags only
//    (dirty, pending, unknown); never text.
//
// data attributes of #session-view: data-events-url, data-screen-url,
// data-worker-id, data-session, data-online ("true" when the page has a panel).
(function () {
  'use strict';

  // ---- terminal style sheet -------------------------------------------------

  var BASE16 = ['#000000', '#cd3131', '#0dbc79', '#e5e510', '#2472c8', '#bc3fbc', '#11a8cd', '#e5e5e5',
    '#666666', '#f14c4c', '#23d18b', '#f5f543', '#3b8eea', '#d670d6', '#29b8db', '#ffffff'];
  var CUBE = [0, 95, 135, 175, 215, 255];

  // The fixed terminal styles (t-b, t-dim, t-i, t-u, t-s, t-fg-bgdef, t-bg-fgdef,
  // t-cur) are in minion.css. Only the palette is built here: one rule per colour
  // class that is on screen (t-fg-N, t-bg-N, t-fgx-rrggbb, t-bgx-rrggbb).
  var BASE_CSS = [];

  var sheet = null;
  var defined = {};

  function paletteColor(n) {
    if (n < 16) { return BASE16[n]; }
    if (n < 232) {
      n -= 16;
      return 'rgb(' + CUBE[Math.floor(n / 36)] + ',' + CUBE[Math.floor(n / 6) % 6] + ',' + CUBE[n % 6] + ')';
    }
    var g = 8 + (n - 232) * 10;
    return 'rgb(' + g + ',' + g + ',' + g + ')';
  }

  function installSheet() {
    if (typeof CSSStyleSheet !== 'function' || !('adoptedStyleSheets' in document)) { return; }
    try {
      sheet = new CSSStyleSheet();
      sheet.replaceSync(BASE_CSS.join('\n'));
      document.adoptedStyleSheets = document.adoptedStyleSheets.concat([sheet]);
    } catch (e) {
      sheet = null;
    }
  }

  // defineColor adds the rule of one colour class the first time it is seen.
  function defineColor(cls) {
    if (!sheet || defined[cls]) { return; }
    var m = /^t-(fg|bg)-(\d{1,3})$/.exec(cls);
    var color = null;
    if (m && +m[2] < 256) {
      color = paletteColor(+m[2]);
    } else if ((m = /^t-(fg|bg)x-([0-9a-f]{6})$/.exec(cls))) {
      color = '#' + m[2];
    }
    if (!color) { return; }
    defined[cls] = true;
    var prop = m[1] === 'fg' ? 'color' : 'background-color';
    try { sheet.insertRule('.' + cls + '{' + prop + ':' + color + '}', sheet.cssRules.length); } catch (e) { /* ignore */ }
  }

  function defineColors(root) {
    var spans = root.querySelectorAll('#screen span[class]');
    for (var i = 0; i < spans.length; i++) {
      var list = spans[i].classList;
      for (var j = 0; j < list.length; j++) { defineColor(list[j]); }
    }
  }

  installSheet();
  defineColors(document);

  // ---- live screen ----------------------------------------------------------

  var view = document.getElementById('session-view');
  if (!view || typeof htmx === 'undefined') { return; }

  var screenURL = view.getAttribute('data-screen-url');
  var eventsURL = view.getAttribute('data-events-url');
  var workerID = view.getAttribute('data-worker-id');
  var session = view.getAttribute('data-session');
  var targetName = view.getAttribute('data-target') || session;
  // A page rendered while the Worker was offline has no screen panel.
  var online = view.getAttribute('data-online') === 'true';
  var MIN_GAP_MS = 200;
  var RETRY_MS = 1000;

  var busy = false;
  var pending = false;
  var timer = null;
  var scroll = null;
  // paused freezes the screen; stream is the state of the read stream.
  var paused = false;
  // suspended: the inspector hides this view (reads stopped); resuming: shown again,
  // writes stay off until a fresh panel read has come back. Both are apart from paused.
  var suspended = false;
  var resuming = false;
  // readGen counts the suspensions and resumptions; the resume read belongs to one
  // generation and an answer of another one is ignored (it fails closed).
  var readGen = 0;
  var ageTimer = null;
  var stream = 'connecting'; // connecting | live | reconnecting | offline
  var lastUpdate = Date.now();
  var answerKept = null;

  function byId(id) { return document.getElementById(id); }
  function qsa(sel) { return typeof document.querySelectorAll === 'function' ? document.querySelectorAll(sel) : []; }
  function qs(sel) { return typeof document.querySelector === 'function' ? document.querySelector(sel) : null; }

  // Only an editable field with text in it is a draft. A button keeps focus
  // after a click (a quick answer) and must never hold the refresh back.
  var NOT_TEXT = { button: 1, submit: 1, reset: 1, checkbox: 1, radio: 1, hidden: 1, image: 1, file: 1, range: 1, color: 1 };

  function isDraftField(a) {
    if (!a || typeof a.value !== 'string' || a.value === '') { return false; }
    var tag = String(a.tagName || '').toLowerCase();
    if (tag === 'textarea') { return true; }
    return tag === 'input' && !NOT_TEXT[String(a.type || 'text').toLowerCase()];
  }

  // A draft is text that was typed into an editable field of the panel and has
  // not been delivered: the answer box (focused or not) or any other focused
  // editable field of the panel. It is checked twice: before a refresh starts
  // and, because the answer box can be typed into while the request is on its
  // way, again when the response is about to be swapped in (htmx:beforeSwap).
  function panelHasDraft() {
    if (isDraftField(document.getElementById('answer-choice'))) { return true; }
    var a = document.activeElement;
    return !!(a && a.closest && a.closest('#screen-panel') && isDraftField(a));
  }

  function later(ms) {
    if (timer !== null || suspended) { return; }
    timer = window.setTimeout(function () { timer = null; refresh(); }, ms);
  }

  // schedule asks for a refresh; bursts of events make one fetch.
  function schedule() { pending = true; later(MIN_GAP_MS); }

  // The read side of other components of this view (the pipe conversation, pipe-chat.js) follows the same stream
  // and the same suspension without a second connection: window.minionSession.subscribe(fn) calls fn(kind) with
  // 'events' (this session changed, a snapshot arrived, or a pause ended: read again), 'pipe' (a message or reply of this
  // session's pipe conversation was committed: the event carries nothing of it), 'suspended' (the view is hidden:
  // read nothing) and 'resumed' (shown again and the fresh read of the screen route came back: reads may start).
  // status() answers flags only. Nothing here writes.
  var subscribers = [];
  function signal(kind) {
    subscribers.slice().forEach(function (fn) { try { fn(kind); } catch (e) { /* a listener cannot break the view */ } });
  }
  window.minionSession = {
    subscribe: function (fn) { if (typeof fn === 'function') { subscribers.push(fn); } },
    status: function () { return { suspended: suspended, paused: paused, resuming: resuming, stream: stream, writes: writesAllowed() }; }
  };

  // probe runs on a page that was rendered while the Worker was offline (it has
  // no panel to swap): when the screen route answers again, the Worker is back
  // and the page is built from scratch. A Worker that is still offline answers
  // 503, so this never loops.
  function probe() {
    pending = false;
    if (typeof window.fetch !== 'function') { return; }
    busy = true;
    var done = function () {
      busy = false;
      if (pending) { later(RETRY_MS); }
    };
    window.fetch(screenURL, { credentials: 'same-origin', cache: 'no-store' }).then(function (r) {
      done();
      if (r && r.ok) { reload(); }
    }, done);
  }

  function refresh() {
    if (!pending) { return; }
    if (busy) { return; }
    if (suspended) { return; } // a suspended view reads nothing; resuming asks again
    if (!online) { probe(); return; }
    if (paused) { return; } // resuming asks again
    if (panelHasDraft()) { later(RETRY_MS); return; } // do not wipe a half typed answer
    pending = false;
    busy = true;
    var screen = document.getElementById('screen');
    scroll = screen ? { top: screen.scrollTop, left: screen.scrollLeft } : null;
    var done = function () {
      busy = false;
      if (pending) { later(MIN_GAP_MS); }
    };
    var p = htmx.ajax('GET', screenURL, { target: '#screen-panel', swap: 'outerHTML' });
    if (p && typeof p.then === 'function') { p.then(done, done); } else { done(); }
  }

  // ---- connection state (what the page shows; it never writes) --------------

  var TEXT = { connecting: 'Connecting', live: 'Live', paused: 'Paused', reconnecting: 'Reconnecting', offline: 'Offline', suspended: 'Suspended', resuming: 'Refreshing' };
  var NOTE = {
    suspended: 'Updates are suspended while this view is hidden. They resume with a fresh read when you return. Text you typed is kept.',
    resuming: 'Refreshing the screen after the view was hidden. Sending is unavailable until the screen is current. Text you typed is kept.',
    paused: 'Paused. The screen is frozen at the last capture and sending is unavailable. The session itself keeps running. Resume to see the current screen.',
    reconnecting: 'Reconnecting. The screen may be out of date and sending is unavailable until the connection returns. Text you typed is kept.',
    offline: 'Live updates stopped. Reload the page to reconnect. Sending is unavailable.'
  };
  var ANNOUNCE = {
    live: 'Live. Updates and sending are available.',
    paused: 'Paused. Sending is unavailable until you resume.',
    reconnecting: 'Reconnecting. Sending is unavailable.',
    offline: 'Live updates stopped. Sending is unavailable.'
  };
  var announced = '';

  function shown() { return paused ? 'paused' : suspended ? 'suspended' : resuming ? 'resuming' : stream; }
  function writesAllowed() { return !paused && !suspended && !resuming && stream !== 'reconnecting' && stream !== 'offline'; }

  function ageText() {
    var secs = Math.max(0, Math.round((Date.now() - lastUpdate) / 1000));
    var unit = secs === 1 ? 'second' : 'seconds';
    if (secs >= 120) { secs = Math.round(secs / 60); unit = 'minutes'; }
    return secs + ' ' + unit;
  }

  function renderAge() {
    var el = byId('age');
    if (!el) { return; }
    el.textContent = (writesAllowed() ? 'Updated ' : 'The last screen update was ') + ageText() + ' ago';
  }

  // applyGate shows the write buttons as unavailable. It is a cue: the request is
  // refused in htmx:beforeRequest whatever the buttons look like, because htmx
  // enables the buttons of a finished request again.
  function applyGate() {
    var ok = writesAllowed();
    var list = qsa('[data-write] button');
    for (var i = 0; i < list.length; i++) {
      var b = list[i];
      if (!ok) {
        b.setAttribute('data-gated', '1');
        b.disabled = true;
      } else if (b.getAttribute('data-gated')) {
        b.removeAttribute('data-gated');
        b.disabled = false;
      }
    }
  }

  function render(announce) {
    var st = shown();
    var c = byId('conn');
    if (c) { c.setAttribute('data-conn', st); c.textContent = TEXT[st]; }
    var note = byId('conn-note');
    if (note) {
      note.textContent = NOTE[st] || '';
      note.hidden = !NOTE[st];
    }
    var live = byId('conn-live');
    if (live && announce && announced !== st && ANNOUNCE[st]) { live.textContent = ANNOUNCE[st]; }
    if (announce) { announced = st; }
    applyGate();
    renderAge();
  }

  function setStream(next) {
    if (stream === next) { return; }
    stream = next;
    render(true);
  }

  function setPaused(on) {
    if (paused === on) { return; }
    paused = on;
    var b = byId('pause-toggle');
    if (b) {
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
      var label = typeof b.querySelector === 'function' ? b.querySelector('span') : null;
      if (label) { label.textContent = on ? 'Resume updates' : 'Pause'; }
    }
    render(true);
    if (!on) { schedule(); signal('events'); }
  }

  function startAge() {
    if (ageTimer === null && typeof window.setInterval === 'function') { ageTimer = window.setInterval(renderAge, 5000); }
  }

  (function initControls() {
    var b = byId('pause-toggle');
    if (b && typeof b.addEventListener === 'function') {
      b.hidden = false;
      b.addEventListener('click', function () { setPaused(!paused); });
    }
    startAge();
    render(false);
  })();

  // ---- results the page itself reports ---------------------------------------

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text !== undefined) { e.textContent = text; }
    return e;
  }

  function clock() {
    var d = new Date();
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return p(d.getUTCHours()) + ':' + p(d.getUTCMinutes()) + ':' + p(d.getUTCSeconds()) + ' UTC';
  }

  function resultBox(kind, title, text) {
    var box = el('div', 'result result-' + kind);
    box.setAttribute('role', 'status');
    var head = el('p', 'result-title');
    head.appendChild(el('strong', '', title + ':'));
    head.appendChild(document.createTextNode(' ' + text));
    box.appendChild(head);
    box.appendChild(el('p', 'result-meta', targetName + ' at ' + clock()));
    return box;
  }

  // A refused attempt (paused or reconnecting) is shown like any result.
  function showResult(kind, title, text) {
    var host = byId('session-result');
    if (!host) { return; }
    host.textContent = '';
    host.appendChild(resultBox(kind, title, text));
  }

  // ---- unknown outcomes ------------------------------------------------------
  //
  // An unknown delivery is the one outcome nothing else on the page can settle. It
  // stays until the operator dismisses it, with the payload that the request
  // carried (read from the request itself when it was configured, not from the box,
  // which the operator may have edited or cleared since). The page never sends
  // again.
  var MAX_UNKNOWN = 10;
  var unknowns = [];
  var unknownSeq = 0;
  var reloadWanted = false;
  var captured = new WeakMap();
  // flying: the tracked writes that left and are not settled yet; their count is
  // reported to the inspector (a view with a request on its way is never discarded).
  var flying = new WeakSet();
  var inflight = 0;
  // flyingKind counts the tracked writes on their way by kind: a second text, or a second key, is
  // refused while one is on its way (htmx would otherwise QUEUE it on the same form and send it later).
  var flyingKind = { text: 0, keys: 0, answer: 0, task: 0, pipe: 0 };
  var kindOf = new WeakMap();
  var ACTION = [[/\/text$/, 'Text'], [/\/keys$/, 'Key'], [/\/answer$/, 'Answer'], [/\/task$/, 'Task'], [/\/pipe$/, 'Message']];

  function kindOfPath(path) {
    var m = /\/(text|keys|answer|task|pipe)$/.exec(String(path || '').split('?')[0]);
    return m ? m[1] : '';
  }

  function actionOf(path) {
    for (var i = 0; i < ACTION.length; i++) { if (ACTION[i][0].test(path)) { return ACTION[i][1]; } }
    return 'Write';
  }

  // tokensOfRequest is the reviewed-dialog identity a request posted: its expect_*
  // parameters, in the order of the form, written like tokensOf writes the form's
  // hidden fields. Empty when the request carries none.
  function tokensOfRequest(d) {
    var out = [];
    if (d.formData && typeof d.formData.entries === 'function') {
      var it = d.formData.entries();
      for (var e = it.next(); !e.done; e = it.next()) {
        if (/^expect_/.test(e.value[0])) { out.push(e.value[0] + '=' + e.value[1]); }
      }
    } else if (d.parameters) {
      Object.keys(d.parameters).forEach(function (k) { if (/^expect_/.test(k)) { out.push(k + '=' + d.parameters[k]); } });
    }
    return out.join('&');
  }

  // dialogStillShown tells whether the answer form on the page still represents the
  // dialog a request was reviewed for. It is true when there is no form (nothing
  // to protect) or the request carried no identity (the server refuses such an
  // answer as "the page lost the dialog it showed"); false only for a form that
  // shows another dialog.
  function dialogStillShown(c) {
    var f = answerForm();
    if (!c || !c.dialog || !f) { return true; }
    return tokensOf(f) === c.dialog;
  }

  // capture remembers what a write of the session routes carries.
  function capture(d) {
    var path = String(d.path || '').split('?')[0];
    if (!isTrackedPath(path)) { return; }
    var payload = null;
    var note = '';
    var submit = null;
    if (/\/task$/.test(path)) {
      payload = formValue(d, 'task');
      note = 'The task is a label for the session: saving it sends no terminal input, but it may have changed the label.';
    } else if (/\/text$/.test(path)) {
      payload = formValue(d, 'text');
      // The submit flag the request really carried (read from the request, as htmx built it).
      submit = formValue(d, 'enter') === '1';
      note = submit ? 'Enter was to be pressed after the text (one request, submit=true).' : 'The text was pasted without Enter (submit=false).';
    } else if (/\/pipe$/.test(path)) {
      payload = formValue(d, 'text');
      note = 'The message was to be sent through the pipe as one message to the agent (one pipe_send request). It was not typed into the terminal.';
    } else if (/\/keys$/.test(path)) {
      payload = formValue(d, 'key');
    } else {
      payload = formValue(d, 'choice_text');
      if (typeof payload !== 'string' || payload.trim() === '') { payload = formValue(d, 'choice'); }
    }
    captured.set(d, { action: actionOf(path), payload: typeof payload === 'string' ? payload : null, note: note, submit: submit, sentAt: clock(), dialog: /\/answer$/.test(path) ? tokensOfRequest(d) : '' });
  }

  function reloadNote(host) {
    var n = null;
    for (var i = 0; i < host.children.length; i++) { if (host.children[i].getAttribute('data-reload-note')) { n = host.children[i]; } }
    return n;
  }

  function dismissUnknown(rec) {
    var i = unknowns.indexOf(rec);
    if (i >= 0) { unknowns.splice(i, 1); }
    if (rec.box.parentNode) { rec.box.parentNode.removeChild(rec.box); }
    var host = byId('session-unknown');
    if (unknowns.length === 0) {
      var note = host ? reloadNote(host) : null;
      if (note && note.parentNode) { note.parentNode.removeChild(note); }
      if (reloadWanted && inflight === 0) { reloadWanted = false; reload(true); return; }
    }
    // The focus was on the Dismiss button that is gone: hand it to the next one, or to the text box.
    var next = unknowns.length ? unknowns[Math.min(i, unknowns.length - 1)].dismiss : byId('send-text');
    if (next && typeof next.focus === 'function') { next.focus(); }
  }

  // showUnknown keeps one outcome. how is "transport" (the answer never came) or
  // "worker" (the Worker answered unknown or partial); info is the Worker's detail.
  function showUnknown(d, how, info) {
    var host = byId('session-unknown');
    if (!host) { return; }
    var c = captured.get(d) || { action: 'Write', payload: null, note: '', sentAt: clock() };
    var partial = how === 'worker' && info && info.outcome === 'partial';
    var reason = how === 'worker' && info && typeof info.reason === 'string' && info.reason ? ' (' + info.reason + ')' : '';
    var id = ++unknownSeq;
    var head = partial ? 'Partly sent' : 'Delivery is unknown';
    var what = how === 'worker'
      ? (partial ? 'The Worker reports that only part of this was sent' : 'The Worker reports that it cannot tell whether this was delivered') + reason + '.'
      : 'The connection failed before the answer arrived, so the page cannot tell whether this was sent.';
    var box = el('div', 'result result-warning');
    box.setAttribute('role', 'status');
    box.setAttribute('data-unknown', how);
    box.setAttribute('data-unknown-id', String(id));
    box.setAttribute('data-unresolved', '');
    var title = el('p', 'result-title');
    title.appendChild(el('strong', '', head + ':'));
    title.appendChild(document.createTextNode(' ' + what + ' Inspect the session before deciding whether to send again. Nothing was resent. What was submitted is shown here until you dismiss this.'));
    box.appendChild(title);
    box.appendChild(el('p', 'result-meta', c.action + ' attempt ' + id + ' to ' + targetName + ', sent at ' + c.sentAt + ', reported at ' + clock()));
    box.appendChild(el('pre', 'unknown-payload', c.payload === null ? '(the page could not read what was submitted)' : c.payload));
    if (c.note) { box.appendChild(el('p', 'result-meta', c.note)); }
    var b = el('button', 'btn btn-secondary', 'Dismiss');
    b.type = 'button';
    b.setAttribute('aria-label', 'Dismiss the unknown ' + c.action.toLowerCase() + ' attempt ' + id);
    var rec = { id: id, box: box, dismiss: b, action: c.action };
    b.addEventListener('click', function () { dismissUnknown(rec); });
    box.appendChild(b);
    unknowns.push(rec);
    var note = reloadNote(host);
    if (note) { host.insertBefore ? host.insertBefore(box, note) : host.appendChild(box); } else { host.appendChild(box); }
  }

  // WRITE: the routes that send terminal input; the view being paused or reconnecting
  // refuses them. TRACKED adds the task save: it sends no terminal input (so a paused
  // view does not refuse it) but it is a write that can change the label, so its
  // unknown outcomes are kept, and bounded, like the others.
  var WRITE = /\/(text|keys|answer|pipe)$/;
  var TRACKED = /\/(text|keys|answer|task|pipe)$/;
  function isWritePath(p) { return typeof p === 'string' && WRITE.test(p.split('?')[0]); }
  function isTrackedPath(p) { return typeof p === 'string' && TRACKED.test(p.split('?')[0]); }

  // notSentText says why a write cannot be sent now, or '' when it can.
  function notSentText() {
    if (writesAllowed()) { return ''; }
    if (paused) { return 'The view is paused. Resume it before you send anything. Nothing was sent. Your text is kept.'; }
    if (stream === 'offline') { return 'Live updates stopped, or your access to this session ended. Reload the page. Nothing was sent. Your text is kept.'; }
    return 'The connection is being restored. Nothing was sent. Your text is kept.';
  }

  document.body.addEventListener('htmx:beforeRequest', function (ev) {
    var d = ev.detail || {};
    var path = d.pathInfo && d.pathInfo.requestPath;
    if (!path && d.requestConfig) { path = d.requestConfig.path; }
    if (!isTrackedPath(path)) { return; }
    if (unknowns.length >= MAX_UNKNOWN) {
      ev.preventDefault();
      showResult('danger', 'Not sent', unknowns.length + ' earlier deliveries are still unknown. Check the session, then Dismiss one of them before you send anything else. Nothing was sent. Your text is kept.');
      return;
    }
    if (isWritePath(path) && !writesAllowed()) {
      ev.preventDefault();
      showResult('warning', 'Not sent', notSentText());
      return;
    }
    // The same blank rule where htmx has built the request: an Enter-send (enter=1) of a whitespace-only text is refused here
    // too. Paste without Enter (enter=0) keeps its explicit whitespace.
    if (/\/text$/.test(String(path).split('?')[0]) && formValue(d.requestConfig || d, 'enter') === '1') {
      var tx = formValue(d.requestConfig || d, 'text');
      if (typeof tx === 'string' && tx.trim() === '') {
        ev.preventDefault();
        noteComposer(BLANK_NOTE);
      }
    }
    if (/\/pipe$/.test(String(path).split('?')[0])) {
      var pm = formValue(d.requestConfig || d, 'text');
      if (typeof pm === 'string' && pm.trim() === '') {
        ev.preventDefault();
        notePipe(BLANK_PIPE_NOTE);
      }
    }
  });

  // A click on the Send buttons or on a key button while one text, or one key, is already on its way is
  // refused out loud (the forms say hx-sync="this:drop", so htmx would drop it silently, and never queue it).
  document.body.addEventListener('click', function (ev) {
    var t = ev.target;
    if (!t || typeof t.closest !== 'function') { return; }
    if (t.closest('#send-primary') && blankDraft()) {
      // The primary Send is the composer's Enter: a blank draft is never sent, and never becomes a remote Enter.
      ev.preventDefault();
      noteComposer(BLANK_NOTE);
      return;
    }
    if (t.closest('#pipe-send') && blankPipeDraft()) {
      ev.preventDefault();
      notePipe(BLANK_PIPE_NOTE);
      return;
    }
    var kind = t.closest('#send-primary, #send-paste') ? 'text' : t.closest('#pipe-send') ? 'pipe' : t.closest('[data-keys] button[name="key"]') ? 'keys' : '';
    if (!kind || flyingKind[kind] === 0) { return; }
    ev.preventDefault();
    showResult('warning', 'Not sent', (kind === 'text' ? 'A text' : kind === 'pipe' ? 'A message' : 'A key') + ' is still on its way. Wait for its result before you send again. Nothing was sent and nothing was queued.');
  });

  // A held Enter is one Enter, on every control that writes. The browser activates a focused button (and submits a
  // form from a field) on EVERY auto-repeated Enter keydown, so a key held past the first response would send again after
  // the in-flight guard above has let go. The repeat is therefore cancelled at the key, where it is still only a key
  // (never a click): the first Enter of a press, a later separate press, Space (which a button activates once, on
  // keyup), Tab, Shift+Enter in the text box and local editing keys are untouched. Nothing is queued or replayed.
  // A repeat is the flag the browser sets, or a second Enter keydown with no keyup in between (a synthetic holder).
  var heldEnter = null;
  function writeControl(t) {
    if (!t || typeof t.closest !== 'function') { return null; }
    if (String(t.tagName || '').toUpperCase() === 'TEXTAREA') { return null; } // the composer owns its Enter and its line breaks
    return t.closest('[data-write]') ? t : null;
  }
  document.body.addEventListener('keydown', function (ev) {
    if (!ev || ev.key !== 'Enter' || ev.isComposing || ev.keyCode === 229) { return; }
    var t = writeControl(ev.target);
    if (!t) { return; }
    if (ev.repeat === true || heldEnter !== null) {
      if (typeof ev.preventDefault === 'function') { ev.preventDefault(); }
      return;
    }
    heldEnter = t;
  }, true);
  document.body.addEventListener('keyup', function (ev) { if (ev && ev.key === 'Enter') { heldEnter = null; } }, true);
  if (typeof window.addEventListener === 'function') { window.addEventListener('blur', function () { heldEnter = null; }); }

  document.body.addEventListener('htmx:afterSwap', function () {
    var screen = document.getElementById('screen');
    if (!screen) { return; }
    if (scroll) { screen.scrollTop = scroll.top; screen.scrollLeft = scroll.left; }
    defineColors(document);
  });

  document.body.addEventListener('minion-screen-refresh', schedule);

  // dropAnswer forgets the answer typed or chosen for a dialog that is gone. A new
  // edit revision keeps a late acknowledgement from touching what is typed next.
  function dropAnswer() {
    var box = document.getElementById('answer-choice');
    if (box) {
      box.value = '';
      revs['answer-choice'] = (revs['answer-choice'] || 0) + 1;
    }
    var radios = qsa('[data-answer-form] input[type="radio"]');
    for (var i = 0; i < radios.length; i++) { radios[i].checked = false; }
  }

  // A refused answer (minion-dialog-stale, from the HX-Trigger header): the
  // dialog the user reviewed is gone. The typed answer was meant for that
  // dialog, so it is dropped (in settle, below, and only while the form on the
  // page still represents the dialog the refused request was reviewed for) and the
  // panel is fetched at once. The user reads the new dialog and answers again;
  // nothing is re-sent for them. This bubbling handler only asks for the refresh:
  // a late event of a request for dialog A must never wipe what is typed or chosen
  // for dialog B, and it cannot tell which request it belongs to.
  document.body.addEventListener('minion-dialog-stale', function () {
    schedule();
  });

  // Drafts and their acknowledgements.
  //
  // Each typed field (the text box and the dialog answer box) counts its edits
  // in revs. A request that carries typed text gets a record (meta) with the field,
  // the text and the revision of that draft, kept in a WeakMap under the request's
  // own htmx request config: the record belongs to that request and to nothing
  // else, so the answer to one request can never use another request's record. A
  // "Send anyway" retry of a refused request reuses the record of the ORIGINAL
  // request (same text), so edits made after the first send (also A, AB, A) are
  // still edits. When a response says "delivered" (the HX-Trigger header names
  // minion-input-sent), the field is cleared only if it still holds exactly the
  // delivered text at the delivered revision. That also releases the refresh guard
  // of a typed answer; a newer draft is never erased.
  //
  // The response of a request is settled by the request's OWN form, with a
  // one-time listener (htmx dispatches its events on the element that issued the
  // request): the form can be replaced by the result of another write before its
  // answer arrives, and events of a detached element never reach document.body.
  // Without the listener the answer would be lost, and a delivered text would stay
  // in the box as a draft that holds the refresh back.
  var TYPED = { 'send-text': true, 'answer-choice': true, 'pipe-text': true };
  var revs = {};
  var origin = {};
  var records = new WeakMap();
  var panelRequests = new WeakSet();
  var finished = new WeakSet();
  // ender: request config -> removes the one-time listeners of that request's form.
  var ender = new WeakMap();

  function formValue(d, name) {
    if (d.formData && typeof d.formData.get === 'function') { return d.formData.get(name); }
    return d.parameters ? d.parameters[name] : undefined;
  }

  // typedRequest tells which field a request to the session routes sends text
  // from, and the text. A quick-answer button posts "choice" without any typing.
  function typedRequest(d) {
    var path = String(d.path || '');
    var force = formValue(d, 'force') === '1';
    if (/\/text$/.test(path)) {
      var text = formValue(d, 'text');
      return typeof text === 'string' ? { field: 'send-text', text: text, force: force, trim: false } : null;
    }
    if (/\/pipe$/.test(path)) {
      var msg = formValue(d, 'text');
      return typeof msg === 'string' ? { field: 'pipe-text', text: msg, force: force, trim: false } : null;
    }
    if (/\/answer$/.test(path)) {
      var typed = formValue(d, 'choice_text');
      if (typeof typed !== 'string' || typed.trim() === '') {
        // A forced retry of a typed answer carries it as "choice".
        typed = force ? formValue(d, 'choice') : '';
      }
      return typeof typed === 'string' && typed.trim() !== '' ? { field: 'answer-choice', text: typed, force: force, trim: true } : null;
    }
    return null;
  }

  function sameDraft(a, b, trim) { return trim ? a.trim() === b.trim() : a === b; }

  function track(d) {
    var t = typedRequest(d);
    if (!t) { return; }
    var o = origin[t.field];
    var meta = o && t.force && sameDraft(o.text, t.text, t.trim) ? o : { field: t.field, text: t.text, rev: revs[t.field] || 0 };
    if (!(o && meta === o)) { origin[t.field] = meta; }
    records.set(d, meta);
  }

  document.body.addEventListener('input', function (ev) {
    var id = ev.target && ev.target.id;
    if (TYPED[id]) { revs[id] = (revs[id] || 0) + 1; }
  });

  document.body.addEventListener('htmx:configRequest', function (ev) {
    var d = ev.detail || {};
    if (String(d.verb || '').toLowerCase() === 'get' && typeof d.path === 'string' && d.path.indexOf(screenURL) === 0) {
      panelRequests.add(d);
      return;
    }
    capture(d);
    track(d);
  });

  function triggersOf(xhr) {
    var h = xhr && typeof xhr.getResponseHeader === 'function' ? xhr.getResponseHeader('HX-Trigger') : null;
    if (!h) { return {}; }
    try {
      var o = JSON.parse(h);
      return o && typeof o === 'object' ? o : {};
    } catch (e) { return {}; }
  }

  function deliveredBy(xhr) { return Object.prototype.hasOwnProperty.call(triggersOf(xhr), 'minion-input-sent'); }

  // focusComposerAfterSend hands the focus back to the text box after a delivered send, but only
  // where it was (the box, the Send buttons that were just used, or nowhere because the button that was
  // clicked became disabled): a person who has gone on to another field (the task, an answer, a key
  // surface, a link) is never pulled back into the composer by a late answer.
  function focusComposerAfterSend(box) {
    var a = document.activeElement;
    var form = box.form || null;
    var inForm = !!(a && form && typeof form.contains === 'function' && form.contains(a));
    if (!a || a === document.body || a === box || inForm) { box.focus(); }
  }

  // settle is the one place that reads the response of a request that carried
  // typed text, a write of the session routes, or both. It runs once per request.
  function settle(d, kind, xhr) {
    if (!d || finished.has(d)) { return; }
    var path = String(d.path || '');
    var write = isTrackedPath(path);
    var meta = records.get(d);
    if (!meta && !write) { return; }
    finished.add(d);
    if (flying.has(d)) {
      flying.delete(d);
      inflight--;
      var fk = kindOf.get(d);
      if (fk && flyingKind[fk] > 0) { flyingKind[fk]--; }
    }
    if (kind === 'load') {
      var status = xhr && typeof xhr.status === 'number' ? xhr.status : 200;
      var trig = triggersOf(xhr);
      if (write && (status === 0 || status >= 500)) { showUnknown(d, 'transport'); }
      else if (write && status >= 400) { showResult('danger', 'Not sent', 'The Hub refused the request (status ' + status + '). Your text is kept.'); }
      else if (write && Object.prototype.hasOwnProperty.call(trig, 'minion-input-unknown')) { showUnknown(d, 'worker', trig['minion-input-unknown']); }
      if (Object.prototype.hasOwnProperty.call(trig, 'minion-dialog-stale')) {
        if (dialogStillShown(captured.get(d))) { dropAnswer(); }
        schedule();
      }
      else if (Object.prototype.hasOwnProperty.call(trig, 'minion-screen-refresh')) { schedule(); }
      if (meta && deliveredBy(xhr)) {
        var box = document.getElementById(meta.field);
        if (box && box.value === meta.text && (revs[meta.field] || 0) === meta.rev) {
          box.value = '';
          if (meta.field === 'send-text' || meta.field === 'pipe-text') { focusComposerAfterSend(box); }
        }
        if (origin[meta.field] === meta) { delete origin[meta.field]; }
      }
    } else if (write) {
      // The request left, the answer never came: nothing here can tell whether it
      // arrived. Report it; never send it again.
      showUnknown(d, 'transport');
    }
    // The request is settled: its one-time listeners on the form are not needed any more.
    var done = ender.get(d);
    if (done) { done(); }
    applyGate();
    // A reload that waited for this request goes ahead now, unless it left an unknown outcome to read.
    if (reloadWanted && inflight === 0 && unknowns.length === 0) {
      reloadWanted = false;
      var h = byId('session-unknown');
      var n = h ? reloadNote(h) : null;
      if (n && n.parentNode) { n.parentNode.removeChild(n); }
      window.location.reload();
    }
  }

  // Normal path: the events reach the body (the form is still in the page).
  document.body.addEventListener('htmx:beforeOnLoad', function (ev) {
    var d = ev.detail || {};
    settle(d.requestConfig, 'load', d.xhr);
  });
  ['htmx:sendError', 'htmx:timeout', 'htmx:sendAbort'].forEach(function (name) {
    document.body.addEventListener(name, function (ev) {
      var d = ev.detail || {};
      settle(d.requestConfig, 'transport', d.xhr);
    });
  });
  document.body.addEventListener('htmx:afterRequest', applyGate);

  // Detached path: one-time listeners on the request's own form. They are attached
  // when the request is really sent and removed when the request is SETTLED, not
  // when htmx says "afterRequest": on a failure htmx triggers htmx:afterRequest
  // first and only then htmx:sendError, htmx:timeout or htmx:sendAbort (see
  // static/htmx.min.js, xhr.onerror / onabort / ontimeout), so removing them at
  // afterRequest would drop exactly the failures of a form that another result has
  // already detached (its events never reach document.body). A request whose
  // afterRequest comes with no answer and no failure event in the same task is
  // reported as unknown too, then the listeners go.
  document.body.addEventListener('htmx:beforeSend', function (ev) {
    var d = ev.detail || {};
    var cfg = d.requestConfig;
    var form = d.elt;
    if (!cfg || !form || typeof form.addEventListener !== 'function') { return; }
    if (!records.has(cfg) && !isTrackedPath(String(cfg.path || ''))) { return; }
    if (isTrackedPath(String(cfg.path || '')) && !flying.has(cfg) && !finished.has(cfg)) {
      flying.add(cfg);
      inflight++;
      var k = kindOfPath(cfg.path);
      if (k) { kindOf.set(cfg, k); flyingKind[k]++; }
    }
    var onLoad = function (e) { var x = (e && e.detail) || {}; if (x.requestConfig === cfg) { settle(cfg, 'load', x.xhr); } };
    var onFail = function (e) { var x = (e && e.detail) || {}; if (x.requestConfig === cfg) { settle(cfg, 'transport', x.xhr); } };
    var names = [['htmx:beforeOnLoad', onLoad], ['htmx:sendError', onFail], ['htmx:timeout', onFail], ['htmx:sendAbort', onFail]];
    var onAfter = function (e) {
      var x = (e && e.detail) || {};
      if (x.requestConfig !== cfg) { return; }
      if (finished.has(cfg)) { end(); return; }
      // Not settled yet: the failure event follows in the same task. Keep listening.
      window.setTimeout(function () { if (!finished.has(cfg)) { settle(cfg, 'transport', null); } end(); }, 0);
    };
    var end = function () {
      names.forEach(function (n) { form.removeEventListener(n[0], n[1]); });
      form.removeEventListener('htmx:afterRequest', onAfter);
    };
    ender.set(cfg, end);
    names.forEach(function (n) { form.addEventListener(n[0], n[1]); });
    form.addEventListener('htmx:afterRequest', onAfter);
  });

  // tokensOf reads the reviewed-dialog tokens of an answer form: a chosen option
  // belongs to those tokens only.
  function answerForm() { return qs('[data-answer-form]'); }
  function tokensOf(f) {
    if (!f || typeof f.querySelectorAll !== 'function') { return null; }
    var hs = f.querySelectorAll('input[type="hidden"]');
    var out = [];
    for (var i = 0; i < hs.length; i++) { if (/^expect_/.test(hs[i].name)) { out.push(hs[i].name + '=' + hs[i].value); } }
    return out.join('&');
  }
  function chosenOf(f) {
    var c = f && typeof f.querySelector === 'function' ? f.querySelector('input[type="radio"]:checked') : null;
    return c ? c.value : null;
  }

  // The swap itself is gated, not only the start of the request: a draft typed
  // while the panel request was on its way must survive its response, and a
  // paused view must not change. The refresh is tried again later (a pause: when
  // it is resumed).
  document.body.addEventListener('htmx:beforeSwap', function (ev) {
    var d = ev.detail || {};
    if (!d.requestConfig || !panelRequests.has(d.requestConfig) || !d.shouldSwap) { return; }
    if (paused) {
      d.shouldSwap = false;
      pending = true;
      return;
    }
    if (panelHasDraft()) {
      d.shouldSwap = false;
      pending = true;
      later(RETRY_MS);
      return;
    }
    // A chosen option stays chosen only while the dialog is the same one.
    var f = answerForm();
    answerKept = f && chosenOf(f) !== null ? { tokens: tokensOf(f), value: chosenOf(f) } : null;
  });

  document.body.addEventListener('htmx:afterSwap', function (ev) {
    var d = ev.detail || {};
    if (!d.requestConfig || !panelRequests.has(d.requestConfig)) { return; }
    lastUpdate = Date.now();
    var kept = answerKept;
    answerKept = null;
    if (kept) {
      var f = answerForm();
      if (f && tokensOf(f) === kept.tokens) {
        var radios = f.querySelectorAll('input[type="radio"]');
        for (var i = 0; i < radios.length; i++) { if (radios[i].value === kept.value) { radios[i].checked = true; } }
      }
    }
    render(false);
  });

  // The screen route answers 503 while the Worker is offline: the page is
  // built from scratch (it then shows the notice instead of a stale screen).
  // A refused panel (404) is a page that is gone or no longer allowed.
  document.body.addEventListener('htmx:responseError', function (ev) {
    var d = ev.detail || {};
    var path = d.pathInfo && d.pathInfo.requestPath;
    var status = d.xhr && d.xhr.status;
    if (typeof path === 'string' && path.indexOf(screenURL) === 0 && (status === 503 || status === 404 || status === 403)) { reload(status !== 503); }
  });

  function parse(ev) {
    try { return JSON.parse(ev.data); } catch (e) { return null; }
  }

  function mine(ev) {
    var d = parse(ev);
    return !!d && d.worker_id === workerID && (d.session === undefined || d.session === '' || d.session === session);
  }

  // reload builds the page again. It would discard the unknown outcomes, which live
  // in this tab only, and the settlement handlers of a request that is still on its
  // way (it would also abort that request's transport), so it waits until nothing is
  // on its way and every unknown outcome is dismissed (force: access is gone, which
  // ends this view whatever it holds). A reload that waits never aborts or replays
  // anything: the request settles by its own form, and what it reports stays.
  function reload(force) {
    // Access that is gone ends this view at once: nothing more may be sent from it while the page reloads.
    if (force) { setStream('offline'); }
    var host = byId('session-unknown');
    if (!force && (unknowns.length > 0 || inflight > 0) && host) {
      reloadWanted = true;
      if (!reloadNote(host)) {
        var n = el('p', 'notice notice-warning', 'The session changed and the page needs to reload. It waits because a request is still on its way or a delivery above is still unknown: nothing is aborted or sent again. Read what was submitted, check the session, then Dismiss it and the page reloads.');
        n.setAttribute('role', 'status');
        n.setAttribute('data-reload-note', '1');
        host.appendChild(n);
      }
      return;
    }
    window.location.reload();
  }

  var es = null;

  // connect opens the read stream. It is called once at start and again when a
  // suspended view is shown again; it never writes.
  function connect() {
    if (typeof EventSource !== 'function' || !eventsURL) { return; }
    es = new EventSource(eventsURL, { withCredentials: true });
    var mineStream = es;
    // The read stream reconnects by itself; only reads resume. A write is never
    // queued, so nothing is replayed when the stream returns.
    es.onopen = function () { if (es === mineStream) { setStream('live'); } };
    es.onerror = function () {
      if (es === mineStream) { setStream(mineStream.readyState === 2 ? 'offline' : 'reconnecting'); }
    };
    ['screen_changed', 'state_changed'].forEach(function (name) {
      es.addEventListener(name, function (ev) { if (mine(ev)) { schedule(); signal('events'); } });
    });
    // The Worker went on- or offline, or the session is gone: the page
    // (controls, notice) is built from scratch.
    ['worker_online', 'worker_offline', 'session_removed'].forEach(function (name) {
      es.addEventListener(name, function (ev) { if (mine(ev)) { reload(); } });
    });
    es.addEventListener('worker_resynced', function (ev) { if (mine(ev)) { schedule(); signal('events'); } });
    // A message or a reply of this session's pipe conversation was committed (metadata only: the event names the Worker and the
    // session and nothing of the conversation). It wakes the pipe component alone, which reads its own authorised route
    // again; it does not refresh the screen and reloads nothing.
    es.addEventListener('pipe_changed', function (ev) {
      var d = parse(ev);
      if (d && d.worker_id === workerID && d.session === session) { signal('pipe'); }
    });
    // Every snapshot reconciles the page: the first one covers what changed
    // between rendering the page and subscribing, a later one (after a
    // reconnect) covers the events that were missed. On a page rendered while the
    // Worker was offline this also notices that it is back.
    es.addEventListener('snapshot', function () { setStream('live'); schedule(); signal('events'); });
    es.addEventListener('access_lost', function () { mineStream.close(); reload(true); });
  }

  // ---- composer: Enter sends once, Shift+Enter is a line break ------------------

  var composer = byId('send-text');
  var composerNote = byId('send-note');

  function noteComposer(text) { if (composerNote) { composerNote.textContent = text; } }
  var BLANK_NOTE = 'Type some text first. Nothing was sent, and your draft is kept. Send and Enter send text; to press Enter in the session on its own, use Send Enter in Keys.';
  function blankDraft() { return !!composer && typeof composer.value === 'string' && composer.value.trim() === ''; }

  // sendComposer is the whole of "Enter sends": the same Send button, clicked once, when it may.
  function sendComposer() {
    var why = notSentText();
    if (why) { noteComposer(why); return; }
    if (flyingKind.text > 0) { noteComposer('A text is still on its way. Wait for its result. Nothing was sent and nothing was queued.'); return; }
    if (typeof composer.value !== 'string' || composer.value.trim() === '') {
      // A blank box is never a remote Enter. Enter on its own is the Send Enter key.
      noteComposer(BLANK_NOTE);
      return;
    }
    var send = byId('send-primary');
    if (!send || send.disabled) { return; }
    noteComposer('');
    // Click the button itself: htmx 2.0.11 takes the submitter's name and value from the click on it
    // (lastButtonClicked), and form.requestSubmit() does not pass the submitter on to it.
    send.click();
  }

  if (composer && typeof composer.addEventListener === 'function') {
    composer.addEventListener('keydown', function (ev) {
      if (ev.key !== 'Enter' || ev.defaultPrevented) { return; }
      if (ev.shiftKey || ev.ctrlKey || ev.altKey || ev.metaKey) { return; } // Shift+Enter: the native line break
      if (ev.isComposing || ev.keyCode === 229) { return; }                  // an IME confirming text: never a send
      ev.preventDefault();                                                   // a plain Enter is never a line break here
      if (ev.repeat) { return; }                                             // a held Enter is one Enter
      sendComposer();
    });
    composer.addEventListener('input', function () { noteComposer(''); });
  }

  // ---- pipe conversation composer: the same chat keys ----------------------------
  // The conversation's message box (#pipe-text, in the form of the pipe route) follows the same rules as the Text
  // composer: a plain Enter sends a non-blank message ONCE by clicking its own Send button (so every guard of the
  // button and of the request applies), Shift+Enter is a line break, an IME Enter or a held Enter sends nothing, and a
  // blank message is never sent. Nothing is queued, replayed or forced here.
  var pipeBox = byId('pipe-text');
  var pipeNote = byId('pipe-note');
  var BLANK_PIPE_NOTE = 'Type a message first. Nothing was sent, and your draft is kept.';
  function notePipe(text) { if (pipeNote) { pipeNote.textContent = text; } }
  function blankPipeDraft() { return !!pipeBox && typeof pipeBox.value === 'string' && pipeBox.value.trim() === ''; }

  function sendPipe() {
    var why = notSentText();
    if (why) { notePipe(why); return; }
    if (flyingKind.pipe > 0) { notePipe('A message is still on its way. Wait for its result. Nothing was sent and nothing was queued.'); return; }
    if (blankPipeDraft()) { notePipe(BLANK_PIPE_NOTE); return; }
    var send = byId('pipe-send');
    if (!send || send.disabled) {
      notePipe('Sending is not available for this session. Your draft is kept.');
      return;
    }
    notePipe('');
    send.click(); // the button itself: its guards and the form's drop-not-queue sync apply
  }

  if (pipeBox && typeof pipeBox.addEventListener === 'function') {
    pipeBox.addEventListener('keydown', function (ev) {
      if (ev.key !== 'Enter' || ev.defaultPrevented) { return; }
      if (ev.shiftKey || ev.ctrlKey || ev.altKey || ev.metaKey) { return; }
      if (ev.isComposing || ev.keyCode === 229) { return; }
      ev.preventDefault();
      if (ev.repeat) { return; }
      sendPipe();
    });
    pipeBox.addEventListener('input', function () { notePipe(''); });
  }

  // ---- remote keys: one explicit keyboard surface -------------------------------

  var surface = byId('remote-keys');
  var surfaceState = byId('remote-keys-state');
  var KEY_OF = { ArrowUp: 'Up', ArrowDown: 'Down', ArrowLeft: 'Left', ArrowRight: 'Right', Enter: 'Enter', ' ': 'Space', Spacebar: 'Space', Backspace: 'BSpace', Delete: 'DC' };

  function noteSurface(text) { if (surfaceState) { surfaceState.textContent = text; } }

  // surfaceKey is the key (as the Keys form names it) that a keydown on the surface stands for, or ''.
  // Shift, Alt and Meta make it not ours; Ctrl is ours only as Ctrl+C.
  function surfaceKey(ev) {
    if (ev.shiftKey || ev.altKey || ev.metaKey) { return ''; }
    if (ev.ctrlKey) { return (ev.key === 'c' || ev.key === 'C') ? 'C-c' : ''; }
    return has(KEY_OF, ev.key) ? KEY_OF[ev.key] : '';
  }
  function has(o, k) { return Object.prototype.hasOwnProperty.call(o, k); }

  function keyButton(name) {
    var list = qsa('[data-keys] button[name="key"]');
    for (var i = 0; i < list.length; i++) { if (list[i].value === name) { return list[i]; } }
    return null;
  }

  function sendRemoteKey(name) {
    var why = notSentText();
    if (why) { noteSurface('Not sent. ' + why.replace(' Your text is kept.', '')); return; }
    if (flyingKind.keys > 0) { noteSurface('Not sent: the previous key is still on its way. Nothing was queued.'); return; }
    var b = keyButton(name);
    if (!b || b.disabled) { noteSurface('Not sent: that key is not available.'); return; }
    noteSurface('Sending ' + (b.textContent || name).trim() + '.');
    b.click(); // the button itself: its name and value travel with it, and its guards apply
  }

  function leaveSurface() {
    var to = byId('tab-keys') || byId('session-controls');
    if (to && typeof to.focus === 'function') { to.focus(); }
  }

  if (surface && typeof surface.addEventListener === 'function') {
    surface.hidden = false;
    surface.addEventListener('focus', function () {
      surface.setAttribute('data-active', 'true');
      noteSurface('Keyboard on: the keys you press go to the session. Escape or Tab leaves.');
    });
    surface.addEventListener('blur', function () {
      surface.removeAttribute('data-active');
      noteSurface('');
    });
    surface.addEventListener('keydown', function (ev) {
      if (ev.target !== surface || ev.defaultPrevented || ev.isComposing) { return; } // not from its button, not twice
      if (ev.key === 'Tab') { return; }                                               // the browser moves the focus on
      if (ev.key === 'Escape') { ev.preventDefault(); leaveSurface(); return; }       // out of the surface, to the page
      if (qs('dialog[open]')) { return; }                                             // a local dialog goes first
      var name = surfaceKey(ev);
      if (!name) { return; }                                                          // any other key is not ours
      ev.preventDefault();                                                            // arrows and Space must not scroll the page
      if (ev.repeat) { return; }                                                      // a held key is one key
      sendRemoteKey(name);
    });
    var leave = byId('remote-keys-leave');
    if (leave && typeof leave.addEventListener === 'function') { leave.addEventListener('click', leaveSurface); }
  }

  // suspend and resume are the inspector's hidden/shown states of this view.
  function suspend() {
    if (suspended) { return; }
    suspended = true;
    readGen++; // a resume read that is still on its way no longer counts
    if (es) { es.close(); es = null; }
    if (timer !== null) { window.clearTimeout(timer); timer = null; }
    if (ageTimer !== null) { window.clearInterval(ageTimer); ageTimer = null; }
    render(false);
    signal('suspended');
  }

  function resume() {
    if (!suspended) { return; }
    suspended = false;
    resuming = true;
    stream = 'connecting';
    readGen++;
    startAge();
    render(true);
    connect();
    freshRead(readGen);
  }

  // freshRead is the read that ends "Refreshing": one GET of the screen route, made
  // after the resume, through the same session and permission checks as every read.
  // It does not touch the panel (a held answer stays as typed); when it succeeds the
  // writes come back and the ordinary panel refresh is asked for. Only the current
  // generation counts: an answer that arrives after a suspend or another resume is
  // dropped. A redirect to sign-in or a 401, 403 or 404 means the access is gone
  // (the page reloads, which shows the refusal and ends this view); 503 is a Worker
  // that is offline; anything else is tried again, with writes off.
  function freshRead(gen) {
    if (gen !== readGen || suspended || !resuming) { return; }
    if (typeof window.fetch !== 'function') { return; } // no way to prove freshness: writes stay off
    var again = function () { if (gen === readGen && !suspended && resuming) { window.setTimeout(function () { freshRead(gen); }, RETRY_MS); } };
    window.fetch(screenURL, { credentials: 'same-origin', cache: 'no-store', redirect: 'manual' }).then(function (r) {
      if (gen !== readGen || suspended || !resuming) { return; }
      var st = r && typeof r.status === 'number' ? r.status : 0;
      if (r && r.type === 'opaqueredirect') { reload(true); return; }
      if (st === 401 || st === 403 || st === 404) { reload(true); return; }
      if (st === 503) {
        if (!online) { resuming = false; render(false); } else { reload(false); }
        return;
      }
      if (!(r && r.ok)) { again(); return; }
      if (!online) { reload(); return; } // the Worker is back: build the page from scratch
      var done = function (text) {
        if (gen !== readGen || suspended || !resuming) { return; }
        resuming = false;
        noteChangedDialog(text);
        render(false);
        schedule();
        signal('resumed');
      };
      if (typeof r.text === 'function') { r.text().then(done, function () { done(''); }); } else { done(''); }
    }, again);
  }

  // noteChangedDialog tells the operator, truthfully, that the dialog this view
  // showed is gone when they had typed or chosen an answer for it: such an answer
  // would be refused by the Worker, and it is never sent again under the new
  // dialog's tokens. Without a way to read the new panel nothing is claimed.
  function noteChangedDialog(text) {
    var f = answerForm();
    if (!f || typeof text !== 'string' || text === '' || typeof window.DOMParser !== 'function') { return; }
    var box = byId('answer-choice');
    if (chosenOf(f) === null && !(box && typeof box.value === 'string' && box.value !== '')) { return; }
    var now;
    try {
      var d = new window.DOMParser().parseFromString(text, 'text/html');
      var nf = d.querySelector('[data-answer-form]');
      now = nf ? tokensOf(nf) : '';
    } catch (e) { return; }
    if (now === tokensOf(f)) { return; }
    dropAnswer();
    showResult('warning', 'Dialog changed', 'The dialog changed while this view was hidden. The answer you had typed was for the earlier dialog, so it was cleared and nothing was sent. Read the screen, then answer again.');
  }

  // frameState is what the inspector may know: flags and counts, never text. A
  // view is "dirty" while it holds something the operator typed or chose and has
  // not sent (text, a typed or chosen answer, a changed task) or a local dialog is open.
  function frameState() {
    var text = byId('send-text');
    var chatBox = byId('pipe-text');
    var answer = byId('answer-choice');
    var task = byId('task-text');
    var radios = qsa('[data-answer-form] input[type="radio"]');
    var chosen = false;
    for (var i = 0; i < radios.length; i++) { if (radios[i].checked) { chosen = true; } }
    var chatDraft = !!(chatBox && typeof chatBox.value === 'string' && chatBox.value !== '');
    var chatUnknown = 0;
    for (var u = 0; u < unknowns.length; u++) { if (unknowns[u].action === 'Message') { chatUnknown++; } }
    var dirty = chatDraft || !!(text && typeof text.value === 'string' && text.value !== '') ||
      !!(answer && typeof answer.value === 'string' && answer.value !== '') || chosen ||
      !!(task && typeof task.value === 'string' && task.value !== task.getAttribute('data-saved')) ||
      !!qs('dialog[open]');
    // chat: the conversation's share of the same flags (its draft, its message on its way, its unknown deliveries). It is
    // always part of dirty, pending and unknown above, so an inspector that does not read it still never discards the view.
    return { dirty: dirty, pending: inflight, unknown: unknowns.length, conn: shown(), suspended: suspended,
      chat: { draft: chatDraft, pending: flyingKind.pipe, unknown: chatUnknown } };
  }

  document.body.addEventListener('minion-frame-control', function (ev) {
    var d = ev.detail;
    if (!d || typeof d.reply !== 'function') { return; }
    if (d.op === 'suspend') { suspend(); } else if (d.op === 'resume') { resume(); }
    d.reply(frameState());
  });

  connect();
  window.addEventListener('pagehide', function () { if (es) { es.close(); } });
})();
