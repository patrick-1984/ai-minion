// Runs static/terminal-input.js against a DOM stub with htmx's request life
// cycle, one fresh page per scenario. The stub follows the event order of the
// vendored htmx 2.0.11 (static/htmx.min.js):
//   htmx:configRequest (detail = the request config, the object every later
//   event of that request carries as requestConfig)
//   -> response -> htmx:beforeOnLoad {xhr, requestConfig}
//   -> HX-Trigger events, dispatched on the issuing element
//   -> htmx:beforeSwap {shouldSwap, target, requestConfig, xhr, pathInfo},
//      dispatched on the target; the DOM is swapped only while
//      detail.shouldSwap is still true (the stub really replaces the panel, so
//      the answer box comes back empty)
//   -> htmx:afterSwap -> htmx:afterRequest.
// Scenarios: events of this session fetch the panel (a burst makes one fetch);
// Worker events reload; colours are defined once; a focused quick-answer button
// never holds a refresh back while a draft does; a delivered send clears only
// the text it carried, per request (forceA/normalB overlap, A -> AB -> A before
// "send anyway"); a typed answer delivered by native Enter releases the
// refresh guard while focus stays in the box, without clearing later edits; a
// draft typed while the panel request is pending survives its swap; every SSE
// snapshot reconciles; an offline page recovers without a reload loop; a 503
// panel reloads.
// Workbench scenarios: a forced send or typed answer whose form was replaced by
// another result before its answer arrived still settles (the events of a
// detached form never reach document.body, so the request's own form listens);
// pause freezes the screen and refuses writes; a reconnecting stream disables
// writes, shows the age of the last update and never replays one; a write whose
// answer never arrives is reported as unknown, persistently, with the draft kept
// and nothing re-sent; the Enter checkbox is unchecked again after a delivery; a
// chosen dialog option survives a refresh only while the dialog tokens are equal.
// Usage: node session_live_test.js <path to terminal-input.js>
'use strict';
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) fail(m + ': got ' + JSON.stringify(a) + ', want ' + JSON.stringify(b)); };
const SCREEN = '/s/pi-1/minion-1/screen';
const TEXT = '/s/pi-1/minion-1/text';
const ANSWER = '/s/pi-1/minion-1/answer';
const KEYS = '/s/pi-1/minion-1/keys';
const PIPE = '/s/pi-1/minion-1/pipe';

// load builds a page and runs the script on it.
//   o.online: data-online; o.dialog: the panel has the answer box; o.active: the
//   focused element; o.fetch: window.fetch; o.manual: panel requests of
//   htmx.ajax stay pending until the test answers them.
function load(o) {
  o = o || {};
  const attrs = { 'data-screen-url': SCREEN, 'data-events-url': '/api/v1/events?all=true&worker=w1',
    'data-worker-id': 'w1', 'data-session': 'minion-1', 'data-target': 'minion-1 on pi-1', 'data-online': o.online === false ? 'false' : 'true' };
  const view = { getAttribute: (k) => attrs[k] };
  const screen = { scrollTop: 0, scrollLeft: 0 };
  const box = { id: 'send-text', value: '', focus() { this.focused = true; } };
  const listeners = {};
  const spans = [{ classList: ['t-fg-196', 't-bgx-00ff7f', 't-b'] }, { classList: ['t-fg-196'] }, { classList: ['"><x', 't-fg-999'] }];
  const rules = [];
  function CSSStyleSheet() { this.cssRules = rules; this.replaceSync = () => {}; this.insertRule = (r) => rules.push(r); }
  // A small element: attributes, listeners and children.
  const node = (id, tag) => {
    const l = {};
    const n = {
      id, tagName: (tag || 'div').toUpperCase(), className: '', attrs: {}, children: [], hidden: false, disabled: false, parentNode: null,
      _text: '', setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
      removeAttribute(k) { delete this.attrs[k]; },
      addEventListener(name, f) { (l[name] = l[name] || []).push(f); },
      removeEventListener(name, f) { l[name] = (l[name] || []).filter((x) => x !== f); },
      listeners: l, fire(name, detail) { (l[name] || []).slice().forEach((f) => f({ detail })); },
      appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
      removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; },
      querySelector() { return null; },
      focus() { this.focused = true; document.activeElement = this; },
    };
    Object.defineProperty(n, 'innerHTML', { set() { fail('innerHTML was set: markup must never be built from request data'); }, get() { return ''; } });
    Object.defineProperty(n, 'textContent', {
      get() { return this._text + this.children.map((c) => c.textContent).join(''); },
      set(v) { this._text = String(v); this.children = []; },
    });
    return n;
  };
  const els = {};
  for (const id of ['conn', 'conn-note', 'conn-live', 'age', 'session-result', 'session-unknown']) { els[id] = node(id); }
  els['conn-note'].hidden = true;
  els['pause-toggle'] = node('pause-toggle', 'button'); els['pause-toggle'].hidden = true;
  els['pause-toggle'].span = node('', 'span'); els['pause-toggle'].span.textContent = 'Pause';
  els['pause-toggle'].querySelector = () => els['pause-toggle'].span;
  els['task-text'] = node('task-text', 'input'); els['task-text'].value = 'saved task'; els['task-text'].attrs['data-saved'] = 'saved task';
  const gated = [node('', 'button'), node('', 'button')];
  // The composer and the keys as the real markup has them (templates/session.html): the Text form with the Send and the
  // Paste-without-Enter buttons (the ONLY source of the enter field), the Keys form with its key buttons, and the explicit
  // keyboard surface with its way out.
  const textForm = node('', 'form');
  const keysForm = node('', 'form');
  box.listeners = {};
  box.addEventListener = (n, f) => { (box.listeners[n] = box.listeners[n] || []).push(f); };
  box.form = textForm;
  box.tagName = 'TEXTAREA';
  const btn = (id, name, value, label, form) => {
    const b = node(id, 'button'); b.name = name; b.value = value; b.type = 'submit'; b.textContent = label; b.form = form;
    b.closest = (sel) => (sel.split(',').map((x) => x.trim()).some((x) => x === '#' + id || x === '[data-write]' || (x === '[data-keys] button[name="key"]' && name === 'key')) ? b : null);
    b.click = () => p.click(b);
    gated.push(b);
    return b;
  };
  els['send-primary'] = btn('send-primary', 'enter', '1', 'Send', textForm);
  els['send-paste'] = btn('send-paste', 'enter', '0', 'Paste without Enter', textForm);
  textForm.contains = (x) => x === box || x === els['send-primary'] || x === els['send-paste'];
  const keyButtons = [['Enter', 'Send Enter'], ['Escape', 'Esc'], ['C-c', 'Interrupt (Ctrl-C)'], ['Tab', 'Tab'], ['BTab', 'Shift-Tab'], ['Space', 'Space'], ['BSpace', 'Backspace'], ['DC', 'Delete'],
    ['Up', 'Up'], ['Down', 'Down'], ['Left', 'Left'], ['Right', 'Right']].map(([k, l]) => btn('', 'key', k, l, keysForm));
  // The pipe conversation composer (templates/_pipe_panel.html), only where a scenario asks for it: its own form (data-write), box and button.
  const pipeForm = node('', 'form');
  let pipeBox = null;
  if (o.pipe) {
    pipeBox = { id: 'pipe-text', value: '', tagName: 'TEXTAREA', form: pipeForm, listeners: {}, focus() { this.focused = true; document.activeElement = this; },
      addEventListener(n, f) { (this.listeners[n] = this.listeners[n] || []).push(f); } };
    els['pipe-note'] = node('pipe-note');
    els['pipe-send'] = btn('pipe-send', '', '', 'Send message', pipeForm);
    pipeForm.contains = (x) => x === pipeBox || x === els['pipe-send'];
  }
  for (const id of ['send-note', 'remote-keys-state']) { els[id] = node(id); }
  els['remote-keys'] = node('remote-keys'); els['remote-keys'].hidden = true;
  els['remote-keys-leave'] = node('remote-keys-leave', 'button');
  els['tab-keys'] = node('tab-keys', 'button');
  els['session-controls'] = node('session-controls');
  const newAnswer = () => ({ id: 'answer-choice', tagName: 'INPUT', type: 'text', value: '', closest: (sel) => (sel === '#screen-panel' || sel === '[data-write]' ? {} : null) });
  const p = {
    attrs, box, rules, ajax: [], sources: [], reloads: 0, fetches: [], panelRequests: [],
    dom: { answer: o.dialog ? newAnswer() : null, swaps: 0 },
    dispatch(name, ev) { (listeners[name] || []).forEach((f) => f(ev || {})); },
    ev: (d) => ({ data: JSON.stringify(d) }),
  };
  const document = {
    adoptedStyleSheets: [], activeElement: o.active || null,
    body: { addEventListener(n, f) { (listeners[n] = listeners[n] || []).push(f); } },
    getElementById(id) {
      return Object.assign({}, els, { 'session-view': view, screen: o.online === false ? null : screen, 'send-text': box, 'answer-choice': p.dom.answer }, pipeBox ? { 'pipe-text': pipeBox } : {})[id] || null;
    },
    querySelectorAll(sel) {
      if (sel === '#screen span[class]') { return spans; }
      if (sel === '[data-write] button') { return gated; }
      if (sel === '[data-keys] button[name="key"]') { return keyButtons; }
      if (sel === '[data-answer-form] input[type="radio"]') { return p.dom.answerForm ? p.dom.answerForm.radios : []; }
      return [];
    },
    querySelector(sel) { return sel === '[data-answer-form]' ? p.dom.answerForm || null : sel === 'dialog[open]' ? p.dom.dialogOpen || null : null; },
    createElement: (tag) => node('', tag),
    createTextNode: (t) => { const n = node('', '#text'); n.textContent = t; return n; },
  };
  p.document = document;
  p.els = els;
  p.gated = gated;
  p.sent = 0;
  // An answer form with the tokens of the dialog and its option radios.
  p.dom.answerForm = null;
  p.showDialog = (tokens) => {
    p.dom.tokens = tokens;
    const radios = ['1', '2', 'y'].map((v) => ({ value: v, checked: false }));
    p.dom.answerForm = {
      radios,
      querySelectorAll: (sel) => (sel === 'input[type="hidden"]' ? [{ name: 'expect_digest', value: p.dom.tokens }] : radios),
      querySelector: () => radios.find((r) => r.checked) || null,
    };
    return p.dom.answerForm;
  };

  // control is what frame-child.js does: ask the view (state, suspend, resume) and read the answer.
  p.control = (op) => { let out = null; p.dispatch('minion-frame-control', { detail: { op, reply: (st) => { out = st; } } }); return out; };

  p.keyButtons = keyButtons;
  p.textForm = textForm;
  p.log = [];       // every request that really left: {path, params}
  p.requestsOpen = []; // the handles of the requests that left, in order (respond / fail)
  p.dropped = 0;    // submissions dropped by hx-sync="this:drop" (never queued)
  p.queued = 0;     // submissions htmx would have QUEUED (must stay 0: the forms say drop)
  // deliver sends an event object to the listeners of one element (a keydown has the event itself, not a detail).
  const deliver = (el, name, ev) => { ((el.listeners || {})[name] || []).slice().forEach((f) => f(ev)); return ev; };
  p.keydown = (el, key, o) => deliver(el, 'keydown', Object.assign({ type: 'keydown', key, target: el, shiftKey: false, ctrlKey: false, altKey: false, metaKey: false,
    isComposing: false, repeat: false, keyCode: 0, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, o || {}));
  // press is a physical key on a focused element the way a browser delivers it: the body (capture) listeners see the keydown first,
  // then the element's own, and unless something cancelled it a button is activated by Enter on every keydown (auto-repeats too),
  // by Space only on its keyup, and a text field submits its form with Enter. It returns what happened, so a test can tell a
  // guard that cancelled the key from one that only dropped the request.
  p.press = (el, key, o) => {
    const ev = Object.assign({ type: 'keydown', key, target: el, shiftKey: false, ctrlKey: false, altKey: false, metaKey: false,
      isComposing: false, repeat: false, keyCode: 0, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, o || {});
    p.dispatch('keydown', ev);
    deliver(el, 'keydown', ev);
    const isButton = el.tagName === 'BUTTON' && !!el.form; // a bare stub button has no form to submit
    if (key === ' ' && isButton && !ev.defaultPrevented) { el.spacePending = true; }
    let clicked = null;
    if (key === 'Enter' && isButton && !ev.defaultPrevented) { clicked = p.click(el) || 'dropped'; }
    return { ev, clicked };
  };
  p.release = (el, key) => {
    const ev = { type: 'keyup', key, target: el, preventDefault() { this.defaultPrevented = true; } };
    p.dispatch('keyup', ev);
    let clicked = null;
    if (key === ' ' && el.spacePending) { el.spacePending = false; clicked = p.click(el) || 'dropped'; }
    return { ev, clicked };
  };
  // click presses a button the way the browser does: the click event reaches the page, and unless something prevents it the form is
  // submitted. The vendored htmx 2.0.11 (static/htmx.min.js) takes the submitter from the click: a click on a button inside a form sets
  // that form's lastButtonClicked, and the request built for the submit has the form's fields plus THAT button's name and value
  // (an event.submitter is never read). hx-sync="this:drop": a form with a request on its way drops the new one (never queues it).
  // hx-disabled-elt="find button" (the Text form) disables its buttons while the request is on its way.
  p.click = (b) => {
    if (b.disabled) { return null; }
    const ev = { type: 'click', target: b, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
    p.dispatch('click', ev);
    p.lastClick = ev;
    if (ev.defaultPrevented) { return null; }
    const isText = b.form === textForm;
    const isPipe = b.form === pipeForm;
    const form = b.form;
    if (form.busy) { p.dropped++; return null; }
    const params = isText ? { text: box.value } : isPipe ? { text: pipeBox.value } : {};
    if (b.name) { params[b.name] = b.value; }
    const path = isText ? TEXT : isPipe ? PIPE : KEYS;
    const r = p.request(path, params, { id: isText ? 'text-form' : isPipe ? 'pipe-form' : 'keys-form' });
    if (r.cancelled) { return r; }
    p.log.push({ path, params });
    form.busy = true;
    const group = isText ? [els['send-primary'], els['send-paste']] : isPipe ? [els['pipe-send']] : [];
    group.forEach((x) => { x.disabled = true; });
    const finish = () => { form.busy = false; group.forEach((x) => { x.disabled = false; }); };
    const respond = r.respond, fail = r.fail;
    r.respond = (...a) => { finish(); respond(...a); };
    r.fail = (...a) => { finish(); fail(...a); };
    return r;
  };

  // field returns the element of a typed field.
  p.field = (id) => (id === 'send-text' ? box : id === 'pipe-text' ? pipeBox : p.dom.answer);
  p.pipeBox = pipeBox;
  // type edits a field like a user (value, then the input event).
  p.type = (id, value) => { const f = p.field(id); f.value = value; p.dispatch('input', { target: f }); };

  // request issues a form post of htmx: configRequest and beforeSend now, the
  // response when the test calls respond(). elt is the issuing element (the
  // form); it can receive the events itself (listeners, like a real element).
  p.request = (path, params, elt) => {
    const e = Object.assign(node('form', 'form'), elt || {});
    const cfg = { verb: 'post', path, formData: { get: (k) => (k in params ? params[k] : null), entries: () => Object.entries(params)[Symbol.iterator]() }, elt: e };
    p.dispatch('htmx:configRequest', { detail: cfg });
    const before = { cancelled: false, preventDefault() { this.cancelled = true; }, detail: { requestConfig: cfg, elt: e, pathInfo: { requestPath: path } } };
    p.dispatch('htmx:beforeRequest', before);
    if (before.cancelled) { return { cfg, cancelled: true, elt: e, respond() {}, fail() {} }; }
    p.sent++;
    p.dispatch('htmx:beforeSend', { detail: { requestConfig: cfg, elt: e } });
    // fire sends an event to the form, then (while the form is still in the page)
    // up to the body, the way DOM events bubble.
    const fire = (name, detail, detached) => { e.fire(name, detail); if (!detached) { p.dispatch(name, { detail }); } };
    const handle = {
      cfg, elt: e,
      // respond answers it; triggers are the names of the HX-Trigger header;
      // opt.detached: the form was replaced before the answer arrived, so its
      // events reach nothing but its own listeners; opt.status: the HTTP status.
      respond(triggers, opt) {
        opt = opt || {};
        const trig = Array.isArray(triggers) ? Object.fromEntries(triggers.map((n) => [n, true])) : triggers || {};
        const header = Object.keys(trig).length ? JSON.stringify(trig) : null;
        triggers = Object.keys(trig);
        const xhr = { status: opt.status === undefined ? 200 : opt.status, getResponseHeader: (h) => (h === 'HX-Trigger' ? header : null) };
        fire('htmx:beforeOnLoad', { xhr, requestConfig: cfg, elt: e }, opt.detached);
        (triggers || []).forEach((n) => { if (!opt.detached) { p.dispatch(n, { target: e, detail: { value: true } }); } });
        const swap = { shouldSwap: true, target: { id: 'session-result' }, requestConfig: cfg, xhr, pathInfo: { requestPath: path } };
        if (!opt.detached) { p.dispatch('htmx:beforeSwap', { detail: swap }); }
        fire('htmx:afterRequest', { xhr, requestConfig: cfg, successful: true }, opt.detached);
      },
      // fail: the request left and no answer came (network error, timeout).
      // The vendored htmx (static/htmx.min.js: g.onerror / g.onabort / g.ontimeout)
      // triggers htmx:afterRequest FIRST and only then htmx:sendError, htmx:sendAbort
      // or htmx:timeout, on the issuing element. A listener that is removed at
      // afterRequest therefore never sees the failure.
      fail(kind, opt) {
        opt = opt || {};
        fire('htmx:afterRequest', { xhr: { status: 0 }, requestConfig: cfg, elt: e }, opt.detached);
        fire(kind || 'htmx:sendError', { xhr: { status: 0 }, requestConfig: cfg, elt: e }, opt.detached);
      },
    };
    p.requestsOpen.push(handle);
    return handle;
  };

  // The panel request of htmx.ajax: configRequest at once, the answer later
  // (manual) or at once. A swap replaces the panel: the answer box is new and empty.
  const htmx = {
    ajax(verb, path, opt) {
      const cfg = { verb: verb.toLowerCase(), path, formData: { get: () => null }, elt: { id: 'body' } };
      p.dispatch('htmx:configRequest', { detail: cfg });
      const entry = { verb, path, opt, cfg };
      p.ajax.push([verb, path, opt]);
      p.panelRequests.push(entry);
      return new Promise((resolve) => {
        entry.answer = (status) => {
          status = status || 200;
          const xhr = { status, getResponseHeader: () => null };
          p.dispatch('htmx:beforeOnLoad', { detail: { xhr, requestConfig: cfg } });
          const detail = { shouldSwap: status < 400, target: { id: 'screen-panel' }, requestConfig: cfg, xhr, pathInfo: { requestPath: path } };
          p.dispatch('htmx:beforeSwap', { detail });
          if (detail.shouldSwap) {
            if (p.dom.answer) { p.dom.answer = newAnswer(); }
            p.dom.swaps++;
            if (p.onSwap) { p.onSwap(); } // the swapped-in panel has its own answer form
            p.dispatch('htmx:afterSwap', { detail });
          }
          if (status >= 400) { p.dispatch('htmx:responseError', { detail: { xhr, pathInfo: { requestPath: path } } }); }
          p.dispatch('htmx:afterRequest', { detail: { xhr, requestConfig: cfg, successful: status < 400 } });
          resolve();
        };
        if (!o.manual) { entry.answer(200); }
      });
    },
  };
  function EventSource(url, opt) { this.url = url; this.opt = opt; this.l = {}; p.sources.push(this); }
  EventSource.prototype.addEventListener = function (n, f) { this.l[n] = f; };
  EventSource.prototype.close = function () { this.closed = true; };
  // Unsent input and unknown outcomes live in this tab's memory and DOM only.
  const noStore = { get() { fail('web storage was read: nothing typed or unresolved may be stored'); }, set() { fail('web storage was written: nothing typed or unresolved may be stored'); } };
  p.intervals = [];
  const window = {
    setTimeout, clearTimeout, addEventListener() {}, location: { reload() { p.reloads++; } },
    setInterval: (f, ms) => { p.intervals.push({ f, ms, cleared: false }); return p.intervals.length; },
    clearInterval: (id) => { if (p.intervals[id - 1]) { p.intervals[id - 1].cleared = true; } },
    get localStorage() { return noStore.get(); }, get sessionStorage() { return noStore.get(); },
    fetch: o.fetch ? (u, opt) => { p.fetches.push([u, opt]); return o.fetch(u, opt); } : undefined,
    DOMParser: o.DOMParser,
  };
  new Function('document', 'window', 'htmx', 'EventSource', 'CSSStyleSheet', src)(document, window, htmx, EventSource, CSSStyleSheet);
  p.es = p.sources[0];
  p.window = window;
  p.screenChanged = () => p.es.l.screen_changed(p.ev({ worker_id: 'w1', session: 'minion-1' }));
  return p;
}

const inPanel = (el) => Object.assign({ closest: (sel) => (sel === '#screen-panel' ? {} : null) }, el);
const field = (value) => inPanel({ tagName: 'INPUT', type: 'text', value });
const button = (value) => inPanel({ tagName: 'BUTTON', type: 'submit', value });
const DELIVERED = ['minion-screen-refresh', 'minion-input-sent'];

const scenarios = [];
const scenario = (name, fn) => scenarios.push([name, fn]);

scenario('events, colours and Worker lifecycle', async () => {
  const p = load();
  const es = p.es;
  if (!es || es.url !== p.attrs['data-events-url'] || !es.opt.withCredentials) fail('no credentialed event source');
  const sheetRules = p.rules.join('\n');
  if (!/\.t-fg-196\{color:rgb\(255,0,0\)\}/.test(sheetRules)) fail('palette rule missing: ' + sheetRules);
  if (!/\.t-bgx-00ff7f\{background-color:#00ff7f\}/.test(sheetRules)) fail('true colour rule missing: ' + sheetRules);
  if (p.rules.filter((r) => r.startsWith('.t-fg-196')).length !== 1) fail('a colour rule was defined twice');
  if (/999|><x/.test(sheetRules)) fail('a bad class produced a rule: ' + sheetRules);

  es.l.screen_changed(p.ev({ type: 'screen_changed', worker_id: 'w1', session: 'minion-1' }));
  es.l.state_changed(p.ev({ type: 'state_changed', worker_id: 'w1', session: 'minion-1' }));
  es.l.screen_changed(p.ev({ type: 'screen_changed', worker_id: 'w1', session: 'other' }));
  es.l.screen_changed(p.ev({ type: 'screen_changed', worker_id: 'w2', session: 'minion-1' }));
  es.l.screen_changed({ data: 'not json' });
  await sleep(400);
  if (p.ajax.length !== 1) fail('want one panel fetch for a burst of events of this session, got ' + p.ajax.length);
  const [m, u, o] = p.ajax[0];
  if (m !== 'GET' || u !== SCREEN || o.target !== '#screen-panel' || o.swap !== 'outerHTML') fail('wrong fetch: ' + JSON.stringify(p.ajax[0]));

  p.dispatch('minion-screen-refresh');
  await sleep(400);
  if (p.ajax.length !== 2) fail('want a second fetch after minion-screen-refresh, got ' + p.ajax.length);

  es.l.worker_offline(p.ev({ type: 'worker_offline', worker_id: 'w1', worker: 'pi-1' }));
  es.l.worker_online(p.ev({ type: 'worker_online', worker_id: 'w2', worker: 'x' }));
  if (p.reloads !== 1) fail('want one reload for the Worker of this view, got ' + p.reloads);
  es.l.session_removed(p.ev({ type: 'session_removed', worker_id: 'w1', session: 'minion-1' }));
  if (p.reloads !== 2) fail('want a reload when the session is removed, got ' + p.reloads);
  es.l.access_lost(p.ev({}));
  if (!es.closed || p.reloads !== 3) fail('access_lost must close the stream and reload');
});

scenario('pipe_changed of this session signals the pipe component only: no screen fetch, no reload, nothing of the event is passed on', async () => {
  const p = load();
  const got = [];
  p.window.minionSession.subscribe((k, ...rest) => got.push([k, rest.length]));
  if (typeof p.es.l.pipe_changed !== 'function') fail('pipe_changed is not listened to');
  p.es.l.pipe_changed(p.ev({ type: 'pipe_changed', worker_id: 'w1', session: 'minion-1' }));
  p.es.l.pipe_changed(p.ev({ type: 'pipe_changed', worker_id: 'w1', session: 'other' }));
  p.es.l.pipe_changed(p.ev({ type: 'pipe_changed', worker_id: 'w2', session: 'minion-1' }));
  p.es.l.pipe_changed(p.ev({ type: 'pipe_changed', worker_id: 'w1' }));
  p.es.l.pipe_changed({ data: 'not json' });
  await sleep(400);
  eq(got, [['pipe', 0]], 'only the event of this Worker and this session signals, with no payload');
  eq(p.ajax.length, 0, 'a pipe event refreshed the screen panel');
  eq(p.reloads, 0, 'a pipe event reloaded the page');
});

scenario('a focused button does not block the refresh', async () => {
  for (const active of [button('2'), button('y'), inPanel({ tagName: 'BUTTON', type: 'button', value: 'x' }), field('')]) {
    const p = load({ active });
    p.screenChanged();
    await sleep(450);
    if (p.ajax.length !== 1) fail('a focused ' + active.tagName + ' (value "' + active.value + '") blocked the refresh: ' + p.ajax.length + ' fetches');
  }
});

scenario('a half typed answer defers the refresh until it is gone', async () => {
  const draft = field('no thanks');
  const p = load({ active: draft });
  p.screenChanged();
  await sleep(450);
  if (p.ajax.length !== 0) fail('a half typed answer was wiped by a refresh');
  draft.value = '';
  await sleep(1200);
  if (p.ajax.length !== 1) fail('the refresh did not run once the draft was gone: ' + p.ajax.length);
  const area = load({ active: inPanel({ tagName: 'TEXTAREA', value: 'abc' }) });
  area.screenChanged();
  await sleep(450);
  if (area.ajax.length !== 0) fail('a textarea draft was wiped');
  // The answer box counts as a draft also when it lost the focus.
  const blurred = load({ dialog: true });
  blurred.type('answer-choice', 'maybe');
  blurred.screenChanged();
  await sleep(450);
  if (blurred.ajax.length !== 0 || blurred.dom.answer.value !== 'maybe') fail('an unfocused answer draft was wiped');
});

// A delivered send clears only the text of THAT request (its own record, in the
// request's own config), at the revision it was typed at.
scenario('a delivered send clears only the text it carried', async () => {
  // Plain send: delivered, unedited: cleared and focused.
  let p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }).respond(DELIVERED);
  if (p.box.value !== '' || !p.box.focused) fail('the delivered text was not cleared');

  // Edited while the send was pending: the newer draft survives.
  p = load();
  p.type('send-text', 'A');
  let r = p.request(TEXT, { text: 'A' });
  p.type('send-text', 'AB');
  r.respond(DELIVERED);
  if (p.box.value !== 'AB') fail('a newer draft was erased: ' + JSON.stringify(p.box.value));

  // Edited and back to the same text while pending: still a newer draft.
  p = load();
  p.type('send-text', 'A');
  r = p.request(TEXT, { text: 'A' });
  p.type('send-text', 'AB'); p.type('send-text', 'A');
  r.respond(DELIVERED);
  if (p.box.value !== 'A') fail('an edited draft was erased');

  // Refused (no trigger), then "send anyway" without an edit: cleared.
  p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }).respond([]);
  if (p.box.value !== 'A') fail('a refused send cleared the box');
  p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' }).respond(DELIVERED);
  if (p.box.value !== '') fail('"send anyway" without an edit did not clear the box');

  // A -> AB -> A before "send anyway": the draft was edited since the ORIGINAL
  // send, so the forced retry (which carries A again) must keep it.
  p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }).respond([]);          // refused
  p.type('send-text', 'AB'); p.type('send-text', 'A');
  p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' }).respond(DELIVERED);
  if (p.box.value !== 'A') fail('A -> AB -> A before "send anyway": the edited draft was erased');

  // Refused A, edit to B, "send anyway" of A: B stays.
  p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }).respond([]);
  p.type('send-text', 'B');
  p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' }).respond(DELIVERED);
  if (p.box.value !== 'B') fail('"send anyway" of A erased the draft B: ' + JSON.stringify(p.box.value));

  // Overlap, force of A and a normal send of B in flight together. B is refused
  // or fails after A's delayed acknowledgement: A's response must not use B's
  // record, so B stays in the box in both orders.
  for (const order of ['A first', 'B first']) {
    p = load();
    p.type('send-text', 'A');
    p.request(TEXT, { text: 'A' }, { id: 'main-form' }).respond([]);  // A refused
    p.type('send-text', 'B');
    const forceA = p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' });
    const normalB = p.request(TEXT, { text: 'B' }, { id: 'main-form' });
    if (order === 'A first') { forceA.respond(DELIVERED); normalB.respond([]); } else { normalB.respond([]); forceA.respond(DELIVERED); }
    if (p.box.value !== 'B') fail('overlap forceA/normalB (' + order + '): the draft B was erased: ' + JSON.stringify(p.box.value));
  }
  // B delivered, A (force) failed: B (unedited) is cleared by its own response only.
  p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }).respond([]);
  p.type('send-text', 'B');
  const fA = p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' });
  const nB = p.request(TEXT, { text: 'B' }, { id: 'main-form' });
  fA.respond([]);
  if (p.box.value !== 'B') fail('a failed force of A cleared the box');
  nB.respond(DELIVERED);
  if (p.box.value !== '') fail('B delivered and unedited was not cleared');

  // A delivered key post (or any request without typed text) clears nothing.
  p = load();
  p.type('send-text', 'keep');
  p.request(KEYS, { key: 'Enter' }).respond(DELIVERED);
  if (p.box.value !== 'keep') fail('a draft was cleared by an unrelated request');
  // The parameters object of htmx is read when there is no formData.
  p = load();
  p.type('send-text', 'A');
  const cfg = { verb: 'post', path: TEXT, parameters: { text: 'A' }, elt: { id: 'f' } };
  p.dispatch('htmx:configRequest', { detail: cfg });
  p.dispatch('htmx:beforeOnLoad', { detail: { requestConfig: cfg, xhr: { getResponseHeader: () => JSON.stringify({ 'minion-input-sent': true }) } } });
  if (p.box.value !== '') fail('the parameters fallback did not clear');
});

// A typed answer delivered by native Enter (the focus stays in the box) must
// release the refresh guard; a later edit must not be cleared.
scenario('a delivered typed answer releases the refresh guard', async () => {
  let p = load({ dialog: true });
  p.dom.answer.focus = () => {};
  p.field('answer-choice').value = '';
  p.type('answer-choice', 'n');
  const active = p.dom.answer;
  p.document.activeElement = active; // native Enter: the box keeps the focus
  p.request(ANSWER, { choice_text: 'n' }, { id: 'typed-form' }).respond(DELIVERED);
  if (p.dom.answer.value !== '') fail('the delivered typed answer stayed in the box');
  p.screenChanged();
  await sleep(450);
  if (p.ajax.length !== 1 || p.dom.swaps !== 1) fail('the panel stayed frozen after the answer was delivered: ' + p.ajax.length + ' fetches, ' + p.dom.swaps + ' swaps');

  // Edited after submitting: the later edit survives and still guards the panel.
  p = load({ dialog: true });
  p.type('answer-choice', 'n');
  const r = p.request(ANSWER, { choice_text: 'n' }, { id: 'typed-form' });
  p.type('answer-choice', 'no');
  r.respond(DELIVERED);
  if (p.dom.answer.value !== 'no') fail('a later edit of the answer was cleared');
  p.screenChanged();
  await sleep(450);
  if (p.ajax.length !== 0) fail('the later edit of the answer was swapped away');

  // A refused typed answer stays; "send anyway" (carries it as "choice", trimmed
  // by the server) delivers it and releases it, but not after an edit.
  p = load({ dialog: true });
  p.type('answer-choice', ' n ');
  p.request(ANSWER, { choice_text: ' n ' }).respond([]);
  if (p.dom.answer.value !== ' n ') fail('a refused answer was cleared');
  p.request(ANSWER, { choice: 'n', force: '1' }, { id: 'force-form' }).respond(DELIVERED);
  if (p.dom.answer.value !== '') fail('the forced typed answer was not released');
  p = load({ dialog: true });
  p.type('answer-choice', 'n');
  p.request(ANSWER, { choice_text: 'n' }).respond([]);
  p.type('answer-choice', 'nx'); p.type('answer-choice', 'n');
  p.request(ANSWER, { choice: 'n', force: '1' }, { id: 'force-form' }).respond(DELIVERED);
  if (p.dom.answer.value !== 'n') fail('"send anyway" of an answer erased an edited draft');

  // A quick-answer button types nothing: a draft in the answer box stays.
  p = load({ dialog: true });
  p.type('answer-choice', 'maybe');
  p.request(ANSWER, { choice: 'y' }, { id: 'quick-form' }).respond(DELIVERED);
  if (p.dom.answer.value !== 'maybe') fail('a quick answer cleared the typed draft');
});

// The draft is protected when the response is swapped, not only when the request
// starts.
scenario('a draft typed while the panel request is pending survives the swap', async () => {
  let p = load({ dialog: true, manual: true });
  p.screenChanged();
  await sleep(300);
  if (p.panelRequests.length !== 1) fail('no panel request started');
  p.type('answer-choice', 'half a word'); // typed after the GET started
  const typedBox = p.dom.answer;
  p.panelRequests[0].answer(200);
  if (p.dom.swaps !== 0 || p.dom.answer !== typedBox || typedBox.value !== 'half a word') fail('the panel response wiped the draft typed meanwhile');
  // The refresh is tried again once the draft is gone.
  p.dom.answer.value = '';
  await sleep(1300);
  if (p.panelRequests.length !== 2) fail('the refresh was not retried: ' + p.panelRequests.length);
  p.panelRequests[1].answer(200);
  if (p.dom.swaps !== 1) fail('the retried refresh did not swap');

  // No draft: the response is swapped in.
  p = load({ dialog: true, manual: true });
  p.screenChanged();
  await sleep(300);
  p.panelRequests[0].answer(200);
  if (p.dom.swaps !== 1) fail('a refresh without a draft was not swapped');

  // Only the panel request is gated: other responses are never blocked.
  p = load({ dialog: true });
  p.type('answer-choice', 'x');
  const d = { shouldSwap: true, target: { id: 'session-result' }, requestConfig: { verb: 'post', path: TEXT }, pathInfo: { requestPath: TEXT } };
  p.dispatch('htmx:beforeSwap', { detail: d });
  if (!d.shouldSwap) fail('a result response was blocked');
});

scenario('every snapshot reconciles the screen', async () => {
  const p = load();
  p.es.l.snapshot(p.ev({ sessions: [] })); // the initial one
  await sleep(400);
  if (p.ajax.length !== 1) fail('the initial snapshot did not refresh the panel: ' + p.ajax.length);
  p.es.l.snapshot(p.ev({ sessions: [] })); // after a reconnect
  await sleep(400);
  if (p.ajax.length !== 2) fail('a reconnect snapshot did not refresh the panel: ' + p.ajax.length);
});

scenario('an offline page comes back when the Worker does, across an SSE outage', async () => {
  let online = false;
  const p = load({ online: false, fetch: () => Promise.resolve({ ok: online, status: online ? 200 : 503 }) });
  p.es.l.snapshot(p.ev({ sessions: [] }));
  await sleep(400);
  if (p.fetches.length !== 1 || p.fetches[0][0] !== SCREEN) fail('no probe of the screen route: ' + JSON.stringify(p.fetches));
  if (p.ajax.length !== 0) fail('an offline page tried to swap a panel it does not have');
  if (p.reloads !== 0) fail('an offline page reloaded while the Worker is still offline (reload loop)');
  online = true;
  p.es.l.snapshot(p.ev({ sessions: [] }));
  await sleep(400);
  if (p.reloads !== 1) fail('the offline page did not rebuild itself after the reconnect: ' + p.reloads);
});

scenario('a refused or offline panel rebuilds the page', async () => {
  for (const status of [503, 404]) {
    const p = load();
    p.dispatch('htmx:responseError', { detail: { xhr: { status }, pathInfo: { requestPath: SCREEN } } });
    if (p.reloads !== 1) fail('status ' + status + ' of the panel did not reload: ' + p.reloads);
  }
  const p = load();
  p.dispatch('htmx:responseError', { detail: { xhr: { status: 503 }, pathInfo: { requestPath: TEXT } } });
  p.dispatch('htmx:responseError', { detail: { xhr: { status: 502 }, pathInfo: { requestPath: SCREEN } } });
  if (p.reloads !== 0) fail('an unrelated error or a gateway error reloaded the page');
});

// A refused answer (the dialog the user reviewed is gone) drops the typed answer
// that was meant for it and shows the current screen at once. The draft would
// otherwise hold the refresh back, and the old dialog would stay on screen.
scenario('a stale answer drops the typed answer and refreshes the panel', async () => {
  const p = load({ dialog: true });
  p.type('answer-choice', 'yes');
  const rq = p.request(ANSWER, { choice_text: 'yes' }, { id: 'answer-form' });
  // The refusal asks for a refresh and names the stale dialog; it is not a delivery.
  rq.respond(['minion-screen-refresh', 'minion-dialog-stale']);
  await sleep(450);
  if (p.dom.answer.value !== '') fail('the answer typed for the old dialog was kept: "' + p.dom.answer.value + '"');
  if (p.ajax.length !== 1 || p.ajax[0][1] !== SCREEN) fail('the current screen was not fetched after a stale answer: ' + JSON.stringify(p.ajax));
  if (p.dom.swaps !== 1) fail('the panel was not swapped in: ' + p.dom.swaps);

  // Without the event a typed answer still holds the refresh back (the draft guard is intact).
  const q = load({ dialog: true });
  q.type('answer-choice', 'yes');
  q.request(ANSWER, { choice_text: 'yes' }, { id: 'answer-form' }).respond(['minion-screen-refresh']);
  await sleep(450);
  if (q.ajax.length !== 0 || q.dom.answer.value !== 'yes') fail('a refused answer without the stale event lost the draft or refreshed over it');

  // An edit after the refusal is a new draft: a late acknowledgement of the old one never erases it.
  const r = load({ dialog: true });
  r.type('answer-choice', 'yes');
  const old = r.request(ANSWER, { choice_text: 'yes' }, { id: 'answer-form' });
  old.respond(['minion-screen-refresh', 'minion-dialog-stale']);
  r.type('answer-choice', 'no');
  await sleep(450);
  if (r.dom.answer.value !== 'no' || r.ajax.length !== 0) fail('a new draft typed after the refusal was erased or refreshed over: "' + r.dom.answer.value + '", ' + r.ajax.length + ' fetches');
});

// The known low issue of S5.5: the result of a write replaced the "Send anyway"
// form of another write before that write was answered. A detached form's events
// never reach document.body, so a response listened to there was lost: the
// delivered text stayed in the box and held the refresh back. The request's own
// form settles it.
scenario('a forced send whose form was replaced before it settled still settles', async () => {
  // Text: refused, "Send anyway", and another write replaces the result meanwhile.
  let p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }, { id: 'main-form' }).respond([]); // refused
  const force = p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' });
  p.request(KEYS, { key: 'Enter' }, { id: 'keys-form' }).respond([]); // replaces #session-result: the force form is gone
  force.respond(DELIVERED, { detached: true });
  if (p.box.value !== '') fail('a forced send answered after its form was replaced was not settled: ' + JSON.stringify(p.box.value));
  if (force.elt.listeners['htmx:beforeOnLoad'].length !== 0) fail('the one-time listener of the form stayed attached');

  // Typed answer: the same, and the answer box must not stay a draft that
  // freezes the screen.
  p = load({ dialog: true });
  p.dom.answer.focus = () => {};
  p.type('answer-choice', 'n');
  p.request(ANSWER, { choice_text: 'n' }, { id: 'typed-form' }).respond([]);
  const forceAnswer = p.request(ANSWER, { choice: 'n', force: '1' }, { id: 'force-form' });
  p.request(KEYS, { key: 'Tab' }, { id: 'keys-form' }).respond([]);
  forceAnswer.respond(DELIVERED, { detached: true });
  if (p.dom.answer.value !== '') fail('the forced typed answer stayed in the box: ' + JSON.stringify(p.dom.answer.value));
  p.screenChanged();
  await sleep(450);
  if (p.ajax.length !== 1 || p.dom.swaps !== 1) fail('the panel stayed frozen after the detached answer: ' + p.ajax.length + ' fetches, ' + p.dom.swaps + ' swaps');

  // A later edit is never erased by the late answer.
  p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }).respond([]);
  const late = p.request(TEXT, { text: 'A', force: '1' }, { id: 'force-form' });
  p.type('send-text', 'AB');
  late.respond(DELIVERED, { detached: true });
  if (p.box.value !== 'AB') fail('a late detached answer erased a newer draft: ' + JSON.stringify(p.box.value));
});

scenario('pause freezes the screen and refuses writes; resume refreshes', async () => {
  const p = load({ dialog: true });
  if (p.els['pause-toggle'].hidden !== false) fail('the pause button was not shown');
  p.els['pause-toggle'].listeners.click[0]();
  if (p.els.conn.attrs['data-conn'] !== 'paused' || p.els.conn.textContent !== 'Paused') fail('not shown as paused: ' + p.els.conn.textContent);
  if (p.els['pause-toggle'].attrs['aria-pressed'] !== 'true' || p.els['pause-toggle'].span.textContent !== 'Resume updates') fail('the pause button does not say Resume');
  if (!p.gated.every((b) => b.disabled)) fail('the write buttons stayed enabled while paused');
  p.screenChanged();
  await sleep(450);
  if (p.ajax.length !== 0) fail('a paused view fetched the screen');
  // A write is refused here and never queued.
  p.type('send-text', 'hello');
  const w = p.request(TEXT, { text: 'hello' }, { id: 'text-form' });
  if (!w.cancelled || p.sent !== 0) fail('a write was sent while paused');
  if (!/Not sent/.test(p.els['session-result'].textContent) || p.box.value !== 'hello') fail('no refusal shown, or the draft was lost');
  p.els['pause-toggle'].listeners.click[0]();
  await sleep(450);
  if (p.ajax.length !== 1) fail('resume did not refresh the screen: ' + p.ajax.length);
  if (p.gated.some((b) => b.disabled)) fail('the write buttons stayed disabled after resume');
  if (p.sent !== 0) fail('resuming sent something');
  // A panel response that arrives while paused is not swapped.
  const q = load({ dialog: true, manual: true });
  q.screenChanged();
  await sleep(300);
  q.els['pause-toggle'].listeners.click[0]();
  q.panelRequests[0].answer(200);
  if (q.dom.swaps !== 0) fail('a paused view was swapped');
});

scenario('a reconnecting stream disables writes, keeps the age and never replays', async () => {
  const p = load();
  p.es.onopen();
  if (p.els.conn.textContent !== 'Live') fail('open stream is not Live: ' + p.els.conn.textContent);
  p.es.readyState = 0;
  p.es.onerror();
  if (p.els.conn.textContent !== 'Reconnecting' || p.els['conn-note'].hidden) fail('not shown as reconnecting');
  if (!/Reconnecting/.test(p.els['conn-live'].textContent)) fail('the transition was not announced');
  if (!/The last screen update was \d+ seconds? ago/.test(p.els.age.textContent)) fail('no age of the last update: ' + p.els.age.textContent);
  if (!p.gated.every((b) => b.disabled)) fail('write buttons stayed enabled while reconnecting');
  p.type('send-text', 'keep me');
  const w = p.request(TEXT, { text: 'keep me' });
  if (!w.cancelled || p.sent !== 0 || p.box.value !== 'keep me') fail('a write went out (or the draft was lost) while reconnecting');
  // The stream returns: reads resume, nothing is replayed.
  p.es.onopen();
  p.es.l.snapshot(p.ev({ sessions: [] }));
  await sleep(450);
  if (p.sent !== 0) fail('the script sent a write after the reconnect');
  if (p.ajax.length !== 1 || p.ajax[0][0] !== 'GET') fail('the only request after reconnecting should be the screen read: ' + JSON.stringify(p.ajax));
  if (p.els.conn.textContent !== 'Live' || p.gated.some((b) => b.disabled)) fail('not live again');
  // A closed stream (no retry by the browser) is Offline.
  p.es.readyState = 2;
  p.es.onerror();
  if (p.els.conn.textContent !== 'Offline') fail('a closed stream is not Offline');
});

scenario('a write without an answer is reported as unknown and never repeated', async () => {
  for (const kind of ['htmx:sendError', 'htmx:timeout']) {
    for (const detached of [false, true]) {
      const p = load();
      p.type('send-text', 'rm -rf build');
      const w = p.request(TEXT, { text: 'rm -rf build' }, { id: 'text-form' });
      w.fail(kind, { detached });
      const host = p.els['session-unknown'];
      if (host.children.length !== 1 || !/Delivery is unknown/.test(host.textContent)) fail(kind + ' detached=' + detached + ': no persistent unknown outcome: ' + host.textContent);
      if (!/Nothing was resent/.test(host.textContent) || !/minion-1 on pi-1/.test(host.textContent)) fail('the unknown outcome does not name the target or the no-retry rule');
      if (p.box.value !== 'rm -rf build') fail('the draft was cleared by an unknown outcome');
      if (p.sent !== 1) fail('the script sent again: ' + p.sent);
      await sleep(1300);
      if (p.sent !== 1 || p.ajax.length > 0 && p.ajax.some((a) => a[0] !== 'GET')) fail('a write was replayed');
      // It stays until it is dismissed.
      const dismiss = host.children[0].children.find((c) => c.tagName === 'BUTTON');
      dismiss.listeners.click[0]();
      if (host.children.length !== 0) fail('Dismiss did not remove the outcome');
    }
  }
  // A gateway error (5xx) cannot say whether it arrived: unknown too. A 4xx refusal is a plain "Not sent".
  let p = load();
  p.request(KEYS, { key: 'Enter' }).respond([], { status: 502 });
  if (p.els['session-unknown'].children.length !== 1) fail('a 502 on a write was not reported as unknown');
  p = load();
  p.request(KEYS, { key: 'Enter' }).respond([], { status: 403 });
  if (p.els['session-unknown'].children.length !== 0 || !/Not sent/.test(p.els['session-result'].textContent)) fail('a 403 was not shown as not sent');
  // Reads (the panel) never produce a write outcome.
  p = load();
  p.dispatch('htmx:sendError', { detail: { requestConfig: { verb: 'get', path: SCREEN }, xhr: { status: 0 } } });
  if (p.els['session-unknown'].children.length !== 0) fail('a failed screen read was reported as an unknown write');
});

// ---- the session inspector: suspend, resume and the state it may know -----------------

// The resume read: a fetch the test answers by hand. It is a GET of the screen route, never a write.
const OK = { status: 200, ok: true, type: 'basic', text: () => Promise.resolve('<div id="screen-panel"></div>') };
const status = (n, extra) => Object.assign({ status: n, ok: n >= 200 && n < 300, type: 'basic', text: () => Promise.resolve('') }, extra);
function withFetch(o) {
  const waiters = [];
  const p = load(Object.assign({}, o, { fetch: (u, opt) => new Promise((res) => { waiters.push({ u, opt, res }); }) }));
  p.waiters = waiters;
  return p;
}
const settleMicro = async () => { for (let i = 0; i < 10; i++) { await Promise.resolve(); } };

scenario('suspend stops the read stream and the timers, refuses writes; resume reads again and offers writes only after its own fresh read', async () => {
  const p = withFetch({ dialog: true, manual: true });
  p.es.onopen();
  if (p.els.conn.textContent !== 'Live' || p.intervals.length !== 1 || p.intervals[0].cleared) fail('not live with its age timer');
  const first = p.es;
  const st = p.control('suspend');
  if (!st.suspended || !first.closed) fail('suspend did not close the read stream: ' + JSON.stringify(st));
  if (!p.intervals[0].cleared) fail('suspend left the age timer running');
  if (p.els.conn.textContent !== 'Suspended' || p.els.conn.attrs['data-conn'] !== 'suspended' || p.els['conn-note'].hidden) fail('a suspended view is not shown as such: ' + p.els.conn.textContent);
  if (!p.gated.every((b) => b.disabled)) fail('the write buttons stayed enabled while suspended');
  p.type('send-text', 'kept text');
  const w = p.request(TEXT, { text: 'kept text' });
  if (!w.cancelled || p.sent !== 0 || !/Not sent/.test(p.els['session-result'].textContent)) fail('a write was sent (or not refused) while suspended');
  // Events of the old stream, a write answer elsewhere and a panel nudge read nothing while hidden.
  p.screenChanged();
  p.dispatch('minion-screen-refresh');
  await sleep(450);
  if (p.ajax.length !== 0 || p.waiters.length !== 0) fail('a suspended view read the screen: ' + p.ajax.length + '/' + p.waiters.length);
  if (p.box.value !== 'kept text') fail('the draft did not survive being hidden');
  // Shown again: a new stream, ONE fresh read of the screen route (a GET), and no writes until it has come back.
  const back = p.control('resume');
  if (back.suspended) fail('still suspended after resume');
  if (p.sources.length !== 2 || p.sources[1].url !== p.attrs['data-events-url'] || !p.sources[1].opt.withCredentials) fail('resume did not open a new credentialed stream');
  if (p.els.conn.textContent !== 'Refreshing' || !p.gated.every((b) => b.disabled)) fail('writes are offered before the fresh read: ' + p.els.conn.textContent);
  eq(p.waiters.length, 1, 'fresh reads started by resume');
  eq([p.waiters[0].u, p.waiters[0].opt.credentials, p.waiters[0].opt.cache, p.waiters[0].opt.redirect, p.waiters[0].opt.method, p.waiters[0].opt.body],
    [SCREEN, 'same-origin', 'no-store', 'manual', undefined, undefined], 'the resume read is a plain credentialed GET');
  p.sources[1].onopen();
  const again = p.request(TEXT, { text: 'kept text' });
  if (!again.cancelled || p.sent !== 0) fail('a write was possible before the fresh read came back');
  p.waiters[0].res(OK);
  await settleMicro();
  if (p.els.conn.textContent !== 'Live' || p.gated.some((b) => b.disabled)) fail('writes did not come back with the fresh read: ' + p.els.conn.textContent);
  if (p.box.value !== 'kept text') fail('the draft was lost on the way');
  if (p.sent !== 0) fail('the script wrote something of its own');
  if (!p.intervals.some((i, n) => n > 0 && !i.cleared)) fail('the age timer did not start again');
  await sleep(450);
  if (p.panelRequests.length !== 1) fail('after the fresh read the ordinary panel refresh was not asked for: ' + p.panelRequests.length);
});

// Review finding 2: the resume freshness depended on a panel SWAP, which a typed answer holds back for good, so the typed answer
// could never be sent after the view came back. The read is now separate and the answer stays exactly as typed.
scenario('a typed dialog answer does not stall the resume read and keeps its reviewed identity', async () => {
  const p = withFetch({ dialog: true, manual: true });
  const form = p.showDialog('tokens-A');
  p.type('answer-choice', 'yes please');
  form.radios[0].checked = false;
  p.control('suspend');
  p.control('resume');
  eq(p.waiters.length, 1, 'the resume read is made although a typed answer holds the panel back');
  p.waiters[0].res(OK);
  await settleMicro();
  if (p.gated.some((b) => b.disabled) || p.els.conn.textContent === 'Refreshing') fail('a typed answer kept the view in "Refreshing" for good: ' + p.els.conn.textContent);
  if (p.dom.answer.value !== 'yes please') fail('the typed answer was not kept as typed');
  eq(p.dom.answerForm.querySelectorAll('input[type="hidden"]')[0].value, 'tokens-A', 'the reviewed-dialog tokens it was typed for');
  // The answer is submitted under ITS tokens; nothing re-binds it.
  const w = p.request(ANSWER, { choice_text: 'yes please', expect_digest: 'tokens-A' }, { id: 'answer-form' });
  if (w.cancelled) fail('the answer could not be submitted after the resume');
  eq(p.sent, 1, 'one answer sent, once');
  // The ordinary panel refresh keeps waiting for the draft to go; it never swaps the dialog under the typed answer.
  await sleep(450);
  if (p.dom.swaps !== 0 || p.dom.answer.value !== 'yes please') fail('the panel was swapped under a typed answer');
});

// Review finding 3: any panel response cleared "resuming", including one started BEFORE the suspension.
scenario('a read from before the suspension, or from an earlier resume, can never unlock the resumed writes', async () => {
  const p = withFetch({ dialog: true, manual: true });
  p.screenChanged();
  await sleep(300);
  eq(p.panelRequests.length, 1, 'a panel read is on its way');
  p.control('suspend');
  p.control('resume');
  eq(p.waiters.length, 1, 'the resume read');
  p.panelRequests[0].answer(200); // the OLD read comes back after the view was shown again
  if (!p.gated.every((b) => b.disabled) || p.els.conn.textContent !== 'Refreshing') fail('a read from before the suspension unlocked the writes');
  const refused = p.request(TEXT, { text: 'x' });
  if (!refused.cancelled) fail('a write was possible on the strength of an old read');
  // Resume, hide, resume: the first resume read comes back late and counts for nothing.
  p.control('suspend');
  p.control('resume');
  eq(p.waiters.length, 2, 'a second resume read');
  p.waiters[0].res(OK);
  await settleMicro();
  if (!p.gated.every((b) => b.disabled)) fail('the read of an earlier resume unlocked the writes');
  p.waiters[1].res(OK);
  await settleMicro();
  if (p.gated.some((b) => b.disabled)) fail('the current read did not unlock the writes');
  // A read that comes back while hidden does nothing either.
  const q = withFetch({ manual: true });
  q.control('suspend'); q.control('resume');
  q.control('suspend');
  q.waiters[0].res(OK);
  await settleMicro();
  if (q.control('state').conn !== 'suspended') fail('a late read changed a hidden view');
});

scenario('a resume read that fails keeps the writes off: refused access reloads, a server error asks again', async () => {
  const cases = [[401, 1], [403, 1], [404, 1]];
  for (const [code, reloads] of cases) {
    const p = withFetch({ manual: true });
    p.control('suspend'); p.control('resume');
    p.waiters[0].res(status(code));
    await settleMicro();
    if (p.reloads !== reloads || !p.gated.every((b) => b.disabled)) fail(code + ': the view must reload and keep writes off (' + p.reloads + ')');
  }
  let p = withFetch({ manual: true });
  p.control('suspend'); p.control('resume');
  p.waiters[0].res({ status: 0, ok: false, type: 'opaqueredirect' });
  await settleMicro();
  if (p.reloads !== 1 || !p.gated.every((b) => b.disabled)) fail('a redirect to sign-in must reload (the sign-in page then ends this view) and keep writes off');
  p = withFetch({ manual: true });
  p.control('suspend'); p.control('resume');
  p.waiters[0].res(status(502));
  await settleMicro();
  if (p.reloads !== 0 || !p.gated.every((b) => b.disabled)) fail('a 502 enabled the writes or reloaded');
  await sleep(1100);
  eq(p.waiters.length, 2, 'a 502 is asked again');
  p.waiters[1].res(OK);
  await settleMicro();
  if (p.gated.some((b) => b.disabled)) fail('the second read did not unlock');
  p = withFetch({ manual: true });
  p.control('suspend'); p.control('resume');
  p.waiters[0].res(Promise.reject(new Error('network')));
  await settleMicro();
  await sleep(1100);
  eq(p.waiters.length, 2, 'a network failure is asked again');
  if (!p.gated.every((b) => b.disabled)) fail('a network failure enabled the writes');
  p = withFetch({ manual: true });
  p.control('suspend'); p.control('resume');
  p.waiters[0].res(status(503));
  await settleMicro();
  if (p.reloads !== 1 || !p.gated.every((b) => b.disabled)) fail('a Worker that is offline must rebuild the page (and keep writes off)');
  // No fetch at all: freshness cannot be proved, so the writes stay off.
  const n = load({ manual: true });
  n.control('suspend'); n.control('resume');
  if (!n.gated.every((b) => b.disabled)) fail('without a way to read, the writes must stay off');
});

scenario('a dialog that changed while the view was hidden clears the typed answer truthfully and sends nothing', async () => {
  const parser = function () { this.parseFromString = (text) => ({ querySelector: (sel) => (sel === '[data-answer-form]' && /FORM:/.test(text) ? { querySelectorAll: () => [{ name: 'expect_digest', value: text.split('FORM:')[1].replace(/<.*/, '') }] } : null) }); };
  for (const [fresh, kept] of [['<p>FORM:tokens-B</p>', false], ['<p>no dialog any more</p>', false], ['<p>FORM:tokens-A</p>', true]]) {
    const p = withFetch({ dialog: true, manual: true, DOMParser: parser });
    p.showDialog('tokens-A');
    p.type('answer-choice', 'my answer');
    p.control('suspend'); p.control('resume');
    p.waiters[0].res(Object.assign(status(200), { text: () => Promise.resolve(fresh) }));
    await settleMicro();
    if ((p.dom.answer.value === 'my answer') !== kept) fail(fresh + ': the typed answer ' + (kept ? 'was dropped for an unchanged dialog' : 'was kept for another dialog'));
    const note = /Dialog changed/.test(p.els['session-result'].textContent);
    if (note === kept) fail(fresh + ': the notice is wrong: ' + p.els['session-result'].textContent);
    if (p.sent !== 0) fail('something was sent');
    if (p.gated.some((b) => b.disabled)) fail('the view is not usable after the fresh read');
  }
});

// Review finding 1 (round 2): the resume read answered 503 while a write started before the suspension was still on its way, and
// reload(false) only waited for UNKNOWN outcomes, so the page reloaded: the settlement handlers and the evidence were destroyed and the
// transport aborted.
scenario('a 503 on the resume read does not reload a view with a write on its way; the reload waits, nothing is aborted or sent again', async () => {
  // A: delivered late.
  const p = withFetch({ manual: true });
  p.type('send-text', 'printf A');
  const w = p.request(TEXT, { text: 'printf A' }, { id: 'text-form' });
  eq(p.control('state').pending, 1, 'the write is on its way');
  p.control('suspend');
  p.control('resume');
  p.waiters[0].res(status(503));
  await settleMicro();
  if (p.reloads !== 0) fail('the page reloaded while a write was on its way (' + p.reloads + ' reloads): its handlers and evidence would be gone');
  if (!p.gated.every((b) => b.disabled)) fail('writes were offered on a 503');
  if (p.sent !== 1) fail('something was sent or aborted: ' + p.sent);
  const note = p.els['session-unknown'].children.find((c) => c.attrs && c.attrs['data-reload-note']);
  if (!note || !/still on its way/.test(note.textContent) || !/nothing is aborted or sent again/.test(note.textContent)) fail('the wait is not explained: ' + p.els['session-unknown'].textContent);
  w.respond(DELIVERED);
  if (p.box.value !== '') fail('the late acknowledgement did not settle its own form');
  if (p.reloads !== 1) fail('the deferred reload did not go ahead after the write settled: ' + p.reloads);
  if (p.sent !== 1) fail('the reload sent again');
  // B: the answer never came: the original outcome is kept, the reload waits for Dismiss, then goes ahead.
  const q = withFetch({ manual: true });
  q.type('send-text', 'printf B');
  const w2 = q.request(TEXT, { text: 'printf B' }, { id: 'text-form' });
  q.control('suspend'); q.control('resume');
  q.waiters[0].res(status(503));
  await settleMicro();
  if (q.reloads !== 0) fail('reloaded with a write on its way');
  w2.fail('htmx:sendError', { detached: true });
  if (q.reloads !== 0) fail('reloaded over an unknown outcome');
  const host = q.els['session-unknown'];
  const rec = host.children.find((c) => c.attrs && c.attrs['data-unknown']);
  if (!rec || !/printf B/.test(rec.textContent) || !/Delivery is unknown/.test(rec.textContent) || !/minion-1 on pi-1/.test(rec.textContent)) fail('the original submitted payload and target are not kept: ' + host.textContent);
  rec.children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
  if (q.reloads !== 1) fail('the reload did not go ahead after the unknown outcome was dismissed: ' + q.reloads);
  eq(q.sent, 1, 'writes sent in total');
  // C: A pending, B's write settled meanwhile: the late acknowledgement and unknown outcome belong to the session that sent them.
  const A = withFetch({ manual: true });
  const B = withFetch({ manual: true });
  A.type('send-text', 'A text');
  const wa = A.request(TEXT, { text: 'A text' }, { id: 'a' });
  A.control('suspend'); A.control('resume');
  A.waiters[0].res(status(503));
  B.type('send-text', 'B draft');
  await settleMicro();
  if (A.reloads !== 0 || B.reloads !== 0) fail('a reload happened');
  wa.respond(DELIVERED);
  if (A.box.value !== '' || B.box.value !== 'B draft') fail('the acknowledgement of A touched B');
  // A access that is really gone (404) still ends the view at once: that is not a generic 503.
  const D = withFetch({ manual: true });
  D.type('send-text', 'x');
  D.request(TEXT, { text: 'x' });
  D.control('suspend'); D.control('resume');
  D.waiters[0].res(status(404));
  await settleMicro();
  if (D.reloads !== 1) fail('a refused access must end the view even with a write on its way');
  // The stream events that rebuild the page wait for a write on its way as well.
  const E = load({});
  E.type('send-text', 'y');
  const we = E.request(TEXT, { text: 'y' });
  E.es.l.worker_offline(E.ev({ worker_id: 'w1' }));
  if (E.reloads !== 0) fail('a Worker event reloaded the page over a write on its way');
  we.respond(DELIVERED);
  eq(E.reloads, 1, 'the deferred reload after the write');
});

scenario('suspend is not Pause: the two flags stay apart', async () => {
  const p = withFetch({ manual: true });
  p.es.onopen();
  p.els['pause-toggle'].listeners.click[0](); // the operator pauses
  p.control('suspend');
  p.control('resume');
  if (p.els['pause-toggle'].attrs['aria-pressed'] !== 'true' || p.els.conn.textContent !== 'Paused') fail('the operator\'s Pause was lost by hiding the view: ' + p.els.conn.textContent);
  p.sources[1].onopen();
  p.waiters[0].res(OK);
  await settleMicro();
  await sleep(450);
  if (p.panelRequests.length !== 0) fail('a paused view read the screen panel after it was shown again');
  if (!p.gated.every((b) => b.disabled)) fail('a paused view offers writes after it was shown again');
  p.els['pause-toggle'].listeners.click[0](); // the operator resumes
  await sleep(450);
  if (p.gated.some((b) => b.disabled) || p.els.conn.textContent !== 'Live') fail('not live after both flags were cleared: ' + p.els.conn.textContent);
  // The other way: a live view hidden and shown never touches the Pause button.
  const q = withFetch({ manual: true });
  q.control('suspend');
  if (q.els['pause-toggle'].attrs['aria-pressed'] === 'true') fail('hiding the view paused it');
  q.control('resume');
  if (q.els['pause-toggle'].attrs['aria-pressed'] === 'true') fail('showing the view paused it');
});

scenario('a request on its way when the view is suspended is neither aborted nor replayed and settles by its own form', async () => {
  const p = load();
  p.type('send-text', 'hello');
  const w = p.request(TEXT, { text: 'hello' }, { id: 'text-form' });
  let st = p.control('state');
  if (st.pending !== 1 || !st.dirty) fail('a request on its way is not reported: ' + JSON.stringify(st));
  p.control('suspend');
  if (p.sent !== 1) fail('suspend sent or aborted something: ' + p.sent);
  st = p.control('state');
  if (st.pending !== 1) fail('suspend dropped the request on its way');
  w.respond(DELIVERED);
  if (p.box.value !== '') fail('the delivered text was not settled by its own form while suspended');
  st = p.control('state');
  if (st.pending !== 0 || st.dirty || st.unknown !== 0) fail('after delivery the view is not clean: ' + JSON.stringify(st));
  p.control('resume');
  if (p.sent !== 1) fail('resume replayed a write');
  // A request whose answer never came: unknown, kept, counted; not replayed by suspend or resume.
  const q = load();
  q.type('send-text', 'rm -rf build');
  const w2 = q.request(TEXT, { text: 'rm -rf build' }, { id: 'text-form' });
  q.control('suspend');
  w2.fail('htmx:sendError', { detached: true });
  let s2 = q.control('state');
  if (s2.pending !== 0 || s2.unknown !== 1 || !s2.dirty) fail('an unknown outcome while hidden is not reported: ' + JSON.stringify(s2));
  q.control('resume');
  await sleep(1300);
  if (q.sent !== 1) fail('an unknown write was sent again');
  const host = q.els['session-unknown'];
  if (host.children.length !== 1 || !/rm -rf build/.test(host.textContent) || !/Delivery is unknown/.test(host.textContent)) fail('the unknown outcome and its payload did not survive: ' + host.textContent);
  host.children[0].children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
  q.box.value = '';
  s2 = q.control('state');
  if (s2.unknown !== 0 || s2.dirty) fail('after Dismiss and an empty box the view is not clean: ' + JSON.stringify(s2));
});

scenario('the frame state is flags and counts: unsent input of every kind makes it dirty, and it carries no text', () => {
  const p = load({ dialog: true });
  const keys = (o) => Object.keys(o).sort().join(',');
  let st = p.control('state');
  if (st.dirty || st.pending !== 0 || st.unknown !== 0 || st.conn !== 'connecting' || st.suspended) fail('a quiet view is not clean: ' + JSON.stringify(st));
  // The contract: the original flags and counts, plus the conversation's own share (chat: flags and counts only).
  if (keys(st) !== 'chat,conn,dirty,pending,suspended,unknown') fail('the state has more than flags and counts: ' + keys(st));
  if (keys(st.chat) !== 'draft,pending,unknown' || st.chat.draft !== false || st.chat.pending !== 0 || st.chat.unknown !== 0) fail('a quiet view has a conversation share: ' + JSON.stringify(st.chat));
  p.box.value = 'secret text';
  st = p.control('state');
  if (!st.dirty || JSON.stringify(st).includes('secret')) fail('a typed draft is not reported as a flag only: ' + JSON.stringify(st));
  p.box.value = '';
  p.dom.answer.value = 'a typed answer';
  if (!p.control('state').dirty) fail('a typed answer is not unsent input');
  p.dom.answer.value = '';
  const form = p.showDialog('t1');
  if (p.control('state').dirty) fail('a dialog alone is not unsent input');
  form.radios[1].checked = true;
  if (!p.control('state').dirty) fail('a chosen option is not unsent input');
  form.radios[1].checked = false;
  p.els['task-text'].value = 'changed task';
  if (!p.control('state').dirty) fail('an unsaved task is not unsent input');
  p.els['task-text'].value = 'saved task';
  p.dom.dialogOpen = {};
  if (!p.control('state').dirty) fail('an open local dialog must keep the view');
  p.dom.dialogOpen = null;
  if (p.control('state').dirty) fail('could not get back to clean');
  if (p.control('state').conn !== 'connecting') fail('state is not the connection label');
  p.es.onopen();
  if (p.control('state').conn !== 'live') fail('the connection label is not live');
});

scenario('two views are two contexts: a request of A completes only A, and its unknown outcome is A\'s', async () => {
  const A = load();
  const B = load();
  A.type('send-text', 'text for A');
  const wa = A.request(TEXT, { text: 'text for A' }, { id: 'a-form' });
  B.type('send-text', 'text for B');
  const wb = B.request(TEXT, { text: 'text for B' }, { id: 'b-form' });
  wb.respond(DELIVERED);
  if (B.box.value !== '' || A.box.value !== 'text for A') fail('B\'s delivery touched A\'s draft');
  if (A.control('state').pending !== 1 || B.control('state').pending !== 0) fail('the pending requests are not per view');
  A.control('suspend');
  wa.fail('htmx:timeout', { detached: true });
  if (A.control('state').unknown !== 1 || B.control('state').unknown !== 0) fail('the unknown outcome of A leaked into B');
  if (!/text for A/.test(A.els['session-unknown'].textContent) || B.els['session-unknown'].children.length !== 0) fail('the unknown payload is not A\'s alone');
  if (A.sent !== 1 || B.sent !== 1) fail('a write was replayed or crossed over');
});

// ---- the composer: Enter sends once, Shift+Enter is a line break (owner contract v0.5.0) ----------------------------

const sentOf = (p) => p.log.map((r) => r.path.split('/').pop() + ':' + JSON.stringify(r.params));

scenario('Enter sends a non-empty draft ONCE as the same atomic request the Send button makes (text + enter=1); Shift+Enter is a line break', () => {
  const p = load();
  p.type('send-text', 'ls -la');
  const e = p.keydown(p.box, 'Enter');
  if (!e.defaultPrevented) fail('a plain Enter inserted a line break instead of sending');
  eq(sentOf(p), ['text:{"text":"ls -la","enter":"1"}'], 'one request, text and enter together');
  // The submitter reached the request through the button click (htmx lastButtonClicked), not through a hidden field or a checkbox.
  eq(Object.keys(p.log[0].params), ['text', 'enter'], 'parameters');
  // The Send button is the very same action.
  const q = load();
  q.type('send-text', 'ls -la');
  q.click(q.els['send-primary']);
  eq(sentOf(q), sentOf(p), 'the Send button and Enter are one action');
  // Shift+Enter (and any other modified Enter) is left to the browser: a line break, nothing sent.
  for (const o of [{ shiftKey: true }, { ctrlKey: true }, { altKey: true }, { metaKey: true }]) {
    const r = load();
    r.type('send-text', 'a');
    const ev = r.keydown(r.box, 'Enter', o);
    if (ev.defaultPrevented || r.log.length) fail('Enter with ' + JSON.stringify(o) + ' was taken over or sent');
  }
  // Paste without Enter is its own, secondary action: the same text, enter=0, no Enter pressed.
  const t = load();
  t.type('send-text', 'ls -la');
  t.click(t.els['send-paste']);
  eq(sentOf(t), ['text:{"text":"ls -la","enter":"0"}'], 'paste without Enter');
  // Other keys of the box are the browser's: arrows, Backspace, Delete, Home, End edit the text locally.
  for (const k of ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Backspace', 'Delete', 'Home', 'End', ' ', 'a', 'Tab', 'Escape']) {
    const ev = p.keydown(p.box, k);
    if (ev.defaultPrevented) fail(k + ' in the text box was taken over');
  }
  eq(p.log.length, 1, 'only the Enter sent');
  if (p.keyButtons.some((b) => b.busy)) fail('test bug');
});

scenario('Enter never sends: while an IME composes, when held, while a text is on its way, paused, suspended or reconnecting, or on a blank box', () => {
  // IME: the Enter that confirms a composition is the browser's (isComposing, or keyCode 229 on older engines).
  for (const o of [{ isComposing: true }, { keyCode: 229 }]) {
    const p = load();
    p.type('send-text', 'にほん');
    const ev = p.keydown(p.box, 'Enter', o);
    if (ev.defaultPrevented || p.log.length) fail('an IME Enter ' + JSON.stringify(o) + ' was taken over or sent');
  }
  // Held: only the first Enter of a press counts, and the repeats do not insert line breaks either.
  const p = load();
  p.type('send-text', 'ls');
  p.keydown(p.box, 'Enter');
  for (let i = 0; i < 5; i++) { const ev = p.keydown(p.box, 'Enter', { repeat: true }); if (!ev.defaultPrevented) fail('a repeated Enter inserted a line break'); }
  eq(p.log.length, 1, 'a held Enter is one send');
  // The same when nothing is on its way any more: the repeats of the press that sent are never new sends (the in-flight check must not be what saves us).
  const h = load();
  h.type('send-text', 'ls');
  h.keydown(h.box, 'Enter');
  h.requestsOpen[0].respond(DELIVERED);
  h.type('send-text', 'next command');
  for (let i = 0; i < 3; i++) { if (!h.keydown(h.box, 'Enter', { repeat: true }).defaultPrevented) fail('a repeated Enter inserted a line break (settled)'); }
  eq(h.log.length, 1, 'a held Enter after the first send settled');
  h.keydown(h.box, 'Enter'); // a NEW press is a new send
  eq(h.log.length, 2, 'a new press after the answer');
  // On its way: a second Enter (a new press) sends nothing, queues nothing, and says why; the text is untouched.
  p.keydown(p.box, 'Enter');
  p.keydown(p.box, 'Enter');
  eq(p.log.length, 1, 'requests while the first is on its way');
  if (p.dropped || p.queued) fail('a second send reached htmx (' + p.dropped + ' dropped, ' + p.queued + ' queued): the script must not even try');
  if (!/still on its way/.test(p.els['send-note'].textContent) || !/nothing was queued/i.test(p.els['send-note'].textContent)) fail('not told: ' + p.els['send-note'].textContent);
  if (p.box.value !== 'ls') fail('the draft was touched');
  // The mouse path on the same form is dropped by hx-sync (the buttons are disabled too), never queued.
  p.click(p.els['send-primary']);
  eq(p.log.length, 1, 'a click while it is on its way');
  // Answered: delivered clears the box and the next Enter sends again, once.
  // Paused, suspended, reconnecting, offline: the writes are off; Enter sends nothing and the text stays.
  const reasons = {
    paused: (q) => { q.els['pause-toggle'].listeners.click[0](); },
    suspended: (q) => { q.control('suspend'); },
    reconnecting: (q) => { q.es.onerror(); },
  };
  for (const [name, off] of Object.entries(reasons)) {
    const q = load();
    q.es.onopen();
    q.type('send-text', 'rm -rf build');
    off(q);
    const ev = q.keydown(q.box, 'Enter');
    if (q.log.length || q.sent) fail(name + ': Enter sent while writes are off');
    if (!ev.defaultPrevented) fail(name + ': Enter turned into a line break (the text would change)');
    if (q.box.value !== 'rm -rf build' || !/^(The view is paused|The connection is being restored)/.test(q.els['send-note'].textContent)) fail(name + ': not told, or the text was touched: ' + q.els['send-note'].textContent);
    q.click(q.els['send-primary']); // disabled by the gate: nothing
    eq(q.log.length, 0, name + ': the Send button sent');
  }
  // Blank (and whitespace-only): never a remote Enter. It says to use the Send Enter key; nothing leaves.
  for (const blank of ['', '   ', '\n']) {
    const q = load();
    q.type('send-text', blank);
    const ev = q.keydown(q.box, 'Enter');
    if (q.log.length || q.sent) fail('a blank box sent something: ' + JSON.stringify(sentOf(q)));
    if (!ev.defaultPrevented || !/Send Enter/.test(q.els['send-note'].textContent)) fail('blank Enter: not told where the Enter key is: ' + q.els['send-note'].textContent);
  }
});

scenario('the delivered text clears only itself; an edit to B while A is on its way survives; focus returns only where it was', async () => {
  const p = load();
  p.type('send-text', 'A');
  p.keydown(p.box, 'Enter');
  // The person goes on typing B while A is on its way (the box is not locked).
  p.type('send-text', 'AB');
  p.type('send-text', 'B');
  // A's answer: delivered. B is a newer draft and stays.
  p.requestsOpen[0].respond(DELIVERED);
  if (p.box.value !== 'B') fail('a delivered A erased the newer draft B: ' + JSON.stringify(p.box.value));
  // Delivered and unedited: cleared; the focus comes back to the box only when it is where the person was.
  const cases = [['nowhere (the clicked button went disabled)', null, true], ['the box', 'box', true], ['the Send button', 'send-primary', true], ['the task field', 'task-text', false], ['the keys surface', 'remote-keys', false]];
  for (const [what, where, refocus] of cases) {
    const q = load();
    q.type('send-text', 'ls');
    q.keydown(q.box, 'Enter');
    q.document.activeElement = where === null ? null : where === 'box' ? q.box : q.els[where];
    q.box.focused = false;
    q.requestsOpen[0].respond(DELIVERED);
    if (q.box.value !== '') fail(what + ': the delivered text was not cleared');
    if (!!q.box.focused !== refocus) fail(what + ': the focus ' + (refocus ? 'was not returned to the box' : 'was pulled into the box'));
  }
});

scenario('a refused send keeps the text, and an unknown outcome keeps the ACTUAL payload and the submit flag of that request', async () => {
  const p = load();
  p.type('send-text', 'make test');
  p.keydown(p.box, 'Enter');
  p.requestsOpen[0].fail('htmx:timeout');
  if (p.box.value !== 'make test') fail('an unknown delivery cleared the draft');
  const host = p.els['session-unknown'];
  const shown = host.textContent;
  if (!/make test/.test(shown) || !/submit=true/.test(shown) || !/Enter was to be pressed/.test(shown)) fail('the unknown outcome lacks the payload or the submit flag: ' + shown);
  eq(p.log.length, 1, 'nothing was resent');
  // Edited afterwards: the evidence is still the original payload.
  p.type('send-text', 'something else');
  if (!/make test/.test(host.textContent) || /something else/.test(host.textContent)) fail('the unknown payload follows the box');
  // The paste-only request says submit=false.
  const q = load();
  q.type('send-text', 'echo hi');
  q.click(q.els['send-paste']);
  q.requestsOpen[0].fail('htmx:timeout');
  if (!/submit=false/.test(q.els['session-unknown'].textContent) || /submit=true/.test(q.els['session-unknown'].textContent)) fail('the paste-only unknown outcome has the wrong submit flag: ' + q.els['session-unknown'].textContent);
  // A "Send anyway" for a refused Enter-send repeats the same single call (the hidden fields carry enter=1); the box edited to B is not cleared by A.
  const r = load();
  r.type('send-text', 'A');
  r.keydown(r.box, 'Enter');
  r.requestsOpen[0].respond(['minion-screen-refresh']); // refused (the Hub shows Send anyway)
  r.type('send-text', 'B');
  const forced = r.request(TEXT, { text: 'A', enter: '1', force: '1' });
  forced.respond(DELIVERED);
  if (r.box.value !== 'B') fail('a forced A erased the newer draft B');
});

// ---- the explicit remote-key surface ------------------------------------------------------------------------------

scenario('the keyboard surface presses one named key per press, only while it has the focus, through the key BUTTON (name and value travel with the click)', () => {
  const p = load();
  const s = p.els['remote-keys'];
  if (s.hidden) fail('the surface was not shown by the script');
  const map = [['ArrowUp', 'Up'], ['ArrowDown', 'Down'], ['ArrowLeft', 'Left'], ['ArrowRight', 'Right'], ['Enter', 'Enter'], [' ', 'Space'], ['Backspace', 'BSpace'], ['Delete', 'DC']];
  for (const [k, want] of map) {
    const q = load();
    q.keydown(q.els['remote-keys'], k);
    eq(sentOf(q), ['keys:{"key":"' + want + '"}'], k);
  }
  const q = load();
  const ev = q.keydown(q.els['remote-keys'], 'c', { ctrlKey: true });
  eq(sentOf(q), ['keys:{"key":"C-c"}'], 'Ctrl+C');
  if (!ev.defaultPrevented) fail('Ctrl+C was left to the browser');
  // Arrows and Space do not scroll the page.
  for (const k of ['ArrowUp', ' ']) { const r = load(); if (!r.keydown(r.els['remote-keys'], k).defaultPrevented) fail(k + ' would scroll the page'); }
  // Not ours: Tab and Shift+Tab move the focus on (never sent, never prevented); Escape leaves (never sent); letters, Home/End, modified keys are not ours.
  const r = load();
  for (const [k, o] of [['Tab', {}], ['Tab', { shiftKey: true }], ['a', {}], ['Home', {}], ['End', {}], ['PageDown', {}], ['F5', {}], ['ArrowUp', { shiftKey: true }], ['ArrowUp', { altKey: true }], ['ArrowUp', { metaKey: true }],
    ['ArrowUp', { ctrlKey: true }], ['c', { ctrlKey: true, shiftKey: true }], ['v', { ctrlKey: true }], ['Enter', { metaKey: true }]]) {
    const e2 = r.keydown(r.els['remote-keys'], k, o);
    if (e2.defaultPrevented) fail(k + ' ' + JSON.stringify(o) + ' was taken over by the surface');
  }
  eq(r.log.length, 0, 'keys that are not the surface\'s sent something');
  // Escape is the local way out: the focus goes to the Keys tab, nothing is sent, and it is handled (the compact frame must not close the inspector for it).
  const esc = r.keydown(r.els['remote-keys'], 'Escape');
  if (!esc.defaultPrevented || r.log.length || !r.els['tab-keys'].focused) fail('Escape did not leave the surface locally');
  // The visible way out.
  const b = load();
  b.els['remote-keys-leave'].listeners.click[0]();
  if (!b.els['tab-keys'].focused || b.log.length) fail('the button did not hand the focus back to the page');
  // A key from the button inside the surface is the button's own (Enter/Space on "Back to the page"), never a remote key.
  const c = load();
  c.keydown(c.els['remote-keys-leave'], 'Enter');
  c.keydown(c.els['remote-keys-leave'], ' ');
  eq(c.log.length, 0, 'the surface took the keys of its own button');
});

scenario('the surface sends nothing when the view is paused, hidden, reconnecting, over a local dialog, held, or while a key is on its way; and queues nothing', () => {
  const off = {
    paused: (q) => { q.els['pause-toggle'].listeners.click[0](); },
    suspended: (q) => { q.control('suspend'); },
    reconnecting: (q) => { q.es.onerror(); },
  };
  for (const [name, f] of Object.entries(off)) {
    const q = load(); q.es.onopen(); f(q);
    q.keydown(q.els['remote-keys'], 'ArrowDown');
    if (q.log.length || q.sent) fail(name + ': a key was sent while writes are off');
    if (!/^Not sent\./.test(q.els['remote-keys-state'].textContent)) fail(name + ': not told: ' + q.els['remote-keys-state'].textContent);
  }
  // A local dialog (Send anyway) goes first: not even prevented.
  const d = load(); d.dom.dialogOpen = {};
  const e = d.keydown(d.els['remote-keys'], 'ArrowDown');
  if (e.defaultPrevented || d.log.length) fail('a key was sent or taken over while a local dialog was open');
  // Held: one key, the repeats are swallowed (they must not scroll either) and sent nothing.
  const h = load();
  h.keydown(h.els['remote-keys'], 'ArrowDown');
  for (let i = 0; i < 5; i++) { if (!h.keydown(h.els['remote-keys'], 'ArrowDown', { repeat: true }).defaultPrevented) fail('a repeated key was left to scroll the page'); }
  eq(h.log.length, 1, 'a held key');
  // A key is on its way: the next press is refused with a message and NEVER queued (htmx would queue it on the same form).
  h.keydown(h.els['remote-keys'], 'ArrowDown');
  h.keydown(h.els['remote-keys'], 'Enter');
  eq(h.log.length, 1, 'presses while a key is on its way');
  if (h.dropped || h.queued) fail('a second key reached htmx: ' + h.dropped + '/' + h.queued);
  if (!/still on its way/.test(h.els['remote-keys-state'].textContent)) fail('not told: ' + h.els['remote-keys-state'].textContent);
  // The mouse path on the key buttons is refused out loud too, and a settled key frees the next one.
  h.click(h.keyButtons[8]);
  eq(h.log.length, 1, 'a mouse click on a key button while one is on its way');
  if (!/still on its way/.test(h.els['session-result'].textContent)) fail('the refused click said nothing: ' + h.els['session-result'].textContent);
  h.requestsOpen[0].respond(['minion-screen-refresh']);
  h.keydown(h.els['remote-keys'], 'ArrowDown');
  eq(h.log.length, 2, 'the next key after the first settled');
  // A key that is disabled (the gate) or missing is never faked.
  const g = load();
  g.keyButtons.forEach((b) => { b.disabled = true; });
  g.keydown(g.els['remote-keys'], 'ArrowDown');
  eq(g.log.length, 0, 'a disabled key button was pressed');
  // An unknown outcome of a key keeps the key that was really sent.
  const u = load();
  u.keydown(u.els['remote-keys'], 'ArrowLeft');
  u.requestsOpen[0].fail('htmx:timeout');
  if (!/Left/.test(u.els['session-unknown'].textContent) && !/Key attempt/.test(u.els['session-unknown'].textContent)) fail('the unknown key is not recorded: ' + u.els['session-unknown'].textContent);
  eq(u.log.length, 1, 'a key was replayed');
});

scenario('nothing else is a remote control: no global key hijack, no click or mouse on the screen, arrows in the text box and the answer form stay local, no default choice', () => {
  const p = load({ dialog: true });
  const form = p.showDialog('A');
  // The page-level keydown: nothing of ours listens (the surface and the composer listen on themselves only).
  for (const k of ['ArrowUp', 'ArrowDown', 'Enter', ' ', 'Backspace', 'Delete', 'Tab', 'Escape']) {
    const ev = { type: 'keydown', key: k, target: p.document.body || {}, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
    p.dispatch('keydown', ev);
    if (ev.defaultPrevented) fail('a global keydown (' + k + ') was taken over by the page script');
  }
  // The text box: the editing keys are the browser's, and none of them sends.
  for (const k of ['ArrowLeft', 'ArrowRight', 'Backspace', 'Delete']) { if (p.keydown(p.box, k).defaultPrevented) fail(k + ' was taken over in the text box'); }
  // The screen is read-only: a click or a mouse press on a cell (or anywhere on the panel) sends nothing and is not taken over.
  const cell = { tagName: 'SPAN', closest: () => null, classList: [] };
  for (const t of ['click', 'mousedown', 'mouseup', 'dblclick', 'contextmenu']) {
    const ev = { type: t, target: cell, button: 0, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
    p.dispatch(t, ev);
    if (ev.defaultPrevented) fail('a ' + t + ' on the screen was taken over');
  }
  eq(p.log.length + p.sent, 0, 'the screen sent something');
  // The question: the options start unchosen, focusing them chooses nothing and sends nothing, and the answer box's Enter (the browser's implicit
  // submission) is not intercepted: the scripts neither prevent nor duplicate it.
  if (form.radios.some((r) => r.checked)) fail('an option is chosen by default');
  for (const k of ['Enter', 'ArrowDown', ' ']) {
    const ev = p.keydown(p.dom.answer, k);
    if (ev.defaultPrevented) fail(k + ' in the answer box was taken over');
  }
  for (const r of form.radios) { const ev = { type: 'focus', target: r }; p.dispatch('focusin', ev); p.dispatch('focus', ev); }
  if (form.radios.some((r) => r.checked) || p.sent) fail('focusing an option chose or sent something');
  // The browser's own submission of the answer carries the reviewed tokens of the CURRENT dialog; the page does not rewrite it.
  p.type('answer-choice', '2');
  const r = p.request(ANSWER, { choice_text: '2', expect_digest: 'A' });
  eq(p.sent, 1, 'the native answer submission');
  r.respond(DELIVERED);
});

scenario('a screen refresh never takes the focus from where the person is typing', async () => {
  for (const where of ['box', 'task-text', 'remote-keys']) {
    const p = load();
    const el = where === 'box' ? p.box : p.els[where];
    p.document.activeElement = el;
    p.box.focused = false; el.focused = false;
    p.screenChanged();
    await sleep(300);
    if (p.box.focused && where !== 'box') fail('a refresh pulled the focus into the text box (focus was on ' + where + ')');
    if (p.document.activeElement !== el) fail('a refresh moved the focus away from ' + where);
    eq(p.ajax.length, 1, 'the refresh happened');
  }
});

// The vendored htmx is what the stub above models. If it is replaced, these facts must be looked at again (the composer clicks the Send
// button because this htmx takes the submitter from the click, and the forms say hx-sync="this:drop" because it queues by default).
scenario('contract of the vendored htmx 2.0.11: submitter from the clicked button, no event.submitter, queue by default, hx-sync drop', () => {
  const h = fs.readFileSync(require('path').join(require('path').dirname(process.argv[2]), 'htmx.min.js'), 'utf8');
  if (!/version:"2\.0\.11"/.test(h)) fail('the vendored htmx changed: re-check the composer and key-surface contract');
  if (/submitter/.test(h)) fail('this htmx reads event.submitter: requestSubmit(button) might now work, re-read the composer');
  if (!/lastButtonClicked=t/.test(h) || !/addEventListener\("click",\w+\)/.test(h)) fail('the click on a button no longer sets lastButtonClicked');
  if (!/const u=s\.lastButtonClicked\|\|e;const a=Q\(u,"name"\);cn\(a,u\.value,o\)/.test(h)) fail('the request no longer takes the name and value from the last clicked button');
  if (!/p="last"/.test(h) || !/d==="drop"&&u\.xhr&&u\.abortable!==true/.test(h)) fail('the queue and drop semantics changed: the forms\' hx-sync="this:drop" must be re-checked');
});

scenario('a chosen dialog option survives a refresh only for the same dialog', async () => {
  // Same tokens: still chosen after the swap.
  let p = load({ dialog: true, manual: true });
  p.showDialog('A').radios[1].checked = true;
  p.onSwap = () => { p.swapped = p.showDialog('A'); };
  p.screenChanged();
  await sleep(300);
  p.dom.answer.value = '';
  p.panelRequests[0].answer(200);
  if (!p.swapped || !p.swapped.radios[1].checked) fail('a chosen option was lost although the dialog is the same');
  // Another dialog (other tokens): the choice is dropped, never carried over.
  p = load({ dialog: true, manual: true });
  p.showDialog('A').radios[1].checked = true;
  p.onSwap = () => { p.swapped = p.showDialog('B'); };
  p.screenChanged();
  await sleep(300);
  p.panelRequests[0].answer(200);
  if (!p.swapped || p.swapped.radios.some((r) => r.checked)) fail('a choice made for another dialog was carried over to the new one');
  // A stale answer clears the chosen option as well as the typed text.
  p = load({ dialog: true });
  p.showDialog('A').radios[0].checked = true;
  p.request(ANSWER, { choice: '1' }, { id: 'answer-form' }).respond(['minion-screen-refresh', 'minion-dialog-stale']);
  if (p.dom.answerForm.radios.some((r) => r.checked)) fail('a stale answer kept the chosen option');
});

// ---- review round 2 ------------------------------------------------------------
// Finding 1: htmx triggers htmx:afterRequest BEFORE sendError / timeout / sendAbort.
// The form's one-time listeners must live until the request is really settled,
// also when the form was detached by the result of another action (its events
// then never reach document.body).
const walk = (n, f) => { f(n); (n.children || []).forEach((c) => walk(c, f)); };
const payloadOf = (box) => { let t = null; walk(box, (n) => { if ((n.className || '').split(' ').includes('unknown-payload')) { t = n.textContent; } }); return t; };
const FORM_EVENTS = ['htmx:beforeOnLoad', 'htmx:sendError', 'htmx:timeout', 'htmx:sendAbort', 'htmx:afterRequest'];
const listening = (elt) => FORM_EVENTS.reduce((n, name) => n + (elt.listeners[name] || []).length, 0);

scenario('every transport failure is settled after afterRequest, also on a detached form', async () => {
  for (const kind of ['htmx:sendError', 'htmx:timeout', 'htmx:sendAbort']) {
    for (const how of ['attached', 'detached', 'replaced-by-another-result']) {
      const p = load();
      p.type('send-text', 'rm -rf build');
      const w = p.request(TEXT, { text: 'rm -rf build' }, { id: 'text-form' });
      if (how === 'replaced-by-another-result') {
        // Another action's result arrives and replaces the area that holds this form.
        p.request(KEYS, { key: 'Enter' }, { id: 'keys-form' }).respond([]);
      }
      // The afterRequest of ANOTHER request that reaches this element must not end this request's listeners.
      w.elt.fire('htmx:afterRequest', { requestConfig: { path: '/other' }, xhr: { status: 200 } });
      if (listening(w.elt) === 0) fail(kind + ' ' + how + ': an afterRequest of another request removed the listeners');
      w.fail(kind, { detached: how !== 'attached' });
      const host = p.els['session-unknown'];
      if (host.children.length !== 1) fail(kind + ' ' + how + ': want exactly one unknown outcome, got ' + host.children.length);
      if (payloadOf(host.children[0]) !== 'rm -rf build') fail(kind + ' ' + how + ': the original payload was not kept: ' + JSON.stringify(payloadOf(host.children[0])));
      if (listening(w.elt) !== 0) fail(kind + ' ' + how + ': listeners stayed attached after the settlement: ' + listening(w.elt));
      // A late repeat of the failure (body and form both see it) is not a second outcome.
      w.elt.fire(kind, { requestConfig: w.cfg, xhr: { status: 0 } });
      p.dispatch(kind, { detail: { requestConfig: w.cfg, xhr: { status: 0 } } });
      if (host.children.length !== 1) fail(kind + ' ' + how + ': a repeated failure event made a second outcome');
      if (p.sent !== (how === 'replaced-by-another-result' ? 2 : 1)) fail('the script sent again');
    }
  }
  // A delivered answer leaves nothing attached either, and a request that was already settled by
  // its answer is not reported as unknown by the afterRequest that follows.
  const p = load();
  p.type('send-text', 'ls');
  const ok = p.request(TEXT, { text: 'ls' }, { id: 'text-form' });
  ok.respond(DELIVERED);
  if (listening(ok.elt) !== 0 || p.els['session-unknown'].children.length !== 0) fail('a delivered request left listeners or an unknown outcome');
});

// Finding 2: an unknown outcome keeps the ORIGINAL submitted payload, target, action,
// id and time until it is dismissed explicitly. Nothing else on the page erases it.
scenario('an unknown outcome keeps its own payload when the box is edited and another send succeeds', async () => {
  const p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }, { id: 'a-form' }).fail('htmx:timeout', { detached: true });
  const host = p.els['session-unknown'];
  const a = host.children[0];
  if (!a || payloadOf(a) !== 'A') fail('A was not kept in the warning');
  if (!/minion-1 on pi-1/.test(a.textContent) || !/\d\d:\d\d:\d\d UTC/.test(a.textContent) || !/Text/.test(a.textContent)) fail('the warning lacks target, time or action: ' + a.textContent);
  if (/kept in the box|stays in the box/i.test(a.textContent)) fail('the warning claims the text is still in the box');
  if (!a.attrs['data-unknown-id']) fail('the warning has no id');
  // The operator edits the box to B and B is delivered: the box clears, A stays.
  p.type('send-text', 'B');
  p.request(TEXT, { text: 'B' }, { id: 'b-form' }).respond(DELIVERED);
  if (p.box.value !== '') fail('B was not cleared');
  if (host.children.length !== 1 || host.children[0] !== a || payloadOf(a) !== 'A') fail('delivering B erased or changed the unknown outcome of A');
  // Keys, a task, an ordinary result and a screen refresh do not touch it either.
  p.request(KEYS, { key: 'Enter' }, { id: 'k' }).respond([]);
  p.request('/s/pi-1/minion-1/task', { task: 'x' }, { id: 't' }).respond(['minion-screen-refresh']);
  p.els['session-result'].textContent = 'something else';
  p.screenChanged();
  await sleep(450);
  if (host.children.length !== 1 || payloadOf(host.children[0]) !== 'A') fail('a later result or refresh removed the unknown outcome of A');
  // The record belongs to its own request: a second unknown outcome adds one, with another id.
  p.request(KEYS, { key: 'C-c' }, { id: 'k2' }).fail('htmx:sendError', { detached: true });
  if (host.children.length !== 2 || payloadOf(host.children[1]) !== 'C-c') fail('a keys payload was not kept: ' + host.children.length);
  if (host.children[0].attrs['data-unknown-id'] === host.children[1].attrs['data-unknown-id']) fail('two unknown outcomes share an id');
  // Only the explicit dismissal of one removes that one.
  host.children[0].children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
  if (host.children.length !== 1 || payloadOf(host.children[0]) !== 'C-c') fail('Dismiss removed the wrong outcome');
  if (p.sent !== 5) fail('something was sent again: ' + p.sent);
});

scenario('a Worker-reported unknown outcome is kept with the submitted payload outside the replaceable result', async () => {
  for (const [path, params, want] of [[TEXT, { text: 'deploy --now\nyes', enter: '1' }, 'deploy --now\nyes'], [KEYS, { key: 'C-c' }, 'C-c'], [ANSWER, { choice_text: 'maybe' }, 'maybe']]) {
    const p = load({ dialog: path === ANSWER });
    if (path === TEXT) p.type('send-text', params.text);
    p.request(path, params, { id: 'f' }).respond({ 'minion-screen-refresh': true, 'minion-input-unknown': { outcome: 'unknown', reason: 'timeout' } }, { detached: true });
    const host = p.els['session-unknown'];
    if (host.children.length !== 1) fail(path + ': a Worker-reported unknown outcome was not kept (' + host.children.length + ')');
    if (payloadOf(host.children[0]) !== want) fail(path + ': wrong payload ' + JSON.stringify(payloadOf(host.children[0])));
    if (!/timeout/.test(host.children[0].textContent) || !/Delivery is unknown/.test(host.children[0].textContent)) fail(path + ': the warning does not carry the Worker outcome');
    // Not delivered: the draft is not cleared by an unknown outcome.
    if (path === TEXT && p.box.value !== params.text) fail('the draft was cleared by an unknown outcome');
    // The next results replace #session-result, never the unknown area.
    p.els['session-result'].textContent = 'Key Enter: Delivered.';
    p.request(KEYS, { key: 'Enter' }, { id: 'g' }).respond([]);
    p.request('/s/pi-1/minion-1/task', { task: 'x' }, { id: 'h' }).respond([]);
    if (host.children.length !== 1 || payloadOf(host.children[0]) !== want) fail(path + ': a later result removed the Worker-reported unknown outcome');
  }
  // A partial outcome keeps the submitted payload the same way.
  const p = load();
  p.type('send-text', 'abc');
  p.request(TEXT, { text: 'abc' }, { id: 'f' }).respond({ 'minion-input-unknown': { outcome: 'partial', reason: 'pane_changed' } });
  if (p.els['session-unknown'].children.length !== 1 || !/Partly sent/.test(p.els['session-unknown'].textContent)) fail('a partial outcome was not kept: ' + p.els['session-unknown'].textContent);
});

const TASK = '/s/pi-1/minion-1/task';

// A task save is a write too: an unknown outcome may have changed the user's label.
// It is kept like any other unknown outcome, with the ORIGINAL task text.
scenario('an unknown task save keeps its original task text, for transport failures and the Worker alike', async () => {
  for (const how of ['transport', 'worker']) {
    const p = load();
    const a = p.request(TASK, { task: 'ship release A' }, { id: 'task-form' });
    if (how === 'transport') { a.fail('htmx:timeout', { detached: true }); }
    else { a.respond({ 'minion-screen-refresh': true, 'minion-input-unknown': { outcome: 'unknown', reason: 'timeout' } }); }
    const host = p.els['session-unknown'];
    if (host.children.length !== 1) fail(how + ': an unknown task save was not kept');
    const rec = host.children[0];
    if (payloadOf(rec) !== 'ship release A') fail(how + ': the task text was not kept: ' + JSON.stringify(payloadOf(rec)));
    if (!/Task/.test(rec.textContent) || !/minion-1 on pi-1/.test(rec.textContent) || !/\d\d:\d\d:\d\d UTC/.test(rec.textContent) || !rec.attrs['data-unknown-id']) fail(how + ': no action, target, time or id: ' + rec.textContent);
    if (how === 'worker' && !/timeout/.test(rec.textContent)) fail('the Worker outcome is not shown');
    if (!/no terminal input/.test(rec.textContent)) fail('the record does not say that a task sends no terminal input');
    // A later action B (a successful task save, a key, text) does not remove it.
    p.request(TASK, { task: 'ship release B' }, { id: 'task-b' }).respond(['minion-screen-refresh', 'minion-task-saved']);
    p.request(KEYS, { key: 'Enter' }, { id: 'k' }).respond([]);
    p.type('send-text', 'ls');
    p.request(TEXT, { text: 'ls' }, { id: 't' }).respond(DELIVERED);
    p.els['session-result'].textContent = 'later';
    if (host.children.length !== 1 || host.children[0] !== rec || payloadOf(rec) !== 'ship release A') fail(how + ': a later success removed or changed the task A record');
    if (p.sent !== 4) fail('something was sent again: ' + p.sent);
    // The record is only gone by an explicit dismissal.
    rec.children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
    if (host.children.length !== 0) fail('Dismiss did not remove the task record');
  }
  // A refused task save (4xx) is not an unknown outcome; the task field is not gated while paused (it sends no terminal input).
  let p = load();
  p.request(TASK, { task: 'x' }).respond([], { status: 403 });
  if (p.els['session-unknown'].children.length !== 0) fail('a 403 on a task save was shown as unknown');
  // Task records count towards the bound too, and nothing is dropped at the limit.
  p = load();
  let n = 0;
  let refused = false;
  for (; n < 100; n++) {
    const r = p.request(TASK, { task: 'task' + n }, { id: 'f' + n });
    if (r.cancelled) { refused = true; break; }
    r.fail('htmx:sendError', { detached: true });
  }
  const host = p.els['session-unknown'];
  if (!refused || n < 2 || n > 20 || host.children.length !== n) fail('the bound does not hold for task records: refused=' + refused + ' n=' + n + ' kept=' + host.children.length);
  for (let i = 0; i < n; i++) { if (payloadOf(host.children[i]) !== 'task' + i) fail('task record ' + i + ' was lost or changed'); }
});

// A refused answer (minion-dialog-stale) belongs to the dialog the request was reviewed
// for. Its late response, after the panel was replaced by dialog B (the form of A is
// detached and its events reach only its own listeners), must not drop what the person
// typed or chose for B. The identity is the reviewed tokens the request posted.
scenario('a late stale answer for dialog A never drops the draft or the choice of dialog B', async () => {
  for (const detached of [true, false]) {
    // Typed answer for B.
    let p = load({ dialog: true });
    p.showDialog('A').radios[0].checked = true;
    const a = p.request(ANSWER, { choice: '1', expect_digest: 'A' }, { id: 'answer-form-A' });
    p.showDialog('B');                       // the screen refresh replaced the form: dialog B
    p.type('answer-choice', 'typed for B');
    a.respond(['minion-screen-refresh', 'minion-dialog-stale'], { detached });
    if (p.dom.answer.value !== 'typed for B') fail('detached=' + detached + ': the stale answer of A dropped the typed answer of B: ' + JSON.stringify(p.dom.answer.value));
    // Chosen option of B.
    p = load({ dialog: true });
    p.showDialog('A').radios[0].checked = true;
    const a2 = p.request(ANSWER, { choice: '1', expect_digest: 'A' }, { id: 'answer-form-A' });
    p.showDialog('B').radios[2].checked = true;
    a2.respond(['minion-screen-refresh', 'minion-dialog-stale'], { detached });
    if (!p.dom.answerForm.radios[2].checked) fail('detached=' + detached + ': the stale answer of A dropped the chosen option of B');
    // The refresh is still asked for (the draft guard holds it back, nothing is wiped).
    await sleep(300);
    // The same dialog A: the stale answer still invalidates it, text and choice.
    p = load({ dialog: true });
    p.showDialog('A').radios[0].checked = true;
    p.type('answer-choice', 'typed for A');
    const same = p.request(ANSWER, { choice_text: 'typed for A', expect_digest: 'A' }, { id: 'answer-form-A' });
    same.respond(['minion-screen-refresh', 'minion-dialog-stale'], { detached });
    if (p.dom.answer.value !== '' || p.dom.answerForm.radios.some((r) => r.checked)) fail('detached=' + detached + ': a stale answer for the dialog that is still shown did not invalidate it');
  }
  // The late response arrives once the answer form is gone altogether: nothing to protect, nothing breaks.
  const p = load({ dialog: true });
  p.showDialog('A');
  const gone = p.request(ANSWER, { choice: '1', expect_digest: 'A' }, { id: 'answer-form-A' });
  p.dom.answerForm = null; p.dom.answer = null;
  gone.respond(['minion-screen-refresh', 'minion-dialog-stale'], { detached: true });
});

scenario('an unknown outcome shows the payload as text and nothing is stored', async () => {
  const p = load();
  const evil = '<img src=x onerror=alert(1)>"\'&`$(id)';
  p.type('send-text', evil);
  p.request(TEXT, { text: evil }, { id: 'f' }).fail('htmx:sendError', { detached: true });
  if (payloadOf(p.els['session-unknown'].children[0]) !== evil) fail('the payload is not shown verbatim as text');
  // The node holding it is a plain element created by the script (innerHTML and web storage are trapped by the stub).
  if (p.fetches.length) fail('the script fetched something');
});

scenario('unresolved outcomes are bounded without losing any: a limit forces an explicit resolution', async () => {
  const p = load();
  let refused = null;
  let n = 0;
  for (; n < 100; n++) {
    p.type('send-text', 'cmd' + n);
    const r = p.request(TEXT, { text: 'cmd' + n }, { id: 'f' + n });
    if (r.cancelled) { refused = r; break; }
    r.fail('htmx:timeout', { detached: true });
  }
  const host = p.els['session-unknown'];
  if (!refused) fail('no limit: the unresolved outcomes grow without bound');
  if (n < 2 || n > 20) fail('implausible limit ' + n);
  if (host.children.length !== n) fail('an unresolved outcome was dropped at the limit: ' + host.children.length + ' of ' + n);
  for (let i = 0; i < n; i++) { if (payloadOf(host.children[i]) !== 'cmd' + i) fail('payload ' + i + ' changed or was lost'); }
  if (!/Not sent/.test(p.els['session-result'].textContent) || !/Dismiss/.test(p.els['session-result'].textContent)) fail('the refused write does not say that outcomes must be resolved: ' + p.els['session-result'].textContent);
  if (p.box.value !== 'cmd' + n) fail('the refused draft was lost');
  if (p.sent !== n) fail('a refused write was sent or an unknown one was replayed: ' + p.sent);
  // Resolving one (explicit dismissal) frees exactly one place.
  host.children[0].children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
  const again = p.request(TEXT, { text: 'cmd' + n }, { id: 'again' });
  if (again.cancelled) fail('the write stayed refused after an outcome was dismissed');
});

scenario('a page reload never discards an unresolved outcome silently', async () => {
  const p = load();
  p.type('send-text', 'A');
  p.request(TEXT, { text: 'A' }, { id: 'f' }).fail('htmx:timeout', { detached: true });
  p.es.l.worker_offline(p.ev({ worker_id: 'w1' }));
  p.es.l.session_removed(p.ev({ worker_id: 'w1', session: 'minion-1' }));
  if (p.reloads !== 0) fail('the page reloaded and lost the unresolved outcome: ' + p.reloads);
  const host = p.els['session-unknown'];
  if (!/Dismiss/.test(host.textContent) || host.children.length !== 2) fail('no explanation that the reload waits: ' + host.textContent);
  host.children[0].children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
  if (p.reloads !== 1) fail('the reload did not run once the outcome was dismissed: ' + p.reloads);
  // Losing access ends the view at once: while the page reloads nothing may be sent from it, by Enter, the buttons or the key surface.
  const z = load();
  z.es.onopen();
  z.type('send-text', 'rm -rf build');
  z.es.l.access_lost(z.ev({}));
  z.keydown(z.box, 'Enter');
  z.keydown(z.els['remote-keys'], 'ArrowDown');
  z.click(z.els['send-primary']);
  eq(z.log.length, 0, 'something was sent after access was lost');
  if (!/access to this session ended/.test(z.els['send-note'].textContent) || !/^Not sent\./.test(z.els['remote-keys-state'].textContent)) fail('not told: ' + z.els['send-note'].textContent);
  // Losing access is not deferred: the page may no longer show the session.
  const q = load();
  q.type('send-text', 'A');
  q.request(TEXT, { text: 'A' }, { id: 'f' }).fail('htmx:timeout', { detached: true });
  q.es.l.access_lost(q.ev({}));
  if (q.reloads !== 1) fail('access_lost did not reload');
});

// ---- held Enter on the write controls, and the blank primary Send ------------------------------------------------------------
const HELD = ['Send Enter', 'primary Send', 'Paste without Enter'];
const heldButton = (p, which) => (which === 'Send Enter' ? p.keyButtons[0] : which === 'primary Send' ? p.els['send-primary'] : p.els['send-paste']);

scenario('a held Enter on a focused write button activates it once, also AFTER the first response settled', () => {
  for (const which of HELD) {
    for (const synthetic of [false, true]) {
      const p = load();
      p.type('send-text', 'ls');
      const b = heldButton(p, which);
      b.focus();
      const first = p.press(b, 'Enter');
      if (first.ev.defaultPrevented || !first.clicked || first.clicked === 'dropped') fail(which + ': the first Enter of a press was not an ordinary activation');
      eq(p.log.length, 1, which + ' first activation');
      // While it is on its way, then after it settled (the in-flight guard has let go: nothing but the key guard can stop the repeat).
      for (let i = 0; i < 3; i++) { if (!p.press(b, 'Enter', { repeat: !synthetic }).ev.defaultPrevented) fail(which + ': a repeat while pending was left to activate the button'); }
      p.requestsOpen[0].respond(DELIVERED);
      const droppedBefore = p.dropped;
      p.type('send-text', 'next');
      for (let i = 0; i < 5; i++) {
        const r = p.press(b, 'Enter', { repeat: !synthetic });
        if (!r.ev.defaultPrevented || r.clicked) fail(which + ': a repeat after settlement activated the button');
      }
      eq(p.log.length, 1, which + ' a held Enter after settlement is one write');
      eq(p.dropped, droppedBefore, which + ': the repeats must be stopped at the key, not dropped by the in-flight guard');
      eq(p.queued, 0, which + ' queued');
      // The key is released: a NEW press is an ordinary activation again.
      p.release(b, 'Enter');
      const again = p.press(b, 'Enter');
      if (again.ev.defaultPrevented || !again.clicked || again.clicked === 'dropped') fail(which + ': a new press after keyup was refused');
      eq(p.log.length, 2, which + ' a new press is a new write');
      p.release(b, 'Enter');
    }
  }
});

scenario('the held-Enter guard covers every write control but takes nothing else over', () => {
  const p = load({ dialog: true });
  // The answer box (implicit form submission) and a force-confirm button are write controls too.
  const force = { tagName: 'BUTTON', id: '', listeners: {}, closest: (sel) => (sel === '[data-write]' ? force : null) };
  for (const el of [p.dom.answer, force]) {
    if (p.press(el, 'Enter').ev.defaultPrevented) fail('the first Enter on a write control was refused');
    if (!p.press(el, 'Enter', { repeat: true }).ev.defaultPrevented) fail('a repeat Enter on a write control was not cancelled');
    p.release(el, 'Enter');
  }
  // Not ours: other keys (local editing), IME confirmation, Shift+Enter in the text box, controls outside the write forms.
  const b = p.keyButtons[0];
  for (const k of ['ArrowLeft', 'ArrowRight', 'Backspace', 'Delete', 'Home', 'End', 'a', 'Tab', 'Escape']) {
    if (p.press(p.dom.answer, k, { repeat: true }).ev.defaultPrevented) fail(k + ' repeat in the answer box was taken over');
    if (p.press(b, k, { repeat: true }).ev.defaultPrevented) fail(k + ' repeat on a key button was taken over');
  }
  for (const o of [{ isComposing: true, repeat: true }, { keyCode: 229, repeat: true }]) {
    if (p.press(p.dom.answer, 'Enter', o).ev.defaultPrevented) fail('an IME Enter ' + JSON.stringify(o) + ' was taken over');
  }
  if (p.press(p.box, 'Enter', { shiftKey: true, repeat: true }).ev.defaultPrevented) fail('Shift+Enter in the text box must stay a native line break');
  const outside = { tagName: 'BUTTON', closest: () => null, listeners: {} };
  p.press(outside, 'Enter');
  if (p.press(outside, 'Enter', { repeat: true }).ev.defaultPrevented) fail('a button outside the write forms was taken over');
  // After a keyup the next press is ordinary (a lost press never leaves later presses refused).
  const q = load();
  q.press(q.keyButtons[0], 'Enter');
  eq(q.log.length, 1, 'first press');
  q.release(q.keyButtons[0], 'Enter');
  q.requestsOpen[0].respond({});
  q.press(q.keyButtons[0], 'Enter');
  eq(q.log.length, 2, 'a separate press after keyup');
});

scenario('Space on a focused write button stays one native activation on keyup, with no repeat write', () => {
  for (const which of HELD) {
    const p = load();
    p.type('send-text', 'ls');
    const b = heldButton(p, which);
    for (let i = 0; i < 6; i++) {
      if (p.press(b, ' ', { repeat: i > 0 }).ev.defaultPrevented) fail(which + ': Space keydown was taken over (native activation must stay accessible)');
    }
    eq(p.log.length, 0, which + ': Space writes only on keyup');
    const up = p.release(b, ' ');
    if (!up.clicked || up.clicked === 'dropped') fail(which + ': Space did not activate on keyup');
    eq(p.log.length, 1, which + ' Space is one write');
  }
});

scenario('the primary Send and Enter share one blank rule: whitespace-only is never sent and the draft is kept', () => {
  for (const blank of ['   ', '\t', '\n\n', '   ', '﻿']) {
    const p = load();
    p.type('send-text', blank);
    p.click(p.els['send-primary']);
    if (!p.lastClick.defaultPrevented) fail(JSON.stringify(blank) + ': the primary click was not refused before htmx dispatch');
    p.keydown(p.box, 'Enter');
    eq(p.log.length, 0, JSON.stringify(blank) + ': a blank draft was sent');
    eq(p.sent, 0, JSON.stringify(blank) + ': a request left');
    eq(p.box.value, blank, 'the blank draft was cleared');
    if (!/Type some text first/.test(p.els['send-note'].textContent) || !/Nothing was sent/.test(p.els['send-note'].textContent)) fail('no friendly status: ' + p.els['send-note'].textContent);
    if (p.els['send-primary'].disabled) fail('the Send button stayed disabled');
    // htmx level backstop (a request that was built anyway): cancelled before it leaves.
    const r = p.request(TEXT, { text: blank, enter: '1' }, { id: 'text-form' });
    if (!r.cancelled) fail('a blank enter=1 request was not cancelled before it left');
    // An explicit paste of whitespace keeps working (enter=0).
    const q = load();
    q.type('send-text', blank);
    q.click(q.els['send-paste']);
    eq(sentOf(q), ['text:' + JSON.stringify({ text: blank, enter: '0' })], 'explicit paste-only whitespace');
  }
  const ok = load();
  ok.type('send-text', ' ls ');
  ok.click(ok.els['send-primary']);
  eq(sentOf(ok), ['text:{"text":" ls ","enter":"1"}'], 'a non-blank send is untrimmed, once');
  ok.click(ok.els['send-primary']);
  eq(ok.log.length, 1, 'a click while it is on its way is dropped');
  eq(ok.queued, 0, 'queued');
});

// ---- the pipe conversation composer (Story 7.5): the same chat keys, the same guards ----------------------------------------------
const pipeSent = (p) => p.log.filter((r) => r.path === PIPE).map((r) => JSON.stringify(r.params));

scenario('pipe composer: Enter sends one message, Shift+Enter is a line break, an IME Enter, a held Enter and a blank draft send nothing', () => {
  const p = load({ pipe: true });
  p.type('pipe-text', 'hello agent');
  p.keydown(p.pipeBox, 'Enter');
  eq(pipeSent(p), ['{"text":"hello agent"}'], 'Enter sends one pipe request');
  eq(p.log.filter((r) => r.path === TEXT || r.path === KEYS).length, 0, 'the pipe is not the terminal');
  // Shift+Enter, Ctrl, Alt, Meta + Enter are the browser's (a native line break): not taken over, nothing sent.
  const q = load({ pipe: true });
  q.type('pipe-text', 'a');
  for (const o of [{ shiftKey: true }, { ctrlKey: true }, { altKey: true }, { metaKey: true }]) {
    if (q.keydown(q.pipeBox, 'Enter', o).defaultPrevented) fail('Enter with ' + JSON.stringify(o) + ' in the message box was taken over');
  }
  eq(q.log.length, 0, 'a modified Enter sent something');
  // IME composition: never a send.
  for (const o of [{ isComposing: true }, { keyCode: 229 }]) {
    const r = load({ pipe: true });
    r.type('pipe-text', 'にほん');
    if (r.keydown(r.pipeBox, 'Enter', o).defaultPrevented || r.log.length) fail('an IME Enter ' + JSON.stringify(o) + ' sent or was taken over');
  }
  // Local editing keys stay native.
  for (const k of ['ArrowLeft', 'ArrowRight', 'Backspace', 'Delete', 'Home', 'End', ' ', 'a', 'Tab', 'Escape']) {
    if (p.keydown(p.pipeBox, k).defaultPrevented) fail(k + ' in the message box was taken over');
  }
  // Held: one Enter after the press settled is not a second message (the in-flight guard has let go).
  const h = load({ pipe: true });
  h.type('pipe-text', 'one');
  h.keydown(h.pipeBox, 'Enter');
  h.requestsOpen[0].respond(DELIVERED);
  h.type('pipe-text', 'next');
  for (let i = 0; i < 4; i++) { if (!h.keydown(h.pipeBox, 'Enter', { repeat: true }).defaultPrevented) fail('a repeated Enter inserted a line break'); }
  eq(pipeSent(h).length, 1, 'a held Enter after settlement');
  h.keydown(h.pipeBox, 'Enter');
  eq(pipeSent(h).length, 2, 'a new press is a new message');
  // Blank or whitespace-only: refused with a friendly status, the draft kept, no request, no click dispatched.
  for (const blank of ['', '   ', '\n\t', '﻿']) {
    const b = load({ pipe: true });
    b.type('pipe-text', blank);
    b.keydown(b.pipeBox, 'Enter');
    b.click(b.els['pipe-send']);
    if (!b.lastClick.defaultPrevented) fail(JSON.stringify(blank) + ': the Send message click was not refused before htmx dispatch');
    eq(b.sent, 0, JSON.stringify(blank) + ': a request left');
    eq(b.pipeBox.value, blank, 'the draft was cleared');
    if (!/Type a message first/.test(b.els['pipe-note'].textContent)) fail('no status: ' + b.els['pipe-note'].textContent);
    if (!b.request(PIPE, { text: blank }, { id: 'pipe-form' }).cancelled) fail('the htmx backstop let a blank message through');
  }
  // While one is on its way a second Enter and a second click send nothing and queue nothing, and say so.
  const f = load({ pipe: true });
  f.type('pipe-text', 'A');
  f.keydown(f.pipeBox, 'Enter');
  f.keydown(f.pipeBox, 'Enter');
  f.click(f.els['pipe-send']);
  eq(pipeSent(f).length, 1, 'requests while one is on its way');
  eq(f.queued, 0, 'queued');
  if (!/still on its way/.test(f.els['pipe-note'].textContent)) fail('no explanation: ' + f.els['pipe-note'].textContent);
});

scenario('pipe composer: a held Enter on the Send message button activates it once, after settlement too', () => {
  const p = load({ pipe: true });
  p.type('pipe-text', 'hi');
  const b = p.els['pipe-send'];
  b.focus();
  const first = p.press(b, 'Enter');
  if (first.ev.defaultPrevented || !first.clicked || first.clicked === 'dropped') fail('the first Enter was not an ordinary activation');
  p.requestsOpen[0].respond(DELIVERED);
  p.type('pipe-text', 'more');
  const dropped = p.dropped;
  for (let i = 0; i < 4; i++) {
    const r = p.press(b, 'Enter', { repeat: true });
    if (!r.ev.defaultPrevented || r.clicked) fail('a repeat after settlement activated the button');
  }
  eq(pipeSent(p).length, 1, 'a held Enter on the button');
  eq(p.dropped, dropped, 'stopped at the key, not by the in-flight guard');
  p.release(b, 'Enter');
  p.press(b, 'Enter');
  eq(pipeSent(p).length, 2, 'a new press after keyup');
  // Space activates once, on keyup.
  const q = load({ pipe: true });
  q.type('pipe-text', 'hi');
  for (let i = 0; i < 5; i++) { if (q.press(q.els['pipe-send'], ' ', { repeat: i > 0 }).ev.defaultPrevented) fail('Space was taken over'); }
  eq(q.log.length, 0, 'Space wrote on keydown');
  q.release(q.els['pipe-send'], ' ');
  eq(pipeSent(q).length, 1, 'Space is one write on keyup');
});

scenario('pipe composer: draft, pending and unknown are counted in the frame state, as a share and in the totals, and carry no text', () => {
  const p = load({ pipe: true });
  let st = p.control('state');
  eq([st.dirty, st.pending, st.unknown, st.chat], [false, 0, 0, { draft: false, pending: 0, unknown: 0 }], 'a quiet view');
  // Draft: the conversation draft alone makes the view dirty and the share says it is the conversation's.
  p.type('pipe-text', 'secret message');
  st = p.control('state');
  eq([st.dirty, st.chat.draft], [true, true], 'a conversation draft');
  if (JSON.stringify(st).includes('secret')) fail('text reached the state');
  // A text draft is dirty but is not the conversation's.
  const t = load({ pipe: true });
  t.type('send-text', 'ls');
  st = t.control('state');
  eq([st.dirty, st.chat.draft], [true, false], 'a terminal-text draft is not a conversation draft');
  // Pending: the message on its way is in both counts; settled, in none.
  p.keydown(p.pipeBox, 'Enter');
  st = p.control('state');
  eq([st.pending, st.chat.pending], [1, 1], 'a pipe message on its way');
  // While it is on its way, suspending touches nothing of it, and the state still says so.
  p.control('suspend');
  st = p.control('state');
  eq([st.pending, st.chat.pending, st.suspended], [1, 1, true], 'suspended with a message on its way');
  p.requestsOpen[0].respond(DELIVERED);
  st = p.control('state');
  eq([st.pending, st.chat.pending, st.dirty], [0, 0, false], 'after the answer (the delivered draft was cleared)');
  // Unknown: a message whose answer never came is kept as the conversation's unknown delivery, with the original text.
  const u = load({ pipe: true });
  u.type('pipe-text', 'keep this <message>');
  u.keydown(u.pipeBox, 'Enter');
  u.requestsOpen[0].fail('htmx:timeout');
  st = u.control('state');
  eq([st.unknown, st.chat.unknown, st.dirty, st.chat.draft], [1, 1, true, true], 'an unknown pipe delivery and the draft it left');
  const host = u.els['session-unknown'];
  const shown = payloadOf(host.children[0]);
  eq(shown, 'keep this <message>', 'the original message is kept as text');
  if (!/Message attempt 1 to minion-1 on pi-1/.test(host.children[0].textContent) || !/pipe/.test(host.children[0].textContent)) fail('the outcome does not say what it was: ' + host.children[0].textContent);
  // Editing the box afterwards neither changes nor clears the kept payload, and nothing is sent again.
  u.type('pipe-text', 'B');
  eq(payloadOf(host.children[0]), 'keep this <message>', 'a later edit changed the kept payload');
  eq(pipeSent(u).length, 1, 'the unknown message was sent again');
  eq(u.control('state').chat.unknown, 1, 'still unknown');
  // Dismissing it ends the count; the draft B stays.
  host.children[0].children.find((c) => c.tagName === 'BUTTON').listeners.click[0]();
  st = u.control('state');
  eq([st.unknown, st.chat.unknown, st.chat.draft], [0, 0, true], 'after Dismiss the draft B is still kept');
  // The Worker's own unknown/partial answer is kept the same way.
  const w = load({ pipe: true });
  w.type('pipe-text', 'partial one');
  w.keydown(w.pipeBox, 'Enter');
  w.requestsOpen[0].respond({ 'minion-input-unknown': { outcome: 'partial', reason: 'worker_lost' } });
  eq(w.control('state').chat.unknown, 1, 'a partial pipe delivery');
  eq(w.pipeBox.value, 'partial one', 'the draft of an unknown outcome was cleared');
});

scenario('pipe composer: A/B drafts: a delivered message clears only what it carried; a later edit survives; the terminal draft is untouched', () => {
  const p = load({ pipe: true });
  p.type('pipe-text', 'A');
  p.type('send-text', 'terminal draft');
  p.keydown(p.pipeBox, 'Enter');
  p.type('pipe-text', 'AB'); // edited while the first is on its way
  p.requestsOpen[0].respond(DELIVERED);
  eq(p.pipeBox.value, 'AB', 'a newer draft was erased by the answer to the older message');
  eq(p.box.value, 'terminal draft', 'the terminal draft was touched by a pipe delivery');
  // The same text again at a new revision (A -> AB -> A) is an edit too.
  const q = load({ pipe: true });
  q.type('pipe-text', 'A');
  q.keydown(q.pipeBox, 'Enter');
  q.type('pipe-text', 'AB'); q.type('pipe-text', 'A');
  q.requestsOpen[0].respond(DELIVERED);
  eq(q.pipeBox.value, 'A', 'an edit that came back to the same text was erased');
  // After a delivery the focus goes back to the message box only where it was (or nowhere): a person who went on to another field is not pulled back.
  const f1 = load({ pipe: true });
  f1.type('pipe-text', 'x'); f1.keydown(f1.pipeBox, 'Enter');
  f1.requestsOpen[0].respond(DELIVERED);
  if (!f1.pipeBox.focused) fail('the focus did not return to the message box after a delivery');
  const f2 = load({ pipe: true });
  f2.type('pipe-text', 'x'); f2.keydown(f2.pipeBox, 'Enter');
  f2.document.activeElement = f2.els['task-text'];
  f2.requestsOpen[0].respond(DELIVERED);
  if (f2.pipeBox.focused) fail('a delivery pulled the focus away from another field');
  // And the other way: a terminal-text delivery never clears the conversation draft.
  const r = load({ pipe: true });
  r.type('send-text', 'ls'); r.type('pipe-text', 'ls');
  r.keydown(r.box, 'Enter');
  r.requestsOpen[0].respond(DELIVERED);
  eq([r.box.value, r.pipeBox.value], ['', 'ls'], 'a terminal delivery cleared the conversation draft');
});

scenario('pipe composer: "Send anyway" repeats the ORIGINAL message once, only on the person\'s act; a refusal and an unknown outcome are never replayed', () => {
  const p = load({ pipe: true });
  p.type('pipe-text', 'original');
  p.keydown(p.pipeBox, 'Enter');
  p.requestsOpen[0].respond({}); // refused, forceable: no delivery signal
  eq(pipeSent(p).length, 1, 'a refusal was retried');
  eq(p.pipeBox.value, 'original', 'a refused message was cleared');
  // The person edits the box to B, then confirms Send anyway for the refused A: the request carries A, and B survives its delivery.
  p.type('pipe-text', 'B');
  const force = p.request(PIPE, { text: 'original', force: '1' }, { id: 'force-form' });
  p.log.push({ path: PIPE, params: { text: 'original', force: '1' } });
  force.respond(DELIVERED);
  eq(p.pipeBox.value, 'B', 'the forced delivery of A erased the draft B');
  // Unknown is not forceable and not replayed: the page makes no request on its own, whatever happens after.
  const u = load({ pipe: true });
  u.type('pipe-text', 'U');
  u.keydown(u.pipeBox, 'Enter');
  u.requestsOpen[0].fail('htmx:sendError');
  u.es.onopen();
  u.screenChanged();
  eq(pipeSent(u).length, 1, 'an unknown message was sent again by a reconnect or a refresh');
  eq(u.queued, 0, 'queued');
});

scenario('pipe composer: paused, suspended, resuming and reconnecting refuse a send and keep the draft; writes return only after a fresh read', async () => {
  const p = withFetch({ pipe: true, manual: true });
  p.es.onopen();
  p.type('pipe-text', 'keep me');
  p.els['pause-toggle'].listeners.click[0]();
  p.keydown(p.pipeBox, 'Enter');
  p.click(p.els['pipe-send']);
  eq(p.log.length, 0, 'sent while paused');
  if (!/paused/i.test(p.els['pipe-note'].textContent)) fail('no reason: ' + p.els['pipe-note'].textContent);
  if (!p.els['pipe-send'].disabled) fail('the button was not gated');
  // Whatever the buttons look like, a pipe request that is built anyway is refused where htmx has built it (a pipe message is a write).
  if (!p.request(PIPE, { text: 'x' }, { id: 'pipe-form' }).cancelled || p.sent !== 0) fail('a pipe request left while paused');
  p.els['pause-toggle'].listeners.click[0]();
  // Suspended: refused, the draft kept, and the state keeps saying so (a frame with a conversation draft is never clean).
  p.control('suspend');
  p.keydown(p.pipeBox, 'Enter');
  eq(p.log.length, 0, 'sent while suspended');
  const st = p.control('state');
  eq([st.dirty, st.chat.draft, st.suspended], [true, true, true], 'the draft is not reported while suspended');
  eq(p.pipeBox.value, 'keep me', 'the draft was lost');
  // Shown again: nothing is sent until the fresh read of the screen route came back.
  p.control('resume');
  p.sources[1].onopen();
  p.keydown(p.pipeBox, 'Enter');
  eq(p.log.length, 0, 'sent while the fresh read is outstanding');
  p.waiters[0].res(OK);
  await settleMicro();
  p.keydown(p.pipeBox, 'Enter');
  eq(pipeSent(p), ['{"text":"keep me"}'], 'the draft is sent once after the fresh read, by the person');
  // Reconnecting.
  const q = load({ pipe: true });
  q.es.onopen();
  q.type('pipe-text', 'x');
  q.es.onerror();
  q.keydown(q.pipeBox, 'Enter');
  eq(q.log.length, 0, 'sent while reconnecting');
  eq(q.pipeBox.value, 'x', 'the draft was lost');
  // Access lost: nothing is sent.
  const z = load({ pipe: true });
  z.es.onopen();
  z.type('pipe-text', 'y');
  z.es.l.access_lost(z.ev({}));
  z.keydown(z.pipeBox, 'Enter');
  z.click(z.els['pipe-send']);
  eq(z.log.length, 0, 'sent after access was lost');
});

scenario('the view tells its readers (the conversation) about events, suspension and the fresh read after a resume', async () => {
  const p = withFetch({ pipe: true, manual: true });
  const got = [];
  p.window.minionSession.subscribe((k) => got.push(k));
  p.es.onopen();
  p.screenChanged();
  p.es.l.state_changed(p.ev({ worker_id: 'w1', session: 'minion-1' }));
  p.es.l.state_changed(p.ev({ worker_id: 'w1', session: 'other' })); // not this session
  eq(got, ['events', 'events'], 'events of this session only');
  p.es.l.snapshot({});
  eq(got.pop(), 'events', 'a snapshot asks for a read');
  got.length = 0;
  p.control('suspend');
  eq(got, ['suspended'], 'suspend');
  eq(p.window.minionSession.status().suspended, true, 'status says suspended');
  p.control('resume');
  eq(got, ['suspended'], 'a read must not start before the fresh read of the screen route came back');
  eq(p.window.minionSession.status().writes, false, 'writes are off while refreshing');
  p.waiters[0].res(OK);
  await settleMicro();
  eq(got, ['suspended', 'resumed'], 'the fresh read opens reading');
  eq(p.window.minionSession.status().writes, true, 'writes are back');
  // A paused view reads nothing, and its end asks for a read.
  got.length = 0;
  p.els['pause-toggle'].listeners.click[0]();
  eq(p.window.minionSession.status().paused, true, 'paused');
  p.els['pause-toggle'].listeners.click[0]();
  eq(got, ['events'], 'the end of a pause asks for a read');
});

(async () => {
  for (const [name, fn] of scenarios) {
    if (process.argv[3] && !name.includes(process.argv[3])) { continue; } // optional: run the scenarios whose name contains argv[3]
    try { await fn(); } catch (e) { fail(name + ': ' + (e && e.stack || e)); }
  }
  console.log('ok');
  process.exit(0); // the script under test keeps retry timers alive
})();
