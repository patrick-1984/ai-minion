// forms.js submits forms that carry data-json-action as JSON, with the CSRF
// header, and shows the error message of the AD-18 envelope on failure.
//
//   data-json-action  URL that receives the POST
//   data-next         where to go on success (a path on this Hub); default: reload
//   data-match        "a,b": the two fields must be equal; b is not sent
//   data-request-id   present (any value): the body gets a fresh "request_id",
//                     a UUIDv7, as every write operation requires
//
// Forms without a [data-form-error] element report errors in #flash-area.
// A response with user.must_change_password sends the browser to
// /account/password, whatever data-next says.
(function () {
  'use strict';

  function csrfToken() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.getAttribute('content') : '';
  }

  // sameOriginPath accepts only "/path" (no scheme, no "//", no backslash), so
  // that a crafted next value cannot send the browser to another site.
  function sameOriginPath(p) {
    return typeof p === 'string' && p.charAt(0) === '/' && p.charAt(1) !== '/' && p.indexOf('\\') < 0;
  }

  // showError writes the message into the form's own [data-form-error]
  // element. A form without one (sign out, banner actions) reports into the
  // flash area of the layout instead, so that a failure is never silent.
  function showError(form, message) {
    var el = form.querySelector('[data-form-error]');
    if (!el) {
      el = sharedError();
      if (!el) { return; }
    }
    el.textContent = message;
    el.hidden = !message;
  }

  function sharedError() {
    var el = document.getElementById('form-error-flash');
    if (el) { return el; }
    var area = document.getElementById('flash-area');
    if (!area) { return null; }
    el = document.createElement('div');
    el.id = 'form-error-flash';
    el.className = 'flash flash-danger';
    el.setAttribute('role', 'alert');
    el.hidden = true;
    area.appendChild(el);
    return el;
  }

  // uuidv7 returns a canonical lowercase UUIDv7 (RFC 9562): 48 bits of Unix
  // time in milliseconds, then random bits with the version and variant set.
  function uuidv7() {
    var b = new Uint8Array(16);
    crypto.getRandomValues(b);
    var ms = Date.now();
    for (var i = 5; i >= 0; i--) {
      b[i] = ms % 256;
      ms = Math.floor(ms / 256);
    }
    b[6] = (b[6] & 0x0f) | 0x70;
    b[8] = (b[8] & 0x3f) | 0x80;
    var h = '';
    for (var j = 0; j < 16; j++) {
      if (j === 4 || j === 6 || j === 8 || j === 10) { h += '-'; }
      h += (b[j] < 16 ? '0' : '') + b[j].toString(16);
    }
    return h;
  }

  function setBusy(form, busy) {
    var buttons = form.querySelectorAll('button[type="submit"]');
    for (var i = 0; i < buttons.length; i++) { buttons[i].disabled = busy; }
  }

  async function submit(form) {
    var body = {};
    new FormData(form).forEach(function (v, k) { body[k] = v; });
    var match = form.dataset.match;
    if (match) {
      var pair = match.split(',');
      if (body[pair[0]] !== body[pair[1]]) {
        showError(form, 'The two passwords do not match.');
        return;
      }
      delete body[pair[1]];
    }
    if (form.dataset.requestId !== undefined) { body.request_id = uuidv7(); }
    showError(form, '');
    setBusy(form, true);
    try {
      var resp = await fetch(form.dataset.jsonAction, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'Accept': 'application/json', 'X-Minion-CSRF': csrfToken() },
        body: JSON.stringify(body)
      });
      var data = null;
      try { data = await resp.json(); } catch (e) { data = null; }
      if (!resp.ok) {
        var msg = data && data.error && data.error.message ? data.error.message : 'The request failed (' + resp.status + ').';
        showError(form, msg);
        setBusy(form, false);
        return;
      }
      if (data && data.user && data.user.must_change_password) {
        window.location.assign('/account/password');
        return;
      }
      var next = form.dataset.next;
      if (sameOriginPath(next)) { window.location.assign(next); } else { window.location.reload(); }
    } catch (e) {
      showError(form, 'The Hub could not be reached.');
      setBusy(form, false);
    }
  }

  document.addEventListener('submit', function (ev) {
    var form = ev.target;
    if (!(form instanceof HTMLFormElement) || !form.dataset.jsonAction) { return; }
    ev.preventDefault();
    submit(form);
  });
})();
