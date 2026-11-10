// Runs static/inspector.js (the controller of the session inspector on the list
// pages) and static/frame-child.js (the frame's half of the conversation) against
// a small DOM stub with a real element tree, a selector subset, fake timers, frames
// with their own contentWindow and postMessage. It models only what the scripts
// use; it is not a browser. The geometry (docked or drawer), the real focus order
// and the real frames are checked in the shared browser, not here.
//
// The scenarios prove, for the controller: a plain click opens the pane with the
// canonical frame address and changes nothing else; modified clicks, other links
// and unusable hrefs are left to the browser; selection is keyed by worker id and
// session name; switching keeps the frame (and so the draft and the request of the
// first session) and only suspends its reading; coming back resumes the same frame;
// a frame is discarded only after a fresh "clean" answer, never on an old one, never
// when dirty, pending, unknown or silent, and never to make room without the
// person's explicit choice; messages are accepted only from the real frame, this
// origin, the frame's target and an allowed type; nothing sent to a frame can carry
// input; Escape closes the topmost local dialog first, then the inspector, never
// reaches a session, and the focus returns to the opener or the heading; a frame that
// moved, was refused or never answered is removed with a fallback; the pane is a modal
// drawer (inert background) when narrow; the page is marked while a frame holds
// something. And for the frame: it answers queries, suspends and resumes on the
// host's word only, reports flags and counts (never text) and asks the host to close
// on Escape unless a local dialog is open.
// Review fixes (first Astra round): a hidden frame is discarded only on its own fresh clean answer, asked
// one frame at a time, void when it changed or was shown meanwhile, never the shown frame; every loaded
// document needs its own ready (a same-address error page is removed); a lost sign-in found by the
// page's own guarded read clears every kept frame while an ordinary refusal removes one; kept frames are
// not shown before a recent sign-in check; the shell's skip link is inert while the drawer is modal.
// Usage: node inspector_test.js <static dir>
'use strict';
const fs = require('fs');
const path = require('path');
const dir = process.argv[2];
const read = (f) => fs.readFileSync(path.join(dir, f), 'utf8');
const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };
const ORIGIN = 'https://hub.example.com';
const DBL = 520; // the double-click time a quiet row waits (DBL_MS of inspector.js; slow system double-click times run to 500 ms)
const CTX = 'ctx-alice-0001'; // the public identity context the server rendered for the page

// ---- the DOM stub --------------------------------------------------------------

let doc = null;
const parseCompound = (s) => {
  const out = { tag: null, id: null, classes: [], attrs: [] };
  const re = /^([a-zA-Z][a-zA-Z0-9]*)|#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:=("[^"]*"|[^\]]*))?\]|:not\(\[([\w-]+)\]\)/g;
  let m;
  while ((m = re.exec(s))) {
    if (m[1]) out.tag = m[1].toUpperCase();
    else if (m[2]) out.id = m[2];
    else if (m[3]) out.classes.push(m[3]);
    else if (m[4]) out.attrs.push([m[4], m[5] === undefined ? null : m[5].replace(/^"|"$/g, ''), false]);
    else if (m[6]) out.attrs.push([m[6], null, true]);
  }
  return out;
};
const matchesCompound = (el, c) => {
  if (c.tag && el.tagName !== c.tag) return false;
  if (c.id && el.id !== c.id) return false;
  for (const k of c.classes) if (!el.classList.contains(k)) return false;
  for (const [n, v, neg] of c.attrs) {
    const has = el.hasAttribute(n);
    if (neg) { if (has) return false; continue; }
    if (!has) return false;
    if (v !== null && el.getAttribute(n) !== v) return false;
  }
  return true;
};
const matchesSelector = (el, sel) => sel.split(',').some((one) => {
  const parts = one.trim().split(/\s+/).map(parseCompound);
  let cur = el;
  if (!matchesCompound(cur, parts[parts.length - 1])) return false;
  for (let i = parts.length - 2; i >= 0; i--) {
    cur = cur.parentNode;
    while (cur && !matchesCompound(cur, parts[i])) cur = cur.parentNode;
    if (!cur) return false;
  }
  return true;
});

class El {
  constructor(tag, attrs) {
    this.tagName = String(tag).toUpperCase();
    this.attrs = Object.assign({}, attrs);
    this.children = [];
    this.parentNode = null;
    this.l = {};
    const classes = new Set(String(this.attrs.class || '').split(/\s+/).filter(Boolean));
    this.classList = {
      add: (c) => classes.add(c), remove: (c) => classes.delete(c), contains: (c) => classes.has(c),
      toggle: (c, on) => { const want = on === undefined ? !classes.has(c) : !!on; if (want) classes.add(c); else classes.delete(c); return want; },
    };
    this.focused = 0;
    this.focusArgs = [];
    this._text = '';
    if (this.tagName === 'IFRAME') {
      const win = { posted: [], location: { href: 'about:blank', origin: 'null', pathname: 'blank', search: '' }, postMessage: (msg, to) => { win.posted.push({ msg, to }); } };
      this.contentWindow = win;
    }
    if (this.tagName === 'DIALOG') {
      this.modal = false; this.showModals = 0;
      this.showModal = () => { this.attrs.open = ''; this.modal = true; this.showModals++; };
      this.close = () => { delete this.attrs.open; this.modal = false; };
    }
  }
  get id() { return this.attrs.id || ''; }
  set id(v) { this.attrs.id = v; }
  get hidden() { return 'hidden' in this.attrs; }
  set hidden(v) { if (v) this.attrs.hidden = ''; else delete this.attrs.hidden; }
  set className(v) { this.attrs.class = v; this.classList.toggle('x', false); String(v).split(/\s+/).filter(Boolean).forEach((c) => this.classList.add(c)); }
  get className() { return this.attrs.class || ''; }
  set type(v) { this.attrs.type = v; }
  get type() { return this.attrs.type || ''; }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children.forEach((c) => { c.parentNode = null; }); this.children = []; }
  setAttribute(k, v) { this.attrs[k] = String(v); if (k === 'class') { this.classList.toggle('x', false); String(v).split(/\s+/).filter(Boolean).forEach((c) => this.classList.add(c)); } }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  hasAttribute(k) { return k in this.attrs; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(n, f) { (this.l[n] = this.l[n] || []).push(f); }
  removeEventListener(n, f) { this.l[n] = (this.l[n] || []).filter((x) => x !== f); }
  fire(n, e) { e = e || {}; e.type = n; e.target = e.target || this; (this.l[n] || []).slice().forEach((f) => f(e)); return e; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; return c; }
  contains(x) { for (let n = x; n; n = n.parentNode) if (n === this) return true; return false; }
  closest(sel) { for (let n = this; n; n = n.parentNode) if (n.tagName && matchesSelector(n, sel)) return n; return null; }
  all() { const out = []; const walk = (n) => n.children.forEach((c) => { out.push(c); walk(c); }); walk(this); return out; }
  querySelectorAll(sel) { return this.all().filter((e) => matchesSelector(e, sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  rendered() { for (let n = this; n; n = n.parentNode) { if (n.hasAttribute && (n.hasAttribute('hidden') || n.hasAttribute('data-unfocusable'))) return false; } return true; }
  focus(opt) { if (!this.rendered()) return; this.focused++; this.focusArgs.push(opt); doc.activeElement = this; }
}
const el = (tag, attrs, children) => { const e = new El(tag, attrs); (children || []).forEach((c) => e.appendChild(c)); return e; };

// ---- the page of a list -----------------------------------------------------------

// sessionRow builds a row like _session_row.html: data-worker, data-session, data-key and the two links.
function sessionRow(worker, session, workerName, o) {
  o = o || {};
  const label = session + ' on ' + workerName;
  const href = 'href' in o ? o.href : '/s/' + encodeURIComponent(workerName) + '/' + encodeURIComponent(session);
  // The real row (templates/_session_row.html): the session link in the row header, a task, the Worker link, a state; no Open button.
  const name = el('a', { href, 'data-control': 'name', 'data-inspect': '', 'data-inspect-label': label });
  const workerLink = el('a', { href: '/workers/' + encodeURIComponent(worker), 'data-control': 'worker', 'data-worker-link': '' });
  const task = el('td', { 'data-label': 'Task' });
  const state = el('td', { 'data-label': 'State' }, [el('span', { class: 'badge' })]);
  const workerCell = el('td', { 'data-label': 'Worker' }, [workerLink]);
  const row = el('tr', { 'data-worker': worker, 'data-session': session, 'data-key': 's-' + worker + '-' + session, 'data-row-link': 'session' }, [el('th', { scope: 'row' }, [name]), task, workerCell, state]);
  return { row, name, workerLink, task, state, workerCell, href };
}

function listPage(o) {
  o = o || {};
  doc = null;
  const timers = [];
  let now = 0;
  const html = el('html');
  const body = el('body');
  html.appendChild(body);
  // The shell's skip link is a sibling of the layout, outside every data-modal-inert subtree, as in layout.html (it is
  // found by its class, which is what keeps it from being reachable behind the drawer).
  const skip = el('a', { class: 'skip-link', href: '#content' });
  const rail = el('aside', { id: 'site-rail', 'data-modal-inert': '' });
  const topbar = el('header', { 'data-modal-inert': '' });
  const h1 = el('h1');
  const rowsHost = el('tbody');
  const main = el('div', { class: 'inspect-main', 'data-modal-inert': '' }, [h1, el('table', {}, [rowsHost])]);
  const kept = el('div', { id: 'inspector-kept', hidden: '' }, [el('ul', { id: 'inspector-kept-list' })]);
  const alertBox = el('p', { id: 'inspector-alert', role: 'alert', hidden: '' });
  main.appendChild(alertBox);
  main.appendChild(kept);
  const title = el('h2', { id: 'inspector-title', tabindex: '-1' });
  const openLink = el('a', { id: 'inspector-open', href: '/sessions' });
  const closeBtn = el('button', { id: 'inspector-close' });
  const status = el('p', { id: 'inspector-status', hidden: '' });
  const framesHost = el('div', { id: 'inspector-frames' });
  const host = el('aside', { id: 'inspector', 'data-context': o.pageCtx === undefined ? CTX : o.pageCtx, hidden: '' }, [el('div', {}, [title, openLink, closeBtn]), status, framesHost]);
  const scrim = el('div', { id: 'inspector-scrim' });
  const dlgList = el('ul', { id: 'inspector-capacity-list' });
  const dlgCancel = el('button', { id: 'inspector-capacity-cancel' });
  const dlg = el('dialog', { id: 'inspector-capacity' }, [dlgList, dlgCancel]);
  const layout = el('div', { id: 'inspect-layout' }, [main, host, scrim, dlg]);
  const content = el('main', { id: 'content' }, [layout]);
  [skip, rail, topbar, content].forEach((c) => body.appendChild(c));
  doc = {
    activeElement: null, body, documentElement: html, l: {},
    getElementById: (id) => html.all().find((e) => e.id === id) || null,
    querySelector: (s) => html.querySelector(s), querySelectorAll: (s) => html.querySelectorAll(s),
    createElement: (t) => el(t),
    addEventListener(n, f) { (doc.l[n] = doc.l[n] || []).push(f); },
  };
  body.addEventListener = (n, f) => { (body.l[n] = body.l[n] || []).push(f); };
  const sent = { fetch: 0, xhr: 0, ajax: 0 };
  const guards = [];
  const GUARD = /^(\/inspector\/principal|\/s\/[^/?#]+\/[^/?#]+\/context)$/;
  let aborts = 0;
  const media = { narrow: !!o.narrow, listeners: [] };
  const win = {
    location: { origin: ORIGIN, href: ORIGIN + '/sessions', assigned: [], assign(u) { win.location.assigned.push(u); }, reloads: 0, reload() { win.location.reloads++; } },
    l: {}, addEventListener(n, f) { (win.l[n] = win.l[n] || []).push(f); },
    selection: null, getSelection() { return win.selection; },
    // The only request the page may make: its own guarded READ (a GET of the sessions of one worker). Anything else counts as a send.
    fetch(url, opt) { if (!GUARD.test(url)) { sent.fetch++; return Promise.resolve({ status: 200, ok: true }); } if (!o.manual && /\/context$/.test(url)) { // the resource check is answered at once (a thenable) unless the test holds it
      guards.push({ url, opt, auto: true });
      const r = { status: 200, ok: true, type: 'basic', json: () => ({ then: (f) => { f({ context: CTX }); } }) };
      return { then: (f) => { f(r); } };
    }
    return new Promise((res, rej) => { guards.push({ url, opt, res, rej }); }); },
    AbortController: function () { this.signal = { aborted: false }; this.abort = () => { this.signal.aborted = true; aborts++; }; }, XMLHttpRequest: function () { sent.xhr++; }, htmx: { ajax() { sent.ajax++; } },
    matchMedia: () => ({ get matches() { return media.narrow; }, addEventListener: (n, f) => media.listeners.push(f) }),
    setTimeout: (f, ms) => { const t = { f, at: now + ms, id: timers.length + 1 }; timers.push(t); return t.id; },
    clearTimeout: (id) => { const t = timers.find((x) => x.id === id); if (t) t.dead = true; },
  };
  const p = { guards, aborts: () => aborts, skip, alertBox, win, doc, html, body, host, title, openLink, closeBtn, status, framesHost, scrim, dlg, dlgList, dlgCancel, kept, h1, main, rail, topbar, rowsHost, layout, sent, media, rows: {} };
  p.addRow = (worker, session, workerName, ro) => { const r = sessionRow(worker, session, workerName, ro); rowsHost.appendChild(r.row); p.rows[worker + '/' + session] = r; return r; };
  p.keptList = () => html.querySelectorAll('[data-kept]');
  p.tick = (ms) => { now += ms; timers.filter((t) => !t.dead && t.at <= now).forEach((t) => { t.dead = true; t.f(); }); };
  // Everything the script listens for on the document, the body and the window.
  p.fireDoc = (n, e) => { e = e || {}; e.type = n; (doc.l[n] || []).slice().forEach((f) => f(e)); return e; };
  p.fireWin = (n, e) => { (win.l[n] || []).slice().forEach((f) => f(e)); };
  const clock = { now: () => 1e12 + now };
  new Function('document', 'window', 'URL', 'Math', 'Date', read('inspector.js'))(doc, win, URL, Math, clock);
  return p;
}

const click = (target, o) => Object.assign({ type: 'click', button: 0, detail: 1, ctrlKey: false, metaKey: false, shiftKey: false, altKey: false, target,
  defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, o || {});
const keydown = (key, target, o) => Object.assign({ type: 'keydown', key, target: target || doc.body, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, o || {});

// ---- frames -----------------------------------------------------------------------

const frameOf = (p, session, worker) => p.framesHost.children.find((f) => f.getAttribute('title') === 'Session ' + session + ' on ' + (worker || 'pi-1')) || null;
const target = (worker, session) => ({ worker, session });
// toParent delivers a message from a frame's window. The document id of the frame's current document is added unless the
// message names one itself (or says doc: undefined to send none).
function toParent(p, frame, data, o) {
  o = o || {};
  if (data && typeof data === 'object' && !('doc' in data) && frame.__doc) { data = Object.assign({ doc: frame.__doc }, data); }
  p.fireWin('message', { origin: o.origin || ORIGIN, source: o.source === undefined ? frame.contentWindow : o.source, data });
}
let docSeq = 0;
const state = (o) => Object.assign({ dirty: false, pending: 0, unknown: 0, conn: 'live', suspended: false, ctrl: true }, o);
const msg = (type, worker, session, extra) => Object.assign({ minion: 'inspector', v: 1, type, target: target(worker, session) }, extra);
// load: the frame's document is our compact view and says ready.
function loadFrame(p, frame, worker, session, s, o) {
  o = o || {};
  frame.contentWindow.location = { href: ORIGIN + frame.getAttribute('src'), origin: ORIGIN, pathname: frame.getAttribute('src').split('?')[0], search: '?embed=1' };
  frame.__doc = 'doc' + (++docSeq);
  frame.__curDoc = frame.__doc; // what the document element of the frame's current document says (data-inspector-doc)
  frame.contentWindow.document = { documentElement: { getAttribute: (k) => (k === 'data-inspector-doc' ? frame.__curDoc : null) } };
  const ready = () => toParent(p, frame, Object.assign(msg('ready', worker, session, state(s)), { ctx: o.ctx === undefined ? CTX : o.ctx }));
  if (o.readyFirst) { ready(); frame.fire('load'); } else { frame.fire('load'); ready(); } // a script may say ready before or after the load event
}
// reloadFrame: the frame loads a new document at the same address (what the child's own reload does).
const reloadFrame = (p, frame, worker, session, s, o) => loadFrame(p, frame, worker, session, s, o);
const flush = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };
const posts = (frame) => frame.contentWindow.posted;
const lastPost = (frame) => posts(frame)[posts(frame).length - 1];
const types = (frame) => posts(frame).map((x) => x.msg.type);
// answer replies to the last query of a frame with the given fresh state.
function answer(p, frame, worker, session, s) {
  const q = posts(frame).filter((x) => x.msg.type === 'query').pop();
  if (!q) fail('no query was sent to ' + session);
  toParent(p, frame, msg('state', worker, session, Object.assign(state(s), { nonce: q.msg.nonce })));
}

// open: a plain click on the name link of a row; the frame loads and is ready.
function open(p, worker, session, workerName, s) {
  const r = p.rows[worker + '/' + session] || p.addRow(worker, session, workerName || 'pi-1');
  const e = click(r.name);
  p.fireDoc('click', e);
  const f = frameOf(p, session, workerName || 'pi-1');
  if (f && !f.__loaded) { f.__loaded = true; loadFrame(p, f, worker, session, s); }
  return { e, f, r };
}

const scenarios = [];
const scenario = (name, fn) => scenarios.push([name, fn]);
const same = (a, b, m) => { if (a !== b) fail(m + ': a different element has it'); };
const eq = (a, b, m) => { if (JSON.stringify(a) !== JSON.stringify(b)) fail(m + ': got ' + JSON.stringify(a) + ', want ' + JSON.stringify(b)); };

// ---- opening ------------------------------------------------------------------------

scenario('a plain click opens the pane with the canonical frame and leaves the list alone', () => {
  const p = listPage();
  const a = p.addRow('w1', 'minion-1', 'pi-1');
  p.addRow('w1', 'minion-2', 'pi-1');
  const before = p.rowsHost.children.length;
  p.doc.activeElement = a.name;
  const e = click(a.name);
  p.fireDoc('click', e);
  if (!e.defaultPrevented) fail('the plain click was not taken over');
  if (p.host.hidden) fail('the pane did not open');
  const f = frameOf(p, 'minion-1');
  if (!f || f.getAttribute('src') !== '/s/pi-1/minion-1?embed=1') fail('wrong frame address: ' + (f && f.getAttribute('src')));
  if (f.getAttribute('referrerpolicy') !== 'same-origin') fail('the frame leaks its referrer');
  if (f.hasAttribute('sandbox')) fail('unexpected sandbox');
  eq([p.openLink.getAttribute('href'), p.title.textContent], ['/s/pi-1/minion-1', 'minion-1 on pi-1'], 'header');
  if (p.rowsHost.children.length !== before || p.win.location.assigned.length || p.win.location.reloads) fail('the list was navigated, reloaded or changed');
  if (a.row.getAttribute('data-inspected') !== 'true' || a.name.getAttribute('aria-current') !== 'true') fail('the selected row is not marked');
  if (a.workerLink.hasAttribute('aria-current')) fail('the Worker link must not look like the selected session');
  if (p.rows['w1/minion-2'].row.hasAttribute('data-inspected')) fail('another row was marked');
  if (p.title.focused !== 1) fail('the focus did not move to the pane heading');
  if (p.status.hidden || !/Loading/.test(p.status.textContent)) fail('no loading note while the frame loads');
  loadFrame(p, f, 'w1', 'minion-1');
  if (!p.status.hidden) fail('the loading note stays after the frame is ready');
  if (p.sent.fetch + p.sent.xhr + p.sent.ajax !== 0) fail('the page sent a request of its own');
});

scenario('a click on an element inside the session link is the link\'s click; the same session is not opened twice', () => {
  const p = listPage();
  const a = p.addRow('w1', 'minion-1', 'pi-1');
  const span = el('span');
  a.name.appendChild(span);
  const e2 = click(span);
  p.fireDoc('click', e2);
  if (!e2.defaultPrevented || !frameOf(p, 'minion-1')) fail('a click inside the link was not taken over');
  p.fireDoc('click', click(a.name));
  eq(p.framesHost.children.length, 1, 'frames (the same session is not opened twice)');
});

scenario('modified clicks, middle click, other links and unusable hrefs keep the browser behaviour', () => {
  const p = listPage();
  const a = p.addRow('w1', 'minion-1', 'pi-1');
  for (const o of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }, { button: 2 }]) {
    const e = click(a.name, o);
    p.fireDoc('click', e);
    if (e.defaultPrevented) fail('the click ' + JSON.stringify(o) + ' was taken over');
  }
  a.name.setAttribute('target', '_blank');
  let e = click(a.name);
  p.fireDoc('click', e);
  if (e.defaultPrevented) fail('a link with a target was taken over');
  a.name.removeAttribute('target');
  a.name.setAttribute('download', '');
  e = click(a.name);
  p.fireDoc('click', e);
  if (e.defaultPrevented) fail('a download link was taken over');
  a.name.removeAttribute('download');
  // Other controls: New session, count link, filter button, Copy (not marked for the inspector).
  const other = el('a', { href: '/s/pi-1/minion-1', 'data-control': 'sessions' });
  const btn = el('button', { 'data-control': 'new-session' });
  a.row.appendChild(other); a.row.appendChild(btn);
  for (const t of [other, btn]) { e = click(t); p.fireDoc('click', e); if (e.defaultPrevented) fail('a control that is not a session link was taken over'); }
  // Unusable hrefs.
  const bad = [
    ['cross-origin', 'https://evil.example/s/pi-1/minion-1'], ['protocol relative', '//evil.example/s/pi-1/minion-1'], ['javascript', 'javascript:alert(1)'],
    ['other path', '/account'], ['deeper path', '/s/pi-1/minion-1/text'], ['query', '/s/pi-1/minion-1?x=1'], ['hash', '/s/pi-1/minion-1#a'], ['embed already', '/s/pi-1/minion-1?embed=1'],
    ['empty', ''], ['short', '/s/pi-1'], ['encoded slash is fine but a literal one is not', '/s/pi-1/a/b'],
  ];
  for (const [why, href] of bad) {
    const r = p.addRow('w9', 'x-' + why, 'pi-9', { href });
    e = click(r.name);
    p.fireDoc('click', e);
    if (e.defaultPrevented) fail('a link with a ' + why + ' href was taken over');
  }
  // A link with no row identity is not taken over either.
  const orphan = el('a', { href: '/s/pi-1/minion-1', 'data-inspect': '' });
  p.main.appendChild(orphan);
  e = click(orphan);
  p.fireDoc('click', e);
  if (e.defaultPrevented) fail('a link without a row identity was taken over');
  e = click(a.name, { defaultPrevented: true });
  p.fireDoc('click', e);
  if (p.framesHost.children.length || !p.host.hidden) fail('a pane was opened by a click that must not open it: ' + p.framesHost.children.length);
});

scenario('a session is keyed by the canonical worker id and the session name, not the display name', () => {
  const p = listPage();
  // Two workers with the same display name (two owners), the same session name.
  const a = p.addRow('w-alice', 'minion-1', 'pi-1');
  const b = p.addRow('w-bob', 'minion-1', 'pi-1', { href: '/s/w-bob/minion-1' });
  p.fireDoc('click', click(a.name));
  p.fireDoc('click', click(b.name));
  eq(p.framesHost.children.length, 2, 'two frames for two workers');
  eq(p.framesHost.children.map((f) => f.getAttribute('src')), ['/s/pi-1/minion-1?embed=1', '/s/w-bob/minion-1?embed=1'], 'addresses');
  if (a.row.hasAttribute('data-inspected') || b.row.getAttribute('data-inspected') !== 'true') fail('the mark went to the wrong row');
  // The same worker and session in another place of the page (an attention item) is the same context.
  const att = el('li', { 'data-worker': 'w-bob', 'data-session': 'minion-1', 'data-key': 'a-w-bob-minion-1' }, [el('a', { href: '/s/w-bob/minion-1', 'data-inspect': '', 'data-control': 'open' })]);
  p.main.appendChild(att);
  p.fireDoc('click', click(att.children[0]));
  eq(p.framesHost.children.length, 2, 'frames after opening the same session from another link');
  if (att.getAttribute('data-inspected') !== 'true' || b.row.getAttribute('data-inspected') !== 'true') fail('both places of the open session are marked');
});

// ---- switching and the cache ----------------------------------------------------------

scenario('switching A to B suspends A (frame and draft stay), returning resumes the same frame, and A is never retargeted', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  A.fire('x'); // nothing
  const B = open(p, 'w1', 'minion-2').f;
  if (!A.parentNode || !B.parentNode) fail('a frame was removed by the switch');
  if (!A.hidden || B.hidden) fail('A must be hidden and B shown');
  eq(A.getAttribute('src'), '/s/pi-1/minion-1?embed=1', 'A keeps its own target');
  eq(types(A), ['suspend'], 'messages to A');
  eq(types(B), [], 'messages to B (a new frame needs no resume)');
  eq(p.title.textContent, 'minion-2 on pi-1', 'header names B');
  // Back to A: the same element, resumed.
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  if (frameOf(p, 'minion-1') !== A || A.hidden || !B.hidden) fail('A was not restored as the same frame');
  eq(types(A), ['suspend', 'resume'], 'messages to A');
  eq(types(B), ['suspend'], 'messages to B');
  eq(p.openLink.getAttribute('href'), '/s/pi-1/minion-1', 'Open full session follows A');
  // A frame that is still loading when it is switched away is told to suspend when it is ready.
  const C = open(p, 'w1', 'minion-3').f;
  const p2 = listPage();
  const r1 = p2.addRow('w1', 'a', 'pi-1'); p2.addRow('w1', 'b', 'pi-1');
  p2.fireDoc('click', click(r1.name));
  const fa = frameOf(p2, 'a');
  p2.fireDoc('click', click(p2.rows['w1/b'].name));
  eq(types(fa), [], 'nothing can be sent to a frame that has not said ready');
  loadFrame(p2, fa, 'w1', 'a');
  eq(types(fa), ['suspend'], 'a hidden frame that became ready is suspended');
  // A reload of a hidden frame starts reading again: it is told again.
  reloadFrame(p2, fa, 'w1', 'a');
  eq(types(fa), ['suspend', 'suspend'], 'a reloaded hidden frame is suspended again');
  if (!C) fail('unreachable');
});

scenario('every message to a frame is read-only, to this exact origin, and names the target', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  const B = open(p, 'w1', 'minion-2').f;
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  p.fireDoc('keydown', keydown('Escape'));
  p.closeBtn.fire('click');
  for (const f of [A, B]) {
    for (const x of posts(f)) {
      if (x.to !== ORIGIN) fail('a message went to ' + x.to);
      if (!['suspend', 'resume', 'query'].includes(x.msg.type)) fail('a message of type ' + x.msg.type + ' was sent');
      const keys = Object.keys(x.msg).sort().join(',');
      if (!/^(doc,minion,target,type,v|doc,minion,nonce,target,type,v)$/.test(keys)) fail('a message carries more than metadata: ' + keys);
      eq(x.msg.v, 1, 'version');
      eq(x.msg.minion, 'inspector', 'tag');
      if (x.msg.target.worker !== 'w1' || !/^minion-[12]$/.test(x.msg.target.session)) fail('wrong target ' + JSON.stringify(x.msg.target));
    }
  }
  if (p.sent.fetch + p.sent.xhr + p.sent.ajax !== 0) fail('the page sent a request of its own');
});

const asked = (f) => types(f).filter((t) => t === 'query').length;

scenario('a full cache asks the hidden frames one at a time, oldest first, and discards one that answers clean now; the shown frame is never a candidate', () => {
  const p = listPage();
  const fr = [];
  for (const s of ['s1', 's2', 's3', 's4']) fr.push(open(p, 'w1', s).f); // all report clean when ready; s4 is shown
  eq(p.framesHost.children.length, 4, 'four frames');
  const r5 = p.addRow('w1', 's5', 'pi-1');
  p.fireDoc('click', click(r5.name));
  eq(fr.map(asked), [1, 0, 0, 0], 'only the oldest hidden frame is asked first');
  if (p.framesHost.children.length !== 4 || p.dlg.modal) fail('something happened before the answer came');
  answer(p, fr[0], 'w1', 's1', { dirty: true }); // dirty NOW although it said clean before
  eq(fr.map(asked), [1, 1, 0, 0], 'the next one is asked only after the first answered');
  if (!fr[0].parentNode) fail('the frame that is dirty now was discarded');
  answer(p, fr[1], 'w1', 's2', {});
  if (fr[1].parentNode) fail('the clean s2 was not discarded');
  if (!fr[0].parentNode || !fr[2].parentNode || !fr[3].parentNode) fail('a frame that was not asked was discarded');
  eq(fr.map(asked), [1, 1, 0, 0], 'the rest were never asked, and the shown frame never');
  const f5 = frameOf(p, 's5', 'pi-1');
  if (!f5) fail('s5 did not open');
  if (!f5.hidden) fail('a new frame is shown before it said ready');
  loadFrame(p, f5, 'w1', 's5');
  if (f5.hidden) fail('s5 is not shown after it said ready');
  eq(p.framesHost.children.length, 4, 'the cache stays at four');
  if (p.dlg.modal) fail('the discard dialog opened although a frame was clean');
  // A frame that does not answer is skipped after its deadline; a frame that is not ready is skipped at once.
  const q = listPage();
  const g = [];
  for (const s of ['a', 'b', 'c', 'd']) g.push(open(q, 'w1', s).f);
  q.fireDoc('click', click(q.addRow('w1', 'e', 'pi-1').name));
  q.tick(1500); // a is silent
  eq(g.map(asked), [1, 1, 0, 0], 'a silent frame is skipped after its deadline');
  answer(q, g[1], 'w1', 'b', {});
  if (g[0].parentNode === null || g[1].parentNode !== null) fail('the silent frame was discarded or the clean one was not');
});

// Review finding 1: the answer was kept while other frames were awaited (up to 1.5 s each) and the shown frame, which can be
// typed into, was a candidate. A clean answer that is overtaken by a newer state, or by the frame being shown, must not delete it.
scenario('a clean answer is void when the frame changed or was shown meanwhile; the frame being edited is never discarded', () => {
  const p = listPage();
  const fr = [];
  for (const s of ['s1', 's2', 's3', 's4']) fr.push(open(p, 'w1', s).f);
  const r5 = p.addRow('w1', 's5', 'pi-1');
  p.fireDoc('click', click(r5.name));
  // s1 starts a request and gets a draft while the question is on its way (its newer report arrives before the old answer).
  const q1 = posts(fr[0]).filter((x) => x.msg.type === 'query').pop();
  toParent(p, fr[0], msg('state', 'w1', 's1', state({ pending: 1, dirty: true })));
  toParent(p, fr[0], msg('state', 'w1', 's1', Object.assign(state(), { nonce: q1.msg.nonce }))); // the stale clean answer
  if (!fr[0].parentNode) fail('a frame with a pending write and a draft was discarded on an old clean answer');
  eq(asked(fr[1]), 1, 'the next frame is asked');
  // s2 is shown by the person while its question is on its way; the clean answer arrives afterwards.
  p.fireDoc('click', click(p.rows['w1/s2'].name));
  answer(p, fr[1], 'w1', 's2', {});
  if (!fr[1].parentNode) fail('a frame that was shown meanwhile was discarded');
  if (frameOf(p, 's5', 'pi-1')) fail('the cancelled choice opened s5');
  // Many clean frames and an active one that is typed into: the shown frame is not asked and not discarded, whatever it says.
  const q = listPage();
  const g = [];
  for (const s of ['a', 'b', 'c', 'd']) g.push(open(q, 'w1', s).f);
  q.fireDoc('click', click(q.addRow('w1', 'e', 'pi-1').name));
  toParent(q, g[3], msg('state', 'w1', 'd', state({ dirty: true, pending: 1, unknown: 1 }))); // the shown one is being typed into
  answer(q, g[0], 'w1', 'a', { dirty: true }); answer(q, g[1], 'w1', 'b', { dirty: true }); answer(q, g[2], 'w1', 'c', { dirty: true });
  eq(asked(g[3]), 0, 'the shown frame was asked');
  if (!q.dlg.modal) fail('no explicit decision although every hidden frame holds something');
  if (g.some((f) => !f.parentNode)) fail('a frame was discarded without the person choosing it');
  const names = q.dlgList.children.map((c) => c.textContent).join(' | ');
  if (!/d on pi-1 keeps unsent input, a request on its way, an unknown delivery/.test(names)) fail('the shown frame is not offered with what it holds: ' + names);
});

scenario('a frame is kept when it is dirty, has a request on its way, an unknown delivery, no answer or no ready; then the person decides', () => {
  const p = listPage();
  const fr = [];
  const mk = (s, st) => { fr.push(open(p, 'w1', s, 'pi-1', st).f); };
  mk('s1', { dirty: true }); mk('s2', { pending: 1 }); mk('s3', { unknown: 2 }); mk('s4', {});
  const r5 = p.addRow('w1', 's5', 'pi-1');
  p.fireDoc('click', click(r5.name));
  answer(p, fr[0], 'w1', 's1', { dirty: true });
  answer(p, fr[1], 'w1', 's2', { pending: 1 });
  answer(p, fr[2], 'w1', 's3', { unknown: 2 });
  eq(asked(fr[3]), 0, 'the shown frame, clean as it is, is not a candidate');
  eq(p.framesHost.children.length, 4, 'nothing was discarded');
  if (!p.dlg.modal || p.dlg.showModals !== 1) fail('no explicit decision was asked for');
  if (frameOf(p, 's5', 'pi-1')) fail('s5 was opened without room');
  const items = p.dlgList.children;
  eq(items.length, 4, 'every frame is named in the decision');
  if (!/s1 on pi-1 keeps unsent input/.test(items[0].textContent) || !/s2 on pi-1 keeps a request on its way/.test(items[1].textContent) || !/s3 on pi-1 keeps an unknown delivery/.test(items[2].textContent)) fail('the decision does not say what is kept: ' + p.dlgList.children.map((c) => c.textContent).join(' | '));
  // Cancel changes nothing.
  p.dlgCancel.fire('click');
  if (p.dlg.modal || p.framesHost.children.length !== 4) fail('Cancel changed something');
  // Discarding one is the person's choice: only that frame goes and s5 opens.
  p.fireDoc('click', click(r5.name));
  answer(p, fr[0], 'w1', 's1', { dirty: true }); answer(p, fr[1], 'w1', 's2', { pending: 1 }); answer(p, fr[2], 'w1', 's3', { unknown: 2 });
  p.dlgList.children[2].children[1].fire('click');
  if (fr[2].parentNode || !fr[0].parentNode || !fr[1].parentNode || !fr[3].parentNode) fail('the wrong frame was discarded');
  if (!frameOf(p, 's5', 'pi-1') || p.dlg.modal) fail('s5 did not open after the decision');
  // A newer choice cancels a waiting one: the late clean answer neither discards nor opens anything.
  const p2 = listPage();
  const g = [];
  for (const s of ['a', 'b', 'c', 'd']) g.push(open(p2, 'w1', s, 'pi-1').f);
  const e5 = p2.addRow('w1', 'e', 'pi-1');
  p2.fireDoc('click', click(e5.name));
  p2.fireDoc('click', click(p2.rows['w1/b'].name)); // back to an open one while the answer is pending
  answer(p2, g[0], 'w1', 'a', {});
  if (frameOf(p2, 'e', 'pi-1')) fail('a cancelled choice opened later');
  if (g.some((f) => !f.parentNode)) fail('a cancelled choice discarded a frame');
});

scenario('the oldest hidden clean frame goes first (least recently shown)', () => {
  const p = listPage();
  const fr = ['s1', 's2', 's3', 's4'].map((s) => open(p, 'w1', s).f);
  p.fireDoc('click', click(p.rows['w1/s1'].name)); // s1 is now the shown one
  const r5 = p.addRow('w1', 's5', 'pi-1');
  p.fireDoc('click', click(r5.name));
  eq(fr.map(asked), [0, 1, 0, 0], 'the oldest hidden frame (s2) is asked, not the shown s1');
  answer(p, fr[1], 'w1', 's2', {});
  if (fr[1].parentNode || !fr[0].parentNode || !fr[2].parentNode || !fr[3].parentNode) fail('not the least recently shown hidden frame was discarded');
});

scenario('closing keeps what holds something, collects what is clean, and restores the opener', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1', 'pi-1', {}).f;
  const B = open(p, 'w1', 'minion-2', 'pi-1', { dirty: true }).f;
  toParent(p, B, msg('state', 'w1', 'minion-2', state({ dirty: true })));
  if (!p.host.hasAttribute('data-frame-dirty')) fail('the page is not marked while a frame holds unsent input');
  const a = p.rows['w1/minion-2'];
  p.doc.activeElement = a.name;
  p.closeBtn.fire('click');
  if (!p.host.hidden) fail('Close did not close the pane');
  if (a.row.hasAttribute('data-inspected')) fail('the selection was not cleared');
  if (types(B).slice(-1)[0] !== 'query' && types(B).slice(-2)[0] !== 'suspend') fail('B was not suspended and asked: ' + types(B));
  answer(p, B, 'w1', 'minion-2', { dirty: true });
  answer(p, A, 'w1', 'minion-1', {});
  if (!B.parentNode) fail('the dirty frame was discarded on Close');
  if (A.parentNode) fail('the clean frame was kept after Close');
  if (!p.host.hasAttribute('data-frame-dirty')) fail('the mark was dropped while a frame still holds input');
  same(p.doc.activeElement, a.name, 'the focus returns to the control that opened it');
  // The kept session is offered, outside the pane, and opens again as the same frame.
  if (p.kept.hidden || p.kept.querySelector('button') === null) fail('a kept session is not offered');
  const keptBtn = p.kept.querySelector('button');
  if (!/minion-2 on pi-1/.test(keptBtn.textContent) || !/unsent input/.test(keptBtn.getAttribute('aria-label'))) fail('the kept button does not say what it keeps: ' + keptBtn.getAttribute('aria-label'));
  keptBtn.fire('click');
  if (p.host.hidden || frameOf(p, 'minion-2', 'pi-1') !== B || B.hidden) fail('the kept session did not return as the same frame');
  eq(types(B).slice(-1)[0], 'resume', 'it is told to resume');
});

scenario('a frame that is clean everywhere ends the dirty mark', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ pending: 1 })));
  if (!p.host.hasAttribute('data-frame-dirty')) fail('a request on its way must mark the page');
  toParent(p, A, msg('state', 'w1', 'minion-1', state()));
  if (p.host.hasAttribute('data-frame-dirty')) fail('the mark stayed after the frame was clean');
  toParent(p, A, msg('state', 'w1', 'minion-1', { minion: 'inspector', v: 1, type: 'state', target: target('w1', 'minion-1') }));
  if (!p.host.hasAttribute('data-frame-dirty')) fail('a state with missing fields must count as not clean');
  // Each field alone: a missing or malformed one keeps the frame, it never frees it.
  for (const broken of [{ dirty: undefined }, { pending: undefined }, { unknown: undefined }, { pending: -1 }, { unknown: 1.5 }, { pending: '0' }, { dirty: 'false' }]) {
    toParent(p, A, msg('state', 'w1', 'minion-1', state()));
    if (p.host.hasAttribute('data-frame-dirty')) fail('could not reset the mark');
    toParent(p, A, msg('state', 'w1', 'minion-1', Object.assign(state(), broken)));
    if (!p.host.hasAttribute('data-frame-dirty')) fail('a malformed field ' + JSON.stringify(broken) + ' counted as clean');
  }
});

// ---- messages ------------------------------------------------------------------------

scenario('messages are accepted only from the real frame, this origin, its target and an allowed type', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  const B = open(p, 'w1', 'minion-2').f;
  const dirtyMark = () => p.host.hasAttribute('data-frame-dirty');
  const D = state({ dirty: true });
  const stranger = { postMessage() {} };
  toParent(p, A, msg('state', 'w1', 'minion-1', D), { origin: 'https://evil.example' });
  toParent(p, A, msg('state', 'w1', 'minion-1', D), { origin: 'null' });
  toParent(p, A, msg('state', 'w1', 'minion-1', D), { source: stranger });
  toParent(p, A, msg('state', 'w1', 'minion-1', D), { source: null });
  toParent(p, A, msg('state', 'w1', 'minion-1', D), { source: B.contentWindow }); // A's data from B's window: wrong source for A's target
  toParent(p, A, msg('state', 'w1', 'minion-2', D)); // A's window claims B's target
  toParent(p, A, msg('state', 'w9', 'minion-1', D)); // the wrong worker
  toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', D), { v: 2 }));
  toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', D), { minion: 'other' }));
  toParent(p, A, msg('constructor', 'w1', 'minion-1', D));
  toParent(p, A, msg('__proto__', 'w1', 'minion-1', D));
  toParent(p, A, msg('send', 'w1', 'minion-1', D));
  toParent(p, A, msg('suspend', 'w1', 'minion-1', D));
  toParent(p, A, 'state'); toParent(p, A, null); toParent(p, A, 42); toParent(p, A, { minion: 'inspector' });
  toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', D), { target: null }));
  if (dirtyMark()) fail('a message that must be ignored changed the state');
  // The real one is accepted.
  toParent(p, A, msg('state', 'w1', 'minion-1', D));
  if (!dirtyMark()) fail('the genuine message was ignored');
  // Frames that are gone are not answered or believed.
  const gone = A.contentWindow;
  A.parentNode.removeChild(A);
  p.rowsHost; // no-op
  toParent(p, A, msg('close-request', 'w1', 'minion-1'), { source: gone });
});

scenario('close-request closes only for the shown frame and not over a local dialog', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  const B = open(p, 'w1', 'minion-2').f;
  toParent(p, A, msg('close-request', 'w1', 'minion-1')); // A is hidden
  if (p.host.hidden) fail('a hidden frame closed the inspector');
  p.dlg.showModal();
  toParent(p, B, msg('close-request', 'w1', 'minion-2'));
  if (p.host.hidden) fail('Escape over a local dialog closed the inspector');
  p.dlg.close();
  p.doc.activeElement = p.rows['w1/minion-2'].name;
  toParent(p, B, msg('close-request', 'w1', 'minion-2'));
  if (!p.host.hidden) fail('the frame could not close the inspector');
  same(p.doc.activeElement, p.rows['w1/minion-2'].name, 'the focus returns to the opener');
  // Escape from a frame never sent anything to any session.
  for (const f of [A, B]) for (const x of posts(f)) if (!['suspend', 'resume', 'query'].includes(x.msg.type)) fail('unexpected ' + x.msg.type);
});

// ---- Escape and focus in the parent --------------------------------------------------

scenario('Escape: topmost local dialog first, the menu next, then the inspector; docked needs the focus inside', () => {
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  p.doc.activeElement = r.name;
  p.fireDoc('click', click(r.name));
  const f = frameOf(p, 'minion-1');
  loadFrame(p, f, 'w1', 'minion-1');
  // docked, the focus on the list: Escape belongs to the list (a search box, say).
  p.doc.activeElement = r.name;
  let e = keydown('Escape', r.name);
  p.fireDoc('keydown', e);
  if (e.defaultPrevented || p.host.hidden) fail('docked: Escape in the list closed the inspector');
  // docked, the focus in the pane.
  e = keydown('Escape', p.closeBtn);
  p.fireDoc('keydown', e);
  if (!e.defaultPrevented || !p.host.hidden) fail('docked: Escape in the pane did not close it');
  same(p.doc.activeElement, r.name, 'the focus returns to the opener');
  // A drawer closes from anywhere; a local dialog goes first, then the menu.
  p.media.narrow = true;
  p.fireDoc('click', click(r.name));
  const dlg = el('dialog', { open: '' });
  p.body.appendChild(dlg);
  e = keydown('Escape', r.name);
  p.fireDoc('keydown', e);
  if (e.defaultPrevented || p.host.hidden) fail('Escape over a local dialog must leave the inspector alone');
  p.body.removeChild(dlg);
  p.rail.classList.add('open');
  e = keydown('Escape', r.name);
  p.fireDoc('keydown', e);
  if (e.defaultPrevented || p.host.hidden) fail('Escape with the menu open must leave the inspector alone');
  p.rail.classList.remove('open');
  e = keydown('Escape', r.name);
  p.fireDoc('keydown', e);
  if (!e.defaultPrevented || !p.host.hidden) fail('the drawer did not close on Escape');
  // Other keys and a prevented Escape are not ours; no key is ever sent to a frame.
  p.fireDoc('click', click(r.name));
  e = keydown('a', r.name); p.fireDoc('keydown', e);
  e = keydown('Escape', r.name, { defaultPrevented: true }); p.fireDoc('keydown', e);
  if (p.host.hidden) fail('another key or a handled Escape closed the drawer');
  for (const x of posts(f)) if (x.msg.type === 'key' || x.msg.type === 'keydown') fail('a key was sent to the session');
});

scenario('Close and the scrim close; the focus goes to the opener, its row, or the heading, never to another control', () => {
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  const other = p.addRow('w1', 'minion-2', 'pi-1');
  const writeBtn = el('button', { 'data-control': 'new-session' });
  r.row.insertBefore = null;
  r.row.appendChild(writeBtn);
  // 1. The opener is still there.
  p.fireDoc('click', click(r.name));
  p.closeBtn.fire('click');
  same(p.doc.activeElement, r.name, 'opener restored');
  // 2. A live update replaced the row: new elements with the same identity.
  p.fireDoc('click', click(r.name));
  const fresh = sessionRow('w1', 'minion-1', 'pi-1');
  p.rowsHost.removeChild(r.row);
  p.rowsHost.children.unshift(fresh.row); fresh.row.parentNode = p.rowsHost; // moved to the top
  p.scrim.fire('click');
  same(p.doc.activeElement, fresh.name, 'the replaced row control is found by identity');
  // 3. The control is gone from the row: the row itself, never the write button.
  p.fireDoc('click', click(fresh.name));
  fresh.name.parentNode.removeChild(fresh.name);
  fresh.row.appendChild(el('button', { 'data-control': 'new-session' }));
  p.closeBtn.fire('click');
  same(p.doc.activeElement, fresh.row, 'the row takes the focus');
  if (fresh.row.getAttribute('tabindex') !== '-1') fail('the row was not made focusable');
  // 4. The row is gone: the heading.
  p.fireDoc('click', click(other.name));
  p.rowsHost.removeChild(other.row);
  p.closeBtn.fire('click');
  same(p.doc.activeElement, p.h1, 'the page heading takes the focus');
  // 5. The control is hidden (a closed disclosure): the heading, not a lost focus.
  const li = el('li', { 'data-worker': 'w2', 'data-session': 's', 'data-key': 'ws-w2-s', 'data-unfocusable': '' }, [el('a', { href: '/s/pi-2/s', 'data-inspect': '', 'data-control': 'session:s' })]);
  p.main.appendChild(li);
  p.fireDoc('click', click(li.children[0]));
  p.closeBtn.fire('click');
  same(p.doc.activeElement, p.h1, 'a closed disclosure (control and row cannot be focused) falls back to the heading');
});

// ---- drawer, docked ----------------------------------------------------------------------

scenario('a narrow pane is a modal drawer with an inert background; docked it is not; crossing the width follows', () => {
  const p = listPage({ narrow: true });
  open(p, 'w1', 'minion-1');
  eq([p.host.getAttribute('role'), p.host.getAttribute('aria-modal'), p.scrim.classList.contains('open')], ['dialog', 'true', true], 'drawer');
  for (const part of [p.rail, p.topbar, p.main]) if (!part.hasAttribute('inert')) fail('the background is not inert');
  if (p.host.hasAttribute('inert')) fail('the pane itself must not be inert');
  p.media.narrow = false;
  p.media.listeners.forEach((f) => f());
  eq([p.host.getAttribute('role'), p.scrim.classList.contains('open'), p.rail.hasAttribute('inert'), p.main.hasAttribute('inert')], [null, false, false, false], 'docked');
  if (!p.layout.classList.contains('inspect-open')) fail('the layout does not give room to the docked pane');
  p.media.narrow = true;
  p.media.listeners.forEach((f) => f());
  if (!p.rail.hasAttribute('inert')) fail('back to a drawer: the background is not inert again');
  p.closeBtn.fire('click');
  if (p.rail.hasAttribute('inert') || p.main.hasAttribute('inert') || p.scrim.classList.contains('open') || p.layout.classList.contains('inspect-open')) fail('closing left the page inert or the scrim up');
});

// ---- frames that fail ------------------------------------------------------------------------

scenario('a frame that moved, was refused, never loaded or never answered is removed with a fallback, without details', () => {
  for (const how of ['cross-origin', 'moved', 'timeout', 'redirect-to-login']) {
    const p = listPage();
    const r = p.addRow('w1', 'minion-1', 'pi-1');
    p.fireDoc('click', click(r.name));
    const f = frameOf(p, 'minion-1');
    if (how === 'cross-origin') {
      Object.defineProperty(f.contentWindow, 'location', { get() { throw new Error('SecurityError'); } });
      f.fire('load');
    } else if (how === 'moved') {
      f.contentWindow.location = { href: ORIGIN + '/s/pi-1/other?embed=1', origin: ORIGIN, pathname: '/s/pi-1/other', search: '?embed=1' };
      f.fire('load');
    } else if (how === 'redirect-to-login') {
      f.contentWindow.location = { href: ORIGIN + '/login?next=x', origin: ORIGIN, pathname: '/login', search: '?next=x' };
      f.fire('load');
    } else {
      f.contentWindow.location = { href: ORIGIN + '/s/pi-1/minion-1?embed=1', origin: ORIGIN, pathname: '/s/pi-1/minion-1', search: '?embed=1' };
      f.fire('load'); // an error page of ours: loads, but no script says ready
      p.tick(9000);
    }
    if (f.parentNode) fail(how + ': the frame was not removed');
    if (p.status.hidden || !/cannot be shown here/.test(p.status.textContent) || !/Open full session/.test(p.status.textContent)) fail(how + ': no fallback: ' + p.status.textContent);
    eq(p.openLink.getAttribute('href'), '/s/pi-1/minion-1', how + ': the full link');
    if (/login|denied|forbidden|403|404/i.test(p.status.textContent)) fail(how + ': the fallback shows a reason');
    if (p.rows['w1/minion-1'].row.hasAttribute('data-inspected')) fail(how + ': a failed session stays marked');
  }
  // The blank document before the first load is not a failure; a ready frame is not timed out.
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  p.fireDoc('click', click(r.name));
  const f = frameOf(p, 'minion-1');
  f.fire('load'); // about:blank
  if (!f.parentNode) fail('about:blank was treated as a failure');
  loadFrame(p, f, 'w1', 'minion-1');
  p.tick(20000);
  if (!f.parentNode) fail('a ready frame was timed out');
});

// ---- the list updates -------------------------------------------------------------------------

scenario('a live update of the list keeps the selection by identity, wherever the row moved', () => {
  const p = listPage();
  open(p, 'w1', 'minion-1');
  p.addRow('w1', 'minion-0', 'pi-1');
  // The swap: new rows, the selected one now second.
  p.rowsHost.children.forEach((c) => { c.parentNode = null; });
  p.rowsHost.children = [];
  const x = sessionRow('w1', 'minion-0', 'pi-1');
  const y = sessionRow('w1', 'minion-1', 'pi-1');
  p.rowsHost.appendChild(x.row); p.rowsHost.appendChild(y.row);
  p.body.fire('htmx:sseMessage', {});
  if (x.row.hasAttribute('data-inspected') || y.row.getAttribute('data-inspected') !== 'true' || y.name.getAttribute('aria-current') !== 'true') fail('the selection did not follow the identity');
  if (p.host.hidden) fail('a live update closed the pane');
  // The session left the list (a filter, a removal): the pane stays, nothing is marked.
  p.rowsHost.removeChild(y.row);
  p.body.fire('htmx:afterSwap', {});
  if (p.host.hidden || x.row.hasAttribute('data-inspected')) fail('a live update that dropped the row changed the pane');
});

// ---- review fixes: the handshake of every document ---------------------------------------------------

// Review finding 5: after the first ready the deadline was cleared for good, so a reload to a compact 403/404 page at the SAME
// address (no bridge script, no ready) stayed "ready" for ever.
scenario('every loaded document must say ready itself: a same-address error page is removed at the deadline', async () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  const B = open(p, 'w1', 'minion-2').f;
  p.fireDoc('click', click(p.rows['w1/minion-1'].name)); // A is shown again
  // The child reloads (session removed, access revoked) to an error page at the same address: it loads, has no script, says nothing.
  A.contentWindow.location = { href: ORIGIN + A.getAttribute('src'), origin: ORIGIN, pathname: '/s/pi-1/minion-1', search: '?embed=1' };
  A.__curDoc = null; // an error page: it has no bridge, so no document id
  A.fire('load');
  if (!A.parentNode) fail('removed before the deadline');
  if (!/Loading/.test(p.status.textContent) || p.status.hidden) fail('a document that has not said ready is not shown as loading');
  // While it has not said ready it is not clean and no eviction may use it.
  freshStateProbe: {
    const before = posts(A).length;
    p.tick(100);
    eq(posts(A).length, before, 'a frame that is not ready is not asked');
  }
  p.tick(9000);
  if (A.parentNode) fail('the silent replacement document was kept as ready');
  if (p.status.hidden || !/cannot be shown here/.test(p.status.textContent)) fail('no fallback for the removed frame: ' + p.status.textContent);
  if (!B.parentNode) fail('another frame was removed with it');
  await flush();
  if (p.guards.length < 1) fail('a failed frame did not ask the page-level guard whether the sign-in is gone');
});

scenario('a new document that says ready, before or after its load event, is ready; the old document no longer speaks', () => {
  for (const readyFirst of [false, true]) {
    const p = listPage();
    const A = open(p, 'w1', 'minion-1').f;
    const oldDoc = A.__doc;
    reloadFrame(p, A, 'w1', 'minion-1', {}, { readyFirst });
    if (A.__doc === oldDoc) fail('test bug: same document');
    p.tick(20000);
    if (!A.parentNode || !p.status.hidden) fail('ready-first=' + readyFirst + ': a successful new document was invalidated by the order of ready and load');
    // The old document is gone: a late message of it is ignored; the new one is heard.
    toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true })), {});
    if (!p.host.hasAttribute('data-frame-dirty')) fail('the new document was not heard');
    toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', state()), { doc: oldDoc }));
    if (!p.host.hasAttribute('data-frame-dirty')) fail('a late message of the old document changed the state');
  }
  // ready without a document id, for another document, or twice for one document: ignored.
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', state({ dirty: true })), { doc: undefined }));
  toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', state({ dirty: true })), { doc: 'someone-else' }));
  toParent(p, A, Object.assign(msg('state', 'w1', 'minion-1', state({ dirty: true })), { doc: '' }));
  if (p.host.hasAttribute('data-frame-dirty')) fail('a message without or with another document id was accepted');
  toParent(p, A, Object.assign(msg('ready', 'w1', 'minion-1', state({ dirty: true })), { doc: A.__doc })); // a replayed ready
  if (p.host.hasAttribute('data-frame-dirty')) fail('a second ready of the same document was accepted');
  // The parent names the document in what it sends, so a message for the previous document cannot act on the next.
  const q = posts(A);
  p.closeBtn.fire('click');
  if (!posts(A).some((x) => x.msg.doc === A.__doc && x.msg.type === 'suspend')) fail('a message to a frame does not name its document: ' + JSON.stringify(q.map((x) => x.msg)));
});

// ---- review fixes: the identity of the page -------------------------------------------------------------

// respond answers a bounded read of the page. 2xx carries the identity context the server derived for THIS request.
const respond = (g, status, type, ctx) => g.res({ status, ok: status >= 200 && status < 300, type: type || 'basic', json: () => Promise.resolve({ context: ctx === undefined ? CTX : ctx }) });
const principalReads = (p) => p.guards.filter((g) => g.url === '/inspector/principal');
const contextReads = (p) => p.guards.filter((g) => /\/context$/.test(g.url));
const alice = (p) => p.guards.length;

// Review finding 4 of round 1, now on the principal: a lost sign-in clears EVERY kept frame at once.
scenario('a lost sign-in found by the page clears EVERY kept frame at once and the links become plain links', async () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  const B = open(p, 'w1', 'minion-2', 'pi-1').f;
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true, unknown: 1 })));
  if (!p.host.hasAttribute('data-frame-dirty')) fail('test bug');
  p.tick(30000); // the periodic check
  eq(principalReads(p).length, 1, 'the periodic check asks who this request is');
  const g = principalReads(p)[0];
  eq([g.url, g.opt.credentials, g.opt.cache, g.opt.redirect, g.opt.method, g.opt.body], ['/inspector/principal', 'same-origin', 'no-store', 'manual', undefined, undefined], 'a plain credentialed GET, no body, no redirect followed');
  respond(g, 401);
  await flush();
  if (A.parentNode || B.parentNode || p.framesHost.children.length) fail('a kept frame survived the loss of the sign-in');
  if (!p.host.hidden || p.host.hasAttribute('data-frame-dirty')) fail('the pane stayed open or the page still claims to hold input');
  if (p.alertBox.hidden || !/signed out|ended/.test(p.alertBox.textContent) || !/cleared/.test(p.alertBox.textContent)) fail('the person is not told: ' + p.alertBox.textContent);
  const e = click(p.rows['w1/minion-2'].name);
  p.fireDoc('click', e);
  if (e.defaultPrevented || p.framesHost.children.length) fail('after the sign-in was lost the link must open the full page (and its sign-in), not another frame');
});

// Review finding 2: Alice parks a frame with a draft, ANOTHER tab signs in as Bob (same cookie), and Alice's old page asks.
// The sessions list of a worker Bob may not read is an empty 200: that proves nothing about WHO is asking.
scenario('another sign-in in another tab (another user, a new login, another act-as) clears every kept frame, whatever the read says', async () => {
  const cases = [['Bob logged in', 'ctx-bob-0002'], ['the same user logged in again', 'ctx-alice-0009'], ['act-as another user', 'ctx-alice-as-carol']];
  for (const [what, other] of cases) {
    const p = listPage();
    const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
    const B = open(p, 'w1', 'minion-2').f;
    toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true })));
    p.tick(30000);
    respond(principalReads(p)[0], 200, 'basic', other); // a 200 as somebody else
    await flush();
    if (A.parentNode || B.parentNode) fail(what + ': a kept frame of the first person survived');
    if (p.alertBox.hidden || !/Another sign-in/.test(p.alertBox.textContent) || !/cleared/.test(p.alertBox.textContent)) fail(what + ': not told: ' + p.alertBox.textContent);
    if (p.host.hasAttribute('data-frame-dirty') || !p.host.hidden) fail(what + ': the page still holds or shows the first person\'s frames');
  }
  // The same, found when a parked frame is chosen again: Bob can read the SAME session, the answer is 200, the draft of Alice must still go.
  const p = listPage({ manual: true });
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true })));
  const B = open(p, 'w1', 'minion-2').f;
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  eq(contextReads(p).length, 1, 'a kept frame is checked against the server before it is shown');
  eq(contextReads(p)[0].url, '/s/pi-1/minion-1/context', 'the resource check of that session');
  if (!A.hidden) fail('the kept frame was shown before the server answered');
  respond(contextReads(p)[0], 200, 'basic', 'ctx-bob-0002');
  await flush();
  if (A.parentNode || B.parentNode) fail('Alice\'s draft survived Bob reading the same session');
  if (p.host.hasAttribute('data-frame-dirty')) fail('still marked');
  // A new frame that loads under another identity says so itself and clears what is kept.
  const q = listPage();
  const C = open(q, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  q.fireDoc('click', click(q.addRow('w1', 'minion-9', 'pi-1').name));
  const D = frameOf(q, 'minion-9', 'pi-1');
  loadFrame(q, D, 'w1', 'minion-9', {}, { ctx: 'ctx-bob-0002' });
  if (C.parentNode || D.parentNode || q.alertBox.hidden) fail('a frame rendered for another identity was kept');
});

scenario('a redirect to sign-in is a lost sign-in; a refusal, a server error or silence proves nothing about it', async () => {
  const mk = async (resp) => {
    const p = listPage();
    open(p, 'w1', 'minion-1'); open(p, 'w1', 'minion-2');
    p.tick(30000);
    const g = principalReads(p)[0];
    if (resp === 'reject') { g.rej(new Error('network')); } else { respond(g, resp.status, resp.type); }
    await flush();
    return p;
  };
  let p = await mk({ status: 0, type: 'opaqueredirect' });
  if (p.framesHost.children.length) fail('an opaque redirect (to sign-in) did not clear the frames');
  for (const r of [{ status: 403 }, { status: 404 }, { status: 500 }, { status: 502 }]) {
    p = await mk(r);
    if (p.framesHost.children.length !== 2 || /signed out|Another sign-in/.test(p.alertBox.textContent)) fail('status ' + r.status + ' was taken for a lost sign-in');
    if (!p.framesHost.children.every((f) => f.hidden)) fail('status ' + r.status + ': the identity is unconfirmed, nothing kept may be shown');
  }
  p = await mk('reject');
  if (p.framesHost.children.length !== 2) fail('a network failure cleared the frames');
});

scenario('an ordinary refusal of one session removes that frame only; the principal check says whether the sign-in is gone', async () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  const B = open(p, 'w1', 'minion-2').f;
  B.contentWindow.location = { href: ORIGIN + '/s/pi-1/minion-2?embed=1', origin: ORIGIN, pathname: '/s/pi-1/minion-2', search: '?embed=1' };
  B.__curDoc = null; // the refusal page has no bridge
  B.fire('load');
  p.tick(9000);
  if (B.parentNode) fail('the refused session was kept');
  await flush();
  respond(principalReads(p)[0], 200);
  await flush();
  if (!A.parentNode || !p.alertBox.hidden) fail('an ordinary per-session failure cleared the other contexts');
  // A kept session that holds nothing and is refused when it is chosen again goes alone (after the sign-in was asked about).
  const q = listPage({ manual: true });
  const C = open(q, 'w1', 'minion-1', 'pi-1').f;
  const D = open(q, 'w1', 'minion-2').f;
  q.fireDoc('click', click(q.rows['w1/minion-1'].name));
  respond(contextReads(q)[0], 404);
  await flush();
  eq(principalReads(q).length, 1, 'a refusal of one session asks who is signed in');
  respond(principalReads(q)[0], 200);
  await flush();
  if (C.parentNode || !D.parentNode) fail('a refused clean kept session must go alone');
  if (!/not available to you any more/.test(q.alertBox.textContent)) fail('not told: ' + q.alertBox.textContent);
  // Same failure with the sign-in gone: everything goes.
  const r = listPage();
  const E = open(r, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  const F = open(r, 'w1', 'minion-2').f;
  Object.defineProperty(F.contentWindow, 'location', { get() { throw new Error('SecurityError'); } }); // the sign-in page cannot be framed
  F.fire('load');
  await flush();
  respond(principalReads(r)[0], 401);
  await flush();
  if (E.parentNode || F.parentNode) fail('a lost sign-in left a kept frame behind');
});

// Supervisor 8, N2: a 404 of ONE session is what Bob's cookie gets for Alice's session. It is also what an ordinary removal
// gets. The page asks WHO is signed in before it decides, and hides what is shown while it waits.
scenario('a refusal of one session asks who is signed in: another sign-in clears EVERY kept frame at once, the shown one included', async () => {
  const p = listPage({ manual: true });
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;      // Alice's draft
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true })));
  const B = open(p, 'w1', 'minion-2', 'pi-1').f;                       // shown
  const Cc = open(p, 'w1', 'minion-3', 'pi-1', { pending: 1 }).f;
  p.fireDoc('click', click(p.rows['w1/minion-2'].name));
  respond(contextReads(p).pop(), 200); await flush();                  // B is the shown one
  if (B.hidden) fail('test bug: B must be shown');
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));               // choose A: Bob's cookie now gets a 404 for it
  respond(contextReads(p).pop(), 404);
  await flush();
  eq(principalReads(p).length, 1, 'one check of who is signed in');
  if (!B.hidden) fail('the shown frame stayed visible while the sign-in was in question');
  if (!A.parentNode || !Cc.parentNode) fail('nothing may be decided before the principal answers');
  respond(principalReads(p)[0], 200, 'basic', 'ctx-bob-0002');         // another context
  await flush();
  if (A.parentNode || B.parentNode || Cc.parentNode || p.framesHost.children.length) fail('another sign-in left a kept frame behind');
  if (!/Another sign-in/.test(p.alertBox.textContent) || /removed or is no longer shared/.test(p.alertBox.textContent)) fail('wrong explanation: ' + p.alertBox.textContent);
  if (p.host.hasAttribute('data-frame-dirty') || !p.host.hidden) fail('the page still holds the first person\'s frames');
  // The principal check is one read: no loop.
  p.tick(120000);
  eq(principalReads(p).length, 1, 'no further reads after authLost');
});

scenario('an ordinary refusal with the same sign-in keeps what holds a draft or evidence (hidden, never shown, nothing sent) and removes a clean frame', async () => {
  for (const held of [{ dirty: true }, { pending: 1 }, { unknown: 1 }]) {
    const p = listPage({ manual: true });
    const A = open(p, 'w1', 'minion-1', 'pi-1', held).f;
    const B = open(p, 'w1', 'minion-2', 'pi-1').f;
    p.fireDoc('click', click(p.rows['w1/minion-1'].name));
    respond(contextReads(p)[0], 404); await flush();
    respond(principalReads(p)[0], 200); await flush();
    const what = JSON.stringify(held);
    if (!A.parentNode || !A.hidden) fail(what + ': a refused frame with something in it must be kept, hidden');
    if (!p.alertBox.textContent || !/kept/.test(p.alertBox.textContent) || /cleared/.test(p.alertBox.textContent)) fail(what + ': not told it is kept: ' + p.alertBox.textContent);
    if (!B.parentNode) fail(what + ': the other frame went');
    if (types(A).some((t) => t === 'resume')) fail(what + ': a refused frame was resumed');
    if (p.sent.fetch + p.sent.xhr + p.sent.ajax !== 0) fail(what + ': the page sent something');
    if (p.kept.hidden || !p.keptList().some((b) => b.getAttribute('data-kept') === 'w1\nminion-1')) fail(what + ': it is not offered in the kept sessions');
    // Choosing it again checks again (once, by the person); when the Worker is back it is shown, with its draft.
    p.fireDoc('click', click(p.rows['w1/minion-1'].name));
    eq(contextReads(p).length, 2, what + ': the second choice asks the server again');
    respond(contextReads(p)[1], 200); await flush();
    if (A.hidden || !B.hidden) fail(what + ': the frame did not come back once the server allowed it');
    p.tick(100000);
    if (principalReads(p).length > 1 + 3) fail(what + ': the page keeps asking: ' + principalReads(p).length);
  }
  // Clean: nothing to lose, removed.
  const q = listPage({ manual: true });
  const C = open(q, 'w1', 'minion-1', 'pi-1').f;
  open(q, 'w1', 'minion-2', 'pi-1');
  q.fireDoc('click', click(q.rows['w1/minion-1'].name));
  respond(contextReads(q)[0], 403); await flush();
  respond(principalReads(q)[0], 200); await flush();
  if (C.parentNode) fail('a clean refused frame was kept');
});

scenario('a refusal when the sign-in cannot be asked fails closed: everything is concealed, nothing is cleared, and the periodic check takes over', async () => {
  const p = listPage({ manual: true });
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  const B = open(p, 'w1', 'minion-2', 'pi-1').f;
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  respond(contextReads(p)[0], 404); await flush();
  respond(principalReads(p)[0], 503); await flush();
  if (!A.parentNode || !B.parentNode || !A.hidden || !B.hidden) fail('unconfirmed identity must conceal, not clear');
  if (!/could not be confirmed/.test(p.alertBox.textContent)) fail('not told: ' + p.alertBox.textContent);
  p.tick(10000);
  eq(principalReads(p).length, 2, 'the check repeats at its own bounded interval, not faster');
});

// Supervisor 8, N1: a restart of the Hub is not another sign-in. The server gives the same context for the same login (a real
// two-process test in internal/hub/server); here the page's side: what it kept survives the outage and the Worker's reconnect.
scenario('a Hub restart: the outage conceals, the same context brings the frames back after a per-resource check; drafts, requests and unknown deliveries are never touched', async () => {
  const p = listPage({ manual: true });
  const A = open(p, 'w1', 'minion-1', 'pi-1').f;
  const B = open(p, 'w1', 'minion-2', 'pi-1').f;     // B is shown, A is kept
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true, pending: 1, unknown: 1 })));
  toParent(p, B, msg('state', 'w1', 'minion-2', state({ dirty: true, unknown: 1 })));
  if (B.hidden) fail('test bug: B must be shown');
  p.tick(30000);                                      // the monitor finds the Hub away
  principalReads(p)[0].rej(new Error('connection refused: the Hub is restarting'));
  await flush();
  if (!A.parentNode || !B.parentNode) fail('the outage cleared a frame');
  if (!A.hidden || !B.hidden) fail('nothing may be shown while the identity is unconfirmed');
  if (/Another sign-in/.test(p.alertBox.textContent)) fail('an outage was called another sign-in');
  p.tick(10000);                                      // the Hub is back and gives the SAME context (its key is persisted)
  respond(principalReads(p)[1], 200, 'basic', CTX);
  await flush();
  // The shown frame does not come back on the strength of who is signed in alone: the server is asked about THIS session.
  eq(contextReads(p).map((g) => g.url), ['/s/pi-1/minion-2/context'], 'the per-resource check of the frame that was shown');
  if (!B.hidden) fail('B came back before the resource check answered');
  respond(contextReads(p)[0], 404);                   // its Worker has not reconnected yet
  await flush();
  respond(principalReads(p)[2], 200, 'basic', CTX);   // the refusal asks who is signed in: still the same person
  await flush();
  if (!B.parentNode || !B.hidden) fail('a refused frame with evidence was cleared or shown');
  if (!A.parentNode || !p.host.hasAttribute('data-frame-dirty')) fail('what the page holds was lost');
  if (/Another sign-in|cleared/.test(p.alertBox.textContent)) fail('wrong explanation: ' + p.alertBox.textContent);
  eq(types(B).filter((t) => t === 'resume').length, 0, 'a refused frame is not resumed');
  // The Worker has reconnected: choosing B shows it again, with everything it kept.
  p.fireDoc('click', click(p.rows['w1/minion-2'].name));
  respond(contextReads(p)[1], 200);
  await flush();
  if (B.hidden || !p.alertBox.hidden) fail('B was not shown after the Worker was back');
  eq(types(B).filter((t) => t === 'resume').length, 1, 'B resumes its own reading once');
  // A kept frame is likewise checked per resource when chosen: it comes back as it was.
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  respond(contextReads(p)[2], 200);
  await flush();
  if (A.hidden) fail('A was not shown');
  if (p.sent.fetch + p.sent.xhr + p.sent.ajax !== 0) fail('the page sent something');
});

// Owner contract v0.5.0: opening or choosing a session moves the focus once, to where the person chose, and a choice that is overtaken
// (another row, Close, Escape) never moves it later, when its server check answers.
scenario('a choice that is overtaken by another row, Close or Escape never shows its frame or takes the focus when its check answers late', async () => {
  const mk = async () => {
    const p = listPage({ manual: true });
    const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
    const B = open(p, 'w1', 'minion-2', 'pi-1').f;
    p.addRow('w1', 'minion-3', 'pi-1');
    return { p, A, B };
  };
  // Another row chosen while A's check is on its way.
  let { p, A, B } = await mk();
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));          // A: kept frame, its resource check is pending
  p.fireDoc('click', click(p.rows['w1/minion-3'].name));          // C: a new frame, shown now, takes the focus once
  const C = frameOf(p, 'minion-3');
  if (!C) fail('test bug: no frame for C');
  loadFrame(p, C, 'w1', 'minion-3');
  if (C.hidden) fail('test bug: C is not shown');
  const focusedBefore = p.title.focused;
  respond(contextReads(p)[0], 200); await flush();                // A's late answer
  if (!A.hidden) fail('the overtaken choice showed its frame');
  if (p.title.textContent !== 'minion-3 on pi-1' || p.title.focused !== focusedBefore) fail('the overtaken choice took the focus or changed the pane (' + p.title.textContent + ', focus ' + p.title.focused + ' vs ' + focusedBefore + ')');
  eq(types(A).filter((t) => t === 'resume').length, 0, 'the overtaken frame was resumed');
  // Close while the check is on its way: the frame is not shown into a closed pane.
  ({ p, A, B } = await mk());
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  p.closeBtn.fire('click');
  const f2 = p.title.focused;
  respond(contextReads(p)[0], 200); await flush();
  if (!p.host.hidden || !A.hidden || p.title.focused !== f2) fail('a late answer re-opened a pane the person closed');
  // Escape the same.
  ({ p, A, B } = await mk());
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  p.fireDoc('keydown', keydown('Escape', p.title));
  const f3 = p.title.focused;
  respond(contextReads(p)[0], 200); await flush();
  if (!p.host.hidden || !A.hidden || p.title.focused !== f3) fail('a late answer re-opened a pane that Escape closed');
  // A live refresh of the list (the selection marks) never moves the focus.
  ({ p, A, B } = await mk());
  p.doc.activeElement = p.rows['w1/minion-3'].name;
  const before = p.title.focused;
  p.body.fire('htmx:sseMessage', {});
  p.body.fire('htmx:afterSwap', {});
  if (p.title.focused !== before || p.doc.activeElement !== p.rows['w1/minion-3'].name) fail('a live update of the list moved the focus');
});

// Review finding 2 (the 30 s skip): a kept frame is checked EVERY time it is chosen, whatever the age of the last good answer.
scenario('a kept frame is never shown before the server confirmed identity and access just now; an answer that cannot be had fails closed', async () => {
  const p = listPage({ manual: true });
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true })));
  const B = open(p, 'w1', 'minion-2').f;
  for (let round = 0; round < 3; round++) { // no time skip: right after the last answer as well as much later
    p.tick(round === 1 ? 20000 : 100);
    const n0 = contextReads(p).length;
    p.fireDoc('click', click(p.rows['w1/minion-1'].name));
    eq(contextReads(p).length, n0 + 1, 'a check for every choice (round ' + round + ')');
    if (!A.hidden) fail('a kept frame was shown before the server answered (round ' + round + ')');
    respond(contextReads(p)[n0], 200);
    await flush();
    if (A.hidden || !B.hidden) fail('a confirmed frame was not shown (round ' + round + ')');
    eq(types(A).slice(-1)[0], 'resume', 'it resumes as it is shown');
    p.fireDoc('click', click(p.rows['w1/minion-2'].name));
    respond(contextReads(p)[contextReads(p).length - 1], 200);
    await flush();
  }
  // The server cannot be asked (503): the frame stays hidden and kept, the person is told, nothing is lost.
  const n1 = contextReads(p).length;
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  respond(contextReads(p)[n1], 503);
  await flush();
  if (!A.hidden || !A.parentNode || p.alertBox.hidden || !/could not be confirmed/.test(p.alertBox.textContent) || !/typed there is kept/.test(p.alertBox.textContent)) fail('an unconfirmed identity did not fail closed: ' + p.alertBox.textContent);
  // Try again: confirmed, shown, warning gone.
  p.fireDoc('click', click(p.rows['w1/minion-1'].name));
  respond(contextReads(p)[n1 + 1], 200);
  await flush();
  if (A.hidden || !p.alertBox.hidden) fail('a confirmed identity did not show the kept frame');
});

// Review finding 5: a hanging read stopped the monitor for good and a chosen frame could wait for ever.
scenario('a hanging read is aborted after its deadline, the kept frames are concealed, the check goes on, and recovery shows them again', async () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  p.fireDoc('click', click(p.rows['w1/minion-1'].name)); // A is the shown frame
  const B = open(p, 'w1', 'minion-2').f;
  p.fireDoc('click', click(p.rows['w1/minion-2'].name));
  if (B.hidden) fail('test bug: B shown');
  toParent(p, B, msg('state', 'w1', 'minion-2', state({ dirty: true, pending: 1 })));
  p.tick(30000); // the periodic check: the request hangs (never answered)
  eq(principalReads(p).length, 1, 'one check');
  eq(p.aborts(), 0, 'not aborted before its deadline');
  p.tick(7999);
  eq(p.aborts(), 0, 'still within its deadline');
  p.tick(2);
  eq(p.aborts(), 1, 'the hanging read was aborted');
  if (!B.hidden || !A.hidden) fail('after an unconfirmed identity the frames must be concealed');
  if (!B.parentNode || !p.host.hasAttribute('data-frame-dirty')) fail('concealing must keep the draft and the request on its way');
  if (p.status.hidden || !/could not be confirmed/.test(p.status.textContent) || !/typed and any request on its way are kept/.test(p.status.textContent)) fail('no actionable warning: ' + p.status.textContent);
  eq(types(B).filter((t) => t === 'suspend' || t === 'resume').length, 0, 'concealing sends the frame nothing');
  // The check repeats (bounded, not a busy loop), also when it hangs again, for ever.
  for (let i = 1; i <= 3; i++) {
    p.tick(10000); // RETRY
    eq(principalReads(p).length, 1 + i, 'a new check after ' + i * 10 + ' s');
    p.tick(8001);
    eq(p.aborts(), 1 + i, 'each hanging read is aborted');
    if (!B.hidden) fail('concealed frames came back without a confirmation');
  }
  if (principalReads(p).length !== 4) fail('the check must not run more often than every 10 s: ' + principalReads(p).length);
  // Recovery: the next check is answered; the shown frame returns, the warning goes.
  p.tick(10000);
  respond(principalReads(p)[4], 200);
  await flush();
  if (B.hidden || !p.status.hidden || !p.alertBox.hidden) fail('the frame did not return after the identity was confirmed');
  p.tick(30000);
  eq(principalReads(p).length, 6, 'back to the regular interval');
  // A chosen kept frame whose check hangs is concealed with a warning too (no indefinite wait).
  const q = listPage({ manual: true });
  const C = open(q, 'w1', 'minion-1', 'pi-1', { dirty: true }).f;
  open(q, 'w1', 'minion-2');
  q.fireDoc('click', click(q.rows['w1/minion-1'].name));
  q.tick(8001);
  eq(q.aborts(), 1, 'the resource check was aborted');
  if (!C.hidden || q.alertBox.hidden || !/could not be confirmed/.test(q.alertBox.textContent)) fail('a chosen frame whose check hangs waited for ever: ' + q.alertBox.textContent);
});

scenario('the page asks only its own bounded reads; no timer runs without frames', async () => {
  const p = listPage();
  eq(p.guards.length, 0, 'reads before any frame');
  const A = open(p, 'w1', 'minion-1').f;
  p.tick(30000);
  respond(principalReads(p)[0], 200);
  await flush();
  p.tick(30000);
  eq(principalReads(p).length, 2, 'the check repeats while a frame is kept');
  respond(principalReads(p)[1], 200);
  await flush();
  p.closeBtn.fire('click');
  answer(p, A, 'w1', 'minion-1', {});
  if (A.parentNode) fail('test bug: the clean frame should be dropped');
  const n = p.guards.length;
  p.tick(30000);
  p.tick(30000);
  eq(p.guards.length, n, 'the check kept running with no frame to guard');
  if (p.sent.fetch + p.sent.xhr + p.sent.ajax !== 0) fail('the page sent something other than its bounded reads');
});

// ---- review fixes: the controller and the document ---------------------------------------------------

// Review finding 3: a document whose controller script failed still looked "ready" and "clean", so its editable textarea
// could be typed into (invisibly tracked) and the frame was discarded as clean.
scenario('a frame is not shown as operational before its controller answered; an unknown controller is never clean', () => {
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  p.fireDoc('click', click(r.name));
  const f = frameOf(p, 'minion-1');
  f.contentWindow.location = { href: ORIGIN + f.getAttribute('src'), origin: ORIGIN, pathname: '/s/pi-1/minion-1', search: '?embed=1' };
  f.contentWindow.document = { documentElement: { getAttribute: () => null } }; // no bridge script at all
  f.fire('load');
  if (!f.hidden) fail('a frame that has not said ready (no controller) is visible and editable');
  if (p.status.hidden || !/Loading/.test(p.status.textContent)) fail('no loading note');
  p.tick(8001);
  if (f.parentNode) fail('a frame without a controller was kept');
  if (p.status.hidden || !/cannot be shown here/.test(p.status.textContent) || !/Open full session/.test(p.status.textContent)) fail('no safe full-page fallback');
  eq(p.openLink.getAttribute('href'), '/s/pi-1/minion-1', 'the ordinary link');
  // A ready that says ctrl false is not accepted either; a state with ctrl false is never clean.
  const q = listPage();
  const A = open(q, 'w1', 'minion-1').f;
  const B = open(q, 'w1', 'minion-2').f;
  const C = open(q, 'w1', 'minion-3').f;
  const D = open(q, 'w1', 'minion-4').f;
  toParent(q, A, msg('state', 'w1', 'minion-1', state({ ctrl: false })));
  if (!q.host.hasAttribute('data-frame-dirty')) fail('a state without a controller is not "clean" and must mark the page');
  q.fireDoc('click', click(q.addRow('w1', 'minion-5', 'pi-1').name));
  answer(q, A, 'w1', 'minion-1', { ctrl: false, dirty: false }); // the controller is gone: dirty is false only because nothing answered
  if (!A.parentNode) fail('a frame whose controller does not answer was discarded as clean');
  answer(q, B, 'w1', 'minion-2', {});
  if (B.parentNode) fail('a clean frame with a working controller should be the one discarded');
  if (!C.parentNode || !D.parentNode) fail('too many frames were discarded');
});

// Review finding 4: lifetime counters (2 readies against 3 loads) rejected a successful document after an error reload.
scenario('an error reload followed by a successful reload that says ready first is not invalidated, in any order', () => {
  for (const readyFirst of [true, false]) {
    const p = listPage();
    const A = open(p, 'w1', 'minion-1').f;
    // 1. a reload to an error page at the same address: loads, no bridge, no ready.
    A.contentWindow.location = { href: ORIGIN + A.getAttribute('src'), origin: ORIGIN, pathname: '/s/pi-1/minion-1', search: '?embed=1' };
    A.__curDoc = null;
    A.fire('load');
    if (!A.hidden) fail('the error page is shown as the operational session');
    p.tick(3000);
    // 2. a successful reload within the deadline: ready before the load event (or after it).
    reloadFrame(p, A, 'w1', 'minion-1', {}, { readyFirst });
    p.tick(20000);
    if (!A.parentNode) fail('readyFirst=' + readyFirst + ': the successful document was removed after the error reload (lifetime counters)');
    if (A.hidden || !p.status.hidden) fail('readyFirst=' + readyFirst + ': the successful document is not shown');
    // 3. and an old document\'s ready that was queued before the reload is rejected.
  }
  const p = listPage();
  const A = open(p, 'w1', 'minion-1').f;
  const oldDoc = A.__doc;
  reloadFrame(p, A, 'w1', 'minion-1', {}, { readyFirst: false });
  const newDoc = A.__doc;
  toParent(p, A, Object.assign(msg('ready', 'w1', 'minion-1', state({ dirty: true })), { doc: oldDoc, ctx: CTX }));
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ dirty: true })), {});
  if (!p.host.hasAttribute('data-frame-dirty')) fail('the current document was not heard');
  if (A.__doc !== newDoc) fail('test bug');
  // A document that only the window can vouch for (same window, another document) is not trusted: the id on the document decides.
  A.__curDoc = 'some-other-document';
  toParent(p, A, Object.assign(msg('ready', 'w1', 'minion-1', state()), { doc: 'brand-new', ctx: CTX }));
  const q = posts(A).length;
  p.closeBtn.fire('click');
  if (!posts(A).slice(q - 0).every((x) => x.msg.doc === newDoc)) fail('a ready for a document that is not the current one was taken over');
});

// ---- review fixes: the modal drawer and the shell's own skip link -----------------------------------------

// Review finding 6: only data-modal-inert subtrees became inert; the skip link of the shell is outside them, outranks the drawer
// and was reachable with Shift+Tab. (The real markup is checked in inspector_test.go.)
scenario('the shell skip link is inert while the drawer is modal, and reachable again afterwards', () => {
  const p = listPage({ narrow: true });
  open(p, 'w1', 'minion-1');
  for (const part of [p.skip, p.rail, p.topbar, p.main]) if (!part.hasAttribute('inert')) fail('a background element can still take the focus: ' + part.tagName + ' ' + part.className);
  p.closeBtn.fire('click');
  for (const part of [p.skip, p.rail, p.topbar, p.main]) if (part.hasAttribute('inert')) fail('the page stayed inert after Close');
  p.media.narrow = false;
  open(p, 'w1', 'minion-1');
  if (p.skip.hasAttribute('inert')) fail('docked: the skip link must stay usable');
});

// ---- whole rows (Story 5.7) ----------------------------------------------------------------------------------

// workerRow is the real Workers row (templates/_session_row.html): the Worker link in the row header, a status, the count link and the
// disclosure of the sessions, the New session form; the row opens the Worker's page.
function workerRow(p, id, name, o) {
  o = o || {};
  const link = el('a', { href: 'href' in o ? o.href : '/workers/' + id + (o.scope ? '?scope=all' : ''), 'data-control': 'name', 'data-worker-link': '' });
  const status = el('td', { 'data-label': 'Status' }, [el('span', { class: 'badge' })]);
  const count = el('a', { href: '/sessions?worker=' + id, 'data-control': 'sessions' });
  const summary = el('summary', { 'data-control': 'sessions-toggle' });
  const sessionsCell = el('td', { class: 'num' }, [count, el('details', { class: 'worker-sessions' }, [summary])]);
  const button = el('button', { 'data-control': 'new-session', type: 'submit' });
  const form = el('form', { method: 'post' }, [button]);
  const actions = el('td', { 'data-label': 'Actions' }, [form]);
  const row = el('tr', { 'data-worker': id, 'data-key': 'w-' + id, 'data-row-link': 'worker' }, [el('th', { scope: 'row' }, [link]), status, sessionsCell, actions]);
  p.rowsHost.appendChild(row);
  return { row, link, status, count, summary, button, form, actions, sessionsCell, head: row.children[0] };
}

scenario('a plain click on a quiet part of a session row opens the pane once, like the session link', () => {
  for (const which of ['task', 'state', 'badge', 'workerCell', 'row', 'head']) {
    const q = listPage();
    const row = q.addRow('w1', 'minion-1', 'pi-1');
    q.addRow('w1', 'minion-2', 'pi-1');
    const t = which === 'badge' ? row.state.children[0] : which === 'row' ? row.row : which === 'head' ? row.row.children[0] : row[which];
    q.fireDoc('click', click(t));
    if (q.framesHost.children.length) fail(which + ': the row acted before the double-click time was over');
    q.tick(DBL);
    const f = frameOf(q, 'minion-1');
    if (!f || f.getAttribute('src') !== '/s/pi-1/minion-1?embed=1') fail(which + ': a click on a quiet part of the row did not open the canonical frame');
    eq(q.framesHost.children.length, 1, which + ': frames');
    if (row.row.getAttribute('data-inspected') !== 'true' || q.rows['w1/minion-2'].row.hasAttribute('data-inspected')) fail(which + ': the selection is not on the clicked row');
    if (q.win.location.assigned.length || q.win.location.reloads) fail(which + ': a row click navigated the page');
    if (q.sent.fetch + q.sent.xhr + q.sent.ajax !== 0) fail(which + ': a row click sent a request');
    if (row.row.hasAttribute('tabindex') || row.row.hasAttribute('role')) fail('the row became a control');
    q.closeBtn.fire('click');
    same(q.doc.activeElement, row.name, which + ': the focus returns to the session link');
  }
  // The same click through the link, and again on the row, is one pane: the link is not handled twice.
  const q = listPage();
  const row = q.addRow('w1', 'minion-1', 'pi-1');
  const e = click(row.name);
  q.fireDoc('click', e);
  if (!e.defaultPrevented || q.framesHost.children.length !== 1) fail('the link click changed');
  q.fireDoc('click', click(row.task));
  q.tick(DBL);
  eq(q.framesHost.children.length, 1, 'frames after link and row');
});

scenario('a click that does something of its own never opens the row: links, controls, editable and focusable elements, opt-outs', () => {
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  const mk = (tag, attrs, inside) => { const n = el(tag, attrs); r.task.appendChild(n); if (!inside) return n; const t = el('span'); n.appendChild(t); return t; };
  const cases = {
    'the Worker link': r.workerLink, 'a link in the task': mk('a', { href: '/x' }), 'a button': mk('button'), 'inside a button': mk('button', {}, true), 'an input': mk('input', { type: 'text' }),
    'a select': mk('select'), 'a textarea': mk('textarea'), 'a summary': mk('summary'), 'inside details': mk('details', {}, true), 'a label': mk('label'), 'inside a form': mk('form', {}, true),
    'contenteditable': mk('div', { contenteditable: 'true' }), 'inside contenteditable': mk('div', { contenteditable: '' }, true), 'a role=button': mk('span', { role: 'button' }),
    'a role=link': mk('span', { role: 'link' }), 'a tabindex=0': mk('span', { tabindex: '0' }), 'an opt-out': mk('span', { 'data-no-row': '' }), 'inside an opt-out': mk('div', { 'data-no-row': '' }, true),
  };
  for (const [what, t] of Object.entries(cases)) {
    p.fireDoc('click', click(t));
    if (p.framesHost.children.length || !p.host.hidden) fail('a click on ' + what + ' opened the pane');
    if (p.win.location.assigned.length) fail('a click on ' + what + ' navigated');
  }
  // The live list's focus fallback (tabindex -1 on the row) is not a control: the row still opens.
  r.row.setAttribute('tabindex', '-1');
  p.fireDoc('click', click(r.task));
  p.tick(DBL);
  if (p.framesHost.children.length !== 1) fail('the focus fallback of the live list blocked the row');
});

scenario('modified clicks, other buttons, double clicks and a text selection do not open the row; a collapsed selection does', () => {
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  for (const o of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }, { button: 2 }, { detail: 2 }, { detail: 3 }, { defaultPrevented: true }]) {
    p.fireDoc('click', click(r.task, o));
    if (p.framesHost.children.length) fail('the click ' + JSON.stringify(o) + ' opened the row');
  }
  p.win.selection = { isCollapsed: false, toString: () => 'copy me' };
  p.fireDoc('click', click(r.task));
  if (p.framesHost.children.length) fail('ending a text selection opened the row');
  p.win.selection = { isCollapsed: true, toString: () => '' };
  p.fireDoc('click', click(r.task));
  p.tick(DBL);
  if (p.framesHost.children.length !== 1) fail('a plain click with nothing selected did not open the row');
});

scenario('a Worker row goes to the Worker page, not to the pane; its own controls keep their actions', () => {
  for (const which of ['status', 'badge', 'sessionsCell', 'actions', 'row', 'head']) {
    const q = listPage();
    const x = workerRow(q, 'w1', 'pi-1');
    const t = which === 'badge' ? x.status.children[0] : which === 'row' ? x.row : x[which];
    q.fireDoc('click', click(t));
    eq(q.win.location.assigned, [], which + ': the Worker row acted before the double-click time was over');
    q.tick(DBL);
    eq(q.win.location.assigned, ['/workers/w1'], which + ': navigation after a click on a quiet part of the Worker row');
    if (q.framesHost.children.length || !q.host.hidden) fail(which + ': a Worker row opened the session pane');
  }
  const q = listPage();
  const x = workerRow(q, 'w1', 'pi-1');
  for (const [what, t] of [['the Worker link', x.link], ['the sessions count link', x.count], ['the disclosure', x.summary], ['New session', x.button], ['inside the form', x.form]]) {
    q.fireDoc('click', click(t));
    if (q.win.location.assigned.length) fail('a click on ' + what + ' was taken over by the row: ' + q.win.location.assigned);
  }
  for (const o of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }, { detail: 2 }]) { q.fireDoc('click', click(x.status, o)); }
  q.win.selection = { isCollapsed: false, toString: () => 'x' };
  q.fireDoc('click', click(x.status));
  if (q.win.location.assigned.length) fail('a modified click, a double click or a selection navigated: ' + q.win.location.assigned);
  // The All users scope travels with the link; a link that is not a Worker page is not followed.
  const s1 = listPage();
  const y = workerRow(s1, 'w1', 'pi-1', { scope: true });
  s1.fireDoc('click', click(y.status));
  s1.tick(DBL);
  eq(s1.win.location.assigned, ['/workers/w1?scope=all'], 'the scope is kept');
  for (const href of ['https://evil.example/workers/w1', '//evil.example/workers/w1', '/account', '/workers/w1/x', '/workers/w1?x=1', '/workers/w1#a', 'javascript:alert(1)', '']) {
    const s2 = listPage();
    const z = workerRow(s2, 'w1', 'pi-1', { href });
    s2.fireDoc('click', click(z.status));
    s2.tick(DBL);
    if (s2.win.location.assigned.length) fail('the row followed ' + JSON.stringify(href));
  }
});

// S5.7 review 1, P2: a double click that selects a word is click(detail 1) with nothing selected yet, then the word is selected,
// then click(detail 2), then dblclick. The first click must not have opened anything.
scenario('a double click that selects a word opens nothing: not on the first click, not later; neither a session row nor a Worker row', () => {
  const seq = (p, t, word, extra) => {
    p.fireDoc('mousedown', { button: 0, detail: 1, target: t });
    p.fireDoc('click', click(t, { detail: 1 }));
    p.tick(100);                                   // the second press arrives within the double-click time
    p.fireDoc('mousedown', { button: 0, detail: 2, target: t });
    p.win.selection = { isCollapsed: false, toString: () => word }; // the browser selects the word
    if (extra) extra();
    p.fireDoc('click', click(t, { detail: 2 }));
    p.fireDoc('dblclick', click(t, { detail: 2 }));
    p.tick(5000);                                  // and nothing happens afterwards either
  };
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  seq(p, r.task, 'copied');
  if (p.framesHost.children.length || !p.host.hidden || r.row.hasAttribute('data-inspected')) fail('a double click selecting a word on a session row opened the pane');
  if (p.win.location.assigned.length || p.sent.fetch + p.sent.xhr + p.sent.ajax) fail('a double click navigated or sent something');
  const q = listPage();
  const w = workerRow(q, 'w1', 'pi-1');
  seq(q, w.status, 'online');
  eq(q.win.location.assigned, [], 'a double click selecting a word on a Worker row navigated');
  // Only the first click and the double click event (a browser that sends no second detail): still nothing.
  const p2 = listPage();
  const r2 = p2.addRow('w1', 'minion-1', 'pi-1');
  p2.fireDoc('click', click(r2.task, { detail: 1 }));
  p2.fireDoc('dblclick', click(r2.task, { detail: 2 }));
  p2.tick(5000);
  if (p2.framesHost.children.length) fail('a double click event did not cancel the pending row');
  // Even when the second click comes late (slow double click: after the time, as a NEW sequence) it is a plain single click.
  const p3 = listPage();
  const r3 = p3.addRow('w1', 'minion-1', 'pi-1');
  p3.fireDoc('click', click(r3.task, { detail: 1 }));
  p3.tick(DBL + 1);
  eq(p3.framesHost.children.length, 1, 'a single click opens once, after the double-click time');
  p3.fireDoc('click', click(r3.task, { detail: 1 }));
  p3.tick(5000);
  eq(p3.framesHost.children.length, 1, 'frames after a second, later click');
});

scenario('a quiet row click acts once after the double-click time; a drag selection, a replaced row, a link click, a touch tap, another press and a lost sign-in are handled', async () => {
  // Plain single click: nothing before the time, one action after, and the opener is the session link (focus returns there).
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1');
  p.fireDoc('mousedown', { button: 0, target: r.task });
  p.fireDoc('click', click(r.task));
  p.tick(DBL - 1);
  if (p.framesHost.children.length) fail('the row acted before the double-click time');
  p.tick(2);
  eq(p.framesHost.children.length, 1, 'frames');
  p.tick(5000);
  eq(p.framesHost.children.length, 1, 'frames after later time: the action is not repeated');
  // A selection made while it waits (drag finished, shift-extend by keyboard) cancels it.
  const q = listPage();
  const r2 = q.addRow('w1', 'minion-1', 'pi-1');
  q.fireDoc('click', click(r2.task));
  q.win.selection = { isCollapsed: false, toString: () => 'x' };
  q.tick(DBL + 1);
  if (q.framesHost.children.length) fail('a selection made while the row waited did not cancel it');
  // The row is replaced by a live update while it waits: cancelled, and the stale element opens nothing.
  const s = listPage();
  const r3 = s.addRow('w1', 'minion-1', 'pi-1');
  s.fireDoc('click', click(r3.task));
  s.rowsHost.removeChild(r3.row);
  s.tick(DBL + 1);
  if (s.framesHost.children.length) fail('a replaced row still opened the pane');
  const w = listPage();
  const wr = workerRow(w, 'w1', 'pi-1');
  w.fireDoc('click', click(wr.status));
  w.rowsHost.removeChild(wr.row);
  w.tick(DBL + 1);
  eq(w.win.location.assigned, [], 'a replaced Worker row navigated');
  // The session link itself is immediate and cancels a waiting row action (one pane, no double handling).
  const l = listPage();
  const r4 = l.addRow('w1', 'minion-1', 'pi-1');
  l.addRow('w1', 'minion-2', 'pi-1');
  l.fireDoc('click', click(r4.task));
  const e = click(l.rows['w1/minion-2'].name);
  l.fireDoc('click', e);
  if (!e.defaultPrevented || !frameOf(l, 'minion-2')) fail('the link did not act at once');
  l.tick(DBL + 1);
  eq(l.framesHost.children.length, 1, 'the earlier waiting row opened a second pane');
  eq(l.title.textContent, 'minion-2 on pi-1', 'the row action overrode the link the person clicked');
  // A touch or pen tap has no double click that selects text: immediate.
  const t = listPage();
  const r5 = t.addRow('w1', 'minion-1', 'pi-1');
  t.fireDoc('click', click(r5.task, { pointerType: 'touch' }));
  eq(t.framesHost.children.length, 1, 'a touch tap acts at once');
  // Two different rows clicked within the time: only the last one is opened.
  const m = listPage();
  m.addRow('w1', 'minion-1', 'pi-1'); const rb = m.addRow('w1', 'minion-2', 'pi-1');
  m.fireDoc('click', click(m.rows['w1/minion-1'].task));
  m.fireDoc('mousedown', { button: 0, target: rb.task });
  m.fireDoc('click', click(rb.task));
  m.tick(DBL + 1);
  eq(m.framesHost.children.length, 1, 'frames');
  eq(m.title.textContent, 'minion-2 on pi-1', 'the later row won');
  // The sign-in is lost while it waits: nothing opens.
  const x = listPage();
  const rx = x.addRow('w1', 'minion-1', 'pi-1');
  open(x, 'w1', 'minion-3');
  x.fireDoc('click', click(rx.task));
  x.tick(30000);
  x.guards[0].res({ status: 401, ok: false, type: 'basic' });
  await flush();
  x.tick(DBL + 1);
  if (x.framesHost.children.length) fail('a waiting row opened after the sign-in was lost');
});

scenario('a slow valid double click (420 and 500 ms between the clicks) selects a word and opens nothing; native anchors and touch stay immediate', () => {
  for (const gap of [420, 500]) {
    for (const kind of ['session', 'worker']) {
      const p = listPage();
      const t = kind === 'session' ? p.addRow('w1', 'minion-1', 'pi-1').task : workerRow(p, 'w1', 'pi-1').status;
      p.fireDoc('mousedown', { button: 0, detail: 1, target: t });
      p.fireDoc('click', click(t, { detail: 1 }));
      p.tick(gap);                                   // the second press of a slow double click
      if (p.framesHost.children.length || p.win.location.assigned.length) fail(kind + ' ' + gap + ' ms: the first click acted before the double-click time');
      p.fireDoc('mousedown', { button: 0, detail: 2, target: t });
      p.win.selection = { isCollapsed: false, toString: () => 'word' };
      p.fireDoc('click', click(t, { detail: 2 }));
      p.fireDoc('dblclick', click(t, { detail: 2 }));
      p.tick(5000);
      if (p.framesHost.children.length || p.win.location.assigned.length) fail(kind + ' ' + gap + ' ms: a slow double click acted');
    }
  }
  // The time is not unbounded: a lone click still acts once, after the window.
  const q = listPage();
  const r = q.addRow('w1', 'minion-1', 'pi-1');
  q.fireDoc('click', click(r.task, { detail: 1 }));
  q.tick(500);
  eq(q.framesHost.children.length, 0, 'acted inside the 500 ms double-click time');
  q.tick(DBL);
  eq(q.framesHost.children.length, 1, 'a lone click never acted');
  // Native anchors and touch stay immediate.
  const l = listPage();
  l.addRow('w1', 'minion-1', 'pi-1');
  const e = click(l.rows['w1/minion-1'].name);
  l.fireDoc('click', e);
  if (!e.defaultPrevented || !frameOf(l, 'minion-1')) fail('the session link no longer acts at once');
  const w = listPage();
  const wr = workerRow(w, 'w1', 'pi-1');
  const we = click(wr.link);
  w.fireDoc('click', we);
  if (we.defaultPrevented) fail('the Worker link was taken over');
  const t = listPage();
  const rt = t.addRow('w1', 'minion-1', 'pi-1');
  t.fireDoc('click', click(rt.task, { pointerType: 'touch' }));
  eq(t.framesHost.children.length, 1, 'a touch tap is not immediate');
  const t2 = listPage();
  const wt = workerRow(t2, 'w1', 'pi-1');
  t2.fireDoc('click', click(wt.status, { pointerType: 'pen' }));
  eq(t2.win.location.assigned.length, 1, 'a pen tap on a Worker row is not immediate');
});

scenario('a quiet row click survives a live list replacement: the same data-key row of the current list acts, a gone or changed identity cancels', () => {
  const swap = (p, build) => {                     // the live list replaces every row with new elements, as the fragment swap does
    p.rowsHost.children.slice().forEach((c) => p.rowsHost.removeChild(c));
    const made = build();
    p.body.fire('htmx:sseMessage', {});
    return made;
  };
  // Session row, replaced by the same entry while it waits: ONE action, on the same session, from the new row.
  const p = listPage();
  const r = p.addRow('w1', 'minion-1', 'pi-1'); p.addRow('w1', 'minion-2', 'pi-1');
  p.fireDoc('click', click(r.task));
  p.tick(200);
  const fresh = swap(p, () => { const b = sessionRow('w1', 'minion-2', 'pi-1'); const a = sessionRow('w1', 'minion-1', 'pi-1'); p.rowsHost.appendChild(b.row); p.rowsHost.appendChild(a.row); return { a, b }; });
  p.tick(DBL);
  eq(p.framesHost.children.length, 1, 'frames after a live replacement');
  eq(frameOf(p, 'minion-1') ? frameOf(p, 'minion-1').getAttribute('src') : null, '/s/pi-1/minion-1?embed=1', 'the SAME session opened');
  if (frameOf(p, 'minion-2')) fail('another row (the one at the old position) was opened');
  if (fresh.a.row.getAttribute('data-inspected') !== 'true' || fresh.b.row.hasAttribute('data-inspected')) fail('the selection is on the wrong row');
  p.tick(5000);
  eq(p.framesHost.children.length, 1, 'repeated');
  // The entry is gone after the swap: nothing opens.
  const q = listPage();
  const r2 = q.addRow('w1', 'minion-1', 'pi-1'); q.addRow('w1', 'minion-2', 'pi-1');
  q.fireDoc('click', click(r2.task));
  swap(q, () => { const b = sessionRow('w1', 'minion-2', 'pi-1'); q.rowsHost.appendChild(b.row); });
  q.tick(DBL);
  if (q.framesHost.children.length) fail('a gone entry opened something (maybe another row)');
  // Same data-key, but the identity changed (the link now names another session): cancelled.
  const s = listPage();
  const r3 = s.addRow('w1', 'minion-1', 'pi-1');
  s.fireDoc('click', click(r3.task));
  swap(s, () => { const n = sessionRow('w1', 'minion-9', 'pi-1'); n.row.setAttribute('data-key', 's-w1-minion-1'); s.rowsHost.appendChild(n.row); });
  s.tick(DBL);
  if (s.framesHost.children.length) fail('a row whose identity changed behind the same key opened something');
  // Worker row: replaced by the same entry, acts once on the same page; a changed href cancels.
  const w = listPage();
  const wr = workerRow(w, 'w1', 'pi-1');
  w.fireDoc('click', click(wr.status));
  swap(w, () => workerRow(w, 'w1', 'pi-1'));
  w.tick(DBL);
  eq(w.win.location.assigned, ['/workers/w1'], 'worker row after a live replacement');
  const w2 = listPage();
  const wr2 = workerRow(w2, 'w1', 'pi-1');
  w2.fireDoc('click', click(wr2.status));
  swap(w2, () => workerRow(w2, 'w1', 'pi-1', { href: '/workers/w2' }));
  w2.tick(DBL);
  eq(w2.win.location.assigned, [], 'a Worker row whose target changed navigated');
  // A selection made after the swap still cancels.
  const u = listPage();
  const ru = u.addRow('w1', 'minion-1', 'pi-1');
  u.fireDoc('click', click(ru.task));
  swap(u, () => u.addRow('w1', 'minion-1', 'pi-1'));
  u.win.selection = { isCollapsed: false, toString: () => 'x' };
  u.tick(DBL);
  if (u.framesHost.children.length) fail('a selection after the swap did not cancel');
});

scenario('rows follow the real list update: after a reorder the click opens the session of THAT row, not of that position', () => {
  const p = listPage();
  p.addRow('w1', 'minion-1', 'pi-1');
  p.addRow('w1', 'minion-2', 'pi-1');
  p.rowsHost.children.forEach((c) => { c.parentNode = null; });
  p.rowsHost.children = [];
  const b2 = sessionRow('w1', 'minion-2', 'pi-1');
  const a2 = sessionRow('w1', 'minion-1', 'pi-1');
  p.rowsHost.appendChild(b2.row); p.rowsHost.appendChild(a2.row);
  p.body.fire('htmx:sseMessage', {});
  p.fireDoc('click', click(b2.task));
  p.tick(DBL);
  eq(frameOf(p, 'minion-2') ? frameOf(p, 'minion-2').getAttribute('src') : null, '/s/pi-1/minion-2?embed=1', 'the row that was clicked');
  if (frameOf(p, 'minion-1')) fail('the row at the old position was opened');
  if (b2.row.getAttribute('data-inspected') !== 'true' || a2.row.hasAttribute('data-inspected')) fail('the selection is on the wrong row');
});

scenario('after the sign-in was lost a row does nothing and the links are plain links', async () => {
  const p = listPage();
  const row = p.addRow('w1', 'minion-1', 'pi-1');
  open(p, 'w1', 'minion-2');
  p.tick(30000);
  p.guards[0].res({ status: 401, ok: false, type: 'basic' });
  await flush();
  p.fireDoc('click', click(row.task));
  p.tick(DBL);
  if (p.framesHost.children.length || !p.host.hidden) fail('a row opened a frame after the sign-in was lost');
  const e = click(row.name);
  p.fireDoc('click', e);
  if (e.defaultPrevented) fail('the link must be a plain link after the sign-in was lost');
});

// ---- the frame's half --------------------------------------------------------------------------

function framePage(o) {
  o = o || {};
  const html = el('html');
  const body = el('body');
  html.appendChild(body);
  const view = el('div', { id: 'session-view', 'data-worker-id': 'w1', 'data-session': 'minion-1', 'data-context': CTX });
  body.appendChild(view);
  doc = { body, documentElement: html, getElementById: (id) => html.all().find((e) => e.id === id) || null, querySelector: (s) => html.querySelector(s), l: {},
    addEventListener(n, f) { (doc.l[n] = doc.l[n] || []).push(f); } };
  body.addEventListener = (n, f) => { (body.l[n] = body.l[n] || []).push(f); };
  const parentPosts = [];
  const parent = { postMessage: (m, to) => parentPosts.push({ m, to }) };
  const timers = [];
  const win = { location: { origin: ORIGIN }, parent: o.top ? null : parent, l: {}, addEventListener(n, f) { (win.l[n] = win.l[n] || []).push(f); }, setTimeout: (f) => { timers.push(f); return timers.length; } };
  if (o.top) win.parent = win;
  const controls = [];
  const control = { state: { dirty: false, pending: 0, unknown: 0, conn: 'live', suspended: false } };
  if (!o.noController) body.addEventListener('minion-frame-control', (e) => {
    const d = e.detail;
    controls.push(d.op);
    if (d.op === 'suspend') control.state.suspended = true;
    if (d.op === 'resume') control.state.suspended = false;
    d.reply(Object.assign({}, control.state));
  });
  function CustomEvent(type, init) { return Object.assign({ type }, init); }
  body.dispatchEvent = (e) => { (body.l[e.type] || []).slice().forEach((f) => f(e)); return true; };
  new Function('document', 'window', 'CustomEvent', read('frame-child.js'))(doc, win, CustomEvent);
  return { win, doc, body, parent, parentPosts, controls, control, timers,
    fromHost: (data, o2) => { o2 = o2 || {}; (win.l.message || []).forEach((f) => f({ origin: o2.origin || ORIGIN, source: o2.source === undefined ? parent : o2.source, data })); },
    flush: () => { while (timers.length) timers.shift()(); } };
}

// ---- the conversation joins the frame-state contract (Story 7.5) ----------------------------------------------------------------
// A frame reports the conversation's own share in state.chat (draft, pending, unknown). It is part of dirty/pending/unknown; the
// inspector also treats a frame whose chat share holds something while its totals say nothing as not clean (never evicted).
const CHAT_HELD = [
  ['a draft message', { dirty: true, chat: { draft: true, pending: 0, unknown: 0 } }, /keeps unsent input \(a conversation message among it\)/],
  ['a message on its way', { pending: 1, chat: { draft: false, pending: 1, unknown: 0 } }, /keeps a request on its way \(a conversation message among it\)/],
  ['an unknown delivery', { unknown: 1, chat: { draft: false, pending: 0, unknown: 1 } }, /keeps an unknown delivery \(a conversation message among it\)/],
  ['a chat share with totals that say nothing', { chat: { draft: true, pending: 0, unknown: 0 } }, /keeps unsent input/],
  ['a malformed chat share', { chat: 'oops' }, /keeps/],
];

scenario('cache full: a frame whose conversation holds a draft, a pending message or an unknown delivery is never evicted as clean; the person decides', () => {
  for (const [what, held, words] of CHAT_HELD) {
    const p = listPage();
    const fr = [];
    fr.push(open(p, 'w1', 's1', 'pi-1', held).f);
    for (const s of ['s2', 's3', 's4']) fr.push(open(p, 'w1', s, 'pi-1').f);
    const r5 = p.addRow('w1', 's5', 'pi-1');
    p.fireDoc('click', click(r5.name));
    // s1 answers the query with its conversation state; the clean ones answer clean.
    answer(p, fr[0], 'w1', 's1', held);
    for (let i = 1; i < 3; i++) { if (posts(fr[i]).some((x) => x.msg.type === 'query')) answer(p, fr[i], 'w1', ['s1', 's2', 's3', 's4'][i], {}); }
    if (!fr[0].parentNode) fail(what + ': the frame with a conversation state was discarded');
    if (frameOf(p, 's5', 'pi-1') && p.dlg.showModals === 0 && p.framesHost.children.length > 4) fail(what + ': more frames than the cache holds');
    // The clean s2 went (oldest clean first), s1 stayed and is named for what it holds.
    if (p.dlg.modal) {
      fail(what + ': the decision dialog opened although clean frames were available');
    }
    p.kept; // referenced to keep the stub honest
    const keptText = p.keptList().map((b) => b.getAttribute('aria-label') || b.textContent).join(' | ');
    if (!words.test(keptText)) fail(what + ': the kept session is not named for the conversation: ' + keptText);
  }
});

scenario('cache full of frames that hold a conversation state: nothing is discarded without the person\'s choice, and the dialog names the conversation', () => {
  const p = listPage();
  const held = CHAT_HELD.slice(0, 3).map((c) => c[1]);
  const fr = held.map((h, i) => open(p, 'w1', 's' + (i + 1), 'pi-1', h).f);
  fr.push(open(p, 'w1', 's4', 'pi-1').f);
  const r5 = p.addRow('w1', 's5', 'pi-1');
  p.fireDoc('click', click(r5.name));
  held.forEach((h, i) => answer(p, fr[i], 'w1', 's' + (i + 1), h));
  eq(p.framesHost.children.length, 4, 'nothing was discarded');
  if (!p.dlg.modal || p.dlg.showModals !== 1) fail('no explicit decision was asked for');
  const text = p.dlgList.children.map((c) => c.textContent).join(' | ');
  for (const [, , words] of CHAT_HELD.slice(0, 3)) { if (!words.test(text)) fail('the decision does not name the conversation: ' + text); }
  if (frameOf(p, 's5', 'pi-1')) fail('s5 opened without room');
  p.dlgCancel.fire('click');
  eq(p.framesHost.children.length, 4, 'Cancel changed something');
});

scenario('A/B: a conversation draft in A survives switching to B and back; B (clean) is the one a full cache gives up', () => {
  const p = listPage();
  const A = open(p, 'w1', 'A', 'pi-1').f;
  toParent(p, A, msg('state', 'w1', 'A', state({ dirty: true, chat: { draft: true, pending: 0, unknown: 0 } })));
  const B = open(p, 'w1', 'B', 'pi-1').f;
  const C = open(p, 'w1', 'C', 'pi-1').f;
  const D = open(p, 'w1', 'D', 'pi-1').f;
  eq(types(A), ['suspend'], 'A is only told to suspend (reads stop; nothing is aborted or sent)');
  const rE = p.addRow('w1', 'E', 'pi-1');
  p.fireDoc('click', click(rE.name));
  // Oldest hidden frame is A, but it holds a conversation draft: the next one that answers clean goes.
  if (posts(A).some((x) => x.msg.type === 'query')) answer(p, A, 'w1', 'A', { dirty: true, chat: { draft: true, pending: 0, unknown: 0 } });
  if (posts(B).some((x) => x.msg.type === 'query')) answer(p, B, 'w1', 'B', {});
  if (!A.parentNode) fail('A (conversation draft) was evicted');
  if (B.parentNode) fail('B (clean) was not the frame given up');
  if (!frameOf(p, 'E', 'pi-1')) fail('E did not open once a clean frame was given up');
  // Back to A: the same frame, resumed, with its draft (nothing was retargeted).
  p.fireDoc('click', click(p.rows['w1/A'].name));
  if (frameOf(p, 'A', 'pi-1') !== A || A.hidden) fail('A did not come back as the same frame');
  eq(types(A).filter((t) => t === 'resume').length, 1, 'A resumed once');
  if (!C || !D) fail('unreachable');
});

scenario('a refusal, an outage or a sign-in change: frames holding a conversation state are kept hidden (never shown, nothing sent) or cleared with the others', async () => {
  for (const [what, held] of CHAT_HELD.slice(0, 3)) {
    const p = listPage({ manual: true });
    const A = open(p, 'w1', 'minion-1', 'pi-1', held[1] || held).f;
    const B = open(p, 'w1', 'minion-2', 'pi-1').f;
    p.fireDoc('click', click(p.rows['w1/minion-1'].name));
    respond(contextReads(p)[0], 404); await flush();
    respond(principalReads(p)[0], 200); await flush();
    if (!A.parentNode || !A.hidden) fail(what + ': a refused frame holding a conversation state must be kept, hidden');
    if (types(A).some((t) => t === 'resume')) fail(what + ': a refused frame was resumed');
    if (p.sent.fetch + p.sent.xhr + p.sent.ajax !== 0) fail(what + ': the page sent something');
    if (!B.parentNode) fail(what + ': the other frame went');
  }
  // Another sign-in clears every frame, conversation drafts and unknown deliveries included.
  for (const what of ['a conversation draft', 'an unknown conversation delivery']) {
    const p = listPage();
    const held = what === 'a conversation draft' ? { dirty: true, chat: { draft: true, pending: 0, unknown: 0 } } : { unknown: 1, chat: { draft: false, pending: 0, unknown: 1 } };
    const A = open(p, 'w1', 'minion-1', 'pi-1', held).f;
    toParent(p, A, msg('state', 'w1', 'minion-1', state(held)));
    const B = open(p, 'w1', 'minion-2').f;
    p.tick(30000);
    respond(principalReads(p)[0], 200, 'basic', 'ctx-bob-0002');
    await flush();
    if (A.parentNode || B.parentNode) fail(what + ': a frame of the first person survived another sign-in');
    if (p.host.hasAttribute('data-frame-dirty')) fail(what + ': the page still holds the first person\'s frames');
  }
});

scenario('a late answer, a state of a frame that changed and a hidden frame\'s messages keep the conversation share exact', () => {
  const p = listPage();
  const A = open(p, 'w1', 'minion-1', 'pi-1').f;
  const B = open(p, 'w1', 'minion-2', 'pi-1').f; // A is hidden now
  // A reports a conversation message on its way while hidden; a later query answer of an older question cannot make it clean.
  toParent(p, A, msg('state', 'w1', 'minion-1', state({ pending: 1, chat: { draft: false, pending: 1, unknown: 0 } })));
  const r3 = p.addRow('w1', 'minion-3', 'pi-1');
  p.fireDoc('click', click(r3.name));
  // Its late 'clean' answer for another nonce is not used.
  toParent(p, A, msg('state', 'w1', 'minion-1', Object.assign(state({}), { nonce: 'not-a-nonce-of-ours' })));
  if (!A.parentNode) fail('a stray clean answer discarded a frame with a message on its way');
  // Hidden frames are only ever asked to suspend or resume or queried: nothing that could abort or send a write.
  for (const f of [A, B]) {
    for (const x of posts(f)) {
      if (!['suspend', 'resume', 'query'].includes(x.msg.type)) fail('a hidden frame was sent a ' + x.msg.type);
    }
  }
});

scenario('frame: the conversation share travels as flags and counts only, never text, and only when the controller reports it', () => {
  const f = framePage();
  f.control.state = { dirty: true, pending: 1, unknown: 0, conn: 'live', suspended: false, chat: { draft: true, pending: 1, unknown: 0, text: 'secret message', draft_text: 'rm -rf /' } };
  f.body.fire('input');
  f.flush();
  const s = f.parentPosts.pop();
  eq(s.m.chat, { draft: true, pending: 1, unknown: 0 }, 'the conversation share');
  if (/secret|rm -rf/.test(JSON.stringify(s.m))) fail('text reached the host: ' + JSON.stringify(s.m));
  // An older controller (no chat) sends no chat member at all.
  const g = framePage();
  g.control.state = { dirty: false, pending: 0, unknown: 0, conn: 'live', suspended: false };
  g.body.fire('input');
  g.flush();
  if ('chat' in g.parentPosts.pop().m) fail('a chat member was invented');
});

scenario('frame: tells the host it is ready; answers a query; suspends and resumes only on the host\'s word', () => {
  const f = framePage();
  eq(f.parentPosts.map((x) => x.m.type), ['ready'], 'first message');
  eq(f.parentPosts[0].to, ORIGIN, 'target origin');
  eq(f.parentPosts[0].m.target, { worker: 'w1', session: 'minion-1' }, 'target');
  f.fromHost({ minion: 'inspector', v: 1, type: 'query', nonce: 'n1', target: target('w1', 'minion-1') });
  const r = f.parentPosts.pop().m;
  eq([r.type, r.nonce, r.dirty, r.pending, r.unknown, r.suspended], ['state', 'n1', false, 0, 0, false], 'reply');
  f.fromHost({ minion: 'inspector', v: 1, type: 'suspend', nonce: 'n2', target: target('w1', 'minion-1') });
  eq(f.controls.filter((c) => c !== 'state'), ['suspend'], 'suspend reached the view');
  if (f.parentPosts.pop().m.suspended !== true) fail('the reply does not say suspended');
  f.fromHost({ minion: 'inspector', v: 1, type: 'resume', target: target('w1', 'minion-1') });
  eq(f.controls.filter((c) => c !== 'state'), ['suspend', 'resume'], 'resume reached the view');
  eq(f.parentPosts.pop().m.nonce, '', 'a message without nonce is answered without one');
});

scenario('frame: ignores anyone but its parent, other origins, other targets and unknown or write-like types', () => {
  const f = framePage();
  const n = f.parentPosts.length;
  const ok = { minion: 'inspector', v: 1, type: 'suspend', target: target('w1', 'minion-1') };
  f.fromHost(ok, { origin: 'https://evil.example' });
  f.fromHost(ok, { source: { postMessage() {} } });
  f.fromHost(ok, { source: null });
  f.fromHost(Object.assign({}, ok, { target: target('w1', 'minion-2') }));
  f.fromHost(Object.assign({}, ok, { target: target('w2', 'minion-1') }));
  f.fromHost(Object.assign({}, ok, { target: null }));
  f.fromHost(Object.assign({}, ok, { v: 2 }));
  f.fromHost(Object.assign({}, ok, { minion: 'x' }));
  for (const t of ['send', 'text', 'keys', 'answer', 'force', 'task', 'submit', 'constructor', '__proto__', 'ready', 'state', 'close-request', 'eval', 'navigate']) f.fromHost(Object.assign({}, ok, { type: t, text: 'rm -rf', key: 'Enter' }));
  f.fromHost('suspend'); f.fromHost(null); f.fromHost(7);
  if (f.parentPosts.length !== n) fail('the frame answered a message it must ignore: ' + JSON.stringify(f.parentPosts.slice(n)));
  if (f.controls.some((c) => c === 'suspend' || c === 'resume')) fail('the view was suspended by a message that must be ignored');
  // The only things the view is ever asked for are state, suspend and resume.
  const allowed = new Set(['state', 'suspend', 'resume']);
  if (f.controls.some((c) => !allowed.has(c))) fail('an unexpected control op: ' + f.controls);
});

scenario('frame: reports flags and counts, never text, and only when they change', () => {
  const f = framePage();
  f.control.state = { dirty: true, pending: 2, unknown: 1, conn: 'live', suspended: false, text: 'secret draft', draft: 'rm -rf /' };
  f.body.fire('input');
  f.flush();
  const s = f.parentPosts.pop();
  eq([s.m.type, s.m.dirty, s.m.pending, s.m.unknown], ['state', true, 2, 1], 'state');
  const wire = JSON.stringify(s.m);
  if (/secret|rm -rf/.test(wire)) fail('text reached the host: ' + wire);
  eq(Object.keys(s.m).sort(), ['conn', 'ctrl', 'dirty', 'doc', 'minion', 'pending', 'suspended', 'target', 'type', 'unknown', 'v'], 'only metadata fields');
  const n = f.parentPosts.length;
  f.body.fire('click');
  f.flush();
  if (f.parentPosts.length !== n) fail('an unchanged state was reported again');
  for (const ev of ['htmx:beforeSend', 'htmx:afterRequest', 'htmx:sendError', 'htmx:timeout', 'htmx:sendAbort']) {
    f.control.state.pending = f.control.state.pending + 1;
    f.body.fire(ev);
    f.flush();
    if (f.parentPosts.length === n && ev === 'htmx:beforeSend') fail('a request did not update the host');
  }
});

// Review finding 3: htmx failed to load, terminal-input.js never installed its controller, and the frame said ready and "clean" over a
// textarea that could be typed into.
scenario('frame: without its controller it never says ready and every state it gives is unknown, never clean', () => {
  const f = framePage({ noController: true });
  eq(f.parentPosts.length, 0, 'messages from a document whose controller did not answer (it must not say ready)');
  if (!f.doc.documentElement.hasAttribute('data-inspector-doc')) fail('the document did not mark itself');
  f.fromHost({ minion: 'inspector', v: 1, type: 'query', nonce: 'n', target: target('w1', 'minion-1') });
  const r = f.parentPosts.pop().m;
  eq([r.type, r.ctrl, r.dirty], ['state', false, true], 'a query without a controller');
  // With the controller: ready carries the identity context and the document id, which is also on the document element.
  const g = framePage();
  const ready = g.parentPosts[0].m;
  eq([ready.type, ready.ctrl, ready.ctx], ['ready', true, CTX], 'ready');
  eq(g.doc.documentElement.getAttribute('data-inspector-doc'), ready.doc, 'the document element carries the id the host reads at the load event');
});

scenario('frame: every message names this document, and a message for another document is ignored', () => {
  const f = framePage();
  const ready = f.parentPosts[0].m;
  if (typeof ready.doc !== 'string' || ready.doc.length < 8) fail('ready has no document id: ' + JSON.stringify(ready));
  f.fromHost({ minion: 'inspector', v: 1, type: 'query', nonce: 'n', target: target('w1', 'minion-1'), doc: ready.doc });
  const r = f.parentPosts.pop().m;
  eq([r.type, r.doc], ['state', ready.doc], 'reply names the document');
  const n = f.parentPosts.length;
  f.fromHost({ minion: 'inspector', v: 1, type: 'suspend', target: target('w1', 'minion-1'), doc: 'the-previous-document' });
  eq(f.parentPosts.length, n, 'a message for another document was answered');
  if (f.controls.includes('suspend')) fail('a message for another document suspended this one');
  // Two documents of the same frame do not share an id.
  const g = framePage();
  if (g.parentPosts[0].m.doc === ready.doc) fail('two documents have the same id');
});

scenario('frame: Escape asks the host to close unless a local dialog is open, and never goes to the session', () => {
  const f = framePage();
  const n = f.parentPosts.length;
  const esc = (o) => (f.doc.l.keydown || []).forEach((h) => h(Object.assign({ key: 'Escape', defaultPrevented: false }, o || {})));
  esc();
  eq(f.parentPosts.slice(n).map((x) => x.m.type), ['close-request'], 'Escape');
  const dialog = el('dialog', { open: '' });
  f.body.appendChild(dialog);
  f.doc.querySelector = (s) => (s === 'dialog[open]' ? dialog : null);
  esc();
  eq(f.parentPosts.length, n + 1, 'Escape over the local dialog');
  f.doc.querySelector = () => null;
  esc({ key: 'a' }); esc({ defaultPrevented: true }); esc({ isComposing: true });
  eq(f.parentPosts.length, n + 1, 'other keys, handled Escape and composition');
  if (f.controls.some((c) => c !== 'state')) fail('Escape reached the view');
});

scenario('frame: a page that is not framed does nothing', () => {
  const f = framePage({ top: true });
  eq(f.parentPosts.length, 0, 'messages from a top-level page');
  if ((f.win.l.message || []).length) fail('a top-level page listens to messages');
});

// ---- run ------------------------------------------------------------------------------------------

(async () => {
  for (const [name, fn] of scenarios) {
    if (process.argv[3] && !name.includes(process.argv[3])) continue;
    try { await fn(); } catch (e) { fail(name + ': ' + (e && e.stack || e)); }
  }
  console.log('ok ' + scenarios.length + ' scenarios');
})();
