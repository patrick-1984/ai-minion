// Runs static/pipe-chat.js against a DOM stub that has real scroll geometry, a controllable htmx request life cycle
// (configRequest, beforeSwap, afterSwap, responseError) and fake timers. It checks what the conversation view does with the
// turn list: it follows new turns only while the reader is at the bottom, keeps a reader who scrolled up where they are and counts
// the unread turns, never moves the focus by itself, reads nothing while the view is suspended, paused or offline, drops an answer
// that arrives after the view was hidden, coalesces bursts into one request and stops on a refusal. The view's event source
// (window.minionSession of terminal-input.js) is a controllable fake here; the real one is checked in session_live_test.js.
// Usage: node pipe_chat_test.js <path to pipe-chat.js>
'use strict';
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) fail(m + ': got ' + JSON.stringify(a) + ', want ' + JSON.stringify(b)); };
const READ = '/s/pi-1/minion-1/pipe/turns';
const LINE = 20, HEIGHT = 100;

function load(o) {
  o = o || {};
  let now = 0;
  const timers = [];
  const listeners = {};
  const intervals = [];
  const node = (id) => {
    const l = {};
    return { id, attrs: {}, hidden: false, children: [], listeners: l, focused: 0, _text: '',
      getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }, setAttribute(k, v) { this.attrs[k] = String(v); },
      addEventListener(n, f) { (l[n] = l[n] || []).push(f); }, fire(n, e) { (l[n] || []).slice().forEach((f) => f(e || {})); },
      focus() { this.focused++; },
      get textContent() { return this._text; }, set textContent(v) { this._text = String(v); } };
  };
  const chat = node('pipe-chat'); chat.attrs['data-pipe-read'] = READ;
  const log = node('pipe-turns');
  log.clientHeight = o.hidden ? 0 : HEIGHT; // a list whose pane is hidden has no layout at all
  log.scrollTop = 0;
  log.turnIds = [];
  log.marker = null;
  Object.defineProperty(log, 'scrollHeight', { get() { return log.clientHeight === 0 ? 0 : Math.max(HEIGHT, log.turnIds.length * LINE); } });
  // scrollTop is clamped like a browser: 0 .. scrollHeight - clientHeight.
  let top = 0;
  Object.defineProperty(log, 'scrollTop', { get() { return top; }, set(v) { top = Math.max(0, Math.min(v, Math.max(0, log.scrollHeight - log.clientHeight))); } });
  log.querySelector = (sel) => (sel === '[data-pipe-walk]' && log.marker ? { getAttribute: (k) => (k === 'data-pipe-walk' ? log.marker.walk || '' : k === 'data-pipe-behind' && log.marker.behind ? '1' : null) } : null);
  log.querySelectorAll = (sel) => (sel === '[data-turn]' ? log.turnIds.map((id) => ({ getAttribute: (k) => (k === 'data-turn' ? id : null) })) : []);
  Object.defineProperty(log, 'innerHTML', { set() { fail('innerHTML was set by the script: the list is only ever what the server rendered'); } });
  const jump = node('pipe-jump'); jump.hidden = true;
  const unread = node('pipe-unread'); const liveRegion = node('pipe-live'); const status = node('pipe-status');
  const els = { 'pipe-chat': chat, 'pipe-turns': log, 'pipe-jump': jump, 'pipe-unread': unread, 'pipe-live': liveRegion, 'pipe-status': status };
  log.turnIds = (o.turns || []).slice();
  log.marker = o.marker || null;
  const liveState = node('pipe-live-state');
  els['pipe-live-state'] = liveState;
  const p = { log, jump, unread, liveRegion, status, liveState, requests: [], fetches: [], intervals, now: () => now, session: null };
  const dispatch = (n, e) => (listeners[n] || []).slice().forEach((f) => f(e || {}));
  p.dispatch = dispatch;
  const document = { visibilityState: 'visible', body: { addEventListener: (n, f) => { (listeners[n] = listeners[n] || []).push(f); } }, getElementById: (id) => els[id] || null,
    querySelector: () => null };
  p.document = document;
  // The fake of window.minionSession: the controllable event source of the view.
  const subs = [];
  p.state = { suspended: false, paused: false, resuming: false, stream: 'live', writes: true };
  p.signal = (k) => subs.slice().forEach((f) => f(k));
  const minionSession = o.noSession ? undefined : { subscribe: (f) => subs.push(f), status: () => Object.assign({}, p.state) };
  const htmx = {
    ajax(verb, path, opt) {
      const cfg = { verb: verb.toLowerCase(), path };
      dispatch('htmx:configRequest', { detail: cfg });
      const entry = { verb, path, opt, cfg };
      p.requests.push(entry);
      return new Promise((resolve) => {
        entry.finish = resolve;
        // answer(ids): 200 with the turn list the server rendered; answer(status): an error answer.
        entry.answer = (v, marker) => {
          if (typeof v === 'number') {
            dispatch('htmx:responseError', { detail: { xhr: { status: v }, requestConfig: cfg, pathInfo: { requestPath: path } } });
          } else {
            const detail = { shouldSwap: true, requestConfig: cfg, target: log, xhr: { status: 200 }, pathInfo: { requestPath: path } };
            dispatch('htmx:beforeSwap', { detail });
            if (detail.shouldSwap) { log.turnIds = v.slice(); log.marker = marker || null; log.scrollTop = 0; /* a rebuilt list starts at the top */ dispatch('htmx:afterSwap', { detail }); }
            entry.swapped = detail.shouldSwap;
          }
          dispatch('htmx:afterRequest', { detail: { requestConfig: cfg } });
          resolve();
        };
        entry.transportError = () => { dispatch('htmx:sendError', { detail: { requestConfig: cfg } }); resolve(); };
      });
    },
  };
  const window = {
    setTimeout: (f, ms) => { timers.push({ f, at: now + ms, dead: false }); return timers.length; },
    clearTimeout: (id) => { if (timers[id - 1]) timers[id - 1].dead = true; },
    setInterval: (f, ms) => { intervals.push({ f, ms }); return intervals.length; },
    minionSession,
  };
  p.roCallbacks = [];
  if (o.resizeObserver) { window.ResizeObserver = function (cb) { p.roCallbacks.push(cb); return { observe() {} }; }; }
  // The component opens no network connection of its own: any fetch is a failure of the test.
  window.fetch = () => { fail('the pipe component opened a connection of its own'); };
  window.EventSource = function () { fail('the pipe component opened an event source of its own'); };
  p.tick = (ms) => {
    const end = now + ms;
    for (;;) {
      const next = timers.filter((t) => !t.dead && t.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      next.dead = true; now = Math.max(now, next.at); next.f();
    }
    now = end;
  };
  p.flush = async () => { for (let i = 0; i < 6; i++) await Promise.resolve(); };
  new Function('document', 'window', 'htmx', src)(document, window, htmx);
  return p;
}

const scenarios = [];
const scenario = (n, f) => scenarios.push([n, f]);
const ids = (n, from) => Array.from({ length: n }, (_, i) => 't' + ((from || 0) + i + 1));
const atBottom = (p) => p.log.scrollHeight - p.log.scrollTop - p.log.clientHeight <= 1;

scenario('it shows the newest turns first without moving the focus', () => {
  const p = load({ turns: ids(20) });
  if (!atBottom(p)) fail('the list does not start at the newest turn');
  eq([p.log.focused, p.jump.focused], [0, 0], 'the focus was moved at load');
  if (!p.jump.hidden) fail('the jump button shows at the bottom');
  eq(p.intervals.length, 1, 'one slow safety read');
});

scenario('a reader at the bottom follows new turns; nothing is counted as unread; no focus is taken', async () => {
  const p = load({ turns: ids(20) });
  p.signal('events');
  eq(p.requests.length, 0, 'a read before the gap');
  p.tick(300);
  eq(p.requests.length, 1, 'requests');
  eq([p.requests[0].verb, p.requests[0].path, p.requests[0].opt.target, p.requests[0].opt.swap], ['GET', READ, '#pipe-turns', 'innerHTML'], 'a plain GET of the read route into the list');
  p.requests[0].answer(ids(23));
  await p.flush();
  if (!atBottom(p)) fail('the reader at the bottom was not taken to the new turn');
  if (!p.jump.hidden || p.unread.textContent !== '') fail('unread was counted for a reader at the bottom');
  eq([p.log.focused, p.jump.focused], [0, 0], 'a refresh moved the focus');
  eq(p.log.getAttribute('data-latest'), 't23', 'the newest turn is recorded');
});

scenario('a reader who scrolled up keeps the place; new turns are counted, announced politely, and Jump to latest goes to the newest on the reader\'s act', async () => {
  const p = load({ turns: ids(20) });
  p.log.scrollTop = 40; // reading older turns
  p.signal('events'); p.tick(300);
  p.requests[0].answer(ids(22));
  await p.flush();
  eq(p.log.scrollTop, 40, 'the reader\'s place moved');
  if (p.jump.hidden || !/2 new/.test(p.unread.textContent)) fail('unread is not offered: ' + p.unread.textContent + ' hidden=' + p.jump.hidden);
  if (!/2 new turns/.test(p.liveRegion.textContent)) fail('not announced: ' + p.liveRegion.textContent);
  eq([p.log.focused, p.jump.focused], [0, 0], 'the focus moved without a click');
  // More arrive: the count adds up and the place is still kept.
  p.signal('events'); p.tick(300);
  p.requests[1].answer(ids(23));
  await p.flush();
  eq(p.log.scrollTop, 40, 'the reader\'s place moved on the second refresh');
  if (!/3 new/.test(p.unread.textContent)) fail('the count did not add up: ' + p.unread.textContent);
  // Scrolling to the bottom by hand clears it.
  p.log.scrollTop = 10000;
  p.log.fire('scroll');
  if (!p.jump.hidden || p.unread.textContent !== '') fail('reaching the bottom did not clear the unread count');
  // And the button: unread again, then Jump.
  p.log.scrollTop = 0;
  p.signal('events'); p.tick(300);
  p.requests[2].answer(ids(24));
  await p.flush();
  if (p.jump.hidden) fail('no jump button');
  p.jump.fire('click');
  if (!atBottom(p) || !p.jump.hidden) fail('Jump to latest did not go to the newest turn');
  eq(p.log.focused, 1, 'the click moved the focus to the list (the button just went away)');
});

scenario('a burst of events is one read; an event during a read makes one more read after it; nothing is queued beyond that', async () => {
  const p = load({ turns: ids(3) });
  for (let i = 0; i < 6; i++) p.signal('events');
  p.tick(300);
  eq(p.requests.length, 1, 'a burst made several reads');
  p.signal('events'); p.signal('events'); p.tick(1000);
  eq(p.requests.length, 1, 'a read was started while one was on its way');
  p.requests[0].answer(ids(3));
  await p.flush();
  p.tick(300);
  eq(p.requests.length, 2, 'the event during the read was not read afterwards');
  p.requests[1].answer(ids(3));
  await p.flush(); p.tick(5000);
  eq(p.requests.length, 2, 'a read with nothing wanted');
});

scenario('a hidden (suspended) view reads nothing; an answer that arrives after it was hidden is dropped; the first read after it returns is fresh', async () => {
  const p = load({ turns: ids(5) });
  p.signal('events'); p.tick(300);
  const inflight = p.requests[0];
  p.state.suspended = true; p.signal('suspended');
  inflight.answer(ids(9)); // late answer of a read that started before the view was hidden
  await p.flush();
  eq(p.log.turnIds, ids(5), 'a late answer changed a hidden view');
  eq(inflight.swapped, false, 'the swap was not refused');
  // Events, the slow timer and a send of the page read nothing while hidden.
  p.signal('events'); p.dispatch('minion-pipe-refresh'); p.intervals[0].f(); p.tick(60000);
  eq(p.requests.length, 1, 'a hidden view read');
  // Shown again, with the fresh read of the screen route back: ONE read, and it is applied.
  p.state.suspended = false; p.signal('resumed'); p.tick(300);
  eq(p.requests.length, 2, 'the view that came back made no read');
  p.requests[1].answer(ids(9));
  await p.flush();
  eq(p.log.turnIds, ids(9), 'the read after the resume was not applied');
  // A write of this page is never touched by any of this: the view has no write path (nothing to abort).
});

scenario('a read that was already waiting when the view was hidden and shown again does not start before the fresh read of the view came back', async () => {
  const p = load({ turns: ids(3) });
  p.signal('events');                                  // a read is waiting for its gap
  p.state.suspended = true; p.signal('suspended');
  p.state.suspended = false; p.state.resuming = true;  // shown again, the fresh read of the screen route is outstanding
  p.tick(5000);
  eq(p.requests.length, 0, 'a read started before the fresh read came back');
  p.signal('events'); p.tick(5000);
  eq(p.requests.length, 0, 'an event while refreshing started a read');
  p.state.resuming = false; p.signal('resumed'); p.tick(300);
  eq(p.requests.length, 1, 'the fresh read did not open reading');
});

scenario('a paused view or a stream that is offline reads nothing; the end of a pause reads', async () => {
  const p = load({ turns: ids(2) });
  p.state.paused = true;
  p.signal('events'); p.tick(5000);
  eq(p.requests.length, 0, 'a paused view read');
  p.state.paused = false; p.signal('events'); p.tick(300);
  eq(p.requests.length, 1, 'the end of the pause did not read');
  p.requests[0].answer(ids(2)); await p.flush();
  p.state.stream = 'offline';
  p.signal('events'); p.tick(5000);
  eq(p.requests.length, 1, 'an offline view read');
});

scenario('a send of this page (minion-pipe-refresh) reads the stored turns once; the slow timer reads only a visible, reading view', async () => {
  const p = load({ turns: ids(2) });
  p.dispatch('minion-pipe-refresh'); p.tick(300);
  eq(p.requests.length, 1, 'a send did not ask for a read');
  p.requests[0].answer(ids(3)); await p.flush();
  p.document.visibilityState = 'hidden';
  p.intervals[0].f(); p.tick(300);
  eq(p.requests.length, 1, 'the slow timer read in a hidden tab');
  p.document.visibilityState = 'visible';
  p.intervals[0].f(); p.tick(300);
  eq(p.requests.length, 2, 'the slow timer did not read a visible view');
});

scenario('a refusal (401, 403, 404) ends the reading and says so; a server error or a lost connection keeps the list and tries again later', async () => {
  const gone = load({ turns: ids(4) });
  gone.signal('events'); gone.tick(300);
  gone.requests[0].answer(404); await gone.flush();
  if (!/no longer be read/.test(gone.status.textContent)) fail('not said: ' + gone.status.textContent);
  gone.signal('events'); gone.tick(120000);
  eq(gone.requests.length, 1, 'it kept reading a session that is gone');
  eq(gone.log.turnIds, ids(4), 'a refusal changed the list');
  for (const st of [500, 502, 503]) {
    const p = load({ turns: ids(4) });
    p.signal('events'); p.tick(300);
    p.requests[0].answer(st); await p.flush();
    eq(p.log.turnIds, ids(4), st + ' changed the list');
    if (!/could not be read just now/.test(p.status.textContent)) fail(st + ': not said');
    p.tick(9000); eq(p.requests.length, 1, st + ': retried too early');
    p.tick(1000); eq(p.requests.length, 2, st + ': not retried');
    p.requests[1].answer(ids(5)); await p.flush();
    eq(p.status.textContent, '', 'the notice stayed after a good read');
  }
  const t = load({ turns: ids(2) });
  t.signal('events'); t.tick(300);
  t.requests[0].transportError(); await t.flush();
  if (!/could not be read just now/.test(t.status.textContent)) fail('a lost connection was not said');
  eq(t.log.turnIds, ids(2), 'a lost connection changed the list');
});

scenario('without the view\'s event source it still renders and stays quiet; a page without the component does nothing', () => {
  const p = load({ turns: ids(3), noSession: true });
  p.dispatch('minion-pipe-refresh'); p.tick(300);
  eq(p.requests.length, 1, 'a send could not ask for a read without a session object');
});

// ---- S7.5 review 1 closures: the reader's place at apply time, generations, long histories, a hidden first layout, live ----

scenario('the reader\'s place is the one at the moment the answer is applied: scrolled up during the read it is kept, back at the bottom during the read it follows', async () => {
  const up = load({ turns: ids(20) });
  up.signal('events'); up.tick(300); // the read starts with the reader at the bottom
  up.log.scrollTop = 40; up.log.fire('scroll'); // and the reader goes up while it is on its way
  up.requests[0].answer(ids(22)); await up.flush();
  eq(up.log.scrollTop, 40, 'the answer pulled a reader who scrolled up during the read to the bottom');
  if (up.jump.hidden || !/2 new/.test(up.unread.textContent)) fail('the unread affordance was lost: ' + up.unread.textContent);
  const down = load({ turns: ids(20) });
  down.log.scrollTop = 40; down.log.fire('scroll');
  down.signal('events'); down.tick(300); // the read starts with the reader up
  down.log.scrollTop = 10000; down.log.fire('scroll'); // and the reader goes to the bottom while it is on its way
  down.requests[0].answer(ids(23)); await down.flush();
  if (!atBottom(down)) fail('a reader who reached the bottom during the read was left on an obsolete position');
  if (!down.jump.hidden) fail('an unread affordance for a reader at the bottom');
  // Jump to latest while a read is on its way is honoured by that read too.
  const jmp = load({ turns: ids(20) });
  jmp.log.scrollTop = 40; jmp.log.fire('scroll');
  jmp.signal('events'); jmp.tick(300);
  jmp.jump.fire('click');
  jmp.log.scrollTop = 0; jmp.log.fire('scroll'); // moved again before the answer; the explicit jump still counts for that answer
  jmp.requests[0].answer(ids(22)); await jmp.flush();
  if (!atBottom(jmp)) fail('Jump to latest pressed during a read was not honoured by its answer');
});

scenario('review: a read that started before the view was hidden is never applied after it is shown again; the resume reads fresh; several cycles', async () => {
  const p = load({ turns: ids(5) });
  p.signal('events'); p.tick(300);
  const old = p.requests[0];
  p.state.suspended = true; p.signal('suspended');
  p.state.suspended = false; p.state.resuming = true;
  p.state.resuming = false; p.signal('resumed'); // the fresh read of the screen route is back: reading again
  old.answer(ids(7)); await p.flush();
  eq(old.swapped, false, 'the answer of the previous visibility generation was applied');
  eq(p.log.turnIds, ids(5), 'the list changed from a stale answer');
  p.tick(300);
  eq(p.requests.length, 2, 'no fresh read after the resume');
  p.requests[1].answer(ids(8)); await p.flush();
  eq(p.log.turnIds, ids(8), 'the fresh read was not applied');
  // Hide, show, hide, show: each generation drops the answers of the ones before.
  const q = load({ turns: ids(3) });
  const inflight = [];
  for (let i = 0; i < 3; i++) {
    q.signal('events'); q.tick(300);
    inflight.push(q.requests[q.requests.length - 1]);
    q.state.suspended = true; q.signal('suspended'); q.state.suspended = false; q.signal('resumed');
    q.tick(300);
  }
  for (const r of inflight) { r.answer(ids(30)); await q.flush(); }
  eq(inflight.map((r) => r.swapped), [false, false, false], 'a stale generation was applied');
  eq(q.log.turnIds, ids(3), 'a stale generation changed the list');
  // A stale error is not shown either: it says nothing about the current generation.
  const e = load({ turns: ids(3) });
  e.signal('events'); e.tick(300);
  const er = e.requests[0];
  e.state.suspended = true; e.signal('suspended'); e.state.suspended = false; e.signal('resumed');
  er.answer(500); await e.flush();
  eq(e.status.textContent, '', 'an error of an earlier generation was shown');
  // The refusal of the CURRENT generation still ends the reading.
  e.tick(300); e.requests[e.requests.length - 1].answer(403); await e.flush();
  if (!/no longer be read/.test(e.status.textContent)) fail('a refusal of the current generation was not shown');
});

scenario('a long history is read in steps: the shown turns say they are not the newest, the steps follow at once with the cursor, the unread count waits for the end, a failed step starts again', async () => {
  const p = load({ turns: ids(20) });
  p.log.scrollTop = 40; p.log.fire('scroll');
  p.signal('events'); p.tick(300);
  eq(p.requests[0].path, READ, 'the first step is the plain read');
  p.requests[0].answer(ids(25, 800), { walk: 'cursor-a', behind: true }); await p.flush();
  eq(p.log.getAttribute('aria-busy'), 'true', 'a list that is behind is not marked busy');
  eq(p.unread.textContent, '', 'turns were counted as new before the newest was reached');
  p.tick(50);
  eq(p.requests.length, 2, 'the next step did not follow at once');
  eq(p.requests[1].path, READ + '?walk=cursor-a', 'the next step does not go on from the cursor');
  p.requests[1].answer(ids(25, 1000), { walk: 'cursor-b', behind: true }); await p.flush();
  p.tick(50);
  eq(p.requests[2].path, READ + '?walk=cursor-b', 'the third step');
  p.requests[2].answer(ids(25, 1001), { walk: 'cursor-c' }); await p.flush();
  p.tick(5000);
  eq(p.requests.length, 3, 'a step was asked for after the newest turns were reached');
  eq(p.log.getAttribute('aria-busy'), 'false', 'still busy at the end');
  eq(p.log.getAttribute('data-latest'), 't1026', 'the newest turn is not the end of the history');
  // The next ordinary read costs only what is new: it goes on from the cursor the last complete read gave.
  p.signal('events'); p.tick(300);
  eq(p.requests[3].path, READ + '?walk=cursor-c', 'the next read does not use the cheap cursor');
  p.requests[3].answer(500); await p.flush();
  p.tick(10000);
  eq(p.requests[4].path, READ, 'after a failed step the read does not start again from the beginning');
});

scenario('a render that stopped at its page bound goes on by itself; a list that is hidden when it first lays out opens at the newest turns, and a reader who went up keeps their place', async () => {
  const p = load({ turns: ids(5), marker: { walk: 'cursor-0', behind: true } });
  p.tick(50);
  eq(p.requests.length, 1, 'the page that is behind did not go on');
  eq(p.requests[0].path, READ + '?walk=cursor-0', 'not from the cursor of the page');
  const h = load({ turns: ids(20), hidden: true, resizeObserver: true });
  eq(h.log.scrollTop, 0, 'a hidden list has a position');
  h.log.clientHeight = HEIGHT; // the pane is shown: the layout callback
  h.roCallbacks.forEach((cb) => cb());
  if (!atBottom(h)) fail('a list that was hidden at first did not open at the newest turns');
  // A reader who went up, then the pane was hidden and shown again: their place, not the bottom.
  const r = load({ turns: ids(20), resizeObserver: true });
  r.log.scrollTop = 40; r.log.fire('scroll');
  r.log.clientHeight = 0; r.roCallbacks.forEach((cb) => cb()); // hidden
  r.log.clientHeight = HEIGHT; r.roCallbacks.forEach((cb) => cb()); // shown
  eq(r.log.scrollTop, 40, 'the reader\'s place was lost when the pane came back');
  // New turns while hidden follow the intention "at the bottom" and are shown at the bottom when the pane comes back.
  const w = load({ turns: ids(5), hidden: true, resizeObserver: true });
  w.signal('events'); w.tick(300);
  w.requests[0].answer(ids(30)); await w.flush();
  w.log.clientHeight = HEIGHT; w.roCallbacks.forEach((cb) => cb());
  if (!atBottom(w)) fail('turns that arrived while the pane was hidden were not shown at the bottom');
});

// Transport expectation changed (S7.5 live closure): the earlier scenarios opened a REST pipe_wait NDJSON stream of the
// component's own, which only saw replies. The component now follows the view's one event stream (terminal-input.js) and its
// 'pipe' signal (a metadata-only pipe_changed event of this session), which also fires for messages another client
// committed. The user behaviour asserted is unchanged: a commit wakes the read at once, nothing of the signal is shown,
// hiding ends reading, showing reads fresh, an unavailable stream is said in general words and the slow read stays.
scenario('live: a pipe signal of the view\'s event stream wakes the read at once; nothing of it is shown; no connection of its own', async () => {
  const p = load({ turns: ids(3) });
  await p.flush();
  eq(p.requests.length, 0, 'a read before anything happened');
  p.signal('pipe'); await p.flush();
  p.tick(300);
  eq(p.requests.length, 1, 'the committed message or reply did not wake a read');
  eq(p.requests[0].path, READ, 'the read is the authorised read route of the page');
  p.requests[0].answer(ids(4)); await p.flush();
  eq(p.log.turnIds, ids(4), 'the list is what the read returned');
  // A burst of signals makes one read, never a parallel one.
  p.signal('pipe'); p.signal('pipe'); p.signal('pipe'); await p.flush(); p.tick(300);
  eq(p.requests.length, 2, 'a burst of signals made more than one read');
  p.requests[1].answer(ids(4)); await p.flush();
  if (/pipe_changed|worker_id/.test(JSON.stringify([p.status.textContent, p.liveRegion.textContent, p.liveState.textContent]))) fail('event text was shown');
});

scenario('live: a hidden view reads nothing on a signal (nothing is applied, written or replayed); showing it again reads fresh', async () => {
  const p = load({ turns: ids(3) });
  p.state.suspended = true; p.signal('suspended'); await p.flush();
  p.signal('pipe'); await p.flush(); p.tick(5000);
  eq(p.requests.length, 0, 'a signal of a hidden view read');
  p.tick(60000); await p.flush();
  eq(p.requests.length, 0, 'a hidden view read on its own');
  p.state.suspended = false; p.signal('resumed'); await p.flush();
  p.tick(300);
  eq(p.requests.length, 1, 'the view that came back did not read fresh');
});

scenario('live: when the view\'s stream is down the page says so in general words (no reason is invented), the slow read stays', async () => {
  const p = load({ turns: ids(3) });
  eq(p.liveState.textContent, '', 'said while the stream is live');
  p.state.stream = 'reconnecting';
  p.intervals[0].f(); p.tick(300);
  if (!/Live updates are not available right now/.test(p.liveState.textContent)) fail('not said: ' + p.liveState.textContent);
  if (/503|forbidden|denied|offline|reconnect/i.test(p.liveState.textContent)) fail('a reason was made up: ' + p.liveState.textContent);
  eq(p.requests.length, 1, 'the slow read is gone');
  p.requests[0].answer(ids(3)); await p.flush();
  p.state.stream = 'live'; p.signal('events'); await p.flush();
  eq(p.liveState.textContent, '', 'still said after the stream came back');
});

(async () => {
  for (const [name, fn] of scenarios) {
    if (process.argv[3] && !name.includes(process.argv[3])) continue;
    try { await fn(); } catch (e) { fail(name + ': ' + (e && e.stack || e)); }
  }
  console.log('ok ' + scenarios.length + ' scenarios');
  process.exit(0);
})();
