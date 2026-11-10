// Runs static/forms.js against a minimal DOM stub and prints the JSON body the
// form would POST. The form's data-* attributes and field values come from the
// caller, so the payload is exactly what the browser would send: nothing is
// injected on the way.
// Usage: node forms_payload_test.js <path to forms.js> '<{"dataset":{...},"fields":{...}}>'
'use strict';
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const spec = JSON.parse(process.argv[3]);

const button = { disabled: false };
const form = { dataset: spec.dataset,
  querySelector() { return null; },
  querySelectorAll() { return [button]; } };
let listener;
const document = {
  querySelector() { return null; },
  getElementById() { return null; },
  createElement() { return {}; },
  addEventListener(name, f) { if (name === 'submit') { listener = f; } },
};
function HTMLFormElement() {}
Object.setPrototypeOf(form, HTMLFormElement.prototype);
function FormData() { return { forEach(cb) { for (const k of Object.keys(spec.fields)) { cb(spec.fields[k], k); } } }; }
const window = { location: { assign() {}, reload() {} } };
const fetch = async (url, init) => {
  console.log(init.body);
  return { ok: true, status: 200, json: async () => ({}) };
};

new Function('document', 'window', 'HTMLFormElement', 'FormData', 'fetch', 'crypto', src)(
  document, window, HTMLFormElement, FormData, fetch, require('crypto').webcrypto);
listener({ target: form, preventDefault() {} });
