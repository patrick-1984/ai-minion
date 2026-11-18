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
