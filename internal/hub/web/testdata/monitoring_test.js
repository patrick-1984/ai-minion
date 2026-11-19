// Runs static/monitoring.js against a DOM stub: the banner follows the event,
// the version rides on every htmx request, a refusal text moves the version and
// a page rendered without a banner gets one.
// Usage: node monitoring_test.js <path to monitoring.js>
'use strict';
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const fail = (m) => { console.error('FAIL: ' + m); process.exit(1); };

function el(tag) {
  const e = { tagName: tag, children: [], attrs: {}, id: '', className: '', textContent: '', parentNode: null,
    setAttribute(k, v) { this.attrs[k] = String(v); }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; }, appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
    insertBefore(c, ref) { c.parentNode = this; this.children.splice(this.children.indexOf(ref), 0, c); return c; },
    querySelector(sel) { return sel === 'span' ? this.children.find((c) => c.tagName === 'span') || null : null; } };
  return e;
}
function build(withBanner) {
  const root = el('div');
  const slot = el('div'); slot.id = 'banner-slot'; root.appendChild(slot);
  let banner = null;
  if (withBanner) {
    banner = el('div'); banner.id = 'monitor-banner'; banner.setAttribute('data-monitor-version', '2');
    const span = el('span'); span.textContent = 'Sessions are monitored: actions, input off (policy v2)'; banner.appendChild(span);
    root.insertBefore(banner, slot);
  }
  const listeners = {};
  const document = {
    getElementById(id) { return root.children.find((c) => c.id === id) || null; },
    createElement: el,
    addEventListener(n, f) { listeners[n] = f; },
  };
  const window = {};
  new Function('document', 'window', src)(document, window);
  return { document, window, listeners, root };
}

// 1. A page with a banner: the header carries its version; an event updates it.
let p = build(true);
let ev = { detail: { headers: {} } };
p.listeners['htmx:configRequest'](ev);
if (ev.detail.headers['Minion-Monitoring-Seen'] !== '2') fail('header = ' + JSON.stringify(ev.detail.headers));
p.window.minionMonitoring.update({ version: 5, notice: 'Sessions are monitored: output, input on (policy v5)' });
let b = p.document.getElementById('monitor-banner');
if (b.getAttribute('data-monitor-version') !== '5' || b.querySelector('span').textContent.indexOf('policy v5') < 0 || 'hidden' in b.attrs) fail('banner not updated');
ev = { detail: { headers: {} } };
p.listeners['htmx:configRequest'](ev);
if (ev.detail.headers['Minion-Monitoring-Seen'] !== '5') fail('the next request must carry the new version');

// 2. Monitoring off: the banner is hidden, the version still moves.
p.window.minionMonitoring.update({ version: 6, notice: '' });
b = p.document.getElementById('monitor-banner');
if (!('hidden' in b.attrs) || b.getAttribute('data-monitor-version') !== '6') fail('off must hide the banner');

// 3. A refusal text moves the version and shows the notice; other text does not.
p = build(true);
p.listeners['htmx:afterRequest']({ detail: { xhr: { responseText: '<p>Sorry: Sessions are monitored: output, input on (policy v9). Check it and repeat the request; nothing was sent.</p>' } } });
b = p.document.getElementById('monitor-banner');
if (b.getAttribute('data-monitor-version') !== '9') fail('refusal version not adopted: ' + b.getAttribute('data-monitor-version'));
p.listeners['htmx:afterRequest']({ detail: { xhr: { responseText: '<p>Saved.</p>' } } });
if (b.getAttribute('data-monitor-version') !== '9') fail('an unrelated answer changed the version');

// 4. A page rendered without a banner (monitoring never was on) gets one.
p = build(false);
ev = { detail: { headers: {} } };
p.listeners['htmx:configRequest'](ev);
if (ev.detail.headers['Minion-Monitoring-Seen'] !== '0') fail('a bannerless page sends 0');
p.window.minionMonitoring.update({ version: 1, notice: 'Sessions are monitored: actions, input off (policy v1)' });
b = p.document.getElementById('monitor-banner');
if (!b || b.getAttribute('data-monitor-version') !== '1' || 'hidden' in b.attrs) fail('banner not created');

// 5. Garbage is ignored.
if (p.window.minionMonitoring.update({ version: 'x' }) !== false) fail('a bad version must be ignored');
console.log('ok');
