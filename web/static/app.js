// ── Theme (runs immediately before app init) ──────────────
(function() {
  // Apply cached theme instantly to avoid flash
  var saved = null;
  try { saved = localStorage.getItem('ws-theme'); } catch(e) {}
  if (saved) document.documentElement.setAttribute('data-theme', saved);
  // Then sync from server (authoritative source)
  fetch('/api/preferences').then(function(r) { return r.json(); }).then(function(prefs) {
    if (prefs.theme) {
      document.documentElement.setAttribute('data-theme', prefs.theme);
      try { localStorage.setItem('ws-theme', prefs.theme); } catch(e) {}
    }
  }).catch(function() {});
})();

window.websessions = (function() {
  const terminals = {};
  const splitInstances = [];
  // splitTree: null for single session, or:
  // { type:'split', dir:'horizontal'|'vertical', children: [node, node] }
  // leaf: { type:'session', id:'...', name:'...' }
  var openTabs = []; // [{id, name, state, type, provider, splitTree: node|null}]
  var activeTabId = null;

  var darkTermTheme = {
    background: '#13141c', foreground: '#d0d4f0',
    cursor: '#6c8cff', selectionBackground: 'rgba(108, 140, 255, 0.2)',
  };
  var lightTermTheme = {
    background: '#f5f6fa', foreground: '#1a1c2b',
    cursor: '#4a6de5', selectionBackground: 'rgba(74, 109, 229, 0.15)',
  };

  function currentTheme() {
    return document.documentElement.getAttribute('data-theme') || 'dark';
  }

  function updateThemeIcon() {
    var btn = document.getElementById('theme-toggle-btn');
    if (btn) btn.textContent = currentTheme() === 'light' ? '\u2600' : '\u263E';
  }

  function savePref(key, value) {
    return fetch('/api/preferences', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: key, value: value }),
    }).catch(function() {});
  }

  function toggleTheme() {
    var next = currentTheme() === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem('ws-theme', next); } catch(e) {}
    savePref('theme', next);
    updateThemeIcon();
    // Update all open xterm instances
    var theme = next === 'light' ? lightTermTheme : darkTermTheme;
    Object.keys(terminals).forEach(function(id) {
      if (terminals[id]) terminals[id].options.theme = theme;
    });
  }

  // Set icon on load
  setTimeout(updateThemeIcon, 0);

  // ── Notification Sounds (Web Audio API) ──────────────────
  var audioCtx = null;
  function getAudioCtx() {
    if (!audioCtx) {
      audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    }
    return audioCtx;
  }

  function playTone(frequencies, durations, type, volume) {
    try {
      var ctx = getAudioCtx();
      var startTime = ctx.currentTime;
      for (var i = 0; i < frequencies.length; i++) {
        var osc = ctx.createOscillator();
        var gain = ctx.createGain();
        osc.type = type || 'sine';
        osc.frequency.value = frequencies[i];
        gain.gain.setValueAtTime(volume || 0.15, startTime);
        gain.gain.exponentialRampToValueAtTime(0.001, startTime + durations[i]);
        osc.connect(gain);
        gain.connect(ctx.destination);
        osc.start(startTime);
        osc.stop(startTime + durations[i]);
        startTime += durations[i] * 0.6; // overlap slightly
      }
    } catch(e) {}
  }

  var notifSounds = {
    completed: function() {
      // Ascending two-note chime — pleasant, success
      playTone([523, 784], [0.15, 0.25], 'sine', 0.12);
    },
    waiting: function() {
      // Three gentle pings — attention needed
      playTone([660, 660, 880], [0.1, 0.1, 0.2], 'sine', 0.1);
    },
    errored: function() {
      // Descending two-note — low, alert
      playTone([440, 330], [0.18, 0.3], 'triangle', 0.14);
    }
  };

  function playNotifSound(eventType) {
    // Check if sounds are enabled (stored in localStorage)
    try {
      if (localStorage.getItem('ws-notif-sounds') === 'false') return;
    } catch(e) {}
    var fn = notifSounds[eventType];
    if (fn) fn();
  }

  function setNotifSoundsEnabled(enabled) {
    try { localStorage.setItem('ws-notif-sounds', enabled ? 'true' : 'false'); } catch(e) {}
  }

  function getNotifSoundsEnabled() {
    try { return localStorage.getItem('ws-notif-sounds') !== 'false'; } catch(e) { return true; }
  }

  // Test sound — plays server-side via paplay/afplay
  function testNotifSound(eventType) {
    fetch('/api/test-sound', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ event: eventType }),
    });
  }
  // ─────────────────────────────────────────────────────────

  function connectSession(sessionID, containerID) {
    const container = document.getElementById(containerID);
    if (!container) return;

    const term = new Terminal({
      cursorBlink: true,
      scrollback: 50000,
      theme: currentTheme() === 'light' ? lightTermTheme : darkTermTheme,
      fontFamily: "'Maple Mono Normal NF', 'IBM Plex Mono', 'JetBrains Mono', 'Fira Code', monospace",
      fontSize: termFontSize,
    });

    // Regex to strip alternate screen escape sequences so all output stays
    // in the normal scrollable buffer. Without this, Claude Code enters
    // alternate screen mode which has no scrollback.
    var altScreenRe = /\x1b\[\?(1049|1047|47)[hl]/g;

    const fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(container);
    fitAddon.fit();

    var session = { term: term, ws: null, fitAddon: fitAddon, resizeObserver: null, closed: false, retries: 0 };

    function openWS() {
      if (session.closed) return;
      var protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      var ws = new WebSocket(protocol + '//' + window.location.host + '/ws/' + sessionID);
      ws.binaryType = 'arraybuffer';
      session.ws = ws;

      ws.onopen = function() {
        session.retries = 0;
        var dims = { type: 'resize', rows: term.rows, cols: term.cols };
        ws.send(JSON.stringify(dims));
      };

      ws.onmessage = function(event) {
        if (event.data instanceof ArrayBuffer) {
          // Decode, strip alternate screen sequences, write
          var text = new TextDecoder().decode(new Uint8Array(event.data));
          var filtered = text.replace(altScreenRe, '');
          if (filtered) term.write(filtered);
        } else {
          try {
            var msg = JSON.parse(event.data);
            if (msg.type === 'notification') { handleNotification(msg); }
          } catch(e) {
            var filtered2 = event.data.replace(altScreenRe, '');
            if (filtered2) term.write(filtered2);
          }
        }
      };

      ws.onclose = function() {
        if (session.closed) return;
        session.retries++;
        if (session.retries > 5) {
          term.write('\r\n\x1b[31m[Session unavailable — connection closed]\x1b[0m\r\n');
          session.closed = true;
          // Auto-close the tab after a brief delay
          setTimeout(function() { closeTab(sessionID); }, 2000);
          return;
        }
        var delay = Math.min(1000 * Math.pow(2, session.retries - 1), 15000);
        term.write('\r\n\x1b[33m[Reconnecting in ' + Math.round(delay / 1000) + 's...]\x1b[0m\r\n');
        setTimeout(openWS, delay);
      };
    }

    openWS();

    term.onData(function(data) {
      if (session.ws && session.ws.readyState === WebSocket.OPEN) {
        session.ws.send(data);
      }
    });

    var resizeObserver = new ResizeObserver(function() {
      fitAddon.fit();
      if (session.ws && session.ws.readyState === WebSocket.OPEN) {
        var dims = { type: 'resize', rows: term.rows, cols: term.cols };
        session.ws.send(JSON.stringify(dims));
      }
    });
    resizeObserver.observe(container);
    session.resizeObserver = resizeObserver;

    terminals[sessionID] = session;
  }

  function disconnectSession(sessionID) {
    var t = terminals[sessionID];
    if (!t) return;
    t.closed = true;
    if (t.ws) t.ws.close();
    t.resizeObserver.disconnect();
    t.term.dispose();
    delete terminals[sessionID];
  }

  function splitPane(currentSessionID, direction) {
    var area = document.getElementById('terminal-area');
    if (!area) return;

    // Prompt user to pick a session for the new pane
    // Fetch sidebar sessions and show a quick picker
    fetch('/api/sessions')
      .then(function(r) { return r.json(); })
      .then(function(sessions) {
        showSplitPicker(currentSessionID, direction, sessions);
      })
      .catch(function() {
        // Fallback: just ask for session ID
        var sid = prompt('Session ID to open in new pane:');
        if (sid) doSplit(currentSessionID, sid, direction);
      });
  }

  function showSplitPicker(currentSessionID, direction, sessions) {
    // Create a small picker overlay
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    overlay.addEventListener('click', function() { overlay.remove(); });
    var content = document.createElement('div');
    content.className = 'modal-content';
    content.style.width = '300px';
    content.addEventListener('click', function(e) { e.stopPropagation(); });
    var title = document.createElement('h2');
    title.textContent = 'Open in split';
    content.appendChild(title);

    // New session options
    var newSection = document.createElement('div');
    newSection.className = 'split-picker-new';

    var newSessionBtn = document.createElement('button');
    newSessionBtn.className = 'split-picker-action';
    newSessionBtn.textContent = '+ New Session';
    newSessionBtn.addEventListener('click', function() {
      overlay.remove();
      htmx.ajax('GET', '/sessions/new', { target: '#modal', swap: 'innerHTML' });
    });
    newSection.appendChild(newSessionBtn);

    var newTermBtn = document.createElement('button');
    newTermBtn.className = 'split-picker-action split-picker-action-term';
    newTermBtn.textContent = '\u2752 New Terminal';
    newTermBtn.addEventListener('click', function() {
      overlay.remove();
      var form = new FormData();
      form.append('work_dir', '~');
      fetch('/sessions/terminal', { method: 'POST', body: form })
        .then(function(r) {
          var sid = r.headers.get('X-Session-ID');
          if (sid) doSplit(currentSessionID, sid, direction);
          refreshSidebar();
          return r.text();
        });
    });
    newSection.appendChild(newTermBtn);
    content.appendChild(newSection);

    // Build set of session IDs already in ANY split group (not standalone tabs)
    var usedIds = {};
    usedIds[currentSessionID] = true;
    openTabs.forEach(function(t) {
      if (t.splitTree) {
        treeSessionIds(t.splitTree).forEach(function(id) { usedIds[id] = true; });
      }
    });

    // Existing sessions
    var hasOthers = false;
    sessions.forEach(function(s) {
      if (usedIds[s.id]) return;
      hasOthers = true;
      var btn = document.createElement('button');
      btn.className = 'recent-item';
      btn.style.width = '100%';
      btn.style.marginBottom = '0.25rem';
      var nameRow = document.createElement('span');
      nameRow.className = 'recent-name';
      nameRow.textContent = s.name + ' ';
      var typeBadge = document.createElement('span');
      typeBadge.className = 'session-type-badge type-' + (s.type || 'claude');
      typeBadge.textContent = s.type || 'claude';
      nameRow.appendChild(typeBadge);
      var pathSpan = document.createElement('span');
      pathSpan.className = 'recent-path';
      pathSpan.textContent = s.work_dir;
      btn.appendChild(nameRow);
      btn.appendChild(pathSpan);
      btn.addEventListener('click', function() {
        overlay.remove();
        doSplit(currentSessionID, s.id, direction);
      });
      content.appendChild(btn);
    });
    if (!hasOthers) {
      var msg = document.createElement('p');
      msg.textContent = 'No other active sessions';
      msg.style.color = 'var(--text-muted)';
      msg.style.textAlign = 'center';
      msg.style.padding = '0.5rem';
      msg.style.fontSize = '0.7rem';
      content.appendChild(msg);
    }
    overlay.appendChild(content);
    document.body.appendChild(overlay);
  }

  var focusedSessionId = null;

  function focusPane(sessionID) {
    focusedSessionId = sessionID;
    document.querySelectorAll('.terminal-pane.pane-focused').forEach(function(el) {
      el.classList.remove('pane-focused');
    });
    var area = document.getElementById('terminal-area');
    if (!area) return;
    area.querySelectorAll('.terminal-pane[data-session-id]').forEach(function(el) {
      if (el.dataset.sessionId === sessionID) {
        el.classList.add('pane-focused');
      }
    });
  }

  function refitAllTerminals() {
    Object.keys(terminals).forEach(function(id) {
      var t = terminals[id];
      if (t && t.fitAddon) t.fitAddon.fit();
    });
  }

  var refitTimer = null;
  function debouncedRefit() {
    if (refitTimer) clearTimeout(refitTimer);
    refitTimer = setTimeout(refitAllTerminals, 16);
  }

  function createSplit(panes, direction) {
    return Split(panes, {
      direction: splitDirection(direction),
      sizes: panes.map(function() { return 100 / panes.length; }),
      minSize: 80,
      gutterSize: 4,
      onDrag: debouncedRefit,
      onDragEnd: refitAllTerminals,
    });
  }

  function execInlineScripts(container) {
    container.querySelectorAll('script').forEach(function(oldScript) {
      var newScript = document.createElement('script');
      newScript.textContent = oldScript.textContent;
      oldScript.parentNode.replaceChild(newScript, oldScript);
    });
  }

  function loadPaneSession(pane, sid) {
    fetch('/sessions/' + encodeURIComponent(sid) + '/open', { method: 'POST' })
      .then(function(r) { return r.text(); })
      .then(function(html) {
        pane.innerHTML = html;
        execInlineScripts(pane);
        var termPane = pane.querySelector('.terminal-pane[data-session-id]');
        if (termPane) {
          var id = termPane.dataset.sessionId;
          if (terminals[id]) disconnectSession(id);
          connectSession(id, 'term-' + id);
          // Focus on click on pane header or terminal area
          var header = termPane.querySelector('.pane-header');
          if (header) header.addEventListener('click', function(e) {
            if (e.target.closest('button')) return;
            focusPane(id);
          });
          var termContainer = document.getElementById('term-' + id);
          if (termContainer) termContainer.addEventListener('mousedown', function() {
            focusPane(id);
          });
        }
      });
  }

  function loadPaneIframe(pane, paneID, iframeUrl, title) {
    // Fetch server-rendered iframe pane HTML (same pattern as loadPaneSession — trusted server response)
    fetch('/panes/iframe/open?url=' + encodeURIComponent(iframeUrl) + '&title=' + encodeURIComponent(title || 'Iframe'), { method: 'POST' })
      .then(function(r) { return r.text(); })
      .then(function(html) {
        pane.innerHTML = html;
        execInlineScripts(pane);
        var iframePane = pane.querySelector('.iframe-pane[data-pane-id]');
        if (iframePane) {
          var header = iframePane.querySelector('.pane-header');
          if (header) header.addEventListener('click', function(e) {
            if (e.target.closest('button')) return;
            focusPane(paneID);
          });
        }
      });
  }

  // Split direction: "horizontal" = horizontal divider (top/bottom), "vertical" = vertical divider (side by side)
  // Matches Terminator convention.
  function splitDirection(direction) {
    // Split.js 'vertical' = top/bottom, 'horizontal' = side by side
    return direction === 'horizontal' ? 'vertical' : 'horizontal';
  }
  function splitFlex(direction) {
    return direction === 'horizontal' ? 'column' : 'row';
  }

  // -- Split tree helpers --
  // Leaf nodes: type === 'session' or type === 'iframe' (anything that's not 'split')
  function isLeafNode(node) { return node && node.type !== 'split'; }

  function treeFind(node, id) {
    if (!node) return null;
    if (isLeafNode(node)) return node.id === id ? node : null;
    for (var i = 0; i < node.children.length; i++) {
      var found = treeFind(node.children[i], id);
      if (found) return found;
    }
    return null;
  }

  function treeFindParent(node, id) {
    if (!node || isLeafNode(node)) return null;
    for (var i = 0; i < node.children.length; i++) {
      if (isLeafNode(node.children[i]) && node.children[i].id === id) return { parent: node, index: i };
      var found = treeFindParent(node.children[i], id);
      if (found) return found;
    }
    return null;
  }

  function treeLeafIds(node) {
    if (!node) return [];
    if (isLeafNode(node)) return [node.id];
    var ids = [];
    node.children.forEach(function(c) { ids = ids.concat(treeLeafIds(c)); });
    return ids;
  }
  // Alias for backwards compatibility
  var treeSessionIds = treeLeafIds;

  function treeRemove(node, id) {
    if (!node || isLeafNode(node)) return node;
    var info = treeFindParent(node, id);
    if (!info) return node;
    info.parent.children.splice(info.index, 1);
    // If only one child left, collapse
    if (info.parent.children.length === 1) {
      var remaining = info.parent.children[0];
      info.parent.type = remaining.type;
      info.parent.children = remaining.children;
      info.parent.id = remaining.id;
      info.parent.name = remaining.name;
      info.parent.url = remaining.url;
      info.parent.dir = remaining.dir;
    }
    return node;
  }

  function doSplit(sessionID1, sessionID2, direction) {
    var termEl = document.getElementById('term-' + sessionID1);
    var existingPane = termEl ? termEl.closest('.split-pane') : null;

    if (!existingPane) {
      // First split — replace terminal-area
      var area = document.getElementById('terminal-area');
      if (!area) return;
      if (terminals[sessionID1]) disconnectSession(sessionID1);
      while (area.firstChild) area.removeChild(area.firstChild);
      area.style.flexDirection = splitFlex(direction);

      var p1 = document.createElement('div');
      p1.className = 'split-pane';
      var p2 = document.createElement('div');
      p2.className = 'split-pane';
      area.appendChild(p1);
      area.appendChild(p2);
      createSplit([p1, p2], direction);
      loadPaneSession(p1, sessionID1);
      loadPaneSession(p2, sessionID2);
    } else {
      // Nested split — replace the specific pane
      var parent = existingPane.parentElement;
      if (terminals[sessionID1]) disconnectSession(sessionID1);

      var container = document.createElement('div');
      container.className = 'split-pane';
      container.style.display = 'flex';
      container.style.flexDirection = splitFlex(direction);
      container.style.overflow = 'hidden';

      var p1 = document.createElement('div');
      p1.className = 'split-pane';
      var p2 = document.createElement('div');
      p2.className = 'split-pane';
      container.appendChild(p1);
      container.appendChild(p2);
      parent.replaceChild(container, existingPane);
      createSplit([p1, p2], direction);
      loadPaneSession(p1, sessionID1);
      loadPaneSession(p2, sessionID2);
    }

    // Update split tree
    var tab = openTabs.find(function(t) {
      return t.id === sessionID1 || (t.splitTree && treeFind(t.splitTree, sessionID1));
    });
    if (tab) {
      var newNode = { type: 'split', dir: direction, children: [
        { type: 'session', id: sessionID1, name: sessionID1 },
        { type: 'session', id: sessionID2, name: sessionID2 }
      ]};
      if (!tab.splitTree) {
        tab.splitTree = newNode;
      } else {
        var info = treeFindParent(tab.splitTree, sessionID1);
        if (info) {
          info.parent.children[info.index] = newNode;
        } else if (tab.splitTree.type === 'session' && tab.splitTree.id === sessionID1) {
          tab.splitTree = newNode;
        }
      }
      // Remove sessionID2 from top-level tabs
      openTabs = openTabs.filter(function(t) { return t.id !== sessionID2; });
      renderTabs();
      // Save and refresh sidebar after persistence completes
      saveTabState().then(function() {
        refreshSidebar();
      });
    }
  }

  function showToast(title, body, event) {
    var container = document.getElementById('toast-container');
    if (!container) {
      container = document.createElement('div');
      container.id = 'toast-container';
      document.body.appendChild(container);
    }
    var toast = document.createElement('div');
    toast.className = 'toast toast-' + (event || 'info');
    var titleEl = document.createElement('div');
    titleEl.className = 'toast-title';
    titleEl.textContent = title;
    var bodyEl = document.createElement('div');
    bodyEl.className = 'toast-body';
    bodyEl.textContent = body;
    toast.appendChild(titleEl);
    toast.appendChild(bodyEl);
    toast.onclick = function() { toast.remove(); };
    container.appendChild(toast);
    setTimeout(function() {
      toast.classList.add('toast-fade');
      setTimeout(function() { toast.remove(); }, 300);
    }, 5000);
  }

  function handleNotification(msg) {
    var badge = document.querySelector('.badge');
    if (badge) {
      var count = parseInt(badge.textContent || '0') + 1;
      badge.textContent = count;
    }
    var title = 'websessions: ' + msg.event;
    var body = 'Session ' + msg.sessionID + ': ' + msg.event;
    if ('Notification' in window && Notification.permission === 'granted') {
      new Notification(title, {
        body: body,
        tag: 'ws-' + msg.sessionID + '-' + msg.event,
      });
    } else {
      showToast(title, body, msg.event);
    }
  }

  if ('Notification' in window && Notification.permission === 'default') {
    Notification.requestPermission();
  }

  // Clean up terminals whose DOM elements were removed before swapping new ones in
  document.addEventListener('htmx:beforeSwap', function(event) {
    if (event.detail.target.id !== 'terminal-area') return;
    // Disconnect all terminals in the area being replaced
    for (var sid in terminals) {
      var container = document.getElementById('term-' + sid);
      if (container && event.detail.target.contains(container)) {
        disconnectSession(sid);
      }
    }
  });

  document.addEventListener('htmx:afterSwap', function(event) {
    // Initialize terminal panes after swap
    var panes = event.detail.target.querySelectorAll('.terminal-pane[data-session-id]');
    panes.forEach(function(pane) {
      var sessionID = pane.dataset.sessionId;
      var containerID = 'term-' + sessionID;
      // Always reconnect — old instance was cleaned up in beforeSwap
      if (terminals[sessionID]) {
        disconnectSession(sessionID);
      }
      connectSession(sessionID, containerID);
    });

    // If a terminal was loaded into the terminal area, ensure a tab exists and refresh sidebar
    if (event.detail.target.id === 'terminal-area' && panes.length > 0) {
      var xhr = event.detail.xhr;
      var sid = xhr ? xhr.getResponseHeader('X-Session-ID') : null;
      var sname = xhr ? xhr.getResponseHeader('X-Session-Name') : null;
      var stype = xhr ? xhr.getResponseHeader('X-Session-Type') : null;
      if (!sid) {
        sid = panes[0].dataset.sessionId;
        var titleEl = panes[0].querySelector('.pane-title');
        sname = titleEl ? titleEl.textContent : sid;
      }
      if (sid) {
        // Add tab without reloading terminal (openTab would clear the area)
        var existing = openTabs.find(function(t) { return t.id === sid; });
        var isNew = !existing;
        var provider = providerFromSessionType(stype);
        if (isNew) {
          openTabs.push({ id: sid, name: sname || sid, state: 'running', type: stype || '', provider: provider });
        } else {
          if (stype && !existing.type) existing.type = stype;
          if (provider && !existing.provider) existing.provider = provider;
        }
        activeTabId = sid;
        currentlyShowingTabId = sid;
        saveTabState();
        renderTabs();
        // Only refresh sidebar for new sessions (takeover/create), not regular opens
        if (isNew) {
          refreshSidebar();
        }
      }
    }
  });

  // Close new session modal after successful form submission
  document.addEventListener('htmx:configRequest', function(event) {
    var form = event.detail.elt;
    if (!form || form.id !== 'new-session-form') return;
    if (!event.detail.parameters) event.detail.parameters = {};
    event.detail.parameters.provider = selectedProvider();
  });

  document.addEventListener('htmx:afterRequest', function(event) {
    var form = event.detail.elt;
    if (form && form.id === 'new-session-form' && event.detail.successful) {
      var modal = form.closest('.modal-overlay');
      if (modal) modal.remove();
      // Auto-open the newly created session
      var xhr = event.detail.xhr;
      if (xhr) {
        var sessionID = xhr.getResponseHeader('X-Session-ID');
        var sessionName = xhr.getResponseHeader('X-Session-Name');
        var sessionType = xhr.getResponseHeader('X-Session-Type') || selectedProvider();
        if (sessionID) {
          openTab(sessionID, sessionName || sessionID, 'running', sessionType);
        }
      }
      refreshSidebar();
    }
  });

  var dirDebounce = null;
  function dirAutocomplete(input) {
    clearTimeout(dirDebounce);
    dirDebounce = setTimeout(function() {
      var q = input.value;
      if (!q) return;
      fetch('/api/dirs?q=' + encodeURIComponent(q))
        .then(function(r) { return r.json(); })
        .then(function(dirs) {
          var box = document.getElementById('dir-suggestions');
          if (!box) return;
          while (box.firstChild) box.removeChild(box.firstChild);
          if (!dirs || dirs.length === 0) return;
          dirs.forEach(function(d) {
            var div = document.createElement('div');
            div.className = 'dir-suggestion';
            // Show folder name highlighted, full path muted
            var name = d.split('/').pop();
            var nameSpan = document.createElement('span');
            nameSpan.className = 'dir-name';
            nameSpan.textContent = name;
            var pathSpan = document.createElement('span');
            pathSpan.className = 'dir-path';
            pathSpan.textContent = d;
            div.appendChild(nameSpan);
            div.appendChild(pathSpan);
            // Single click = select this dir and close
            div.addEventListener('click', function(e) {
              e.stopPropagation();
              selectDir(d, false);
            });
            // Double click = drill into subdirectories
            div.addEventListener('dblclick', function(e) {
              e.stopPropagation();
              selectDir(d, true);
            });
            box.appendChild(div);
          });
        });
    }, 200);
  }

  function selectDir(path, drillDown) {
    var input = document.getElementById('work_dir');
    if (input) input.value = path;
    var box = document.getElementById('dir-suggestions');
    if (box) { while (box.firstChild) box.removeChild(box.firstChild); }
    if (drillDown && input) {
      input.value = path + '/';
      dirAutocomplete(input);
    } else {
      var nameInput = document.getElementById('name');
      if (nameInput && !nameInput.value) {
        nameInput.value = path.split('/').pop();
      }
      if (nameInput) nameInput.focus();
      // Load resumable provider sessions for the selected directory
      loadProviderSessions(path);
    }
  }

  function closeDirSuggestions() {
    var box = document.getElementById('dir-suggestions');
    if (box) { while (box.firstChild) box.removeChild(box.firstChild); }
  }

  // Close suggestions when clicking outside
  document.addEventListener('click', function(e) {
    if (!e.target.closest('.form-group')) { closeDirSuggestions(); }
  });

  // Rename session — double-click on pane title
  function startRename(titleEl) {
    var sessionID = titleEl.getAttribute('data-session-id');
    var currentName = titleEl.textContent.trim();
    var input = document.createElement('input');
    input.type = 'text';
    input.value = currentName;
    input.className = 'rename-input';
    input.addEventListener('keydown', function(e) {
      if (e.key === 'Enter') { finishRename(titleEl, input, sessionID); }
      if (e.key === 'Escape') { cancelRename(titleEl, input, currentName); }
    });
    input.addEventListener('blur', function() { finishRename(titleEl, input, sessionID); });
    titleEl.textContent = '';
    titleEl.appendChild(input);
    input.focus();
    input.select();
  }

  function finishRename(titleEl, input, sessionID) {
    var newName = input.value.trim();
    if (!newName) newName = sessionID;
    titleEl.textContent = newName;
    // Persist rename to server
    fetch('/sessions/' + encodeURIComponent(sessionID) + '/rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'name=' + encodeURIComponent(newName),
    }).then(function() {
      // Refresh sidebar to show new name
      refreshSidebar();
    });
  }

  function cancelRename(titleEl, input, originalName) {
    titleEl.textContent = originalName;
  }

  // Populate form fields from a recent project selection
  function quickSession(btn) {
    var dir = btn.getAttribute('data-dir');
    var name = btn.getAttribute('data-name');
    var nameInput = document.getElementById('name');
    var dirInput = document.getElementById('work_dir');
    var promptInput = document.getElementById('prompt');
    var resumeInput = document.getElementById('resume_id');
    if (dirInput) dirInput.value = dir;
    if (nameInput) { nameInput.value = name; nameInput.focus(); nameInput.select(); }
    if (promptInput) promptInput.value = '';
    if (resumeInput) resumeInput.value = '';
    // Load resumable provider sessions for this directory
    loadProviderSessions(dir);
    // Scroll to form
    var form = document.getElementById('new-session-form');
    if (form) form.scrollIntoView({ behavior: 'smooth' });
  }

  // Tab management
  var currentlyShowingTabId = null;

  function openTab(sessionID, name, state) {
    var sessionType = arguments.length > 3 ? arguments[3] : '';
    var focusTarget = sessionID; // remember which session to focus

    // Check if session is inside an existing split group — focus that tab instead
    var groupTab = openTabs.find(function(t) {
      return t.splitTree && treeFind(t.splitTree, sessionID);
    });
    if (groupTab && groupTab.id !== sessionID) {
      sessionID = groupTab.id;
      name = groupTab.name;
      state = groupTab.state;
      sessionType = groupTab.type || sessionType;
    }

    // Add to tabs if not already open
    var existing = openTabs.find(function(t) { return t.id === sessionID; });
    var provider = providerFromSessionType(sessionType);
    if (!existing) {
      openTabs.push({ id: sessionID, name: name || sessionID, state: state || 'running', type: sessionType || '', provider: provider });
      saveTabState();
    } else {
      if (sessionType && !existing.type) existing.type = sessionType;
      if (provider && !existing.provider) existing.provider = provider;
    }

    // If already showing this tab AND its terminal is in the DOM, just focus
    if (sessionID === currentlyShowingTabId) {
      var termInDom = document.getElementById('term-' + focusTarget);
      if (termInDom) {
        focusPane(focusTarget);
        return;
      }
      // Terminal not in DOM — fall through to reload
      currentlyShowingTabId = null;
    }

    activeTabId = sessionID;
    currentlyShowingTabId = sessionID;
    renderTabs();

    var tab = openTabs.find(function(t) { return t.id === sessionID; });

    // Clear the terminal area first
    var area = document.getElementById('terminal-area');
    if (area) {
      for (var sid in terminals) {
        var el = document.getElementById('term-' + sid);
        if (el && area.contains(el)) disconnectSession(sid);
      }
      while (area.firstChild) area.removeChild(area.firstChild);
      area.style.flexDirection = '';
    }

    // If this tab has a split tree, rebuild the split layout
    if (tab && tab.splitTree) {
      rebuildSplitLayout(tab);
      // Focus the target pane after a short delay for DOM to settle
      setTimeout(function() { focusPane(focusTarget); }, 300);
      return;
    }

    // Iframe tab
    if (tab && tab.type === 'iframe') {
      var pane = document.createElement('div');
      pane.className = 'split-pane';
      area.appendChild(pane);
      loadPaneIframe(pane, tab.id, tab.url, tab.name);
      return;
    }

    // Single session tab
    htmx.ajax('POST', '/sessions/' + encodeURIComponent(sessionID) + '/open', {
      target: '#terminal-area',
      swap: 'innerHTML'
    });
    setTimeout(function() { focusPane(focusTarget); }, 300);
  }

  function rebuildSplitLayout(tab) {
    var area = document.getElementById('terminal-area');
    if (!area || !tab.splitTree) return;

    function buildNode(node, container) {
      if (node.type === 'iframe') {
        var pane = document.createElement('div');
        pane.className = 'split-pane';
        container.appendChild(pane);
        loadPaneIframe(pane, node.id, node.url, node.name);
        return pane;
      }
      if (node.type === 'session') {
        var pane = document.createElement('div');
        pane.className = 'split-pane';
        container.appendChild(pane);
        loadPaneSession(pane, node.id);
        return pane;
      }
      // Split node
      container.style.display = 'flex';
      container.style.flexDirection = splitFlex(node.dir);
      container.style.overflow = 'hidden';

      var panes = [];
      node.children.forEach(function(child) {
        var pane = document.createElement('div');
        pane.className = 'split-pane';
        pane.style.overflow = 'hidden';
        container.appendChild(pane);
        panes.push(pane);
        if (child.type === 'split') {
          buildNode(child, pane);
        } else if (child.type === 'iframe') {
          loadPaneIframe(pane, child.id, child.url, child.name);
        } else {
          loadPaneSession(pane, child.id);
        }
      });

      createSplit(panes, node.dir);
    }

    buildNode(tab.splitTree, area);
  }

  function openIframeTab(iframeUrl, title) {
    var id = 'iframe-' + Date.now();
    var name = title || 'Iframe';
    var tab = { id: id, name: name, state: 'iframe', type: 'iframe', url: iframeUrl };
    openTabs.push(tab);
    activeTabId = id;
    currentlyShowingTabId = id;
    renderTabs();

    var area = document.getElementById('terminal-area');
    if (area) {
      for (var sid in terminals) {
        var el = document.getElementById('term-' + sid);
        if (el && area.contains(el)) disconnectSession(sid);
      }
      while (area.firstChild) area.removeChild(area.firstChild);
      area.style.flexDirection = '';
      var pane = document.createElement('div');
      pane.className = 'split-pane';
      area.appendChild(pane);
      loadPaneIframe(pane, id, iframeUrl, name);
    }
    saveTabState();
  }

  function closeTab(sessionID, e) {
    if (e) { e.stopPropagation(); e.preventDefault(); }
    var tab = openTabs.find(function(t) { return t.id === sessionID; });
    var hadGroup = tab && tab.splitTree;
    // Disconnect all sessions in the group
    if (hadGroup) {
      treeSessionIds(tab.splitTree).forEach(function(sid) { if (terminals[sid]) disconnectSession(sid); });
    }
    openTabs = openTabs.filter(function(t) { return t.id !== sessionID; });
    if (terminals[sessionID]) disconnectSession(sessionID);
    // Refresh sidebar to update group tree
    if (hadGroup) {
      saveTabState().then(function() {
        refreshSidebar();
      });
    } else {
      saveTabState();
    }
    if (activeTabId === sessionID) {
      if (openTabs.length > 0) {
        openTab(openTabs[openTabs.length - 1].id, openTabs[openTabs.length - 1].name, openTabs[openTabs.length - 1].state);
      } else {
        activeTabId = null;
        renderTabs();
        var area = document.getElementById('terminal-area');
        if (area) {
          while (area.firstChild) area.removeChild(area.firstChild);
          var empty = document.createElement('div');
          empty.className = 'empty-state';
          var p = document.createElement('p');
          p.textContent = 'Select a session from the sidebar or create a new one';
          empty.appendChild(p);
          area.appendChild(empty);
        }
      }
    } else {
      renderTabs();
    }
  }

  function renderTabs() {
    var bar = document.getElementById('tab-bar');
    if (!bar) return;
    while (bar.firstChild) bar.removeChild(bar.firstChild);
    openTabs.forEach(function(tab) {
      var btn = document.createElement('div');
      btn.className = 'tab' + (tab.id === activeTabId ? ' tab-active' : '');
      btn.setAttribute('data-tab-id', tab.id);
      btn.setAttribute('draggable', 'true');
      btn.addEventListener('click', function() { openTab(tab.id, tab.name, tab.state); });
      btn.addEventListener('dragstart', function(e) { tabDragStart(e, tab.id); });
      btn.addEventListener('dragover', function(e) { tabDragOver(e); });
      btn.addEventListener('drop', function(e) { tabDrop(e, tab.id); });
      btn.addEventListener('dragend', tabDragEnd);

      var dot = document.createElement('span');
      dot.className = 'tab-dot state-' + (tab.state || 'running');
      btn.appendChild(dot);

      var nameSpan = document.createElement('span');
      nameSpan.className = 'tab-name';
      nameSpan.textContent = tab.name;
      nameSpan.addEventListener('dblclick', function(e) {
        e.stopPropagation();
        var input = document.createElement('input');
        input.className = 'tab-rename-input';
        input.value = tab.name;
        input.addEventListener('keydown', function(ev) {
          if (ev.key === 'Enter') {
            tab.name = input.value || tab.name;
            saveTabState();
            renderTabs();
          } else if (ev.key === 'Escape') {
            renderTabs();
          }
        });
        input.addEventListener('blur', function() {
          tab.name = input.value || tab.name;
          saveTabState();
          renderTabs();
        });
        nameSpan.replaceWith(input);
        input.focus();
        input.select();
      });
      btn.appendChild(nameSpan);

      if (tabProvider(tab) === 'opencode') {
        var providerBadge = document.createElement('span');
        providerBadge.className = 'tab-provider-badge';
        providerBadge.textContent = 'OC';
        providerBadge.title = 'OpenCode session';
        btn.appendChild(providerBadge);
      }

      if (tab.splitTree) {
        var ids = treeSessionIds(tab.splitTree);
        if (ids.length > 1) {
          var splitBadge = document.createElement('span');
          splitBadge.className = 'tab-split-badge';
          splitBadge.textContent = ids.length;
          splitBadge.title = ids.join(' | ');
          btn.appendChild(splitBadge);
        }
      }

      var closeSpan = document.createElement('span');
      closeSpan.className = 'tab-close';
      closeSpan.textContent = '\u00d7';
      closeSpan.title = 'Close tab (session keeps running)';
      closeSpan.addEventListener('click', function(e) { closeTab(tab.id, e); });
      btn.appendChild(closeSpan);

      // Right-click context menu
      btn.addEventListener('contextmenu', function(e) {
        e.preventDefault();
        showTabContextMenu(e, tab.id, tab.name);
      });

      bar.appendChild(btn);
    });

    // Add "+" button
    var newBtn = document.createElement('div');
    newBtn.className = 'tab tab-new';
    newBtn.textContent = '+';
    newBtn.addEventListener('click', function() {
      htmx.ajax('GET', '/sessions/new', { target: '#modal', swap: 'innerHTML' });
    });
    bar.appendChild(newBtn);
  }

  // Tab drag and drop (reorder tabs + drop-to-split on terminal area)
  var draggedTabId = null;

  function tabDragStart(e, tabId) {
    draggedTabId = tabId;
    e.target.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', tabId);
    // Show drop zones on terminal area after a short delay
    setTimeout(function() { showDropZones(); }, 100);
  }

  function tabDragOver(e) { e.preventDefault(); e.dataTransfer.dropEffect = 'move'; }

  function tabDrop(e, targetId) {
    e.preventDefault();
    hideDropZones();
    if (!draggedTabId || draggedTabId === targetId) return;
    var fromIdx = openTabs.findIndex(function(t) { return t.id === draggedTabId; });
    var toIdx = openTabs.findIndex(function(t) { return t.id === targetId; });
    if (fromIdx < 0 || toIdx < 0) return;
    var item = openTabs.splice(fromIdx, 1)[0];
    openTabs.splice(toIdx, 0, item);
    saveTabState();
    renderTabs();
  }

  function tabDragEnd(e) {
    e.target.classList.remove('dragging');
    draggedTabId = null;
    hideDropZones();
  }

  // Drop zones on terminal area for split-by-drag
  function showDropZones() {
    var area = document.getElementById('terminal-area');
    if (!area || !draggedTabId) return;
    // Don't show if no active session to split with
    if (!activeTabId || activeTabId === draggedTabId) return;

    var zones = document.createElement('div');
    zones.id = 'drop-zones';
    zones.className = 'drop-zones';

    ['left', 'right', 'top', 'bottom'].forEach(function(side) {
      var zone = document.createElement('div');
      zone.className = 'drop-zone drop-zone-' + side;
      zone.addEventListener('dragover', function(e) {
        e.preventDefault();
        e.stopPropagation();
        e.dataTransfer.dropEffect = 'move';
        zone.classList.add('drop-zone-active');
      });
      zone.addEventListener('dragleave', function() {
        zone.classList.remove('drop-zone-active');
      });
      zone.addEventListener('drop', function(e) {
        e.preventDefault();
        e.stopPropagation();
        hideDropZones();
        if (!draggedTabId || draggedTabId === activeTabId) return;
        var direction = (side === 'left' || side === 'right') ? 'horizontal' : 'vertical';
        // For left/top, the dragged tab goes first; for right/bottom, it goes second
        if (side === 'left' || side === 'top') {
          doSplit(draggedTabId, activeTabId, direction);
        } else {
          doSplit(activeTabId, draggedTabId, direction);
        }
        draggedTabId = null;
      });
      zones.appendChild(zone);
    });

    area.style.position = 'relative';
    area.appendChild(zones);
  }

  function hideDropZones() {
    var zones = document.getElementById('drop-zones');
    if (zones) zones.remove();
  }

  function saveTabState() {
    var tabsJson = JSON.stringify(openTabs);
    var activeJson = activeTabId || '';
    try { localStorage.setItem('ws-open-tabs', tabsJson); } catch(e) {}
    try { localStorage.setItem('ws-active-tab', activeJson); } catch(e) {}
    return Promise.all([
      savePref('open-tabs', tabsJson),
      savePref('active-tab', activeJson),
    ]);
  }

  function loadTabState() {
    // Load from localStorage first (fast), then server overrides
    try {
      var saved = JSON.parse(localStorage.getItem('ws-open-tabs'));
      if (saved && saved.length) openTabs = saved;
      activeTabId = localStorage.getItem('ws-active-tab') || null;
    } catch(e) {}
  }

  // Sync tab state from server (runs after initial load)
  function syncTabStateFromServer() {
    fetch('/api/preferences').then(function(r) { return r.json(); }).then(function(prefs) {
      if (prefs['open-tabs']) {
        try {
          var serverTabs = JSON.parse(prefs['open-tabs']);
          if (serverTabs && serverTabs.length) {
            // Merge: prefer local split trees over server ones (server may be stale)
            var localMap = {};
            openTabs.forEach(function(t) { localMap[t.id] = t; });
            serverTabs.forEach(function(st) {
              var local = localMap[st.id];
              if (local && local.splitTree && !st.splitTree) {
                st.splitTree = local.splitTree;
              }
            });
            openTabs = serverTabs;
            // Push merged state back to server
            saveTabState();
          }
        } catch(e) {}
      }
      if (prefs['active-tab']) {
        activeTabId = prefs['active-tab'];
        try { localStorage.setItem('ws-active-tab', activeTabId); } catch(e) {}
      }
      renderTabs();
      if (activeTabId) {
        currentlyShowingTabId = null;
        openTab(activeTabId);
      }
    }).catch(function() {});
  }

  // Prune tabs whose sessions no longer exist on the server
  function pruneDeadTabs() {
    fetch('/api/sessions')
      .then(function(r) { return r.json(); })
      .then(function(sessions) {
        var activeIds = {};
        (sessions || []).forEach(function(s) { activeIds[s.id] = true; });
        var changed = false;
        openTabs = openTabs.filter(function(tab) {
          // Iframe tabs have no server-side session — keep them
          if (tab.type === 'iframe') return true;
          // For split tabs, check if at least one session is alive
          if (tab.splitTree) {
            var ids = treeSessionIds(tab.splitTree);
            var anyAlive = ids.some(function(id) { return activeIds[id]; });
            if (!anyAlive) { changed = true; return false; }
            return true;
          }
          if (!activeIds[tab.id]) { changed = true; return false; }
          return true;
        });
        if (changed) {
          if (activeTabId && !openTabs.find(function(t) { return t.id === activeTabId; })) {
            activeTabId = openTabs.length > 0 ? openTabs[0].id : null;
          }
          saveTabState();
          renderTabs();
          if (activeTabId) {
            currentlyShowingTabId = null;
            openTab(activeTabId);
          }
        }
      }).catch(function() {});
  }

  // Load tabs on page load (localStorage for instant render, then sync from server)
  loadTabState();
  document.addEventListener('DOMContentLoaded', function() {
    renderTabs();
    // Fix sidebar nesting (browser parser breaks it on initial load due to inline scripts)
    refreshSidebar();
    // Prune dead tabs first, then restore active tab
    pruneDeadTabs();
    // Reopen the active tab (handles both single and split tabs)
    if (activeTabId) {
      currentlyShowingTabId = null; // force re-render
      openTab(activeTabId);
    }
    // Sync from server (authoritative) — overrides localStorage if different
    syncTabStateFromServer();
  });

  // Quick terminal creation
  function openTerminal() {
    var form = new FormData();
    form.append('work_dir', '~');
    fetch('/sessions/terminal', { method: 'POST', body: form })
      .then(function(r) {
        var sid = r.headers.get('X-Session-ID');
        var sname = r.headers.get('X-Session-Name');
        var stype = r.headers.get('X-Session-Type');
        if (sid) {
          openTab(sid, sname || 'terminal', 'running', stype || 'terminal');
        }
        refreshSidebar();
        return r.text(); // consume body
      });
  }

  function killAllSessions() {
    // Count running sessions
    var running = openTabs.filter(function(t) { return t.state === 'running' || t.state === 'waiting'; });
    var count = document.querySelectorAll('.session-item.state-running, .session-item.state-waiting').length;
    if (count === 0 && running.length === 0) {
      showToast('No sessions', 'No running sessions to kill.', 'info');
      return;
    }

    // Show confirmation modal
    var modal = document.getElementById('modal');
    if (!modal) return;
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    var dialog = document.createElement('div');
    dialog.className = 'modal-dialog kill-all-dialog';

    var title = document.createElement('h3');
    title.textContent = 'Kill all running sessions?';
    title.className = 'modal-title';

    var desc = document.createElement('p');
    desc.className = 'modal-desc';
    desc.textContent = 'This will terminate all running and waiting provider sessions. This action cannot be undone.';

    var actions = document.createElement('div');
    actions.className = 'modal-actions';

    var cancelBtn = document.createElement('button');
    cancelBtn.className = 'btn-cancel';
    cancelBtn.textContent = 'Cancel';
    cancelBtn.onclick = function() { modal.textContent = ''; };

    var confirmBtn = document.createElement('button');
    confirmBtn.className = 'btn-danger';
    confirmBtn.textContent = 'Kill All Sessions';
    confirmBtn.onclick = function() {
      confirmBtn.textContent = 'Killing...';
      confirmBtn.disabled = true;
      fetch('/api/kill-all', { method: 'POST' })
        .then(function(r) { return r.json(); })
        .then(function(data) {
          modal.textContent = '';
          // Close all tabs
          openTabs.forEach(function(t) { if (terminals[t.id]) disconnectSession(t.id); });
          openTabs = [];
          activeTabId = null;
          saveTabState();
          renderTabs();
          document.getElementById('terminal-area').innerHTML = '<div class="empty-state"><p>All sessions killed</p></div>';
          refreshSidebar();
          showToast('Sessions killed', data.killed + ' session(s) terminated.', 'completed');
        });
    };

    actions.appendChild(cancelBtn);
    actions.appendChild(confirmBtn);
    dialog.appendChild(title);
    dialog.appendChild(desc);
    dialog.appendChild(actions);
    overlay.appendChild(dialog);
    overlay.onclick = function(e) { if (e.target === overlay) modal.textContent = ''; };
    modal.textContent = '';
    modal.appendChild(overlay);
  }

  function killSession(sessionID) {
    if (!confirm('Kill session "' + sessionID + '"?')) return;
    fetch('/sessions/' + encodeURIComponent(sessionID) + '/kill', { method: 'POST' })
      .then(function(r) {
        if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
        // If session is in a split group, remove the pane; otherwise close the tab
        var groupTab = openTabs.find(function(t) {
          return t.splitTree && treeFind(t.splitTree, sessionID);
        });
        if (groupTab) {
          unsplitPane(sessionID);
        } else {
          closeTab(sessionID);
        }
        setTimeout(function() {
          refreshSidebar();
        }, 500);
      })
      .catch(function(err) { console.error('kill failed:', err); });
  }

  // Close a split pane without killing the session
  function unsplitPane(sessionID) {
    var area = document.getElementById('terminal-area');
    if (!area) return;

    // Find which tab owns this session
    var tab = openTabs.find(function(t) {
      return t.splitTree && treeFind(t.splitTree, sessionID);
    });

    // Not in a split — just close the tab
    if (!tab) {
      closeTab(sessionID);
      return;
    }

    // Disconnect the terminal being removed
    if (terminals[sessionID]) disconnectSession(sessionID);

    // Remove from split tree
    treeRemove(tab.splitTree, sessionID);

    // If tree collapsed to a single session, clear it
    if (tab.splitTree && tab.splitTree.type === 'session') {
      tab.splitTree = null;
    }
    saveTabState();

    // Disconnect all remaining terminals and rebuild the layout
    var remainingIds = tab.splitTree ? treeSessionIds(tab.splitTree) : [tab.id];
    remainingIds.forEach(function(sid) {
      if (terminals[sid]) disconnectSession(sid);
    });

    // Clear and rebuild
    while (area.firstChild) area.removeChild(area.firstChild);
    area.style.flexDirection = '';
    currentlyShowingTabId = null;
    // Save then refresh sidebar after persistence
    saveTabState().then(function() {
      refreshSidebar();
    });
    openTab(tab.id);
  }

  // Git diff viewer
  function showDiff(sessionID) {
    fetch('/sessions/' + encodeURIComponent(sessionID) + '/diff')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        var overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.addEventListener('click', function() { overlay.remove(); });

        var content = document.createElement('div');
        content.className = 'modal-content diff-modal';
        content.addEventListener('click', function(e) { e.stopPropagation(); });

        var header = document.createElement('div');
        header.className = 'diff-header';

        var title = document.createElement('h2');
        title.textContent = 'Changes in ' + data.work_dir.split('/').pop();
        header.appendChild(title);

        var closeBtn = document.createElement('button');
        closeBtn.className = 'btn-cancel';
        closeBtn.textContent = 'Close';
        closeBtn.addEventListener('click', function() { overlay.remove(); });
        header.appendChild(closeBtn);

        content.appendChild(header);

        // File list summary
        if (data.files && data.files.length > 0) {
          var summary = document.createElement('div');
          summary.className = 'diff-summary';
          data.files.forEach(function(f) {
            var fileDiv = document.createElement('div');
            fileDiv.className = 'diff-file-entry';
            var status = f.substring(0, 2).trim();
            var fname = f.substring(3);
            var statusSpan = document.createElement('span');
            statusSpan.className = 'diff-file-status diff-status-' + status.charAt(0).toLowerCase();
            statusSpan.textContent = status;
            var nameSpan = document.createElement('span');
            nameSpan.textContent = fname;
            fileDiv.appendChild(statusSpan);
            fileDiv.appendChild(nameSpan);
            summary.appendChild(fileDiv);
          });
          content.appendChild(summary);
        }

        // Diff output
        if (data.diff) {
          var diffContainer = document.createElement('div');
          diffContainer.className = 'diff-content';
          renderDiff(diffContainer, data.diff);
          content.appendChild(diffContainer);
        } else {
          var noChanges = document.createElement('p');
          noChanges.className = 'diff-empty';
          noChanges.textContent = 'No changes detected';
          content.appendChild(noChanges);
        }

        overlay.appendChild(content);
        document.body.appendChild(overlay);
      });
  }

  function renderDiff(container, diffText) {
    var lines = diffText.split('\n');
    var pre = document.createElement('pre');
    pre.className = 'diff-pre';

    lines.forEach(function(line) {
      var span = document.createElement('div');
      span.className = 'diff-line';
      if (line.startsWith('+++') || line.startsWith('---')) {
        span.className += ' diff-line-file';
      } else if (line.startsWith('@@')) {
        span.className += ' diff-line-hunk';
      } else if (line.startsWith('+')) {
        span.className += ' diff-line-add';
      } else if (line.startsWith('-')) {
        span.className += ' diff-line-del';
      } else if (line.startsWith('diff ')) {
        span.className += ' diff-line-header';
      }
      span.textContent = line;
      pre.appendChild(span);
    });

    container.appendChild(pre);
  }

  // Update check
  function checkForUpdate() {
    var feedback = document.getElementById('update-feedback');
    var status = document.getElementById('update-status');
    if (feedback) { feedback.textContent = 'Checking...'; feedback.className = 'hooks-feedback'; }

    fetch('/api/check-update')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        if (data.error) {
          if (feedback) { feedback.textContent = 'Error: ' + data.error; feedback.className = 'hooks-feedback hooks-feedback-err'; }
          return;
        }
        if (!data.UpdateAvail) {
          if (feedback) { feedback.textContent = 'You are on the latest version (' + data.LatestVersion + ')'; feedback.className = 'hooks-feedback hooks-feedback-ok'; }
          return;
        }
        // Update available — show details and update button
        if (feedback) { feedback.className = ''; }
        if (status) {
          while (status.firstChild) status.removeChild(status.firstChild);

          var info = document.createElement('div');
          info.className = 'update-available';

          var badge = document.createElement('span');
          badge.className = 'hooks-badge hooks-active';
          badge.textContent = data.LatestVersion + ' available';
          info.appendChild(badge);

          var btn = document.createElement('button');
          btn.type = 'button';
          btn.className = 'btn-create btn-small';
          btn.textContent = 'Update now';
          btn.addEventListener('click', function() { selfUpdate(feedback); });
          info.appendChild(btn);

          var link = document.createElement('a');
          link.href = data.ReleaseURL;
          link.target = '_blank';
          link.className = 'update-release-link';
          link.textContent = 'Release notes';
          info.appendChild(link);

          status.appendChild(info);
        }
      })
      .catch(function(err) {
        if (feedback) { feedback.textContent = 'Error: ' + err.message; feedback.className = 'hooks-feedback hooks-feedback-err'; }
      });
  }

  function selfUpdate(feedback) {
    if (feedback) { feedback.textContent = 'Downloading update...'; feedback.className = 'hooks-feedback'; }

    fetch('/api/self-update', { method: 'POST' })
      .then(function(r) { return r.json(); })
      .then(function(data) {
        if (data.error) {
          if (feedback) { feedback.textContent = 'Error: ' + data.error; feedback.className = 'hooks-feedback hooks-feedback-err'; }
          return;
        }
        if (feedback) {
          feedback.textContent = data.message;
          feedback.className = 'hooks-feedback hooks-feedback-ok';
        }
      })
      .catch(function(err) {
        if (feedback) { feedback.textContent = 'Error: ' + err.message; feedback.className = 'hooks-feedback hooks-feedback-err'; }
      });
  }

  // Background service management (systemd on Linux, launchd on macOS)
  function manageService(action) {
    var feedback = document.getElementById('service-feedback');
    if (feedback) { feedback.textContent = 'Processing...'; feedback.className = 'hooks-feedback'; }

    fetch('/settings/service', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action }),
    })
    .then(function(r) { return r.json().then(function(d) { return { ok: r.ok, data: d }; }); })
    .then(function(result) {
      if (feedback) {
        if (result.ok) {
          feedback.textContent = 'Done: ' + result.data.status;
          feedback.className = 'hooks-feedback hooks-feedback-ok';
        } else {
          feedback.textContent = 'Error: ' + (result.data.error || 'Unknown');
          feedback.className = 'hooks-feedback hooks-feedback-err';
        }
        setTimeout(function() { feedback.textContent = ''; feedback.className = ''; }, 4000);
      }
      // Reload settings page to update buttons
      setTimeout(function() { window.location.reload(); }, 500);
    })
    .catch(function(err) {
      if (feedback) {
        feedback.textContent = 'Error: ' + err.message;
        feedback.className = 'hooks-feedback hooks-feedback-err';
      }
    });
  }

  function selectedProvider() {
    var providerInput = document.getElementById('provider');
    if (!providerInput || !providerInput.value) return 'claude';
    return providerInput.value;
  }

  function providerLabel(provider) {
    if (provider === 'opencode') return 'OpenCode';
    return 'Claude';
  }

  function providerFromSessionType(sessionType) {
    if (sessionType === 'opencode') return 'opencode';
    if (sessionType === 'claude') return 'claude';
    return '';
  }

  function tabProvider(tab) {
    if (!tab) return '';
    if (tab.provider === 'opencode' || tab.provider === 'claude') return tab.provider;
    return providerFromSessionType(tab.type);
  }

  function setProviderSessionsLabel(provider) {
    var label = document.getElementById('provider-sessions-label');
    if (!label) return;
    label.textContent = 'Resume previous ' + providerLabel(provider) + ' session';
  }

  function renderProviderSessions(list, sessions) {
    while (list.firstChild) list.removeChild(list.firstChild);
    sessions.forEach(function(s) {
      var div = document.createElement('div');
      div.className = 'recent-item provider-session-item';
      div.setAttribute('role', 'button');
      div.setAttribute('tabindex', '0');

      var externalID = s.external_session_id || s.id || '';
      var nameSpan = document.createElement('span');
      nameSpan.className = 'recent-name';
      nameSpan.textContent = s.summary || (externalID ? externalID.substring(0, 8) + '...' : 'Previous session');

      var metaSpan = document.createElement('span');
      metaSpan.className = 'recent-path';
      metaSpan.textContent = s.date + ' · ' + s.size_kb + 'KB';

      div.appendChild(nameSpan);
      div.appendChild(metaSpan);

      div.addEventListener('click', function() {
        var resumeInput = document.getElementById('resume_id');
        if (resumeInput) resumeInput.value = externalID;
        list.querySelectorAll('.provider-session-item').forEach(function(el) {
          el.classList.remove('selected');
        });
        div.classList.add('selected');
      });
      list.appendChild(div);
    });
  }

  function loadProviderSessions(dir, provider) {
    var resolvedDir = dir || ((document.getElementById('work_dir') || {}).value || '');
    var resolvedProvider = provider || selectedProvider();
    var section = document.getElementById('provider-sessions-section');
    var list = document.getElementById('provider-sessions-list');
    if (!section || !list) return;

    setProviderSessionsLabel(resolvedProvider);

    if (!resolvedDir) {
      section.style.display = 'none';
      return;
    }

    fetch('/api/provider-sessions?provider=' + encodeURIComponent(resolvedProvider) + '&work_dir=' + encodeURIComponent(resolvedDir))
      .then(function(r) {
        if (!r.ok) throw new Error('provider sessions unavailable');
        return r.json();
      })
      .catch(function() {
        if (resolvedProvider !== 'claude') return [];
        return fetch('/api/claude-sessions?dir=' + encodeURIComponent(resolvedDir))
          .then(function(r) {
            if (!r.ok) throw new Error('claude sessions unavailable');
            return r.json();
          })
          .catch(function() { return []; });
      })
      .then(function(sessions) {
        var resumeInput = document.getElementById('resume_id');
        if (resumeInput) resumeInput.value = '';
        while (list.firstChild) list.removeChild(list.firstChild);
        if (!sessions || sessions.length === 0) {
          section.style.display = 'none';
          return;
        }
        section.style.display = 'block';
        renderProviderSessions(list, sessions);
      });
  }

  function providerChanged(provider) {
    var normalizedProvider = provider || selectedProvider();
    var resumeInput = document.getElementById('resume_id');
    if (resumeInput) resumeInput.value = '';
    setProviderSessionsLabel(normalizedProvider);
    loadProviderSessions(null, normalizedProvider);
  }

  // Backward-compatible wrapper used by older templates.
  function loadClaudeSessions(dir) {
    loadProviderSessions(dir, 'claude');
  }

  // Hooks management
  function manageHooks(action) {
    var feedback = document.getElementById('hooks-feedback');
    if (feedback) { feedback.textContent = action === 'install' ? 'Installing...' : 'Uninstalling...'; feedback.className = 'hooks-feedback'; }

    fetch('/settings/hooks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action }),
    })
    .then(function(r) { return r.json().then(function(d) { return { ok: r.ok, data: d }; }); })
    .then(function(result) {
      var status = document.getElementById('hooks-status');
      if (!status) return;
      while (status.firstChild) status.removeChild(status.firstChild);

      if (result.ok && result.data.installed) {
        var badge = document.createElement('span');
        badge.className = 'hooks-badge hooks-active';
        badge.textContent = 'Installed';
        status.appendChild(badge);

        var uninstBtn = document.createElement('button');
        uninstBtn.type = 'button';
        uninstBtn.className = 'btn-cancel btn-small';
        uninstBtn.textContent = 'Uninstall';
        uninstBtn.addEventListener('click', function() { manageHooks('uninstall'); });
        status.appendChild(uninstBtn);

      } else {
        var badge2 = document.createElement('span');
        badge2.className = 'hooks-badge hooks-inactive';
        badge2.textContent = 'Not installed';
        status.appendChild(badge2);

        var instBtn = document.createElement('button');
        instBtn.type = 'button';
        instBtn.className = 'btn-create btn-small';
        instBtn.textContent = 'Install Hooks';
        instBtn.addEventListener('click', function() { manageHooks('install'); });
        status.appendChild(instBtn);
      }

      if (feedback) {
        if (result.ok) {
          feedback.textContent = action === 'install' ? 'Hooks installed successfully' : 'Hooks removed successfully';
          feedback.className = 'hooks-feedback hooks-feedback-ok';
        } else {
          feedback.textContent = 'Error: ' + (result.data.error || 'Unknown error');
          feedback.className = 'hooks-feedback hooks-feedback-err';
        }
        setTimeout(function() { feedback.textContent = ''; feedback.className = ''; }, 4000);
      }
    })
    .catch(function(err) {
      if (feedback) {
        feedback.textContent = 'Error: ' + err.message;
        feedback.className = 'hooks-feedback hooks-feedback-err';
      }
    });
  }

  function managePlannotator(action) {
    var feedback = document.getElementById('plannotator-feedback');
    if (feedback) { feedback.textContent = action === 'enable' ? 'Enabling...' : 'Disabling...'; feedback.className = 'hooks-feedback'; }

    fetch('/settings/plannotator', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: action }),
    })
    .then(function(r) { return r.json().then(function(d) { return { ok: r.ok, data: d }; }); })
    .then(function(result) {
      var status = document.getElementById('plannotator-status');
      if (!status) return;
      while (status.firstChild) status.removeChild(status.firstChild);

      if (result.ok && result.data.enabled) {
        var badge = document.createElement('span');
        badge.className = 'hooks-badge hooks-active';
        badge.textContent = 'Enabled';
        status.appendChild(badge);

        var disableBtn = document.createElement('button');
        disableBtn.type = 'button';
        disableBtn.className = 'btn-cancel btn-small';
        disableBtn.textContent = 'Disable';
        disableBtn.addEventListener('click', function() { managePlannotator('disable'); });
        status.appendChild(disableBtn);
      } else {
        var badge2 = document.createElement('span');
        badge2.className = 'hooks-badge hooks-inactive';
        badge2.textContent = 'Disabled';
        status.appendChild(badge2);

        var enableBtn = document.createElement('button');
        enableBtn.type = 'button';
        enableBtn.className = 'btn-create btn-small';
        enableBtn.textContent = 'Enable';
        enableBtn.addEventListener('click', function() { managePlannotator('enable'); });
        status.appendChild(enableBtn);
      }

      if (feedback) {
        if (result.ok) {
          feedback.textContent = action === 'enable' ? 'Plannotator integration enabled' : 'Plannotator integration disabled';
          feedback.className = 'hooks-feedback hooks-feedback-ok';
        } else {
          feedback.textContent = 'Error: ' + (result.data.error || 'Unknown error');
          feedback.className = 'hooks-feedback hooks-feedback-err';
        }
        setTimeout(function() { feedback.textContent = ''; feedback.className = ''; }, 4000);
      }
    })
    .catch(function(err) {
      if (feedback) {
        feedback.textContent = 'Error: ' + err.message;
        feedback.className = 'hooks-feedback hooks-feedback-err';
      }
    });
  }

  // Settings page directory picker
  var settingsDirDebounce = null;
  function settingsDirAutocomplete(input) {
    clearTimeout(settingsDirDebounce);
    settingsDirDebounce = setTimeout(function() {
      var q = input.value;
      if (!q) return;
      fetch('/api/dirs?q=' + encodeURIComponent(q))
        .then(function(r) { return r.json(); })
        .then(function(dirs) {
          var box = document.getElementById('settings-dir-suggestions');
          if (!box) return;
          while (box.firstChild) box.removeChild(box.firstChild);
          if (!dirs || dirs.length === 0) return;
          dirs.forEach(function(d) {
            var div = document.createElement('div');
            div.className = 'dir-suggestion';
            var nameSpan = document.createElement('span');
            nameSpan.className = 'dir-name';
            nameSpan.textContent = d.split('/').pop();
            var pathSpan = document.createElement('span');
            pathSpan.className = 'dir-path';
            pathSpan.textContent = d;
            div.appendChild(nameSpan);
            div.appendChild(pathSpan);
            div.addEventListener('click', function(e) {
              e.stopPropagation();
              input.value = d;
              while (box.firstChild) box.removeChild(box.firstChild);
            });
            div.addEventListener('dblclick', function(e) {
              e.stopPropagation();
              input.value = d + '/';
              settingsDirAutocomplete(input);
            });
            box.appendChild(div);
          });
        });
    }, 200);
  }

  // Tab context menu
  function showTabContextMenu(e, tabId, tabName) {
    closeTabContextMenu();
    var menu = document.createElement('div');
    menu.id = 'tab-context-menu';
    menu.className = 'tab-context-menu';
    menu.style.left = e.clientX + 'px';
    menu.style.top = e.clientY + 'px';

    var items = [
      { label: 'Close tab', action: function() { closeTab(tabId); } },
      { label: 'Close & stop session', cls: 'ctx-danger', action: function() { killSession(tabId); } },
      { type: 'separator' },
      { label: 'Close other tabs', action: function() {
        var keep = openTabs.filter(function(t) { return t.id === tabId; });
        openTabs.forEach(function(t) { if (t.id !== tabId && terminals[t.id]) disconnectSession(t.id); });
        openTabs = keep;
        activeTabId = tabId;
        saveTabState();
        renderTabs();
        openTab(tabId, tabName, 'running');
      }},
      { label: 'Close all tabs', action: function() {
        openTabs.forEach(function(t) { if (terminals[t.id]) disconnectSession(t.id); });
        openTabs = [];
        activeTabId = null;
        saveTabState();
        renderTabs();
        var area = document.getElementById('terminal-area');
        if (area) {
          while (area.firstChild) area.removeChild(area.firstChild);
          var empty = document.createElement('div');
          empty.className = 'empty-state';
          var p = document.createElement('p');
          p.textContent = 'Select a session from the sidebar or create a new one';
          empty.appendChild(p);
          area.appendChild(empty);
        }
      }},
    ];

    items.forEach(function(item) {
      if (item.type === 'separator') {
        var sep = document.createElement('div');
        sep.className = 'ctx-separator';
        menu.appendChild(sep);
        return;
      }
      var el = document.createElement('div');
      el.className = 'ctx-item' + (item.cls ? ' ' + item.cls : '');
      el.textContent = item.label;
      el.addEventListener('click', function(ev) {
        ev.stopPropagation();
        closeTabContextMenu();
        item.action();
      });
      menu.appendChild(el);
    });

    document.body.appendChild(menu);

    // Close on next click anywhere
    setTimeout(function() {
      document.addEventListener('click', closeTabContextMenu, { once: true });
    }, 0);
  }

  function closeTabContextMenu() {
    var menu = document.getElementById('tab-context-menu');
    if (menu) menu.remove();
  }

  // Focus a session from a notification click
  function focusNotifSession(sessionID, sessionName, eventType) {
    toggleNotifications(); // close the dropdown

    // For active sessions (waiting), open the tab normally
    if (eventType === 'waiting') {
      openTab(sessionID, sessionName, eventType);
      return;
    }

    // For finished sessions, check if it's still active
    fetch('/api/sessions')
      .then(function(r) { return r.json(); })
      .then(function(sessions) {
        var active = sessions && sessions.find(function(s) { return s.id === sessionID; });
        if (active) {
          // Still in active manager — open normally
          openTab(sessionID, sessionName, active.state);
        } else {
          // Not active — switch to History tab and highlight
          var historyBtn = document.querySelector('.sidebar-tab:nth-child(2)');
          if (historyBtn) switchSidebarTab('history', historyBtn);
          // Scroll to the session in history if visible
          setTimeout(function() {
            var items = document.querySelectorAll('.history-list .session-item');
            items.forEach(function(item) {
              // Flash highlight matching session
              var nameEl = item.querySelector('.session-name');
              if (nameEl && nameEl.textContent.trim() === sessionName) {
                item.scrollIntoView({ behavior: 'smooth', block: 'center' });
                item.classList.add('notif-highlight');
                setTimeout(function() { item.classList.remove('notif-highlight'); }, 2000);
              }
            });
          }, 200);
        }
      })
      .catch(function() {
        // Fallback — try opening anyway
        openTab(sessionID, sessionName, eventType);
      });
  }

  // Notification panel
  var notifOpen = false;
  var shortcutsOpen = false;
  function toggleShortcuts() {
    var dropdown = document.getElementById('shortcuts-dropdown');
    if (!dropdown) return;
    shortcutsOpen = !shortcutsOpen;
    dropdown.classList.toggle('open', shortcutsOpen);
    if (shortcutsOpen) {
      setTimeout(function() {
        document.addEventListener('click', closeShortcutsOnOutside, { once: true });
      }, 0);
    }
  }
  function closeShortcutsOnOutside(e) {
    var wrapper = document.querySelector('.shortcuts-wrapper');
    if (wrapper && !wrapper.contains(e.target)) {
      shortcutsOpen = false;
      var dropdown = document.getElementById('shortcuts-dropdown');
      if (dropdown) dropdown.classList.remove('open');
    }
  }

  function toggleNotifications() {
    var dropdown = document.getElementById('notification-dropdown');
    if (!dropdown) return;
    if (notifOpen) {
      while (dropdown.firstChild) dropdown.removeChild(dropdown.firstChild);
      dropdown.classList.remove('dropdown-open');
      notifOpen = false;
    } else {
      dropdown.classList.add('dropdown-open');
      notifOpen = true;
      htmx.ajax('GET', '/notifications', { target: '#notification-dropdown', swap: 'innerHTML' });
    }
  }

  function snoozeNotification(id, sessionID) {
    fetch('/notifications/snooze', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: sessionID, minutes: 15 }),
    }).then(function() {
      // Dismiss the notification from the panel
      dismissNotification(id);
    });
  }

  function dismissNotification(id) {
    fetch('/notifications/' + id + '/read', { method: 'POST' })
      .then(function() {
        var el = document.getElementById('notif-' + id);
        if (el) el.remove();
        updateBadgeCount(-1);
        // If no more items, show empty state
        var body = document.querySelector('.notif-panel-body');
        if (body && !body.querySelector('.notif-item')) {
          while (body.firstChild) body.removeChild(body.firstChild);
          var empty = document.createElement('div');
          empty.className = 'notif-empty';
          var span = document.createElement('span');
          span.textContent = 'No notifications';
          empty.appendChild(span);
          body.appendChild(empty);
          // Hide clear all button
          var clearBtn = document.querySelector('.notif-clear-all');
          if (clearBtn) clearBtn.style.display = 'none';
        }
      });
  }

  function clearAllNotifications() {
    fetch('/notifications/clear', { method: 'POST' })
      .then(function() {
        var dropdown = document.getElementById('notification-dropdown');
        if (dropdown) {
          htmx.ajax('GET', '/notifications', { target: '#notification-dropdown', swap: 'innerHTML' });
        }
        // Reset badge
        var badge = document.getElementById('notif-badge');
        if (badge) badge.remove();
        updateFaviconBadge(0);
      });
  }

  function updateBadgeCount(delta) {
    var badge = document.getElementById('notif-badge');
    if (!badge) return;
    var count = parseInt(badge.textContent || '0') + delta;
    if (count <= 0) {
      badge.remove();
    } else {
      badge.textContent = String(count);
    }
  }

  // Close notification panel when clicking outside
  document.addEventListener('click', function(e) {
    if (notifOpen && !e.target.closest('.notification-wrapper')) {
      var dropdown = document.getElementById('notification-dropdown');
      if (dropdown) {
        while (dropdown.firstChild) dropdown.removeChild(dropdown.firstChild);
        dropdown.classList.remove('dropdown-open');
      }
      notifOpen = false;
    }
  });

  // Session search/filter
  // Collapsed group state persisted in localStorage
  function getCollapsedGroups() {
    try { return JSON.parse(localStorage.getItem('ws-collapsed-groups') || '{}'); } catch(e) { return {}; }
  }
  function saveCollapsedGroups(collapsed) {
    try { localStorage.setItem('ws-collapsed-groups', JSON.stringify(collapsed)); } catch(e) {}
  }
  function toggleGroupCollapsed(header) {
    var group = header.parentElement;
    group.classList.toggle('collapsed');
    var name = group.querySelector('.session-group-name');
    if (name) {
      var collapsed = getCollapsedGroups();
      if (group.classList.contains('collapsed')) {
        collapsed[name.textContent] = true;
      } else {
        delete collapsed[name.textContent];
      }
      saveCollapsedGroups(collapsed);
    }
  }

  // Refresh sidebar using plain fetch + innerHTML (not htmx, which flattens nested groups)
  function refreshSidebar() {
    fetch('/sidebar').then(function(r) { return r.text(); }).then(function(html) {
      var sidebar = document.getElementById('sidebar');
      if (sidebar) {
        sidebar.innerHTML = html;
        htmx.process(sidebar);
        // Restore collapsed state from localStorage
        var collapsed = getCollapsedGroups();
        sidebar.querySelectorAll('.session-group').forEach(function(g) {
          var name = g.querySelector('.session-group-name');
          if (name && collapsed[name.textContent]) {
            g.classList.add('collapsed');
          }
        });
        restoreSidebarTab();
      }
    }).catch(function() {});
  }

  // Periodic sidebar refresh
  setInterval(refreshSidebar, 30000);

  function filterSessions(query) {
    var q = (query || '').toLowerCase();
    var items = document.querySelectorAll('.session-item');
    items.forEach(function(item) {
      var name = (item.querySelector('.session-name') || {}).textContent || '';
      var dir = (item.querySelector('.session-dir') || {}).textContent || '';
      if (!q || name.toLowerCase().indexOf(q) >= 0 || dir.toLowerCase().indexOf(q) >= 0) {
        item.style.display = '';
      } else {
        item.style.display = 'none';
      }
    });
  }

  // Sidebar tab switching
  function switchSidebarTab(tabName, btn) {
    // Toggle tab buttons
    var tabs = document.querySelectorAll('.sidebar-tab');
    tabs.forEach(function(t) { t.classList.remove('sidebar-tab-active'); });
    if (btn) btn.classList.add('sidebar-tab-active');
    // Toggle panels
    document.querySelectorAll('.sidebar-panel').forEach(function(p) { p.classList.remove('sidebar-panel-visible'); });
    var panel = document.getElementById('sidebar-' + tabName);
    if (panel) panel.classList.add('sidebar-panel-visible');
    // Remember which tab is active
    try { localStorage.setItem('ws-sidebar-tab', tabName); } catch(e) {}
  }

  // Restore sidebar tab on load/refresh
  function restoreSidebarTab() {
    var saved = null;
    try { saved = localStorage.getItem('ws-sidebar-tab'); } catch(e) {}
    if (saved && saved !== 'active') {
      var btn = document.querySelector('.sidebar-tab:nth-child(2)');
      switchSidebarTab(saved, btn);
    }
  }

  // Drag and drop reordering
  var draggedEl = null;

  function dragStart(e) {
    draggedEl = e.target.closest('.session-item');
    if (!draggedEl) return;
    draggedEl.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', ''); // required for Firefox
  }

  function dragOver(e) {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    var target = e.target.closest('.session-item');
    if (!target || target === draggedEl) return;
    var list = target.parentNode;
    var items = Array.from(list.children);
    var dragIdx = items.indexOf(draggedEl);
    var targetIdx = items.indexOf(target);
    if (dragIdx < targetIdx) {
      list.insertBefore(draggedEl, target.nextSibling);
    } else {
      list.insertBefore(draggedEl, target);
    }
  }

  function drop(e) {
    e.preventDefault();
    saveSessionOrder();
  }

  function dragEnd(e) {
    if (draggedEl) {
      draggedEl.classList.remove('dragging');
      draggedEl = null;
    }
  }

  function saveSessionOrder() {
    var list = document.getElementById('session-list');
    if (!list) return;
    var order = [];
    list.querySelectorAll('.session-item[data-session-id]').forEach(function(el) {
      order.push(el.getAttribute('data-session-id'));
    });
    try { localStorage.setItem('ws-session-order', JSON.stringify(order)); } catch(e) {}
  }

  function applySessionOrder() {
    var list = document.getElementById('session-list');
    if (!list) return;
    var order;
    try { order = JSON.parse(localStorage.getItem('ws-session-order')); } catch(e) { return; }
    if (!order || !order.length) return;
    var items = {};
    list.querySelectorAll('.session-item[data-session-id]').forEach(function(el) {
      items[el.getAttribute('data-session-id')] = el;
    });
    // Re-insert items in saved order; items not in saved order stay at end
    order.forEach(function(id) {
      if (items[id]) {
        list.appendChild(items[id]);
        delete items[id];
      }
    });
    // Append remaining items not in the saved order
    Object.keys(items).forEach(function(id) {
      list.appendChild(items[id]);
    });
  }

  // Apply saved order after sidebar loads/refreshes
  var origAfterSwap = null;
  document.addEventListener('htmx:afterSettle', function(event) {
    if (event.detail.target.id === 'sidebar' || event.detail.target.querySelector('#session-list')) {
      applySessionOrder();
      restoreSidebarTab();
    }
  });

  // Also apply on initial page load
  document.addEventListener('DOMContentLoaded', function() {
    applySessionOrder();
    restoreSidebarTab();
    // Restore notification sounds toggle on settings page
    var cb = document.getElementById('notif-sounds-toggle');
    if (cb) cb.checked = getNotifSoundsEnabled();
  });

  // Global notification WebSocket
  var notifWs = null;
  function connectNotifications() {
    var protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    notifWs = new WebSocket(protocol + '//' + window.location.host + '/ws/notifications');
    notifWs.onmessage = function(event) {
      try {
        var msg = JSON.parse(event.data);
        if (msg.type === 'notification') {
          // Update badge
          var bell = document.querySelector('.notification-bell');
          var badge = bell ? bell.querySelector('.badge') : null;
          if (!badge && bell) {
            badge = document.createElement('span');
            badge.className = 'badge';
            badge.textContent = '0';
            bell.appendChild(badge);
          }
          if (badge) {
            badge.textContent = String(parseInt(badge.textContent || '0') + 1);
            updateFaviconBadge(parseInt(badge.textContent));
          }
          // Play sound
          playNotifSound(msg.event);
          // Desktop notification (or in-app toast fallback for webview/GUI mode)
          var notifTitle = 'websessions: ' + msg.event;
          var notifBody = 'Session ' + msg.sessionID + (msg.message ? ': ' + msg.message : '');
          if ('Notification' in window && Notification.permission === 'granted') {
            new Notification(notifTitle, {
              body: notifBody,
              tag: 'ws-' + msg.sessionID + '-' + msg.event,
            });
          } else {
            showToast(notifTitle, notifBody, msg.event);
          }
          // Refresh sidebar to update session states
          refreshSidebar();
        }
        if (msg.type === 'iframe-open') {
          openIframeTab(msg.url, msg.title);
        }
      } catch(e) {}
    };
    notifWs.onclose = function() {
      // Reconnect after 3 seconds
      setTimeout(connectNotifications, 3000);
    };
  }
  connectNotifications();

  // ── Favicon badge ───────────────────────────────────────────
  function updateFaviconBadge(count) {
    var link = document.querySelector("link[rel~='icon']");
    if (!link) return;
    if (count <= 0) {
      link.href = '/static/favicon.svg';
      document.title = 'websessions';
      return;
    }
    document.title = '(' + count + ') websessions';
  }

  // ── Periodic update check (every 30 minutes, cached) ──────
  var updateBannerShown = false;
  var lastUpdateCheck = 0;
  function backgroundUpdateCheck() {
    if (updateBannerShown) return;
    var now = Date.now();
    if (now - lastUpdateCheck < 30 * 60 * 1000 && lastUpdateCheck > 0) return;
    lastUpdateCheck = now;
    fetch('/api/check-update')
      .then(function(r) { return r.json(); })
      .then(function(data) {
        if (data.UpdateAvail && !updateBannerShown) {
          updateBannerShown = true;
          showUpdateBanner(data.LatestVersion, data.ReleaseURL);
        }
      })
      .catch(function() {}); // silently ignore errors
  }

  function showUpdateBanner(version, url) {
    var existing = document.getElementById('update-banner');
    if (existing) return;

    var banner = document.createElement('div');
    banner.id = 'update-banner';
    banner.className = 'update-banner';

    var text = document.createElement('span');
    text.className = 'update-banner-text';
    text.textContent = 'A new version of websessions is available: ' + version;

    var actions = document.createElement('span');
    actions.className = 'update-banner-actions';

    var viewBtn = document.createElement('a');
    viewBtn.className = 'update-banner-btn';
    viewBtn.textContent = 'View release';
    viewBtn.href = url || 'https://github.com/IgorDeo/claude-websessions/releases/latest';
    viewBtn.target = '_blank';
    viewBtn.rel = 'noopener';

    var updateBtn = document.createElement('button');
    updateBtn.className = 'update-banner-btn update-banner-btn-primary';
    updateBtn.textContent = 'Update now';
    updateBtn.onclick = function() {
      updateBtn.textContent = 'Updating...';
      updateBtn.disabled = true;
      fetch('/api/self-update', { method: 'POST' })
        .then(function(r) { return r.json(); })
        .then(function(data) {
          if (data.error) {
            updateBtn.textContent = 'Failed: ' + data.error;
          } else {
            text.textContent = 'Updated to ' + version + '. Restart websessions to apply.';
            updateBtn.remove();
            viewBtn.remove();
          }
        })
        .catch(function(err) {
          updateBtn.textContent = 'Failed';
        });
    };

    var dismiss = document.createElement('button');
    dismiss.className = 'update-banner-dismiss';
    dismiss.textContent = '\u00d7';
    dismiss.title = 'Dismiss';
    dismiss.onclick = function() { banner.remove(); };

    actions.appendChild(viewBtn);
    actions.appendChild(updateBtn);
    banner.appendChild(text);
    banner.appendChild(actions);
    banner.appendChild(dismiss);
    document.body.insertBefore(banner, document.body.firstChild);
  }

  // Check on startup (after 10s delay) then every 30 minutes
  setTimeout(backgroundUpdateCheck, 10000);
  setInterval(backgroundUpdateCheck, 30 * 60 * 1000);

  // ── Sidebar resize drag ────────────────────────────────────
  (function() {
    var handle = document.getElementById('sidebar-resize-handle');
    var sidebar = document.getElementById('sidebar');
    if (!handle || !sidebar) return;

    var dragging = false;

    handle.addEventListener('mousedown', function(e) {
      e.preventDefault();
      dragging = true;
      handle.classList.add('dragging');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
    });

    document.addEventListener('mousemove', function(e) {
      if (!dragging) return;
      var newWidth = e.clientX;
      if (newWidth < 130) newWidth = 130;
      if (newWidth > 600) newWidth = 600;
      sidebar.style.width = newWidth + 'px';
    });

    document.addEventListener('mouseup', function() {
      if (!dragging) return;
      dragging = false;
      handle.classList.remove('dragging');
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      try { localStorage.setItem('ws-sidebar-width', sidebar.style.width); } catch(e) {}
    });

    // Restore saved width
    try {
      var saved = localStorage.getItem('ws-sidebar-width');
      if (saved) sidebar.style.width = saved;
    } catch(e) {}
  })();

  // ── Terminal font zoom ──────────────────────────────────────
  var termFontSize = 14;
  try {
    var savedSize = localStorage.getItem('ws-term-font-size');
    if (savedSize) termFontSize = parseInt(savedSize);
  } catch(e) {}

  function applyTermFontSize() {
    Object.keys(terminals).forEach(function(id) {
      var t = terminals[id];
      if (t && t.term) {
        t.term.options.fontSize = termFontSize;
        if (t.fitAddon) t.fitAddon.fit();
      }
    });
    try { localStorage.setItem('ws-term-font-size', String(termFontSize)); } catch(e) {}
  }

  // ── Keyboard shortcuts ─────────────────────────────────────
  document.addEventListener('keydown', function(e) {
    var tag = (e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return;

    var ctrl = e.ctrlKey || e.metaKey;

    // Ctrl+= / Ctrl+- — terminal font zoom
    if (ctrl && (e.key === '=' || e.key === '+')) {
      e.preventDefault();
      termFontSize = Math.min(termFontSize + 1, 28);
      applyTermFontSize();
      return;
    }
    if (ctrl && e.key === '-') {
      e.preventDefault();
      termFontSize = Math.max(termFontSize - 1, 8);
      applyTermFontSize();
      return;
    }
    if (ctrl && e.key === '0') {
      e.preventDefault();
      termFontSize = 14;
      applyTermFontSize();
      return;
    }

    // Ctrl+] — next tab, Ctrl+[ — previous tab
    if (ctrl && (e.key === ']' || e.key === '[')) {
      e.preventDefault();
      if (openTabs.length < 2) return;
      var idx = openTabs.findIndex(function(t) { return t.id === activeTabId; });
      if (e.key === '[') {
        idx = (idx - 1 + openTabs.length) % openTabs.length;
      } else {
        idx = (idx + 1) % openTabs.length;
      }
      openTab(openTabs[idx].id, openTabs[idx].name, openTabs[idx].state);
      return;
    }

    // Ctrl+W — close active tab
    if (ctrl && e.key === 'w') {
      e.preventDefault();
      if (activeTabId) closeTab(activeTabId);
      return;
    }

    // Ctrl+N — new provider session
    if (ctrl && e.key === 'n') {
      e.preventDefault();
      htmx.ajax('GET', '/sessions/new', { target: '#modal', swap: 'innerHTML' });
      return;
    }

    // Ctrl+T — new terminal
    if (ctrl && e.key === 't') {
      e.preventDefault();
      openTerminal();
      return;
    }

    // Ctrl+1-9 — switch to tab by number
    if (ctrl && e.key >= '1' && e.key <= '9') {
      e.preventDefault();
      var tabIdx = parseInt(e.key) - 1;
      if (tabIdx < openTabs.length) {
        openTab(openTabs[tabIdx].id, openTabs[tabIdx].name, openTabs[tabIdx].state);
      }
      return;
    }
  });
  return {
    connectSession: connectSession,
    disconnectSession: disconnectSession,
    openTab: openTab,
    openIframeTab: openIframeTab,
    closeTab: closeTab,
    showDiff: showDiff,
    unsplitPane: unsplitPane,
    splitPane: splitPane,
    openTerminal: openTerminal,
    toggleTheme: toggleTheme,
    toggleShortcuts: toggleShortcuts,
    killAllSessions: killAllSessions,
    focusPane: focusPane,
    toggleGroupCollapsed: toggleGroupCollapsed,
    refreshSidebar: refreshSidebar,
    killSession: killSession,
    startRename: startRename,
    dirAutocomplete: dirAutocomplete,
    focusNotifSession: focusNotifSession,
    toggleNotifications: toggleNotifications,
    setNotifSoundsEnabled: setNotifSoundsEnabled,
    getNotifSoundsEnabled: getNotifSoundsEnabled,
    testNotifSound: testNotifSound,
    dismissNotification: dismissNotification,
    snoozeNotification: snoozeNotification,
    clearAllNotifications: clearAllNotifications,
    switchSidebarTab: switchSidebarTab,
    filterSessions: filterSessions,
    manageHooks: manageHooks,
    managePlannotator: managePlannotator,
    checkForUpdate: checkForUpdate,
    manageService: manageService,
    settingsDirAutocomplete: settingsDirAutocomplete,
    providerChanged: providerChanged,
    loadProviderSessions: loadProviderSessions,
    loadClaudeSessions: loadClaudeSessions,
    selectDir: selectDir,
    quickSession: quickSession,
    dragStart: dragStart,
    dragOver: dragOver,
    drop: drop,
    dragEnd: dragEnd,
    terminals: terminals,
  };
})();
