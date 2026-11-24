// Runs static/forms.js against a minimal DOM stub: a failed sign-out request
// (HTTP 500) must show an error in the flash area and re-enable the button.
// Usage: node forms_failed_test.js <path to forms.js>
'use strict';
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');

function el(tag) {
  return { tagName: tag, children: [], attrs: {}, hidden: false, textContent: '', className: '', id: '',
    setAttribute(k, v) { this.attrs[k] = v; }, appendChild(c) { this.children.push(c); return c; } };
}
const area = el('div'); area.id = 'flash-area';
const button = el('button'); button.disabled = false;
const form = { dataset: { jsonAction: '/auth/logout', next: '/login' },
  querySelector() { return null; },
  querySelectorAll() { return [button]; } };
let listener;
const document = {
  querySelector() { return null; },
  getElementById(id) { return id === 'flash-area' ? area : area.children.find(c => c.id === id) || null; },
  createElement: el,
  addEventListener(name, f) { if (name === 'submit') { listener = f; } },
};
function HTMLFormElement() {}
Object.setPrototypeOf(form, HTMLFormElement.prototype);
function FormData() { return { forEach() {} }; }
let navigated = null;
const window = { location: { assign(u) { navigated = u; }, reload() { navigated = 'reload'; } } };
const fetch = async () => ({ ok: false, status: 500, json: async () => ({ error: { message: 'session revoke failed' } }) });

new Function('document', 'window', 'HTMLFormElement', 'FormData', 'fetch', src)(document, window, HTMLFormElement, FormData, fetch);
let prevented = false;
listener({ target: form, preventDefault() { prevented = true; } });
setTimeout(() => {
  const e = area.children[0];
  const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };
  if (!prevented) fail('submit not intercepted');
  if (!e || e.hidden || e.textContent !== 'session revoke failed') fail('no visible error in flash area: ' + JSON.stringify(e));
  if (button.disabled) fail('button still disabled');
  if (navigated) fail('navigated after failure: ' + navigated);
  idleRun();
}, 20);

// A99: the quiet 401 of a session whose idle lifetime ran out is not an error of the form: the page says it is
// paused and reloads on the next click (that page load signs in again). An htmx request gets the same.
function idleRun() {
  const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };
  const area2 = el('div'); area2.id = 'flash-area';
  const button2 = el('button');
  const form2 = { dataset: { jsonAction: '/api/v1/ops/x' }, querySelector() { return null; }, querySelectorAll() { return [button2]; },
    closest() { return null; } };
  Object.setPrototypeOf(form2, HTMLFormElement.prototype);
  const l = {};
  const doc2 = {
    querySelector() { return null; },
    getElementById(id) { return id === 'flash-area' ? area2 : area2.children.find(c => c.id === id) || null; },
    createElement: el,
    addEventListener(n, f) { (l[n] = l[n] || []).push(f); },
    removeEventListener(n, f) { l[n] = (l[n] || []).filter((x) => x !== f); },
  };
  let reloads = 0;
  const win2 = { location: { assign() { fail('navigated'); }, reload() { reloads++; } } };
  const idleResp = async () => ({ ok: false, status: 401, headers: { get: (h) => (h === 'X-Minion-Session' ? 'idle' : null) },
    json: async () => ({ error: { message: 'the session is idle; reload the page to continue' } }) });
  new Function('document', 'window', 'HTMLFormElement', 'FormData', 'fetch', src)(doc2, win2, HTMLFormElement, FormData, idleResp);
  l.submit[0]({ target: form2, preventDefault() {} });
  setTimeout(() => {
    const note = area2.children.find((c) => c.attrs['data-session-paused'] !== undefined);
    if (!note || !/Session paused/.test(note.textContent)) fail('no paused note: ' + JSON.stringify(area2.children));
    if (area2.children.some((c) => c.id === 'form-error-flash' && !c.hidden && c.textContent)) fail('the idle 401 was shown as an error');
    if (button2.disabled) fail('button still disabled');
    if (reloads) fail('reloaded without a click');
    // An htmx 401 of the same kind does not add a second note.
    l['htmx:responseError'][0]({ detail: { elt: { closest() { return null; } }, xhr: { status: 401, getResponseHeader: (h) => (h === 'X-Minion-Session' ? 'idle' : null) } } });
    if (area2.children.filter((c) => c.attrs['data-session-paused'] !== undefined).length !== 1) fail('a second note');
    let prevented = false;
    (l.click || []).slice().forEach((f) => f({ preventDefault() { prevented = true; }, stopPropagation() {} }));
    if (reloads !== 1 || !prevented) fail('the click did not reload: ' + reloads);
    console.log('ok');
  }, 20);
}
