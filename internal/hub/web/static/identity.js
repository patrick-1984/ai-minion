// identity.js drives the Hub identity page: it re-authenticates the session,
// sends each identity operation as a JSON POST of the browser to the REST
// operation, and clears every passphrase field after every submission. It
// keeps no passphrase anywhere: values are read from the fields at submit time,
// put in the request body and dropped. Nothing is written to the console, the
// URL, the history or any storage.
(function () {
  'use strict';

  function csrfToken() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.getAttribute('content') : '';
  }

  function show(form, message) {
    var el = form.querySelector('[data-form-error]');
    if (!el) { return; }
    el.textContent = message;
    el.hidden = !message;
  }

  function result(message) {
    var el = document.getElementById('id-result');
    if (!el) { return; }
    el.textContent = message;
    el.hidden = !message;
  }

  function clearSecrets(form) {
    var fields = form.querySelectorAll('input[type="password"], input[name="word"]');
    for (var i = 0; i < fields.length; i++) {
      fields[i].value = '';
      fields[i].dispatchEvent(new Event('input'));
    }
  }

  // Count the characters of a new passphrase as Unicode code points, as the
  // Hub does. The value is only measured, never stored or sent anywhere.
  function codePoints(value) { return Array.from(value).length; }

  function bindCounters() {
    var inputs = document.querySelectorAll('input[data-count-passphrase]');
    for (var i = 0; i < inputs.length; i++) {
      (function (input) {
        var holder = input.closest('section');
        var out = holder ? holder.querySelector('[data-passphrase-count]') : null;
        if (!out) { return; }
        function update() {
          var n = codePoints(input.value);
          out.textContent = n === 0 ? '' : n + ' of 14 characters' + (n >= 14 ? ', enough.' : ' so far.');
        }
        input.addEventListener('input', update);
        update();
      })(inputs[i]);
    }
  }

  function setBusy(form, busy) {
    var buttons = form.querySelectorAll('button[type="submit"]');
    for (var i = 0; i < buttons.length; i++) { buttons[i].disabled = busy; }
  }

  async function post(url, body) {
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json', 'X-Minion-CSRF': csrfToken() },
      body: JSON.stringify(body)
    });
  }

  function errorText(resp, data) {
    var msg = data && data.error && data.error.message ? data.error.message : 'The request failed (' + resp.status + ').';
    var details = data && data.error && data.error.details;
    if (details && details.retry_at) {
      msg += ' Try again after ' + details.retry_at.replace('T', ' ').replace(/\.\d+Z$/, ' UTC') + '.';
    }
    return msg;
  }

  async function reauth(form) {
    show(form, '');
    setBusy(form, true);
    try {
      var kind = form.dataset.reauth;
      if (kind === 'oidc') {
        var r1 = await post('/auth/reauth/oidc', {});
        var d1 = null;
        try { d1 = await r1.json(); } catch (e) { d1 = null; }
        if (!r1.ok || !d1 || !d1.redirect_url) {
          show(form, errorText(r1, d1));
          setBusy(form, false);
          return;
        }
        window.location.assign(d1.redirect_url);
        return;
      }
      var pw = form.querySelector('input[name="password"]');
      var body = { password: pw ? pw.value : '' };
      var resp = await post('/auth/reauth', body);
      body = null;
      clearSecrets(form);
      var data = null;
      try { data = await resp.json(); } catch (e) { data = null; }
      if (!resp.ok) {
        show(form, errorText(resp, data));
        setBusy(form, false);
        return;
      }
      window.location.assign('/admin/identity?reauth=ok');
    } catch (e) {
      clearSecrets(form);
      show(form, 'The Hub could not be reached.');
      setBusy(form, false);
    }
  }

  async function operate(form) {
    var op = form.dataset.identityOp;
    var body = {};
    new FormData(form).forEach(function (v, k) {
      if (k === 'validity_days' || k === 'serial_floor') {
        if (v !== '') { body[k] = Number(v); }
      } else {
        body[k] = v;
      }
    });
    show(form, '');
    result('');
    setBusy(form, true);
    try {
      var resp = await post('/api/v1/ops/' + op, body);
      body = null;
      clearSecrets(form);
      var data = null;
      try { data = await resp.json(); } catch (e) { data = null; }
      if (!resp.ok) {
        var code = data && data.error ? data.error.code : '';
        if (code === 'reauth_required') {
          show(form, 'Confirm it is you first (above), then repeat. ' + errorText(resp, data));
        } else {
          show(form, errorText(resp, data));
        }
        setBusy(form, false);
        if (code === 'rate_limited') { window.setTimeout(function () { window.location.reload(); }, 1500); }
        return;
      }
      result('Done. The change is durable.');
      window.setTimeout(function () { window.location.reload(); }, 1200);
    } catch (e) {
      clearSecrets(form);
      show(form, 'The Hub could not be reached.');
      setBusy(form, false);
    }
  }

  document.addEventListener('submit', function (ev) {
    var form = ev.target;
    if (!(form instanceof HTMLFormElement)) { return; }
    if (form.dataset.reauth) { ev.preventDefault(); reauth(form); return; }
    if (form.dataset.identityOp) { ev.preventDefault(); operate(form); }
  });

  bindCounters();

  // The lock countdown: whole seconds left, until the page can be used again.
  var lock = document.getElementById('id-lock');
  if (lock) {
    var until = Number(lock.dataset.until) * 1000;
    var left = document.getElementById('id-lock-left');
    var timer = window.setInterval(function () {
      var s = Math.max(0, Math.ceil((until - Date.now()) / 1000));
      if (left) { left.textContent = String(s); }
      if (s === 0) { window.clearInterval(timer); window.location.reload(); }
    }, 1000);
  }
})();
