// Runs the scripts of the workbench shell against a DOM stub: static/shell.js
// (theme, mobile Menu, heading focus, unsent drafts), static/live-list.js (focus,
// scroll and the polite summary of a live list) and static/session-ui.js (dock
// tabs, right pane, Send anyway confirmation, task state, dialog answer).
// The stub models only what these scripts use; it is not a browser. The browser
// behaviour that depends on layout (breakpoints, forced colours, reduced motion)
// is covered by the shared-browser checks, not here.
// Usage: node workbench_ui_test.js <static dir>
'use strict';
const fs = require('fs');
const path = require('path');
const dir = process.argv[2];
const read = (f) => fs.readFileSync(path.join(dir, f), 'utf8');
const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };

// node builds an element: attributes, class list, listeners, focus tracking.
function node(o) {
  o = o || {};
  const l = {};
  const classes = new Set((o.className || '').split(' ').filter(Boolean));
  const n = {
    id: o.id || '', tagName: (o.tag || 'div').toUpperCase(), value: o.value || '', attrs: Object.assign({}, o.attrs), hidden: !!o.hidden,
    checked: !!o.checked, disabled: false, tabIndex: 0, children: o.children || [], focused: 0, focusArgs: [], listeners: l, parentNode: null,
    classList: {
      add: (c) => classes.add(c), remove: (c) => classes.delete(c), contains: (c) => classes.has(c),
      toggle: (c, on) => { const want = on === undefined ? !classes.has(c) : !!on; if (want) { classes.add(c); } else { classes.delete(c); } return want; },
    },
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    hasAttribute(k) { return k in this.attrs; }, removeAttribute(k) { delete this.attrs[k]; },
    addEventListener(name, f) { (l[name] = l[name] || []).push(f); },
    removeEventListener(name, f) { l[name] = (l[name] || []).filter((x) => x !== f); },
    focus(opt) { this.focused++; this.focusArgs.push(opt); doc.activeElement = this; },
    contains(x) { return x === this || this.children.some((c) => c === x || (c.contains && c.contains(x))); },
    closest(sel) { return (o.closest && o.closest[sel]) || null; },
    querySelector(sel) { return (o.q && o.q[sel] && o.q[sel][0]) || null; },
    querySelectorAll(sel) { return (o.q && o.q[sel]) || []; },
    dispatchEvent(ev) { (l[ev.type] || []).slice().forEach((f) => f(ev)); return true; },
    showModal() { this.attrs.open = ''; this.modal = true; }, close() { delete this.attrs.open; this.modal = false; },
    textContent: o.text || '',
  };
  return n;
}
const ev = (type, extra) => Object.assign({ type, defaultPrevented: false, immediate: false, preventDefault() { this.defaultPrevented = true; }, stopImmediatePropagation() { this.immediate = true; } }, extra);

let doc;
// load runs one script on a fresh page. o.selectors maps a selector string to the
// elements document.querySelectorAll returns for it.
function load(file, o) {
  o = o || {};
  const dl = {};
  const wl = {};
  const store = o.store || {};
  doc = {
    activeElement: null, referrer: o.referrer || '', body: node({ tag: 'body', className: o.bodyClass }), documentElement: node({ tag: 'html' }),
    getElementById: (id) => (o.ids && o.ids[id]) || null,
    querySelector: (sel) => (o.query ? o.query(sel) : (o.selectors && o.selectors[sel] && o.selectors[sel][0]) || null),
    querySelectorAll: (sel) => (o.selectors && o.selectors[sel]) || [],
    addEventListener(n, f, cap) { (dl[n] = dl[n] || []).push({ f, cap: !!cap }); },
    createRange: () => ({ selectNodeContents() {} }),
  };
  const confirms = [];
  const win = {
    location: { origin: 'https://hub.example.com', reload() { win.reloads = (win.reloads || 0) + 1; } },
    // Anything that would send a request counts: the shell scripts must never write on their own.
    fetch() { win.sent = (win.sent || 0) + 1; }, XMLHttpRequest: function () { win.sent = (win.sent || 0) + 1; }, htmx: { ajax() { win.sent = (win.sent || 0) + 1; } },
    localStorage: o.brokenStorage ? { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); }, removeItem() { throw new Error('blocked'); } }
      : { getItem: (k) => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: (k) => { delete store[k]; } },
    matchMedia: (q) => ({ matches: !!(o.media && o.media[q]), addEventListener: (n, f) => { win.mediaListener = f; } }),
    addEventListener(n, f) { (wl[n] = wl[n] || []).push(f); },
    confirm: (m) => { confirms.push(m); return o.confirm !== false; },
    scrollX: 0, scrollY: 0, scrolls: [], scrollTo(x, y) { this.scrolls.push([x, y]); }, setTimeout: (f) => f && 0,
  };
  const p = {
    doc, win, store, confirms,
    // fire delivers a document event to the capture listeners first, then the others.
    fire(type, e) {
      e = e || ev(type);
      const list = dl[type] || [];
      for (const x of list.filter((y) => y.cap)) { x.f(e); if (e.immediate) { return e; } }
      for (const x of list.filter((y) => !y.cap)) { x.f(e); if (e.immediate) { return e; } }
      return e;
    },
    fireWindow(type, e) { e = e || ev(type); (wl[type] || []).forEach((f) => f(e)); return e; },
  };
  // The body gets the same events (htmx events and custom ones bubble to it).
  doc.body.addEventListener = (n, f) => { (dl[n] = dl[n] || []).push({ f, cap: false }); };
  new Function('document', 'window', 'navigator', 'Event', read(file))(doc, win, { clipboard: o.clipboard }, function Event(type, init) { return ev(type, init); });
  return p;
}

const scenarios = [];
const scenario = (name, fn) => scenarios.push([name, fn]);

// ---- shell.js ----------------------------------------------------------------

function shellPage(o) {
  o = o || {};
  const first = node({ tag: 'a', attrs: { href: '/' } });
  const second = node({ tag: 'a', attrs: { href: '/workers' } });
  const current = node({ tag: 'a', attrs: { href: '/sessions', 'aria-current': 'page' } });
  const signOut = node({ tag: 'button' });
  const rail = node({ id: 'site-rail', q: {
    'a[href], button:not([disabled]), select, input:not([disabled])': [first, second, current, signOut],
    '.nav-list a[aria-current="page"]': [current], '.nav-list a': [first, second, current] } });
  rail.children = [first, second, current, signOut];
  const button = node({ id: 'menu-button', tag: 'button', attrs: { 'aria-expanded': 'false' } });
  const scrim = node({ id: 'scrim' });
  const close = node({ id: 'menu-close', tag: 'button' });
  const select = node({ tag: 'select', value: 'system' });
  const h1 = node({ tag: 'h1' });
  const draft = node({ tag: 'textarea', value: o.draft || '' });
  const logout = node({ tag: 'form', attrs: { 'data-json-action': '/auth/logout' } });
  const unresolved = o.unresolved ? [node({ attrs: { 'data-unresolved': '' } })] : [];
  const p = load('shell.js', Object.assign({
    query: (sel) => (sel === '#content h1' ? h1 : sel === 'dialog[open]' ? (o.dialogOpen ? node({ tag: 'dialog' }) : null) : sel === '[autofocus]' ? (o.autofocus ? node({}) : null) : null),
    ids: { 'menu-button': button, 'site-rail': rail, scrim, 'menu-close': close },
    selectors: { '[data-theme-select]': [select], '#content h1': [h1], '[data-draft]': [draft], '[data-unresolved]': unresolved, '[data-frame-dirty]': o.frameDirty ? [node({ attrs: { 'data-frame-dirty': '' } })] : [] },
    referrer: 'https://hub.example.com/workers',
  }, o));
  return Object.assign(p, { first, second, current, signOut, rail, button, scrim, close, select, h1, draft, logout });
}

scenario('theme: System, Light and Dark are stored as a display preference', () => {
  let p = shellPage({ store: { 'minion-theme': 'dark' } });
  if (p.select.value !== 'dark') fail('the stored theme is not selected: ' + p.select.value);
  p.select.value = 'light';
  p.select.listeners.change[0]();
  if (p.store['minion-theme'] !== 'light' || p.doc.documentElement.attrs['data-theme'] !== 'light') fail('light was not applied and stored');
  p.select.value = 'system';
  p.select.listeners.change[0]();
  if ('minion-theme' in p.store || 'data-theme' in p.doc.documentElement.attrs) fail('system did not remove the preference');
  p.select.value = 'bogus';
  p.select.listeners.change[0]();
  if (p.select.value !== 'system') fail('an unknown theme value was accepted: ' + p.select.value);
  // Blocked storage never breaks the page.
  p = shellPage({ brokenStorage: true });
  p.select.value = 'dark';
  p.select.listeners.change[0]();
  if (p.doc.documentElement.attrs['data-theme'] !== 'dark') fail('a blocked storage stopped the theme from applying');
});

scenario('menu: expanded state, focus, containment, Escape and return focus', () => {
  const p = shellPage();
  p.doc.activeElement = p.button;
  p.button.listeners.click[0]();
  if (!p.rail.classList.contains('open') || p.button.attrs['aria-expanded'] !== 'true' || !p.scrim.classList.contains('open')) fail('the menu did not open');
  if (p.current.focused !== 1) fail('opening the menu did not focus the current page link');
  // Tab from the last control wraps to the first; Shift+Tab from the first wraps to the last.
  p.doc.activeElement = p.signOut;
  let e = ev('keydown', { key: 'Tab', shiftKey: false });
  p.fire('keydown', e);
  if (!e.defaultPrevented || p.first.focused !== 1) fail('Tab did not stay inside the open menu');
  p.doc.activeElement = p.first;
  e = ev('keydown', { key: 'Tab', shiftKey: true });
  p.fire('keydown', e);
  if (!e.defaultPrevented || p.signOut.focused !== 1) fail('Shift+Tab did not wrap inside the open menu');
  // A focus that left the menu is brought back.
  p.doc.activeElement = node();
  e = ev('keydown', { key: 'Tab', shiftKey: false });
  p.fire('keydown', e);
  if (!e.defaultPrevented) fail('a focus outside the open menu was not brought back');
  // Escape closes and returns the focus to what opened it.
  e = ev('keydown', { key: 'Escape' });
  p.fire('keydown', e);
  if (p.rail.classList.contains('open') || p.button.attrs['aria-expanded'] !== 'false' || p.button.focused < 1) fail('Escape did not close the menu and return the focus');
  // Closed: Escape and Tab are not touched.
  e = ev('keydown', { key: 'Escape' });
  p.fire('keydown', e);
  if (e.defaultPrevented) fail('Escape was swallowed while the menu is closed');
  // The scrim and the close button close it too.
  p.button.listeners.click[0]();
  p.scrim.listeners.click[0]();
  if (p.rail.classList.contains('open')) fail('the scrim did not close the menu');
  p.button.listeners.click[0]();
  p.close.listeners.click[0]();
  if (p.rail.classList.contains('open')) fail('Close menu did not close the menu');
  // A wide window closes it without stealing the focus.
  p.button.listeners.click[0]();
  const before = p.button.focused;
  p.win.mediaListener({ matches: true });
  if (p.rail.classList.contains('open') || p.button.focused !== before) fail('widening the window did not close the menu quietly');
});

scenario('menu: Escape belongs to an open dialog first', () => {
  const p = shellPage({ dialogOpen: true });
  p.button.listeners.click[0]();
  const e = ev('keydown', { key: 'Escape' });
  p.fire('keydown', e);
  if (e.defaultPrevented || !p.rail.classList.contains('open')) fail('Escape was taken from the dialog');
});

scenario('heading focus only after navigation inside the Hub', () => {
  let p = shellPage();
  if (p.h1.focused !== 1 || p.h1.attrs.tabindex !== '-1' || !p.h1.focusArgs[0].preventScroll) fail('the heading was not focused after an in-app navigation');
  p = shellPage({ referrer: 'https://elsewhere.example.org/' });
  if (p.h1.focused !== 0) fail('the heading was focused for a visit from outside');
  p = shellPage({ referrer: '' });
  if (p.h1.focused !== 0) fail('the heading was focused without a referrer');
  p = shellPage({ autofocus: true });
  if (p.h1.focused !== 0) fail('the heading took the focus from an autofocus field');
});

// An unknown delivery (kept by terminal-input.js with the payload that was submitted) is unresolved
// input: signing out or leaving asks first, like an unsent draft; Dismiss resolves it.
scenario('unresolved unknown deliveries count as unsent input for sign out and leaving', () => {
  let p = shellPage({ unresolved: true, confirm: false });
  const e = ev('submit', { target: p.logout });
  p.fire('submit', e);
  if (!e.defaultPrevented || p.confirms.length !== 1 || !/unknown|not sent/i.test(p.confirms[0])) fail('sign out with an unresolved outcome did not ask: ' + JSON.stringify(p.confirms));
  if (!p.fireWindow('beforeunload').defaultPrevented) fail('leaving with an unresolved outcome did not ask the browser to confirm');
  p = shellPage({ unresolved: false });
  if (p.fireWindow('beforeunload').defaultPrevented) fail('nothing is unresolved but leaving asked');
});

// The session inspector (list pages) keeps frames; the page marks itself with data-frame-dirty while one
// holds unsent input, a request on its way or an unknown delivery, because the top document cannot see into them.
scenario('a frame of the session inspector that holds something counts for sign out and leaving', () => {
  let p = shellPage({ frameDirty: true, confirm: false });
  const e = ev('submit', { target: p.logout });
  p.fire('submit', e);
  if (!e.defaultPrevented || p.confirms.length !== 1) fail('sign out with a frame that holds input did not ask');
  if (!p.fireWindow('beforeunload').defaultPrevented) fail('leaving with a frame that holds input did not ask the browser to confirm');
  p = shellPage({ frameDirty: false });
  if (p.fireWindow('beforeunload').defaultPrevented) fail('no frame holds anything but leaving asked');
});

scenario('the compact view in the inspector frame never takes the focus when it loads', () => {
  const p = shellPage({ bodyClass: 'embed' });
  if (p.h1.focused !== 0) fail('the heading of an embedded view took the focus from its host');
  const q = shellPage();
  if (q.h1.focused !== 1) fail('the full page lost its heading focus');
});

scenario('theme.js follows a theme chosen in another document of this origin', () => {
  const store = {};
  const p = load('theme.js', { store });
  if (p.doc.documentElement.attrs['data-theme']) fail('no theme stored but one applied');
  store['minion-theme'] = 'dark';
  p.fireWindow('storage', { key: 'minion-theme' });
  if (p.doc.documentElement.attrs['data-theme'] !== 'dark') fail('the dark theme was not applied by the storage event');
  delete store['minion-theme'];
  p.fireWindow('storage', { key: 'minion-theme' });
  if ('data-theme' in p.doc.documentElement.attrs) fail('System did not remove the theme');
  store['minion-theme'] = 'light';
  p.fireWindow('storage', { key: 'unrelated' });
  if ('data-theme' in p.doc.documentElement.attrs) fail('another key changed the theme');
});

scenario('unsent drafts: sign out asks, leaving asks the browser', () => {
  let p = shellPage({ draft: 'half a command' });
  let e = ev('submit', { target: p.logout });
  p.confirm = false;
  p.fire('submit', Object.assign(e, { target: p.logout }));
  void p;
  // Declined sign out: the submit is cancelled and nothing else sees it.
  p = shellPage({ draft: 'half a command', confirm: false });
  e = ev('submit', { target: p.logout });
  p.fire('submit', e);
  if (!e.defaultPrevented || !e.immediate || p.confirms.length !== 1 || !/not sent/.test(p.confirms[0])) fail('a declined sign out was not cancelled');
  const unload = p.fireWindow('beforeunload');
  if (!unload.defaultPrevented) fail('leaving with a draft did not ask the browser to confirm');
  // Accepted: the sign out goes on and leaving no longer asks.
  p = shellPage({ draft: 'half a command', confirm: true });
  e = ev('submit', { target: p.logout });
  p.fire('submit', e);
  if (e.defaultPrevented || p.confirms.length !== 1) fail('an accepted sign out was cancelled');
  if (p.fireWindow('beforeunload').defaultPrevented) fail('leaving after a confirmed discard asked again');
  // No draft: no question; another form is never asked about.
  p = shellPage({ draft: '   ' });
  e = ev('submit', { target: p.logout });
  p.fire('submit', e);
  if (p.confirms.length !== 0 || e.defaultPrevented) fail('a blank draft caused a question');
  if (p.fireWindow('beforeunload').defaultPrevented) fail('a blank draft blocked leaving');
  p = shellPage({ draft: 'x' });
  const other = node({ tag: 'form', attrs: { 'data-json-action': '/api/other' } });
  e = ev('submit', { target: other });
  p.fire('submit', e);
  if (p.confirms.length !== 0) fail('a form other than sign out asked about the draft');
});

// ---- live-list.js ------------------------------------------------------------

scenario('a live list keeps focus, scroll and announces only a changed summary', () => {
  const summaryNode = (t) => node({ attrs: { 'data-summary': t } });
  const status = node({ id: 'list-announce' });
  // The list before the update.
  const link = node({ tag: 'a', attrs: { 'data-control': 'name' } });
  const row = node({ attrs: { 'data-key': 's-w1-ci' }, q: { 'a, button, input, select, textarea, summary': [link] } });
  link.closest = (sel) => (sel === '[data-key]' ? row : null);
  const wrap = node({}); wrap.scrollLeft = 30; wrap.scrollTop = 5;
  const list = node({ id: 'results', attrs: { 'data-live-list': '' }, q: { '.table-wrap': [wrap], '[data-key]': [row], '[data-summary]': [summaryNode('2 sessions')] } });
  list.contains = (x) => x === link || x === list;
  const p = load('live-list.js', { ids: { 'list-announce': status }, selectors: { '[data-live-list]': [list] } });
  p.win.scrollX = 3; p.win.scrollY = 120;
  p.doc.activeElement = link;
  p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list }));
  // The swap replaces the rows: a new row with the same key and its own link.
  const link2 = node({ tag: 'a', attrs: { 'data-control': 'name' } });
  const row2 = node({ attrs: { 'data-key': 's-w1-ci' }, q: { 'a, button, input, select, textarea, summary': [link2] } });
  const wrap2 = node({});
  const after = { '.table-wrap': [wrap2], '[data-key]': [row2], '[data-summary]': [summaryNode('3 sessions')] };
  list.querySelectorAll = (sel) => after[sel] || [];
  list.querySelector = (sel) => (after[sel] || [])[0] || null;
  p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list }));
  if (link2.focused !== 1 || !link2.focusArgs[0].preventScroll) fail('the focused row control was not focused again without scrolling');
  if (wrap2.scrollLeft !== 30 || wrap2.scrollTop !== 5) fail('the table scroll was not restored');
  if (!p.win.scrolls.length || p.win.scrolls[0][0] !== 3 || p.win.scrolls[0][1] !== 120) fail('the page scroll was not restored: ' + JSON.stringify(p.win.scrolls));
  if (status.textContent !== 'Updated: 3 sessions.') fail('the summary was not announced: ' + status.textContent);
  // An update with the same summary stays quiet.
  status.textContent = '';
  p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list }));
  p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list }));
  if (status.textContent !== '') fail('an unchanged summary was announced again');
  // Another element (not a live list) is ignored.
  const other = node({});
  p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: other }));
  p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: other }));
  // A focused element that was removed from the list is not forced back.
  const gone = node({ tag: 'a' });
  gone.closest = () => null;
  p.doc.activeElement = gone;
  p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list }));
  p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list }));
});

// The sessions of a Workers row are a disclosure; a live update must not shut it.
scenario('a live list keeps an open disclosure open and restores focus to its summary', () => {
  const det = (id, open) => node({ tag: 'details', attrs: open ? { 'data-keep-open': id, open: '' } : { 'data-keep-open': id } });
  const summary = node({ tag: 'summary', attrs: { 'data-control': 'sessions-toggle' } });
  const row = node({ attrs: { 'data-key': 'w-w1' }, q: { 'a, button, input, select, textarea, summary': [summary] } });
  summary.closest = (sel) => (sel === '[data-key]' ? row : null);
  const list = node({ id: 'results', attrs: { 'data-live-list': '' }, q: { '.table-wrap': [], '[data-key]': [row], '[data-summary]': [], 'details[data-keep-open]': [det('ws-w1', true), det('ws-w2', false)] } });
  list.contains = (x) => x === summary || x === list;
  const p = load('live-list.js', { selectors: { '[data-live-list]': [list] } });
  p.doc.activeElement = summary;
  p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list }));
  // The swap: fresh, closed disclosures with the same ids.
  const n1 = det('ws-w1', false), n2 = det('ws-w2', false), n3 = det('ws-w3', false);
  const summary2 = node({ tag: 'summary', attrs: { 'data-control': 'sessions-toggle' } });
  const row2 = node({ attrs: { 'data-key': 'w-w1' }, q: { 'a, button, input, select, textarea, summary': [summary2] } });
  summary2.closest = (sel) => (sel === '[data-key]' ? row2 : null);
  const after = { '.table-wrap': [], '[data-key]': [row2], '[data-summary]': [], 'details[data-keep-open]': [n1, n2, n3] };
  list.querySelectorAll = (sel) => after[sel] || [];
  list.querySelector = (sel) => (after[sel] || [])[0] || null;
  p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list }));
  if (!n1.hasAttribute('open')) fail('the open disclosure was shut by the update');
  if (n2.hasAttribute('open') || n3.hasAttribute('open')) fail('a closed disclosure was opened by the update');
  if (summary2.focused !== 1) fail('the focused summary did not get the focus back');
});

// Focus is restored by the stable identity of the control (data-control) inside
// the keyed row, never by its position. A worker row shows its session-count link
// only while the worker has sessions, so the position of "New session" changes
// when the first session appears or the last one goes.
scenario('live list focus follows the control identity, not its position (0->1 and 1->0 sessions)', () => {
  const SEL = 'a, button, input, select, textarea, summary';
  const ctl = (name, tag) => node({ tag: tag || 'a', attrs: name ? { 'data-control': name } : {} });
  const keyed = (controls) => { const r = node({ attrs: { 'data-key': 'w-w1' }, q: { [SEL]: controls } }); r.attrs.tabindex = undefined; delete r.attrs.tabindex; controls.forEach((c) => { c.closest = (s) => (s === '[data-key]' ? r : null); }); return r; };
  const heading = node({ tag: 'h1' });
  const status = node({ id: 'list-announce' });
  // run focuses control `focusName` of the row before, applies the update and returns the page and the new row.
  const run = (before, focusName, after) => {
    const rowBefore = keyed(before.map((n) => ctl(n, n === 'new' ? 'button' : 'a')));
    const list = node({ id: 'results', attrs: { 'data-live-list': '' }, q: { '.table-wrap': [], '[data-key]': [rowBefore], '[data-summary]': [node({ attrs: { 'data-summary': 'x' } })] } });
    list.contains = (x) => rowBefore.q_has(x) || x === list;
    rowBefore.q_has = (x) => rowBefore.querySelectorAll(SEL).includes(x);
    const p = load('live-list.js', { ids: { 'list-announce': status }, selectors: { '[data-live-list]': [list] }, query: (sel) => (sel === '#content h1' || sel === 'h1' ? heading : null) });
    const active = rowBefore.querySelectorAll(SEL).find((c) => c.getAttribute('data-control') === focusName);
    p.doc.activeElement = active;
    p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list }));
    const rowAfter = keyed(after.map((n) => ctl(n, n === 'new' ? 'button' : 'a')));
    const now = { '.table-wrap': [], '[data-key]': [rowAfter], '[data-summary]': [node({ attrs: { 'data-summary': 'y' } })] };
    list.querySelectorAll = (sel) => now[sel] || [];
    list.querySelector = (sel) => (now[sel] || [])[0] || null;
    p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list }));
    const find = (n) => rowAfter.querySelectorAll(SEL).find((c) => c.getAttribute('data-control') === n);
    return { p, rowAfter, find, heading };
  };

  // 0 -> 1: the focused "New session" button stays the focused control although it moves from index 0 to 1.
  let r = run(['new'], 'new', ['sessions', 'new']);
  if (r.find('new').focused !== 1 || r.find('sessions').focused !== 0 || r.p.doc.activeElement !== r.find('new')) fail('0->1: the focus moved off "New session" onto ' + (r.p.doc.activeElement && r.p.doc.activeElement.attrs['data-control']));
  // 1 -> 0: the focused session-count link is gone; the focus must NOT land on "New session" (Enter there creates a session).
  r = run(['sessions', 'new'], 'sessions', ['new']);
  if (r.find('new').focused !== 0 || r.p.doc.activeElement === r.find('new')) fail('1->0: the focus was moved onto the "New session" write button');
  if (r.rowAfter.focused !== 1 || r.rowAfter.attrs.tabindex !== '-1' || !r.rowAfter.focusArgs[0].preventScroll) fail('1->0: the focus did not go to the non-mutating row, without scrolling');
  // The same row also keeps "New session" focused when only the link goes and New session was focused.
  r = run(['sessions', 'new'], 'new', ['new']);
  if (r.find('new').focused !== 1) fail('1->0 with New session focused: the focus was lost');
  // The row itself is gone: the heading takes the focus (not another row's control).
  const list2 = node({ id: 'results', attrs: { 'data-live-list': '' }, q: { '.table-wrap': [], '[data-key]': [] } });
  const oldCtl = ctl('sessions'); const oldRow = node({ attrs: { 'data-key': 'w-w1' }, q: { [SEL]: [oldCtl] } }); oldCtl.closest = () => oldRow;
  list2.contains = (x) => x === oldCtl;
  const p2 = load('live-list.js', { ids: { 'list-announce': status }, selectors: { '[data-live-list]': [list2] }, query: (sel) => (sel === '#content h1' || sel === 'h1' ? heading : null) });
  p2.doc.activeElement = oldCtl;
  p2.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list2 }));
  const other = ctl('new', 'button'); const otherRow = node({ attrs: { 'data-key': 'w-w2' }, q: { [SEL]: [other] } });
  const now2 = { '.table-wrap': [], '[data-key]': [otherRow] };
  list2.querySelectorAll = (sel) => now2[sel] || [];
  p2.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list2 }));
  if (other.focused !== 0) fail('the focus went to a control of another row');
  if (heading.focused !== 1 || heading.attrs.tabindex !== '-1') fail('a removed row did not hand the focus to the heading');
});

// S5.7 review 1: the Worker detail page's two controls (New session; In the session list) sit in groups that have a data-key (the
// canonical Worker id) and carry their own data-control, as the real markup does (checked against the real template in
// page_worker_test.go). Without that identity the focus was lost on every live update.
scenario('Worker detail controls keep the keyboard focus across a live replacement, by identity; a removed one falls back safely', () => {
  const SEL = 'a, button, input, select, textarea, summary';
  const heading = node({ tag: 'h1' });
  // page builds the fragment: o.actions (New session) and o.sessions (the link) say which controls this update has.
  const fragment = (o) => {
    const groups = [];
    const mk = (key, name, tag) => { const c = node({ tag, attrs: { 'data-control': name } }); const g = node({ attrs: { 'data-key': key }, q: { [SEL]: [c] } }); c.closest = (sel) => (sel === '[data-key]' ? g : null); g.ctl = c; groups.push(g); return g; };
    if (o.actions) { mk('worker-actions-w1', 'new-session', 'button'); }
    if (o.sessions) { mk('worker-sessions-w1', 'session-list', 'a'); }
    else { const head = node({ attrs: { 'data-key': 'worker-sessions-w1' }, q: { [SEL]: [] } }); head.ctl = null; groups.push(head); } // the section header stays without sessions; only the link goes
    return groups;
  };
  const run = (before, focusName, after) => {
    const b = fragment(before);
    const list = node({ id: 'results', attrs: { 'data-live-list': '' }, q: { '.table-wrap': [], '[data-key]': b, '[data-summary]': [node({ attrs: { 'data-summary': 'x' } })] } });
    list.contains = (x) => b.some((g) => g.ctl && g.ctl === x) || x === list;
    const p = load('live-list.js', { ids: { 'list-announce': node({ id: 'list-announce' }) }, selectors: { '[data-live-list]': [list] }, query: (sel) => (sel === '#content h1' || sel === 'h1' ? heading : null) });
    p.doc.activeElement = b.map((g) => g.ctl).find((c) => c && c.getAttribute('data-control') === focusName);
    p.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: list }));
    const a = fragment(after);                      // the swap replaced every element
    const now = { '.table-wrap': [], '[data-key]': a, '[data-summary]': [node({ attrs: { 'data-summary': 'y' } })] };
    list.querySelectorAll = (sel) => now[sel] || [];
    list.querySelector = (sel) => (now[sel] || [])[0] || null;
    p.doc.activeElement = p.doc.body;               // removing the focused subtree dropped the native focus
    p.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: list }));
    return { p, ctl: (n) => (a.map((g) => g.ctl).find((c) => c && c.getAttribute('data-control') === n) || null), group: (k) => a.find((g) => g.getAttribute('data-key') === k) || null };
  };
  // Each control, focused, is focused again after the replacement and it is that very action.
  let r = run({ actions: true, sessions: true }, 'new-session', { actions: true, sessions: true });
  if (r.ctl('new-session').focused !== 1 || r.ctl('session-list').focused !== 0 || r.p.doc.activeElement !== r.ctl('new-session')) fail('New session lost the focus on a live update');
  if (!r.ctl('new-session').focusArgs[0].preventScroll) fail('the restore scrolled the page');
  r = run({ actions: true, sessions: true }, 'session-list', { actions: true, sessions: true });
  if (r.ctl('session-list').focused !== 1 || r.ctl('new-session').focused !== 0) fail('"In the session list" lost the focus on a live update');
  // The last session went: the link is gone. The focus must not move to New session (Enter there starts a session); the group takes it.
  r = run({ actions: true, sessions: true }, 'session-list', { actions: true, sessions: false });
  if (r.ctl('new-session').focused !== 0 || r.p.doc.activeElement === r.ctl('new-session')) fail('the focus was moved onto the New session write button');
  const g = r.group('worker-sessions-w1');
  if (!g || g.focused !== 1 || g.attrs.tabindex !== '-1' || r.p.doc.activeElement !== g) fail('with only the link gone the section header (not an action) must take the focus');
  heading.focused = 0;
  // The Worker went offline: New session (and its group) is gone: the heading, never another action.
  r = run({ actions: true, sessions: true }, 'new-session', { actions: false, sessions: true });
  if (r.ctl('session-list').focused !== 0 || r.p.doc.activeElement === r.ctl('session-list')) fail('the focus was substituted by another control when New session went away');
  if (heading.focused !== 1 || heading.attrs.tabindex !== '-1') fail('a removed control did not hand the focus to the heading');
  heading.focused = 0;
  // Identity without a key (the situation of the review) restores nothing: the proof that the keys above are what does it.
  const bare = node({ tag: 'button', attrs: { 'data-control': 'new-session' } });
  const l2 = node({ id: 'results', attrs: { 'data-live-list': '' } });
  l2.contains = (x) => x === bare;
  const p2 = load('live-list.js', { selectors: { '[data-live-list]': [l2] } });
  p2.doc.activeElement = bare;
  p2.fire('htmx:sseBeforeMessage', ev('htmx:sseBeforeMessage', { target: l2 }));
  const bare2 = node({ tag: 'button', attrs: { 'data-control': 'new-session' } });
  p2.doc.activeElement = p2.doc.body;
  l2.contains = (x) => x === bare2;
  p2.fire('htmx:sseMessage', ev('htmx:sseMessage', { target: l2 }));
  if (bare2.focused !== 0) fail('test bug: an unkeyed control cannot be restored by this script');
});

// ---- session-ui.js -----------------------------------------------------------

function sessionPage(o) {
  o = o || {};
  const tabs = ['text', 'keys', 'task'].map((n, i) => node({ id: 'tab-' + n, tag: 'button', attrs: { role: 'tab', 'aria-controls': 'dock-' + n, 'aria-selected': i === 0 ? 'true' : 'false' } }));
  const panels = ['text', 'keys', 'task'].map((n) => node({ id: 'dock-' + n }));
  const tablist = node({ id: 'dock-tabs', hidden: true, q: { '[role="tab"]': tabs } });
  const toggle = node({ id: 'pane-toggle', tag: 'button', hidden: true, attrs: { 'aria-expanded': 'false' } });
  const close = node({ id: 'pane-close', tag: 'button', hidden: true });
  const paneLink = node({ tag: 'a', attrs: { href: '/x' } });
  const paneLast = node({ tag: 'button' });
  const paneHidden = node({ tag: 'button', hidden: true });
  const paneTabOff = node({ tag: 'button' }); paneTabOff.tabIndex = -1;
  const paneItems = [close, paneLink, paneHidden, paneTabOff, paneLast];
  const pane = node({ id: 'session-pane', children: paneItems, q: { 'a[href], button, input, select, textarea, summary, [tabindex]': paneItems } });
  const paneScrim = node({ id: 'pane-scrim' });
  const grid = node({ id: 'session-grid' });
  const draft = node({ tag: 'textarea', value: 'unsent draft' });
  const dock = node({ id: 'session-controls' });
  const view = node({ id: 'session-view' });
  const task = node({ id: 'task-text', tag: 'input', value: 'old task', attrs: { 'data-saved': 'old task' } });
  const taskState = node({ id: 'task-state', hidden: true });
  const ids = { 'session-view': view, 'dock-tabs': tablist, 'pane-toggle': toggle, 'session-pane': pane, 'session-grid': grid, 'pane-close': close, 'pane-scrim': paneScrim,
    'session-controls': dock, 'task-text': task, 'task-state': taskState, 'site-rail': o.rail || null };
  tabs.forEach((t, i) => { ids[t.id] = t; ids['dock-' + ['text', 'keys', 'task'][i]] = panels[i]; });
  const p = load('session-ui.js', { ids, media: o.media, store: o.store, selectors: {},
    query: (sel) => (sel === 'dialog[open]' ? (o.dialogOpen ? node({ tag: 'dialog' }) : null) : null) });
  return Object.assign(p, { tabs, panels, tablist, toggle, pane, grid, close, dock, view, task, taskState, paneScrim, paneLink, paneLast, draft });
}

scenario('dock tabs follow the keyboard tab pattern', () => {
  const p = sessionPage();
  if (p.tablist.hidden || !p.dock.classList.contains('tabbed')) fail('the tab list was not shown');
  if (!p.panels[1].hidden || !p.panels[2].hidden || p.panels[0].hidden) fail('only the first panel should be visible');
  p.doc.activeElement = p.tabs[0];
  let e = ev('keydown', { key: 'ArrowRight' });
  p.tablist.listeners.keydown[0](e);
  if (!e.defaultPrevented || p.tabs[1].attrs['aria-selected'] !== 'true' || p.tabs[1].focused !== 1 || p.tabs[0].tabIndex !== -1 || p.panels[1].hidden) fail('ArrowRight did not select and focus the next tab');
  e = ev('keydown', { key: 'End' });
  p.doc.activeElement = p.tabs[1];
  p.tablist.listeners.keydown[0](e);
  if (p.tabs[2].attrs['aria-selected'] !== 'true') fail('End did not select the last tab');
  p.doc.activeElement = p.tabs[2];
  p.tablist.listeners.keydown[0](ev('keydown', { key: 'ArrowRight' }));
  if (p.tabs[0].attrs['aria-selected'] !== 'true') fail('ArrowRight did not wrap');
  p.doc.activeElement = p.tabs[0];
  p.tablist.listeners.keydown[0](ev('keydown', { key: 'ArrowLeft' }));
  if (p.tabs[2].attrs['aria-selected'] !== 'true') fail('ArrowLeft did not wrap');
  p.tablist.listeners.keydown[0](ev('keydown', { key: 'Home' }));
  p.doc.activeElement = p.tabs[2];
  p.tablist.listeners.keydown[0](ev('keydown', { key: 'Home' }));
  if (p.tabs[0].attrs['aria-selected'] !== 'true') fail('Home did not select the first tab');
  // Any other key does nothing (no input is ever synthesised).
  e = ev('keydown', { key: 'a' });
  p.tablist.listeners.keydown[0](e);
  if (e.defaultPrevented) fail('a letter key was swallowed by the tab list');
  // A click selects.
  p.tablist.listeners.click[0](ev('click', { target: node({ closest: { '[role="tab"]': p.tabs[1] } }) }));
  if (p.tabs[1].attrs['aria-selected'] !== 'true') fail('a click did not select the tab');
});

const DRAWER = { '(max-width: 1279px)': true };

scenario('right pane as a drawer: modal semantics, focus in, Escape and scrim out, never stored', () => {
  const p = sessionPage({ media: DRAWER, store: { 'minion-pane': 'open' } });
  if (p.toggle.hidden || p.close.hidden) fail('the pane controls were not shown');
  // A desktop preference of "open" must not put a full-screen drawer over a narrow window on load.
  if (p.pane.classList.contains('open') || p.toggle.attrs['aria-expanded'] !== 'false' || p.paneScrim.classList.contains('open')) fail('the drawer opened on load because of the stored desktop preference');
  p.toggle.listeners.click[0]();
  if (!p.pane.classList.contains('open') || p.toggle.attrs['aria-expanded'] !== 'true') fail('Details did not open the pane');
  if (p.pane.attrs.role !== 'dialog' || p.pane.attrs['aria-modal'] !== 'true') fail('the open drawer is not a modal dialog for assistive technology');
  if (!p.paneScrim.classList.contains('open')) fail('the drawer has no scrim');
  if (p.close.focused !== 1 || p.doc.activeElement !== p.close) fail('the focus did not move into the drawer');
  if (p.store['minion-pane'] !== 'open') fail('a drawer changed the stored desktop preference: ' + p.store['minion-pane']);
  // Escape closes it, returns the focus to Details and is not forwarded to anything else.
  let e = ev('keydown', { key: 'Escape' });
  p.fire('keydown', e);
  if (!e.defaultPrevented) fail('Escape in the drawer was not handled here');
  if (p.pane.classList.contains('open') || p.doc.activeElement !== p.toggle || p.toggle.attrs['aria-expanded'] !== 'false') fail('Escape did not close the drawer and return the focus to Details');
  if (p.pane.attrs.role !== undefined || p.pane.attrs['aria-modal'] !== undefined || p.paneScrim.classList.contains('open')) fail('a closed drawer kept its dialog role or scrim');
  // The scrim and the Close button also close it and return the focus.
  p.toggle.listeners.click[0]();
  p.paneScrim.listeners.click[0]();
  if (p.pane.classList.contains('open') || p.doc.activeElement !== p.toggle) fail('the scrim did not close the drawer');
  p.toggle.listeners.click[0]();
  p.close.listeners.click[0]();
  if (p.pane.classList.contains('open') || p.doc.activeElement !== p.toggle) fail('Close did not close the drawer');
  // Nothing of the session is touched or sent by opening and closing: the draft stays, no request, no reload.
  if (p.draft.value !== 'unsent draft') fail('the draft changed');
  if (p.win.sent || p.win.reloads) fail('the drawer sent a request or reloaded the page');
});

scenario('right pane as a drawer: Tab stays inside it', () => {
  const p = sessionPage({ media: DRAWER });
  p.toggle.listeners.click[0]();
  // The first and last focusable controls are the visible ones (a hidden control and a tabindex -1 tab are skipped).
  p.doc.activeElement = p.paneLast;
  let e = ev('keydown', { key: 'Tab' });
  p.fire('keydown', e);
  if (!e.defaultPrevented || p.doc.activeElement !== p.close) fail('Tab on the last control did not wrap to the first');
  p.doc.activeElement = p.close;
  e = ev('keydown', { key: 'Tab', shiftKey: true });
  p.fire('keydown', e);
  if (!e.defaultPrevented || p.doc.activeElement !== p.paneLast) fail('Shift+Tab on the first control did not wrap to the last');
  // A control inside the drawer moves normally.
  p.doc.activeElement = p.paneLink;
  e = ev('keydown', { key: 'Tab' });
  p.fire('keydown', e);
  if (e.defaultPrevented) fail('Tab between inner controls was taken');
  // Focus that is somewhere behind the drawer is pulled back in.
  p.doc.activeElement = p.toggle;
  e = ev('keydown', { key: 'Tab' });
  p.fire('keydown', e);
  if (!e.defaultPrevented || p.doc.activeElement !== p.close) fail('Tab from behind the drawer did not return into it');
});

scenario('right pane as a drawer: a dialog or the open Menu has Escape first', () => {
  const d = sessionPage({ media: DRAWER, dialogOpen: true });
  d.toggle.listeners.click[0]();
  let e = ev('keydown', { key: 'Escape' });
  d.fire('keydown', e);
  if (!d.pane.classList.contains('open') || e.defaultPrevented) fail('Escape was taken from an open dialog');
  const menu = node({}); menu.classList.add('open');
  const m = sessionPage({ media: DRAWER, rail: menu });
  m.toggle.listeners.click[0]();
  e = ev('keydown', { key: 'Escape' });
  m.fire('keydown', e);
  if (!m.pane.classList.contains('open')) fail('Escape closed the pane while the Menu was open');
});

scenario('right pane docked (wide): a stored preference, no modal semantics, Escape untouched', () => {
  const w = sessionPage({ media: {}, store: { 'minion-pane': 'open' } });
  if (!w.pane.classList.contains('open') || !w.grid.classList.contains('pane-open')) fail('the stored open state was not applied');
  if (w.pane.attrs.role !== undefined || w.pane.attrs['aria-modal'] !== undefined || w.paneScrim.classList.contains('open')) fail('a docked pane is not a modal dialog');
  let e = ev('keydown', { key: 'Escape' });
  w.fire('keydown', e);
  if (!w.pane.classList.contains('open') || e.defaultPrevented) fail('Escape closed a docked pane');
  e = ev('keydown', { key: 'Tab' });
  w.fire('keydown', e);
  if (e.defaultPrevented) fail('a docked pane trapped Tab');
  // The toggle writes the preference; the focus follows only when the pane closes.
  w.toggle.listeners.click[0]();
  if (w.pane.classList.contains('open') || w.store['minion-pane'] !== 'closed' || w.doc.activeElement !== w.toggle) fail('the toggle did not close the docked pane and store the preference');
  w.toggle.listeners.click[0]();
  if (!w.pane.classList.contains('open') || w.store['minion-pane'] !== 'open') fail('the toggle did not reopen the docked pane');
  if (w.doc.activeElement === w.close) fail('opening a docked pane stole the focus');
});

scenario('right pane: crossing the breakpoint turns a drawer into the preference and back', () => {
  const media = { '(max-width: 1279px)': true };
  const p = sessionPage({ media, store: { 'minion-pane': 'open' } });
  p.toggle.listeners.click[0]();
  if (!p.pane.classList.contains('open') || p.pane.attrs.role !== 'dialog') fail('the drawer did not open');
  // The window grows to desktop width: the pane is docked and follows the stored preference (open).
  media['(max-width: 1279px)'] = false;
  p.win.mediaListener();
  if (!p.pane.classList.contains('open') || p.pane.attrs.role !== undefined || p.paneScrim.classList.contains('open')) fail('the pane did not become the docked, stored-open pane');
  // The window shrinks again: an open docked pane becomes a closed drawer, and the focus inside it returns to Details.
  p.doc.activeElement = p.paneLink;
  media['(max-width: 1279px)'] = true;
  p.win.mediaListener();
  if (p.pane.classList.contains('open') || p.doc.activeElement !== p.toggle || p.paneScrim.classList.contains('open')) fail('the docked pane did not close into a drawer with the focus on Details');
});

scenario('Send anyway asks first, with Cancel, and only its confirm button sends', () => {
  const p = sessionPage();
  const dialog = node({ tag: 'dialog' });
  const form = node({ tag: 'form', q: { dialog: [dialog] } });
  const open = node({ tag: 'button', closest: { 'form[data-force] > button': null } });
  open.form = form;
  open.closest = (sel) => (sel === 'form[data-force] > button' ? open : null);
  open.parentNode = form;
  let e = ev('click', { target: open });
  p.fire('click', e);
  if (!e.defaultPrevented || !dialog.modal) fail('the first click sent instead of asking');
  // Cancel closes the dialog and sends nothing.
  const cancel = node({ tag: 'button' });
  cancel.closest = (sel) => (sel === '[data-dialog-cancel]' ? cancel : sel === 'dialog' ? dialog : null);
  e = ev('click', { target: cancel });
  p.fire('click', e);
  if (dialog.modal) fail('Cancel did not close the dialog');
  // The confirm button submits the form; the dialog closes with the submit.
  dialog.showModal();
  const submitForm = node({ tag: 'form', q: { 'dialog[open]': [dialog] } });
  p.fire('submit', ev('submit', { target: submitForm }));
  if (dialog.modal) fail('the dialog stayed open after the confirm button');
  // Without dialog support the button sends as before.
  const plain = node({ tag: 'button' });
  const plainForm = node({ tag: 'form', q: { dialog: [node({ tag: 'dialog' })] } });
  delete plainForm.querySelector('dialog').showModal;
  plain.form = plainForm;
  plain.closest = (sel) => (sel === 'form[data-force] > button' ? plain : null);
  e = ev('click', { target: plain });
  p.fire('click', e);
  if (e.defaultPrevented) fail('a browser without <dialog> could not send');
});

// The acknowledgement of a task save names the task THAT REQUEST posted, per request
// (its own request config, also when the form that issued it was replaced by a result and
// its events never reach document.body). The field's current value is never used: a forced
// save of A while the user edited the field to B must not mark B as saved.
const TASKPATH = '/s/pi-1/minion-1/task';
function taskRequest(p, params, o) {
  o = o || {};
  const elt = node({ tag: 'form' });
  const cfg = { verb: 'post', path: TASKPATH, elt };
  if (!o.noPayload) { cfg.formData = { get: (k) => (k in params ? params[k] : null) }; }
  p.fire('htmx:configRequest', ev('htmx:configRequest', { detail: cfg }));
  p.fire('htmx:beforeSend', ev('htmx:beforeSend', { detail: { requestConfig: cfg, elt } }));
  return {
    cfg, elt,
    // saved: the response carries minion-task-saved (the HX-Trigger header); detached: the form is gone.
    respond(saved, opt) {
      opt = opt || {};
      const header = saved ? JSON.stringify({ 'minion-screen-refresh': true, 'minion-task-saved': true }) : JSON.stringify({ 'minion-screen-refresh': true });
      const detail = { xhr: { status: 200, getResponseHeader: (h) => (h === 'HX-Trigger' ? header : null) }, requestConfig: cfg, elt };
      elt.dispatchEvent(ev('htmx:beforeOnLoad', { detail }));
      if (!opt.detached) { p.fire('htmx:beforeOnLoad', ev('htmx:beforeOnLoad', { detail })); } // bubbles to the body
      elt.dispatchEvent(ev('htmx:afterRequest', { detail }));
      if (!opt.detached) { p.fire('htmx:afterRequest', ev('htmx:afterRequest', { detail })); }
    },
  };
}

scenario('task shows unsaved changes until it was saved', () => {
  const p = sessionPage();
  p.task.value = 'new task';
  p.task.listeners.input[0]();
  if (p.taskState.hidden) fail('an edited task is not marked as unsaved');
  const r = taskRequest(p, { task: 'new task' });
  p.task.value = 'new task, edited again while saving';
  p.task.listeners.input[0]();
  r.respond(true);
  if (p.task.attrs['data-saved'] !== 'new task') fail('the saved state took the text typed after the request: ' + p.task.attrs['data-saved']);
  if (p.taskState.hidden) fail('the later edit is not marked as unsaved');
  p.task.value = 'new task';
  p.task.listeners.input[0]();
  if (!p.taskState.hidden) fail('a saved task is still marked as unsaved');
});

scenario('a forced task save of A never marks the edited field B as saved', () => {
  for (const detached of [false, true]) {
    const p = sessionPage();
    // The user saved A, got a forceable refusal, then edited the field to B; the force form posts A.
    p.task.value = 'task B (edited, unsent)';
    p.task.listeners.input[0]();
    const force = taskRequest(p, { task: 'task A', force: '1' });
    force.respond(true, { detached });
    if (p.task.attrs['data-saved'] !== 'task A') fail('detached=' + detached + ': the acknowledgement did not name the posted task A: ' + JSON.stringify(p.task.attrs['data-saved']));
    if (p.task.value !== 'task B (edited, unsent)') fail('the unsent edit B was changed');
    if (p.taskState.hidden) fail('detached=' + detached + ': B is unsaved but "Unsaved changes" is hidden');
    // One acknowledgement per request: the same response seen by the form and by the body is one.
    p.task.attrs['data-saved'] = 'someone else';
    force.elt.dispatchEvent(ev('htmx:beforeOnLoad', { detail: { xhr: { status: 200, getResponseHeader: () => JSON.stringify({ 'minion-task-saved': true }) }, requestConfig: force.cfg, elt: force.elt } }));
    if (p.task.attrs['data-saved'] !== 'someone else') fail('the same request was acknowledged twice');
    if (((force.elt.listeners['htmx:beforeOnLoad'] || []).length) !== 0) fail('the one-time listeners of the form stayed attached');
  }
  // A refusal (no minion-task-saved) saves nothing; a request whose payload cannot be read saves nothing.
  let p = sessionPage();
  p.task.value = 'B'; p.task.listeners.input[0]();
  taskRequest(p, { task: 'A' }).respond(false);
  if (p.task.attrs['data-saved'] !== 'old task' || p.taskState.hidden) fail('a refused save changed the saved state');
  taskRequest(p, {}, { noPayload: true }).respond(true);
  if (p.task.attrs['data-saved'] !== 'old task') fail('an acknowledgement without a readable payload marked something saved: ' + p.task.attrs['data-saved']);
  // Two requests in flight: each acknowledgement names its own payload.
  p = sessionPage();
  const one = taskRequest(p, { task: 'one' });
  const two = taskRequest(p, { task: 'two' });
  two.respond(true, { detached: true });
  one.respond(true, { detached: true });
  if (p.task.attrs['data-saved'] !== 'one') fail('the acknowledgement of one request used another request\'s payload');
});

scenario('a dialog option and a typed answer exclude each other', () => {
  const radios = [node({ tag: 'input', checked: true }), node({ tag: 'input' })];
  radios.forEach((r) => { r.type = 'radio'; });
  const form = node({ tag: 'form', attrs: { 'data-answer-form': '' } });
  const box = node({ id: 'answer-choice', tag: 'input', value: '' });
  form.q = { 'input[type="radio"]': radios, '#answer-choice': [box] };
  form.querySelectorAll = (sel) => (sel === 'input[type="radio"]' ? radios : []);
  form.querySelector = (sel) => (sel === '#answer-choice' ? box : null);
  box.form = form;
  radios.forEach((r) => { r.form = form; });
  const p = sessionPage();
  // Typing clears the chosen option.
  box.value = 'something else';
  p.fire('input', ev('input', { target: box }));
  if (radios.some((r) => r.checked)) fail('typing an answer left an option chosen');
  // Choosing an option clears the typed text and counts it as an edit.
  radios[1].checked = true;
  let edits = 0;
  p.doc.body.addEventListener('input', () => { edits++; });
  box.dispatchEvent = (e) => { edits++; return true; };
  p.fire('change', ev('change', { target: radios[1] }));
  if (box.value !== '' || edits < 1) fail('choosing an option did not clear the typed text as an edit');
  // Empty text and other fields change nothing.
  box.value = '';
  radios[0].checked = true;
  p.fire('input', ev('input', { target: box }));
  if (!radios[0].checked) fail('an empty answer box cleared the chosen option');
});

for (const [name, fn] of scenarios) {
  try { fn(); } catch (e) { fail(name + ': ' + (e && e.stack || e)); }
}
console.log('ok');
