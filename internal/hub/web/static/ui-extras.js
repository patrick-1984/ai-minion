// Round 8 UI classes (styled by minion.css):
// live-status + is-live/is-reconnecting/is-down: connection and update time;
// row-unsaved, unsaved-badge, row-reset: edited settings and local reset;
// filter-bar, filter-input, filter-count, filter-clear: local table search;
// filter-chips, filter-chip: removable list filters;
// rel-time: relative timestamp; pw-toggle: password visibility;
// field-hint, field-error, field-muted: conditional help and password mismatch;
// sel-count: share selection summary; is-busy: request label;
// cell-clamp + is-expanded: focusable/tappable long table values.
(function () {
  'use strict';

  function addResult(results, label, group, href) {
    label = label.trim();
    if (label) { results.push({ label: label, group: group, href: href }); }
  }

  function sectionHeading(section) {
    var headings = section.querySelectorAll('h1, h2, h3, h4, h5, h6');
    for (var index = 0; index < headings.length; index++) {
      if (headings[index].closest('section[id]') === section) { return headings[index]; }
    }
    return null;
  }

  function searchResults(nav) {
    var results = [];
    var links = nav.querySelectorAll('a[href]');
    for (var index = 0; index < links.length; index++) {
      var link = links[index];
      var group = link.closest('.nav-group');
      var title = group && group.querySelector('.nav-label');
      addResult(results, link.textContent, title ? title.textContent.trim() : '', link.href);
    }
    var sections = document.querySelectorAll('#content section[id], #content h2[id]');
    var headings = [];
    for (var sectionIndex = 0; sectionIndex < sections.length; sectionIndex++) {
      var section = sections[sectionIndex];
      var heading = section.tagName === 'H2' ? section : sectionHeading(section);
      if (!heading || heading.closest('[hidden], dialog:not([open])') || headings.indexOf(heading) !== -1) { continue; }
      headings.push(heading);
      addResult(results, heading.textContent, 'On this page', '#' + encodeURIComponent(section.id));
    }
    return results;
  }

  function words(text) {
    return text.toLowerCase().split(/[\s-]+/).filter(Boolean);
  }

  function matches(label, terms) {
    var labelWords = words(label);
    return terms.every(function (term) {
      return labelWords.some(function (word) {
        var previous = -1;
        for (var index = 0; index < term.length; index++) {
          previous = word.indexOf(term.charAt(index), previous + 1);
          if (previous === -1) { return false; }
        }
        return true;
      });
    });
  }

  function initSearch(nav, search) {
    var input = search.querySelector('#top-search-input');
    var list = search.querySelector('#top-search-results');
    // The role is set here, not in the markup: a page test checks that no
    // user name ("bob") appears in a 404 page, and "combobox" contains it.
    if (input) { input.setAttribute('role', 'combobox'); }
    var options = [];
    var selected = -1;

    function select(index, scroll) {
      if (selected !== -1) { options[selected].setAttribute('aria-selected', 'false'); }
      selected = index;
      if (selected === -1) {
        input.removeAttribute('aria-activedescendant');
        return;
      }
      var option = options[selected];
      option.setAttribute('aria-selected', 'true');
      input.setAttribute('aria-activedescendant', option.id);
      if (scroll) { option.scrollIntoView({ block: 'nearest' }); }
    }

    function close() {
      select(-1, false);
      list.hidden = true;
      input.setAttribute('aria-expanded', 'false');
    }

    function openResult(result) {
      close();
      window.location.href = result.href;
    }

    function addNote(className, text) {
      var note = document.createElement('li');
      note.className = className;
      note.setAttribute('role', 'presentation');
      note.textContent = text;
      list.appendChild(note);
    }

    function addOption(result) {
      var option = document.createElement('li');
      option.className = 'top-search-item';
      option.id = 'ts-opt-' + options.length;
      option.setAttribute('role', 'option');
      option.setAttribute('aria-selected', 'false');
      var name = document.createElement('span');
      name.className = 'top-search-label';
      name.textContent = result.label;
      option.appendChild(name);
      var group = document.createElement('span');
      group.className = 'top-search-meta';
      group.textContent = result.group;
      option.appendChild(group);
      option.addEventListener('click', function () { openResult(result); });
      list.appendChild(option);
      options.push(option);
    }

    function render() {
      var terms = words(input.value);
      select(-1, false);
      list.textContent = '';
      options = [];
      var group = '';
      searchResults(nav).forEach(function (result) {
        if (!matches(result.label, terms)) { return; }
        if (result.group !== group) {
          group = result.group;
          if (group) { addNote('top-search-group', group); }
        }
        addOption(result);
      });
      if (options.length) { select(0, false); }
      else { addNote('top-search-empty', 'No matches'); }
      list.hidden = false;
      list.scrollTop = 0;
      input.setAttribute('aria-expanded', 'true');
    }

    input.addEventListener('focus', render);
    input.addEventListener('click', function () { if (list.hidden) { render(); } });
    input.addEventListener('input', render);
    input.addEventListener('keydown', function (event) {
      if (event.isComposing) { return; }
      if (event.key === 'Escape') {
        event.preventDefault();
        if (!list.hidden) {
          event.stopPropagation();
          close();
        }
      } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault();
        var step = event.key === 'ArrowDown' ? 1 : -1;
        if (list.hidden) {
          render();
          if (step < 0 && options.length) { select(options.length - 1, true); }
        } else if (options.length) {
          select((selected + step + options.length) % options.length, true);
        }
      } else if (event.key === 'Enter' && selected !== -1) {
        event.preventDefault();
        options[selected].click();
      }
    });
    list.addEventListener('mousedown', function (event) {
      if (event.target.closest('.top-search-item')) { event.preventDefault(); }
    });

    function closeOutside(event) {
      if (!list.hidden && !search.contains(event.target)) { close(); }
    }
    document.addEventListener('click', closeOutside, true);
    document.addEventListener('focusin', closeOutside);

    document.addEventListener('keydown', function (event) {
      if (event.defaultPrevented || event.repeat || event.isComposing || event.altKey) { return; }
      if (event.ctrlKey || event.metaKey) {
        if (event.shiftKey || event.key.toLowerCase() !== 'k') { return; }
      } else {
        if (event.key !== '/') { return; }
        var target = event.target;
        if (target && (target.isContentEditable || (target.closest && target.closest('input, textarea, select')))) { return; }
      }
      if (document.querySelector('dialog[open], .rail.open')) { return; }
      event.preventDefault();
      if (document.activeElement !== input) { input.focus(); }
      else if (list.hidden) { render(); }
    });
  }

  var nav = document.getElementById('site-nav');
  var search = document.querySelector('[data-search]');
  if (nav && search) { initSearch(nav, search); }

  function copyFeedback(button, copied) {
    window.clearTimeout(button.copyFeedbackTimer);
    button.classList.toggle('is-copied', copied);
    var copyIcon = button.querySelector('.copy-icon');
    var checkIcon = button.querySelector('.copy-check');
    if (copyIcon) { copyIcon.hidden = copied; }
    if (checkIcon) { checkIcon.hidden = !copied; }
    button.title = copied ? 'Copied' : 'Copy';
    if (copied) {
      var label = button.getAttribute('aria-label');
      button.setAttribute('aria-label', 'Copied');
      button.copyFeedbackTimer = window.setTimeout(function () {
        button.setAttribute('aria-label', label);
        copyFeedback(button, false);
      }, 1500);
    }
  }

  function fallbackCopy(text) {
    var active = document.activeElement;
    var selection = window.getSelection();
    var ranges = [];
    if (selection) {
      for (var index = 0; index < selection.rangeCount; index++) { ranges.push(selection.getRangeAt(index).cloneRange()); }
    }
    // A selectable temporary field also works for disabled configuration inputs.
    var field = document.createElement('textarea');
    field.className = 'sr-only';
    field.value = text;
    field.readOnly = true;
    field.setAttribute('aria-label', 'Value to copy');
    document.body.appendChild(field);
    var copied = false;
    try {
      field.focus({ preventScroll: true });
      field.select();
      copied = document.execCommand('copy');
    } catch (error) { copied = false; }
    finally {
      field.remove();
      if (active && active.isConnected) { active.focus({ preventScroll: true }); }
      if (selection) {
        selection.removeAllRanges();
        ranges.forEach(function (range) { selection.addRange(range); });
      }
    }
    return copied;
  }

  // Read-only keys and tokens show in full: the box grows to the value and
  // selects it when focused.
  function sizeCopyWrap(root) {
    var list = (root || document).querySelectorAll('textarea.copy-wrap');
    for (var i = 0; i < list.length; i++) {
      list[i].style.height = 'auto';
      list[i].style.height = list[i].scrollHeight + 'px';
    }
  }
  document.addEventListener('focusin', function (event) {
    var t = event.target;
    if (t && t.matches && t.matches('textarea.copy-wrap')) { t.select(); }
  });
  document.addEventListener('htmx:afterSwap', function () { sizeCopyWrap(); });
  window.addEventListener('resize', function () { sizeCopyWrap(); });
  sizeCopyWrap();

  document.addEventListener('click', function (event) {
    var button = event.target.closest && event.target.closest('button[data-copy-target]');
    if (!button || button.disabled) { return; }
    // Legacy data-copy attributes remain for existing page contracts. Handle
    // these buttons in capture so onboarding.js cannot copy or replace icons.
    if (button.hasAttribute('data-copy')) { event.stopPropagation(); }
    if (!button.copyLabel) { button.copyLabel = button.getAttribute('aria-label'); }
    var selector = button.getAttribute('data-copy-target');
    var target = !selector ? null : selector.charAt(0) === '#' && selector.indexOf(' ') === -1
      ? document.getElementById(selector.slice(1)) : document.querySelector(selector);
    if (!target) { return; }
    var text = typeof target.value === 'string' ? target.value : target.textContent;
    function fallback() {
      if (fallbackCopy(text)) { copied(); }
      else {
        // Both methods failed: select the value itself and ask for Ctrl+C.
        try {
          if (typeof target.select === 'function') { target.focus(); target.select(); }
          else {
            var range = document.createRange();
            range.selectNodeContents(target);
            var sel = window.getSelection();
            sel.removeAllRanges();
            sel.addRange(range);
          }
        } catch (error) { /* the prompt below still tells what to do */ }
        var mac = /Mac|iPhone|iPad/i.test((navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || '');
        var field = button.closest('.copy-field') || button.parentNode;
        var hint = field.parentNode.querySelector('.copy-hint');
        if (!hint) {
          hint = document.createElement('span');
          hint.className = 'copy-hint';
          hint.setAttribute('role', 'status');
          hint.setAttribute('aria-live', 'polite');
          field.parentNode.insertBefore(hint, field.nextSibling);
        }
        hint.textContent = '';
        hint.textContent = mac ? 'Selected. Press \u2318C to copy.' : 'Selected. Press Ctrl+C to copy.';
      }
    }
    function copied() {
      // Restore the original accessible label before restarting feedback.
      button.setAttribute('aria-label', button.copyLabel);
      copyFeedback(button, true);
    }
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(copied, fallback);
      } else { fallback(); }
    } catch (error) { fallback(); }
  }, true);

  document.addEventListener('click', function (event) {
    var button = event.target.closest && event.target.closest('[data-audit-time]');
    if (!button) { return; }
    var input = document.querySelector(button.getAttribute('data-audit-time'));
    if (!input) { return; }
    if (input.type === 'date') {
      input.type = 'text';
      button.textContent = 'Use calendar';
    } else if (!input.value || /^\d{4}-\d{2}-\d{2}$/.test(input.value)) {
      var value = input.value;
      input.type = 'date';
      if (input.value !== value) {
        input.type = 'text';
        input.value = value;
      } else {
        button.textContent = 'Enter a date or RFC 3339 time';
        button.removeAttribute('title');
      }
    } else {
      button.title = 'Enter a YYYY-MM-DD date or clear the field to use the calendar';
    }
    input.focus();
  });

  var workerRowTimer = null;

  function cancelWorkerRow() {
    window.clearTimeout(workerRowTimer);
    workerRowTimer = null;
  }

  function selectingText() {
    var selection = window.getSelection();
    return selection && !selection.isCollapsed;
  }

  function openWorkerRow(event) {
    cancelWorkerRow();
    if (event.defaultPrevented || event.altKey || event.shiftKey || event.detail > 1) { return; }
    if (event.type === 'auxclick' ? event.button !== 1 : event.button !== 0) { return; }
    var target = event.target;
    if (!target || !target.closest) { return; }
    var row = target.closest('.worker-table tbody tr[data-row-link="worker"]');
    if (!row || selectingText()) { return; }
    if (target.closest('a, button, input, textarea, select, option, summary, details, label, form, dialog, iframe, video, audio, [contenteditable], [data-no-row], [role], [tabindex]:not([tabindex="-1"])')) { return; }
    var link = row.querySelector('th[scope="row"] a[data-worker-link][data-control="name"][href]');
    if (!link) { return; }
    // ui-extras loads before inspector.js; prevent its worker-row fallback
    // from handling the same click while leaving links and controls alone.
    event.preventDefault();
    var href = link.href;
    if (event.button === 1 || event.ctrlKey || event.metaKey) {
      window.open(href, '_blank', 'noopener');
      return;
    }
    function navigate() {
      workerRowTimer = null;
      if (row.isConnected && link.href === href && !selectingText()) { window.location.assign(href); }
    }
    // Let a second press or a drag select text before committing navigation.
    if (event.pointerType === 'touch' || event.pointerType === 'pen') { navigate(); }
    else { workerRowTimer = window.setTimeout(navigate, 520); }
  }

  document.addEventListener('mousedown', cancelWorkerRow);
  document.addEventListener('dblclick', cancelWorkerRow);
  document.addEventListener('click', openWorkerRow);
  document.addEventListener('auxclick', openWorkerRow);

  var tableSelector = 'table[data-sortable]';
  var sorts = Object.create(null);
  var sortStorageKey = 'minion:table-sorts:' + window.location.pathname;
  try {
    var storedSorts = JSON.parse(window.sessionStorage.getItem(sortStorageKey));
    if (storedSorts && typeof storedSorts === 'object' && !Array.isArray(storedSorts)) {
      Object.keys(storedSorts).forEach(function (key) {
        var saved = storedSorts[key];
        if (saved && typeof saved.column === 'string' &&
            (saved.direction === 'ascending' || saved.direction === 'descending')) { sorts[key] = saved; }
      });
    }
  } catch (error) { /* Storage can be unavailable or contain obsolete data. */ }

  function cellValue(cell, numeric) {
    if (!cell) { return numeric ? NaN : ''; }
    var override = cell.getAttribute('data-sort-value');
    var text = (override === null ? cell.textContent : override).trim();
    return numeric ? parseFloat(text) : text.toLowerCase();
  }

  function sortRows(table, header, direction) {
    var numeric = header.getAttribute('data-sort') === 'number';
    var column = header.cellIndex;
    var active = document.activeElement;
    var focusedRow = active && active.closest('tr');
    for (var bodyIndex = 0; bodyIndex < table.tBodies.length; bodyIndex++) {
      var body = table.tBodies[bodyIndex];
      var rows = Array.prototype.map.call(body.rows, function (row, index) {
        return { row: row, value: cellValue(row.cells[column], numeric), index: index };
      });
      rows.sort(function (left, right) {
        var order;
        if (numeric) {
          if (!isFinite(left.value)) { return isFinite(right.value) ? 1 : left.index - right.index; }
          if (!isFinite(right.value)) { return -1; }
          order = left.value - right.value;
        } else {
          order = left.value.localeCompare(right.value);
        }
        return (direction === 'ascending' ? order : -order) || left.index - right.index;
      });
      var next = null;
      for (var rowIndex = rows.length - 1; rowIndex >= 0; rowIndex--) {
        var row = rows[rowIndex].row;
        if (row !== focusedRow && row.nextElementSibling !== next) { body.insertBefore(row, next); }
        next = row;
      }
    }
  }

  function sortableHeaders(table) {
    return table.tHead ? table.tHead.querySelectorAll('th[data-sort]') : [];
  }

  function applySort(table, headers, activeHeader, direction) {
    for (var index = 0; index < headers.length; index++) {
      var header = headers[index];
      if (header === activeHeader) { header.setAttribute('aria-sort', direction); }
      else { header.removeAttribute('aria-sort'); }
    }
    sortRows(table, activeHeader, direction);
  }

  function prepareTable(table, key) {
    var headers = sortableHeaders(table);
    var sort = sorts[key];
    var activeHeader = null;
    for (var index = 0; index < headers.length; index++) {
      var header = headers[index];
      var button = header.querySelector('button.th-sort');
      if (!button) {
        button = document.createElement('button');
        button.type = 'button';
        button.className = 'th-sort';
        while (header.firstChild) { button.appendChild(header.firstChild); }
        header.appendChild(button);
      }
      var column = JSON.stringify([index, button.textContent.trim(), header.getAttribute('data-sort')]);
      button.id = 'table-sort-' + encodeURIComponent(JSON.stringify([key, column]));
      if (sort && sort.column === column) { activeHeader = header; }
      connectSortButton(button, header, table, key, column);
    }
    if (activeHeader) { applySort(table, headers, activeHeader, sort.direction); }
  }

  function connectSortButton(button, header, table, key, column) {
    button.removeEventListener('click', button.sortRows);
    button.sortRows = function () {
      var direction = header.getAttribute('aria-sort') === 'ascending' ? 'descending' : 'ascending';
      sorts[key] = { column: column, direction: direction };
      try { window.sessionStorage.setItem(sortStorageKey, JSON.stringify(sorts)); } catch (error) { /* Optional preference. */ }
      applySort(table, sortableHeaders(table), header, direction);
    };
    button.addEventListener('click', button.sortRows);
  }

  function refreshTables(event) {
    var tables = document.querySelectorAll(tableSelector);
    var counts = Object.create(null);
    for (var index = 0; index < tables.length; index++) {
      var table = tables[index];
      var scope = table.closest('[id], [data-key]');
      var family = JSON.stringify([scope && scope.id, scope && scope.getAttribute('data-key'), table.className]);
      var occurrence = counts[family] || 0;
      counts[family] = occurrence + 1;
      if (event && !event.target.contains(table) && !table.contains(event.target)) { continue; }
      prepareTable(table, JSON.stringify([family, occurrence]));
    }
  }

  refreshTables();
  document.addEventListener('htmx:afterSwap', refreshTables);
  document.addEventListener('htmx:sseMessage', refreshTables);
  function make(tag, className, text) {
    var element = document.createElement(tag);
    if (className) { element.className = className; }
    if (text !== undefined) { element.textContent = text; }
    return element;
  }

  function button(className, text) {
    var element = make('button', className, text);
    element.type = 'button';
    return element;
  }

  var extraID = 0;
  function describe(field, hint) {
    if (!hint.id) { hint.id = 'ui-hint-' + (++extraID); }
    var ids = (field.getAttribute('aria-describedby') || '').split(/\s+/).filter(Boolean);
    if (ids.indexOf(hint.id) === -1) { ids.push(hint.id); }
    field.setAttribute('aria-describedby', ids.join(' '));
  }

  function relativeTime(stamp) {
    var seconds = (stamp - Date.now()) / 1000;
    var units = [[31536000, 'year'], [2592000, 'month'], [86400, 'day'], [3600, 'hour'], [60, 'minute']];
    for (var i = 0; i < units.length; i++) {
      if (Math.abs(seconds) >= units[i][0]) {
        var count = Math.round(Math.abs(seconds) / units[i][0]);
        var text = count + ' ' + units[i][1] + (count === 1 ? '' : 's');
        return seconds > 0 ? 'in ' + text : text + ' ago';
      }
    }
    return seconds > 0 ? 'in less than a minute' : 'just now';
  }

  function refreshTimes() {
    document.querySelectorAll('time[datetime]').forEach(function (time) {
      var value = time.getAttribute('datetime');
      // Never guess a timezone or parse human-formatted dates.
      if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) { return; }
      var stamp = Date.parse(value);
      if (!isFinite(stamp)) { return; }
      // Preserve sub-millisecond precision from the source while converting
      // offsets to UTC; Date alone only retains milliseconds.
      var fraction = value.match(/\.\d+/);
      time.title = new Date(stamp).toISOString().replace(/\.\d{3}Z$/, (fraction ? fraction[0] : '') + 'Z');
      time.setAttribute('aria-label', time.title);
      var relative = time.nextElementSibling;
      if (!relative || !relative.hasAttribute('data-ui-relative')) {
        relative = make('span', 'rel-time');
        relative.setAttribute('data-ui-relative', '');
        relative.setAttribute('aria-hidden', 'true');
        time.after(relative);
      }
      relative.textContent = ' (' + relativeTime(stamp) + ')';
    });
  }

  var liveSelector = '[data-live-list][sse-connect], [data-live-list][data-sse-connect]';
  function liveBadge(list) {
    if (list.uiLiveBadge && list.uiLiveBadge.isConnected) { return list.uiLiveBadge; }
    var badge = make('p', 'live-status is-reconnecting', 'Reconnecting…');
    badge.setAttribute('role', 'status');
    list.before(badge);
    list.uiLiveBadge = badge;
    list.uiLiveState = 'reconnecting';
    return badge;
  }

  function paintLive(list) {
    var badge = liveBadge(list);
    var state = list.uiLiveState;
    badge.className = 'live-status is-' + state;
    var text = state === 'live' ? 'Live' : state === 'down' ? 'Disconnected' : 'Reconnecting…';
    if (list.uiLiveUpdated) { text += ' · Updated ' + relativeTime(list.uiLiveUpdated); }
    if (badge.textContent !== text) { badge.textContent = text; }
  }

  ['htmx:sseOpen', 'htmx:sseError', 'htmx:sseClose', 'htmx:sseMessage'].forEach(function (name) {
    document.addEventListener(name, function (event) {
      var list = event.target.closest && event.target.closest(liveSelector);
      if (!list) { return; }
      liveBadge(list);
      var source = event.detail && event.detail.source;
      if (name === 'htmx:sseOpen' || name === 'htmx:sseMessage') { list.uiLiveState = 'live'; }
      else { list.uiLiveState = name === 'htmx:sseClose' || (source && source.readyState === 2) ? 'down' : 'reconnecting'; }
      if (name === 'htmx:sseMessage') { list.uiLiveUpdated = Date.now(); }
      paintLive(list);
    });
  });

  // Passwords are hidden again before a form is sent.
  document.addEventListener('click', function (event) {
    var submitter = event.target.closest && event.target.closest('button, input[type="submit"], input[type="image"]');
    if (!submitter || submitter.disabled || !submitter.hasAttribute('data-confirm-proxy')) { return; }
    if (!event.defaultPrevented) { hidePasswords(submitter.form); }
  }, true);
  document.addEventListener('submit', function (event) {
    if (!event.defaultPrevented) { hidePasswords(event.target); }
  }, true);

  function fieldDefault(field) {
    if (field.tagName === 'SELECT') {
      var option = Array.prototype.find.call(field.options, function (item) { return item.defaultSelected; });
      return option ? option.value : field.options.length ? field.options[0].value : '';
    }
    return field.defaultValue;
  }

  function updateSetting(form) {
    var field = form.querySelector('[name="value"]');
    if (!field || !form.uiUnsaved) { return; }
    var dirty = field.value !== fieldDefault(field);
    form.classList.toggle('row-unsaved', dirty);
    form.uiUnsaved.hidden = !dirty;
    form.uiReset.hidden = !dirty;
  }

  function prepareSettings() {
    document.querySelectorAll('#settings-sections form.srow').forEach(function (form) {
      var field = form.querySelector('[name="value"]');
      if (!field || field.disabled || form.uiUnsaved) { return; }
      var badge = make('span', 'unsaved-badge', 'Unsaved');
      badge.setAttribute('role', 'status');
      var reset = button('row-reset', 'Reset');
      reset.addEventListener('click', function () {
        field.value = fieldDefault(field);
        field.dispatchEvent(new Event('input', { bubbles: true }));
        field.dispatchEvent(new Event('change', { bubbles: true }));
        field.focus();
      });
      form.uiUnsaved = badge;
      form.uiReset = reset;
      form.querySelector('.s-actions').append(badge, reset);
      form.addEventListener('input', function () { updateSetting(form); });
      form.addEventListener('change', function () { updateSetting(form); });
      updateSetting(form);
    });
  }

  window.addEventListener('beforeunload', function (event) {
    if (!document.querySelector('#settings-sections .row-unsaved')) { return; }
    event.preventDefault();
    event.returnValue = '';
  });

  function requestForm(event) {
    var element = event.detail && event.detail.elt || event.target;
    return element && element.closest ? element.closest('form') : null;
  }

  // Settings success uses HX-Refresh; a 200 alone can also contain an error.
  // Observe the success header before htmx starts its reload.
  document.addEventListener('htmx:beforeOnLoad', function (event) {
    var form = requestForm(event);
    var xhr = event.detail && event.detail.xhr;
    if (!form || !form.uiUnsaved || !xhr || xhr.status < 200 || xhr.status >= 300 ||
        xhr.getResponseHeader('HX-Refresh') !== 'true') { return; }
    var field = form.querySelector('[name="value"]');
    var sent = form.uiSettingSent;
    if (!field || !sent) { return; }
    var current = field.value;
    if (field.tagName === 'SELECT') {
      Array.prototype.forEach.call(field.options, function (option) { option.defaultSelected = option.value === sent.value; });
    } else { field.defaultValue = sent.value; }
    field.value = current;
    updateSetting(form);
  }, true);

  function prepareFilters() {
    ['accounts', 'apikey-list', 'invitation-list'].forEach(function (id) {
      var heading = document.getElementById(id);
      var section = heading && heading.closest('section');
      var table = section && section.querySelector('table');
      if (!table || table.uiFilter) { return; }
      table.uiFilter = true;
      var bar = make('div', 'filter-bar');
      var input = make('input', 'filter-input');
      input.type = 'search';
      input.placeholder = 'Filter rows';
      input.setAttribute('aria-label', 'Filter ' + heading.textContent.trim());
      var count = make('span', 'filter-count');
      count.setAttribute('role', 'status');
      var clear = button('filter-clear', 'Clear');
      function filter() {
        var query = input.value.trim().toLowerCase();
        var shown = 0;
        var total = 0;
        Array.prototype.forEach.call(table.tBodies, function (body) {
          Array.prototype.forEach.call(body.rows, function (row) {
            if (!row.querySelector('th[scope="row"]')) { return; }
            total++;
            row.hidden = row.textContent.toLowerCase().indexOf(query) === -1;
            if (!row.hidden) { shown++; }
          });
        });
        count.textContent = shown + ' of ' + total + ' rows';
        clear.disabled = !input.value;
      }
      input.addEventListener('input', filter);
      clear.addEventListener('click', function () { input.value = ''; filter(); input.focus(); });
      bar.append(input, count, clear);
      table.closest('.table-wrap').before(bar);
      filter();
    });
  }

  function prepareChips() {
    document.querySelectorAll('form[method="get"][role="search"]').forEach(function (form) {
      if (!/^\/(workers|sessions)$/.test(form.getAttribute('action')) || form.uiChips) { return; }
      var chips = make('div', 'filter-chips');
      form.after(chips);
      form.uiChips = chips;
      ['q', 'status', 'kind', 'owner', 'worker'].forEach(function (name) {
        var field = form.elements.namedItem(name);
        if (!field || !field.value) { return; }
        var value = field.tagName === 'SELECT' ? field.options[field.selectedIndex].textContent.trim() : field.value;
        var label = { q: 'Search', status: 'Status', kind: 'Kind', owner: 'Owner', worker: 'Worker' }[name];
        var chip = button('filter-chip', label + ': ' + value + ' ×');
        chip.setAttribute('aria-label', 'Remove ' + label.toLowerCase() + ' filter: ' + value);
        chip.addEventListener('click', function () {
          // Omitting an empty filter requires excluding this control from the
          // native GET once. Its name and all other form contracts stay intact.
          var disabled = field.disabled;
          field.disabled = true;
          try { form.requestSubmit(); }
          finally { field.disabled = disabled; }
        });
        chips.appendChild(chip);
      });
    });
  }

  function busyLabel(text) {
    if (/^(?:save|apply|use the default|change password)/i.test(text)) { return 'Saving…'; }
    if (/^(?:create|new session)/i.test(text)) { return 'Creating…'; }
    if (/^revoke/i.test(text)) { return 'Revoking…'; }
    if (/^(?:delete|remove|forget|detach)/i.test(text)) { return 'Removing…'; }
    if (/^deactivate/i.test(text)) { return 'Deactivating…'; }
    return 'Working…';
  }

  function showBusy(form, busy) {
    form.querySelectorAll('button[type="submit"], input[type="submit"]').forEach(function (submitter) {
      if (busy === !!submitter.uiBusy) { return; }
      if (busy) {
        var state = { aria: submitter.getAttribute('aria-busy'), label: submitter.getAttribute('aria-label') };
        var label = busyLabel(submitter.textContent.trim() || submitter.value);
        if (submitter.tagName === 'INPUT') { state.value = submitter.value; /* Keep its submitted value unchanged. */ }
        else {
          state.nodes = document.createDocumentFragment();
          while (submitter.firstChild) { state.nodes.appendChild(submitter.firstChild); }
          submitter.appendChild(document.createTextNode(label));
        }
        submitter.uiBusy = state;
        submitter.setAttribute('aria-label', label);
        submitter.setAttribute('aria-busy', 'true');
        submitter.classList.add('is-busy');
      } else {
        var saved = submitter.uiBusy;
        if (saved.nodes) { submitter.replaceChildren(saved.nodes); }
        if (saved.aria === null) { submitter.removeAttribute('aria-busy'); } else { submitter.setAttribute('aria-busy', saved.aria); }
        if (saved.label === null) { submitter.removeAttribute('aria-label'); } else { submitter.setAttribute('aria-label', saved.label); }
        submitter.classList.remove('is-busy');
        submitter.uiBusy = null;
      }
    });
  }

  document.addEventListener('htmx:beforeRequest', function (event) {
    var form = requestForm(event);
    if (!form || event.defaultPrevented) { return; }
    if (form.uiUnsaved) { form.uiSettingSent = { value: form.querySelector('[name="value"]').value }; }
    showBusy(form, true);
  });
  ['htmx:afterRequest', 'htmx:sendError', 'htmx:timeout', 'htmx:sendAbort'].forEach(function (name) {
    document.addEventListener(name, function (event) { var form = requestForm(event); if (form) { showBusy(form, false); } });
  });

  function prepareJSONBusy() {
    if (!window.MutationObserver) { return; }
    document.querySelectorAll('form[data-json-action], form[data-identity-op], form[data-reauth]').forEach(function (form) {
      if (form.uiBusyObserver) { return; }
      // forms.js and identity.js have no custom request events. They toggle
      // disabled around fetch; observe that state without wrapping fetch.
      var observer = new MutationObserver(function () {
        if (!form.isConnected) { observer.disconnect(); return; }
        var submits = form.querySelectorAll('button[type="submit"]');
        var busy = submits.length > 0 && Array.prototype.every.call(submits, function (submitter) { return submitter.disabled; });
        if (busy) { hidePasswords(form); }
        form.querySelectorAll('input').forEach(function (field) {
          if (field.uiPasswordToggle) {
            var disabled = busy || field.matches(':disabled');
            if (field.uiPasswordToggle.disabled !== disabled) { field.uiPasswordToggle.disabled = disabled; }
          }
        });
        showBusy(form, busy);
      });
      observer.observe(form, { subtree: true, attributes: true, attributeFilter: ['disabled'] });
      form.uiBusyObserver = observer;
    });
  }

  function shareState(form) {
    return Array.prototype.map.call(form.querySelectorAll('input[name="mode"], input[type="checkbox"][name="name"]'), function (field) {
      return [field.name, field.value, field.checked];
    });
  }

  function updateShare(form) {
    var fields = form.querySelectorAll('input[type="checkbox"][name="name"]');
    var selected = Array.prototype.filter.call(fields, function (field) { return field.checked; }).length;
    var dirty = JSON.stringify(shareState(form)) !== form.uiShareSaved;
    form.uiShareCount.textContent = selected + ' of ' + fields.length + ' sessions selected' + (dirty ? ' · Unsaved' : '');
  }

  function prepareShares() {
    document.querySelectorAll('form[id^="share-form-"]').forEach(function (form) {
      if (form.uiShareCount) { return; }
      form.uiShareSaved = JSON.stringify(shareState(form));
      form.uiShareCount = make('p', 'sel-count');
      form.uiShareCount.setAttribute('role', 'status');
      var actions = form.querySelector('.form-actions');
      (actions || form).prepend(form.uiShareCount);
      // share-bulk.js runs later in the bubbling event; read after it syncs mode.
      form.addEventListener('change', function () { queueMicrotask(function () { updateShare(form); }); });
      form.addEventListener('click', function (event) {
        if (event.target.closest('[data-share-bulk]')) { queueMicrotask(function () { updateShare(form); }); }
      });
      updateShare(form);
    });
  }

  function passwordIcon(visible) {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'icon');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', visible ? 'M3 3l18 18M10.6 10.6a2 2 0 002.8 2.8M9.9 5.2A11 11 0 0112 5c7 0 10 7 10 7a17 17 0 01-3.1 4.2M6.1 6.1A17 17 0 002 12s3 7 10 7a11 11 0 005.9-1.8' :
      'M2 12s3-7 10-7 10 7 10 7-3 7-10 7S2 12 2 12zm12 0a2 2 0 11-4 0 2 2 0 014 0');
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.5');
    path.setAttribute('stroke-linecap', 'round');
    path.setAttribute('stroke-linejoin', 'round');
    svg.appendChild(path);
    return svg;
  }

  function setPasswordVisible(field, visible) {
    field.type = visible ? 'text' : 'password';
    var toggle = field.uiPasswordToggle;
    toggle.replaceChildren(passwordIcon(visible), document.createTextNode(visible ? 'Hide' : 'Show'));
    toggle.setAttribute('aria-label', visible ? 'Hide password' : 'Show password');
    toggle.setAttribute('aria-pressed', String(visible));
  }

  function hidePasswords(form) {
    if (!form) { return; }
    // identity.js clears secrets by type=password after a request. Restore that
    // type before its handlers read the form so its clearing behavior survives.
    form.querySelectorAll('input').forEach(function (field) {
      if (field.uiPasswordToggle && field.type !== 'password') { setPasswordVisible(field, false); }
    });
  }

  function preparePasswords() {
    document.querySelectorAll('input[type="password"]').forEach(function (field) {
      if (field.uiPasswordToggle) { return; }
      var toggle = button('pw-toggle');
      toggle.append(passwordIcon(false), document.createTextNode('Show'));
      toggle.setAttribute('aria-label', 'Show password');
      toggle.setAttribute('aria-pressed', 'false');
      toggle.disabled = field.disabled;
      toggle.addEventListener('click', function () {
        setPasswordVisible(field, field.type === 'password');
      });
      field.after(toggle);
      field.uiPasswordToggle = toggle;
    });
    document.querySelectorAll('form[data-match]').forEach(function (form) {
      if (form.uiPasswordHint) { return; }
      var pair = form.getAttribute('data-match').split(',');
      var first = form.elements.namedItem(pair[0]);
      var second = form.elements.namedItem(pair[1]);
      if (!first || !second) { return; }
      var hint = make('span', 'field-error', "Passwords don't match");
      hint.setAttribute('role', 'status');
      second.closest('.field').appendChild(hint);
      describe(second, hint);
      function check(submitting) {
        var mismatch = first.value !== second.value;
        hint.hidden = !mismatch || (!submitting && !second.value);
        if (mismatch && !hint.hidden) { second.setAttribute('aria-invalid', 'true'); }
        else { second.removeAttribute('aria-invalid'); }
        return mismatch;
      }
      form.addEventListener('input', function () { check(false); });
      form.addEventListener('submit', function (event) {
        if (check(true)) {
          event.preventDefault();
          second.focus();
          // The existing forms.js mismatch check still runs and reports its error.
        }
      }, true);
      form.uiPasswordHint = hint;
      check(false);
    });
  }

  function prepareConditionalFields() {
    var mode = document.getElementById('inv-mode');
    var uses = document.getElementById('inv-maxuses');
    if (mode && uses && !uses.uiConditional) {
      var hint = make('span', 'field-hint');
      uses.after(hint);
      describe(uses, hint);
      function updateUses() {
        var reusable = mode.value === 'reusable';
        uses.closest('.field').classList.toggle('field-muted', !reusable);
        hint.textContent = reusable ? 'Enrollments apply to reusable keys.' : 'Enrollments apply only when Reusable is chosen.';
      }
      mode.addEventListener('change', updateUses);
      uses.uiConditional = true;
      updateUses();
    }
    var session = document.getElementById('key-session');
    if (session && !session.uiConditional) {
      var sessionHint = make('span', 'field-hint');
      session.after(sessionHint);
      describe(session, sessionHint);
      function updateSession() {
        var resources = session.form.querySelectorAll('input[name="resource"]:checked');
        var oneWorker = resources.length === 1 && resources[0].value.indexOf('worker:') === 0;
        session.closest('.field').classList.toggle('field-muted', !oneWorker);
        sessionHint.textContent = oneWorker ? 'Exactly one Worker selected. You can restrict the key to one session.' :
          'Choose exactly one Worker to restrict the key to one session.';
      }
      session.form.addEventListener('change', updateSession);
      session.uiConditional = true;
      updateSession();
    }
  }

  function prepareClamps() {
    document.querySelectorAll('.cell-clamp').forEach(function (cell) {
      if (cell.uiClamp) { return; }
      cell.uiClamp = true;
      if (!cell.title) { cell.title = cell.textContent.trim(); }
      if (!cell.hasAttribute('tabindex')) { cell.tabIndex = 0; }
      cell.addEventListener('pointerdown', function () { cell.uiClampWasExpanded = cell.classList.contains('is-expanded'); });
      cell.addEventListener('focusin', function () { cell.classList.add('is-expanded'); });
      cell.addEventListener('focusout', function (event) {
        if (!cell.contains(event.relatedTarget)) { cell.classList.remove('is-expanded'); }
      });
      cell.addEventListener('click', function (event) {
        if (event.target.closest('a, button, input, select, textarea, summary') || selectingText()) { return; }
        event.preventDefault();
        event.stopPropagation();
        cell.classList.toggle('is-expanded', cell.uiClampWasExpanded === undefined ? !cell.classList.contains('is-expanded') : !cell.uiClampWasExpanded);
        cell.uiClampWasExpanded = undefined;
      });
      cell.addEventListener('keydown', function (event) {
        if (event.target !== cell) { return; }
        if (event.key === 'Escape') { cell.classList.remove('is-expanded'); }
        if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); cell.classList.toggle('is-expanded'); }
      });
    });
  }

  function refreshExtras() {
    prepareSettings();
    prepareFilters();
    prepareChips();
    prepareJSONBusy();
    prepareShares();
    preparePasswords();
    prepareConditionalFields();
    prepareClamps();
    refreshTimes();
    document.querySelectorAll(liveSelector).forEach(function (list) { paintLive(list); });
  }
  refreshExtras();
  document.addEventListener('htmx:afterSwap', refreshExtras);
  document.addEventListener('htmx:sseMessage', function () { queueMicrotask(refreshExtras); });
  window.setInterval(function () {
    refreshTimes();
    document.querySelectorAll(liveSelector).forEach(paintLive);
  }, 30000);
})();
