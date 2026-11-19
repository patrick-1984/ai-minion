// Monitoring notice (A49 T16a). The Hub says in the page which monitoring policy
// it was rendered with (data-monitor-version of #monitor-banner). This script
//   - sends that version with every htmx request (Minion-Monitoring-Seen), so a page that is
//     older than a stricter policy is refused once with the notice;
//   - updates the banner and the version when the event stream says the policy changed;
//   - adopts the version named in the Minion-Monitoring header of a refusal (a response header only the Hub sets), so the retry
//     goes through. A draft is never touched here: the form keeps its fields when its request is refused.
(function () {
  'use strict';
  var HEADER = 'Minion-Monitoring-Seen';

  var KEPT = null; // the version of a page that rendered no banner (monitoring never was on)
  function banner() { return document.getElementById('monitor-banner'); }
  // ensure creates the banner when the page was rendered without one (monitoring was off and never had a version).
  function ensure() {
    var b = banner();
    if (b) { return b; }
    var slot = document.getElementById('banner-slot') || document.getElementById('content');
    if (!slot || !slot.parentNode) { return null; }
    b = document.createElement('div');
    b.id = 'monitor-banner';
    b.className = 'banner banner-warning';
    b.setAttribute('role', 'status');
    b.setAttribute('data-monitor-version', KEPT || '0');
    b.appendChild(document.createElement('span'));
    b.setAttribute('hidden', '');
    slot.parentNode.insertBefore(b, slot);
    return b;
  }
  function version() {
    var b = banner();
    return b ? (b.getAttribute('data-monitor-version') || '0') : '0';
  }

  // update applies a policy: {version, notice} (a text of "" hides the banner).
  function update(p) {
    var b = ensure();
    if (!b || !p) { return false; }
    var v = String(p.version === undefined ? '' : p.version);
    if (v === '' || !/^[0-9]+$/.test(v)) { return false; }
    // An older policy never replaces a newer one (events can arrive late).
    if (Number(v) < Number(b.getAttribute('data-monitor-version') || '0')) { return false; }
    b.setAttribute('data-monitor-version', v);
    var text = typeof p.notice === 'string' ? p.notice : '';
    var span = b.querySelector('span');
    if (span) { span.textContent = text; }
    if (text) { b.removeAttribute('hidden'); } else { b.setAttribute('hidden', ''); }
    return true;
  }

  // fromHeader applies a Minion-Monitoring response header ("v=3; level=output; input=on; traffic=payload"),
  // which only the Hub's own refusal carries. The text of a response is never read: session content can say anything.
  function fromHeader(h) {
    if (typeof h !== 'string' || h === '') { return false; }
    var f = {};
    h.split(';').forEach(function (part) {
      var kv = part.trim().split('=');
      if (kv.length === 2) { f[kv[0]] = kv[1]; }
    });
    if (!/^[0-9]+$/.test(f.v || '')) { return false; }
    var parts = [];
    if (f.level === 'actions' || f.level === 'output') { parts.push(f.level + ', input ' + (f.input === 'on' ? 'on' : 'off')); }
    if (f.traffic === 'payload') { parts.push('Worker traffic with frame contents'); }
    var notice = parts.length ? 'Sessions are monitored: ' + parts.join('; ') + ' (policy v' + f.v + ')' : '';
    return update({ version: f.v, notice: notice });
  }

  document.addEventListener('htmx:configRequest', function (ev) {
    if (ev.detail && ev.detail.headers) { ev.detail.headers[HEADER] = version(); }
  });
  var after = function (ev) {
    var x = ev.detail && ev.detail.xhr;
    if (x && typeof x.getResponseHeader === 'function') { fromHeader(x.getResponseHeader('Minion-Monitoring')); }
  };
  document.addEventListener('htmx:afterRequest', after);

  window.minionMonitoring = { update: update, version: version, fromHeader: fromHeader };
})();
