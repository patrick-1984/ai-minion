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
        button.title = 'Select the value and press Ctrl+C to copy';
        button.setAttribute('aria-label', button.title);
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
})();
