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
  console.log('ok');
}, 20);
