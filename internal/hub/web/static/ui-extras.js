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

  function initSearch(nav) {
    var dialog = document.createElement('dialog');
    if (typeof dialog.showModal !== 'function') { return; }
    dialog.id = 'search-dialog';
    dialog.className = 'search-dialog';
    dialog.setAttribute('aria-labelledby', 'search-label');

    var label = document.createElement('label');
    label.id = 'search-label';
    label.htmlFor = 'search-input';
    label.textContent = 'Search';
    dialog.appendChild(label);

    var input = document.createElement('input');
    input.type = 'search';
    input.id = 'search-input';
    input.autocomplete = 'off';
    input.setAttribute('role', 'combobox');
    input.setAttribute('aria-autocomplete', 'list');
    input.setAttribute('aria-controls', 'search-results');
    input.setAttribute('aria-expanded', 'false');
    dialog.appendChild(input);

    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'btn btn-secondary';
    close.textContent = 'Close';
    close.addEventListener('click', function () { dialog.close(); });
    dialog.appendChild(close);

    var list = document.createElement('ul');
    list.id = 'search-results';
    list.setAttribute('role', 'listbox');
    list.setAttribute('aria-label', 'Search results');
    dialog.appendChild(list);

    var status = document.createElement('p');
    status.setAttribute('role', 'status');
    dialog.appendChild(status);
    document.body.appendChild(dialog);

    var opener = null;
    var trigger = document.querySelector('[data-search-open]');
    var results = [];
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

    function openResult(result) {
      dialog.close();
      window.location.href = result.href;
    }

    function addOption(result) {
      var option = document.createElement('li');
      option.id = 'search-option-' + options.length;
      option.setAttribute('role', 'option');
      option.setAttribute('aria-selected', 'false');
      var name = document.createElement('span');
      name.textContent = result.label;
      option.appendChild(name);
      var group = document.createElement('small');
      group.textContent = result.group;
      option.appendChild(group);
      option.addEventListener('click', function () { openResult(result); });
      list.appendChild(option);
      options.push(option);
    }

    function render() {
      var terms = words(input.value);
      list.textContent = '';
      options = [];
      selected = -1;
      results.forEach(function (result) {
        if (matches(result.label, terms)) { addOption(result); }
      });
      input.setAttribute('aria-expanded', options.length ? 'true' : 'false');
      select(options.length ? 0 : -1, false);
      status.textContent = options.length ? '' : 'No results.';
      status.hidden = options.length > 0;
    }

    function canOpen() {
      return !document.querySelector('dialog[open]');
    }

    function open(from) {
      opener = from && from !== document.body ? from : trigger;
      results = searchResults(nav);
      input.value = '';
      render();
      dialog.showModal();
      input.focus();
    }

    input.addEventListener('input', render);
    input.addEventListener('keydown', function (event) {
      if (event.isComposing || !options.length) { return; }
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault();
        var step = event.key === 'ArrowDown' ? 1 : -1;
        select((selected + step + options.length) % options.length, true);
      } else if (event.key === 'Enter') {
        event.preventDefault();
        options[selected].click();
      }
    });
    dialog.addEventListener('keydown', function (event) {
      if (event.key === 'Escape' && !event.isComposing) {
        event.preventDefault();
        event.stopPropagation();
        dialog.close();
      }
    });
    dialog.addEventListener('close', function () {
      if (dialog.open) { return; }
      if (opener && opener.isConnected) { opener.focus({ preventScroll: true }); }
      else if (trigger) { trigger.focus({ preventScroll: true }); }
      opener = null;
    });
    if (trigger) {
      trigger.addEventListener('click', function () { if (canOpen()) { open(trigger); } });
    }
    document.addEventListener('keydown', function (event) {
      if (event.defaultPrevented || event.repeat || event.isComposing || event.altKey) { return; }
      if (event.ctrlKey || event.metaKey) {
        if (event.shiftKey || event.key.toLowerCase() !== 'k') { return; }
      } else {
        if (event.key !== '/') { return; }
        var target = event.target;
        if (target && (target.isContentEditable || (target.closest && target.closest('input, textarea, select')))) { return; }
      }
      if (!canOpen()) { return; }
      event.preventDefault();
      open(document.activeElement);
    });
  }

  var nav = document.getElementById('site-nav');
  if (nav) { initSearch(nav); }

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
