// session-files.js runs the Files view of the session page (files.v1).
//
// Download: the page admits the transfer (POST .../files/pull with the CSRF
// header), then reads the Hub's data leg (GET /files/v1/transfers/<id>/content)
// with the admission's single-use token in the Minion-Leg-Token header (never in
// a URL). A file is written as it arrives: into a file the person picks
// (showSaveFilePicker, where the browser has it) or into memory, saved at the end.
// An interrupted file download can be resumed: a new admission, then Range and
// If-Range with the strong ETag; a 200 instead of 206, or another ETag, starts
// again from the first byte. A folder comes as a tar stream and is saved
// gzip-compressed (.tar.gz).
//
// Upload: one file. The page admits it (POST .../files/push), then sends it with
// XMLHttpRequest (progress and cancel) to the upload route, which adds the
// digests the Worker verifies. A cancelled upload publishes nothing.
(function () {
  'use strict';

  var panel = document.getElementById('files-panel');
  if (!panel) { return; }
  var pullURL = panel.getAttribute('data-files-pull');
  var pushURL = panel.getAttribute('data-files-push');
  var uploadURL = panel.getAttribute('data-files-upload');
  var area = document.getElementById('files-transfers');
  var live = document.getElementById('files-live');

  function csrfToken() {
    var m = document.querySelector('meta[name="csrf-token"]');
    return m ? m.getAttribute('content') : '';
  }

  function say(text) { if (live) { live.textContent = text; } }

  function human(n) {
    var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    var i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (i === 0 ? String(n) : n.toFixed(1)) + ' ' + units[i];
  }

  function errorText(body, fallback) {
    if (body && body.error && body.error.message) { return body.error.message; }
    return fallback;
  }

  // admit posts one admission form and returns the admission.
  function admit(url, fields) {
    var form = new URLSearchParams();
    Object.keys(fields).forEach(function (k) { form.set(k, fields[k]); });
    return fetch(url, {
      method: 'POST', credentials: 'same-origin', cache: 'no-store',
      headers: { 'X-Minion-CSRF': csrfToken(), 'Content-Type': 'application/x-www-form-urlencoded' },
      body: form.toString()
    }).then(function (resp) {
      return resp.json().catch(function () { return null; }).then(function (body) {
        if (!resp.ok || !body || !body.leg_token) {
          throw new Error(errorText(body, 'The transfer was refused.'));
        }
        return body;
      });
    });
  }

  // -- the transfer rows -----------------------------------------------------

  function row(label) {
    var el = document.createElement('div');
    el.className = 'files-transfer';
    var p = document.createElement('p');
    var status = document.createElement('span');
    var name = document.createElement('strong');
    name.textContent = label;
    p.appendChild(name);
    p.appendChild(document.createTextNode(': '));
    p.appendChild(status);
    var bar = document.createElement('progress');
    bar.max = 1;
    bar.value = 0;
    bar.setAttribute('aria-label', 'Progress of ' + label);
    var actions = document.createElement('div');
    actions.className = 'form-actions';
    el.appendChild(p);
    el.appendChild(bar);
    el.appendChild(actions);
    area.appendChild(el);
    var r = {
      el: el, bar: bar,
      status: function (text) { status.textContent = text; },
      progress: function (done, total) {
        if (total > 0) { bar.max = total; bar.value = Math.min(done, total); } else { bar.removeAttribute('value'); }
      },
      button: function (text, fn) {
        var b = document.createElement('button');
        b.type = 'button';
        b.className = 'btn btn-secondary btn-sm';
        b.textContent = text;
        b.addEventListener('click', fn);
        actions.appendChild(b);
        return b;
      },
      clear: function () { while (actions.firstChild) { actions.removeChild(actions.firstChild); } },
      remove: function () { if (el.parentNode) { el.parentNode.removeChild(el); } }
    };
    return r;
  }

  // -- sinks: where the bytes of a download go ------------------------------

  // pickerSink writes into a file the person chose; it must be asked for in the
  // click itself (the browser requires the gesture).
  function pickerSink(name) {
    return window.showSaveFilePicker({ suggestedName: name }).then(function (handle) {
      return handle.createWritable();
    }).then(function (w) {
      return {
        write: function (chunk) { return w.write(chunk); },
        reset: function () { return w.truncate(0).then(function () { return w.seek(0); }); },
        finish: function () { return w.close(); },
        abort: function () { return w.abort(); }
      };
    });
  }

  // memorySink keeps the bytes and saves them through a link at the end.
  function memorySink(name) {
    var parts = [];
    return Promise.resolve({
      write: function (chunk) { parts.push(chunk); return Promise.resolve(); },
      reset: function () { parts = []; return Promise.resolve(); },
      finish: function () {
        var url = URL.createObjectURL(new Blob(parts));
        parts = [];
        var a = document.createElement('a');
        a.href = url;
        a.download = name;
        a.hidden = true;
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(function () { URL.revokeObjectURL(url); }, 60000);
        return Promise.resolve();
      },
      abort: function () { parts = []; return Promise.resolve(); }
    });
  }

  function sinkFor(name) {
    if (typeof window.showSaveFilePicker === 'function') {
      return pickerSink(name).catch(function (err) {
        if (err && err.name === 'AbortError') { throw err; }
        return memorySink(name);
      });
    }
    return memorySink(name);
  }

  // -- download ----------------------------------------------------------------

  function download(btn) {
    var kind = btn.getAttribute('data-kind');
    var path = btn.getAttribute('data-path');
    var name = btn.getAttribute('data-name') || 'download';
    var gzip = typeof CompressionStream === 'function';
    var saveName = kind === 'dir' ? name + (gzip ? '.tar.gz' : '.tar') : name;
    var state = { received: 0, etag: '', size: 0, controller: null, sink: null };
    var r = null;

    function run(resume) {
      r.clear();
      r.status(resume ? 'Resuming...' : 'Starting...');
      state.controller = new AbortController();
      r.button('Cancel', function () { state.controller.abort(); });
      return admit(pullURL, { path: path }).then(function (adm) {
        var headers = { 'Minion-Leg-Token': adm.leg_token };
        var restart = state.received > 0;
        if (kind === 'file' && state.received > 0 && state.etag && adm.etag === state.etag) {
          headers.Range = 'bytes=' + state.received + '-';
          headers['If-Range'] = adm.etag;
          restart = false;
        }
        state.etag = adm.etag || '';
        state.size = adm.size || 0;
        return fetch(adm.leg_path, { method: 'GET', credentials: 'same-origin', cache: 'no-store', headers: headers, signal: state.controller.signal })
          .then(function (resp) {
            if (resp.status !== 200 && resp.status !== 206) {
              return resp.json().catch(function () { return null; }).then(function (body) {
                throw new Error(errorText(body, 'The download was refused (' + resp.status + ').'));
              });
            }
            var ready = Promise.resolve();
            if (resp.status === 200 && (restart || state.received > 0)) {
              state.received = 0;
              ready = state.sink.reset();
              r.status('The file changed or cannot be resumed: starting again.');
            }
            var body = resp.body;
            if (kind === 'dir' && gzip) {
              body = body.pipeThrough(new CompressionStream('gzip'));
            }
            var reader = body.getReader();
            function pump() {
              return reader.read().then(function (step) {
                if (step.done) { return null; }
                state.received += step.value.byteLength;
                r.progress(state.received, kind === 'file' ? state.size : 0);
                r.status(human(state.received) + (kind === 'file' && state.size ? ' of ' + human(state.size) : ''));
                return state.sink.write(step.value).then(pump);
              });
            }
            return ready.then(pump);
          });
      }).then(function () {
        r.clear();
        return state.sink.finish().then(function () {
          r.progress(1, 1);
          r.status('Downloaded (' + human(state.received) + ').');
          say(saveName + ' downloaded.');
          r.button('Dismiss', function () { r.remove(); });
        });
      }).catch(function (err) {
        r.clear();
        if (state.controller.signal.aborted) {
          r.status('Cancelled.');
          state.sink.abort();
          r.button('Dismiss', function () { r.remove(); });
          return;
        }
        r.status((err && err.message ? err.message : 'The download stopped.') + (kind === 'file' && state.etag ? ' It can be resumed.' : ''));
        say(saveName + ': the download stopped.');
        r.button(kind === 'file' && state.etag ? 'Resume' : 'Start again', function () { run(true); });
        r.button('Discard', function () { state.sink.abort(); r.remove(); });
      });
    }

    sinkFor(saveName).then(function (sink) {
      state.sink = sink;
      r = row(saveName);
      run(false);
    }).catch(function () { /* the person closed the save dialog */ });
  }

  // -- upload --------------------------------------------------------------------

  function currentDir() {
    var list = document.querySelector('#files-list [data-files-dir]');
    return list ? list.getAttribute('data-files-dir') : '';
  }

  function refreshList() {
    var crumb = document.querySelector('#files-list .files-crumbs [aria-current="location"]');
    if (crumb && crumb.form && typeof crumb.form.requestSubmit === 'function') { crumb.form.requestSubmit(); }
  }

  function upload(form) {
    var input = form.querySelector('#files-upload-file');
    var overwrite = form.querySelector('#files-upload-overwrite');
    var errEl = form.querySelector('#files-upload-error');
    var file = input && input.files && input.files[0];
    errEl.hidden = true;
    if (!file) {
      errEl.textContent = 'Choose a file first.';
      errEl.hidden = false;
      input.focus();
      return;
    }
    var r = row(file.name);
    var xhr = null;
    var cancelled = false;
    r.status('Starting...');
    r.button('Cancel', function () { cancelled = true; if (xhr) { xhr.abort(); } });
    admit(pushURL, { dir: currentDir(), name: file.name, size: String(file.size), overwrite: overwrite && overwrite.checked ? '1' : '' })
      .then(function (adm) {
        if (cancelled) { throw new Error('Cancelled. Nothing was uploaded.'); }
        return new Promise(function (resolve, reject) {
          xhr = new XMLHttpRequest();
          xhr.open('PUT', uploadURL + encodeURIComponent(adm.transfer_id));
          xhr.setRequestHeader('X-Minion-CSRF', csrfToken());
          xhr.setRequestHeader('Minion-Leg-Token', adm.leg_token);
          xhr.setRequestHeader('Minion-File-Name', encodeURIComponent(file.name));
          xhr.setRequestHeader('Content-Type', 'application/octet-stream');
          xhr.upload.onprogress = function (ev) {
            r.progress(ev.loaded, file.size);
            r.status(human(ev.loaded) + ' of ' + human(file.size));
          };
          xhr.onload = function () {
            var body = null;
            try { body = JSON.parse(xhr.responseText); } catch (e) { body = null; }
            if (xhr.status === 201 && body && (body.outcome === 'delivered' || body.outcome === 'partial')) { resolve(body); return; }
            reject(new Error(errorText(body, body && body.reason ? 'The Worker did not keep the file (' + body.reason + ').' : 'The upload was refused (' + xhr.status + ').')));
          };
          xhr.onerror = function () { reject(new Error('The upload stopped. Nothing was published unless the history says so.')); };
          xhr.onabort = function () { reject(new Error('Cancelled. Nothing was published.')); };
          xhr.send(file);
        });
      })
      .then(function () {
        r.clear();
        r.progress(1, 1);
        r.status('Uploaded.');
        say(file.name + ' uploaded.');
        r.button('Dismiss', function () { r.remove(); });
        form.reset();
        refreshList();
      })
      .catch(function (err) {
        r.clear();
        r.status(err && err.message ? err.message : 'The upload failed.');
        say(file.name + ': ' + (err && err.message ? err.message : 'the upload failed.'));
        r.button('Dismiss', function () { r.remove(); });
      });
  }

  panel.addEventListener('click', function (ev) {
    var btn = ev.target.closest && ev.target.closest('[data-files-get]');
    if (btn && panel.contains(btn)) {
      ev.preventDefault();
      download(btn);
    }
  });
  var form = document.getElementById('files-upload');
  if (form) {
    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      upload(form);
    });
  }
}());
