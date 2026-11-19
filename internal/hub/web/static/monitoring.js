// Monitoring notice (A49 T16a). The Hub says in the page which monitoring policy
// it was rendered with (data-monitor-version of #monitor-banner). This script
//   - sends that version with every htmx request (Minion-Monitoring-Seen), so a page that is
//     older than a stricter policy is refused once with the notice;
//   - updates the banner and the version when the event stream says the policy changed;
//   - adopts the version named in a refusal, so the retry goes through. A draft is never
//     touched here: the form keeps its fields when its request is refused.
(function () {
  'use strict';
  var HEADER = 'Minion-Monitoring-Seen';

  var KEPT = null; // the version of a page that rendered no banner (monitoring never was on)
  function banner() { return document.getElementById('monitor-banner'); }
  // ensure creates the banner when the page was rendered without one (monitoring was off and never had a version).
  function ensure() {
    var b = banner();
    if (b) { return b; }
    var slot = document.getElementById('banner-slot');
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
    b.setAttribute('data-monitor-version', v);
    var text = typeof p.notice === 'string' ? p.notice : '';
    var span = b.querySelector('span');
    if (span) { span.textContent = text; }
    if (text) { b.removeAttribute('hidden'); } else { b.setAttribute('hidden', ''); }
    return true;
  }

  // adopt reads the version of a refusal ("... (policy v3). Check it ...") out of a response text.
  function adopt(text) {
    var m = /Sessions are monitored: [^()]*\(policy v([0-9]+)\)/.exec(text || '');
    if (!m) { return false; }
    var b = ensure();
    if (!b) { return false; }
    var span = b.querySelector('span');
    var line = /Sessions are monitored: [^()]*\(policy v[0-9]+\)/.exec(text)[0];
    return update({ version: m[1], notice: line }) && !!span;
  }

  document.addEventListener('htmx:configRequest', function (ev) {
    if (ev.detail && ev.detail.headers) { ev.detail.headers[HEADER] = version(); }
  });
  var after = function (ev) {
    var x = ev.detail && ev.detail.xhr;
    if (x && typeof x.responseText === 'string') { adopt(x.responseText); }
  };
  document.addEventListener('htmx:afterRequest', after);

  window.minionMonitoring = { update: update, version: version, adopt: adopt };
})();
