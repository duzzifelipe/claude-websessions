# Tab Management and Split Pane System

## Overview

The tab and split pane system provides a multi-session workspace inside the browser. Users can open multiple sessions as tabs, split any tab into multiple panes (horizontally or vertically), nest splits arbitrarily deep, and reorder tabs via drag-and-drop. Each tab tracks its own split tree, which is persisted to both localStorage and the server so layouts survive page reloads and cross-device access.

Key files:

| File | Role |
|------|------|
| `web/static/app.js` | All client-side tab/split logic |
| `web/templates/tabs.templ` | Server-rendered tab bar (initial page load) |
| `web/templates/terminal.templ` | Session pane HTML with split/unsplit/kill buttons |
| `web/templates/iframe.templ` | Iframe pane HTML |
| `internal/server/handlers.go` | Server-side tab group extraction + open session/iframe handlers |
| `web/static/style.css` | Tab bar, split pane, gutter, drop zone styles |

---

## Tab Bar

### Structure

```
 .-------------------------------------------------------------------.
 | [. my-project] [. api-service  3] [. data-pipe] [+]               |
 '-------------------------------------------------------------------'
   ^               ^             ^                   ^
   tab-dot         tab-name      split-badge          new-tab button
   (state color)                 (pane count)
```

The tab bar is a horizontally-scrolling flex container (`#tab-bar`). Each tab shows:

- **State dot** -- colored circle reflecting session state (running, waiting, errored, completed)
- **Tab name** -- the session name, double-click to rename inline
- **Provider badge** -- `OC` badge for OpenCode sessions so provider tabs are visually distinct
- **Split badge** -- number of panes, only shown when the tab has a split tree with 2+ leaves
- **Close button** -- appears on hover, closes the tab (session keeps running)

### Tab Lifecycle

```
Sidebar click / API call
        |
        v
   openTab(sessionID, name, state, sessionType)
        |
        +-- Session already in a split group? --> activate that group's tab
        |
        +-- Tab already open?
        |     yes --> just activate it (no re-fetch if DOM is intact)
        |     no  --> push to openTabs[] (including provider/type metadata), save state
        |
        +-- Tab has splitTree? --> rebuildSplitLayout()
        |
        +-- Tab type === 'iframe'? --> loadPaneIframe()
        |
        +-- Otherwise: htmx POST /sessions/{id}/open --> server renders Terminal template
        |
        v
   renderTabs()  (rebuilds tab bar DOM)
```

### Closing a Tab

```
closeTab(sessionID)
        |
        +-- Tab has splitTree? --> disconnect all sessions in the tree
        |
        +-- Remove from openTabs[]
        +-- Disconnect terminal if present
        +-- Save state
        |
        +-- Was this the active tab?
              yes --> activate the last remaining tab (or show empty state)
              no  --> just re-render tab bar
```

When the last tab is closed, the terminal area shows an empty state message: "Select a session from the sidebar or create a new one".

### Tab Rename

Double-clicking the tab name replaces the `<span>` with an `<input>`:

```
Before:  [. my-project  x]
During:  [. [my-project_] x]    <-- input field, focused + selected
After:   [. new-name     x]    <-- on Enter or blur, saves + re-renders
```

Press Escape to cancel (re-renders without saving the change).

---

## Split Tree Data Structure

Each tab optionally has a `splitTree` property. When `null`, the tab shows a single session. When set, it describes a binary tree of panes.

### Node Types

| Type | Fields | Description |
|------|--------|-------------|
| `session` | `id`, `name` | Terminal pane connected to a session via WebSocket |
| `iframe` | `id`, `name`, `url` | Embedded iframe pointing to a localhost URL |
| `split` | `dir`, `children[]` | Internal node, splits into 2+ children |

### Tree Shape Examples

**Single session (no split tree):**

```
Tab: { id: "sess-1", name: "my-project", splitTree: null }

+-----------------------------------+
|          sess-1 (terminal)        |
+-----------------------------------+
```

**Two-pane horizontal split:**

```
Tab: {
  id: "sess-1",
  splitTree: {
    type: "split",
    dir: "horizontal",
    children: [
      { type: "session", id: "sess-1" },
      { type: "session", id: "sess-2" }
    ]
  }
}

+-----------------+-----------------+
|    sess-1       |    sess-2       |
|   (terminal)    |   (terminal)    |
+-----------------+-----------------+
```

**Nested split (3 panes):**

```
Tab: {
  id: "sess-1",
  splitTree: {
    type: "split",
    dir: "horizontal",
    children: [
      { type: "session", id: "sess-1" },
      { type: "split",
        dir: "vertical",
        children: [
          { type: "session", id: "sess-2" },
          { type: "session", id: "sess-3" }
        ]
      }
    ]
  }
}

+-----------------+-----------------+
|                 |    sess-2       |
|    sess-1       +-----------------+
|   (terminal)    |    sess-3       |
|                 |   (terminal)    |
+-----------------+-----------------+
```

**Mixed session and iframe panes:**

```
splitTree: {
  type: "split",
  dir: "vertical",
  children: [
    { type: "session", id: "sess-1" },
    { type: "iframe", id: "iframe-1710000000", url: "http://localhost:3000", name: "Dev Server" }
  ]
}

+-----------------------------------+
|          sess-1 (terminal)        |
+-----------------------------------+
|    iframe-1710000000 (iframe)     |
|    http://localhost:3000          |
+-----------------------------------+
```

---

## How Splits Work

Splitting is direction-aware and follows Terminator conventions:

| Term used | Split.js direction | Flex direction | Visual result |
|-----------|-------------------|----------------|---------------|
| `horizontal` | `horizontal` (side-by-side) | `row` | Left \| Right |
| `vertical` | `vertical` (stacked) | `column` | Top / Bottom |

The mapping is handled by two functions:

```
splitDirection("horizontal") --> "horizontal"  (Split.js: side by side)
splitDirection("vertical")   --> "vertical"    (Split.js: top/bottom)

splitFlex("horizontal") --> "row"
splitFlex("vertical")   --> "column"
```

### First Split (no existing split tree)

When the tab has no split tree yet:

```
doSplit(sessionID1, sessionID2, direction)
        |
        v
   1. Disconnect sessionID1 terminal
   2. Clear terminal-area of all children
   3. Set terminal-area flex-direction
   4. Create two .split-pane divs
   5. Append both to terminal-area
   6. Initialize Split.js on the two panes
   7. loadPaneSession(pane1, sessionID1)
   8. loadPaneSession(pane2, sessionID2)
   9. Create new splitTree node:
      { type: "split", dir: direction,
        children: [session1-leaf, session2-leaf] }
  10. Set tab.splitTree = new node
  11. Remove sessionID2 from top-level tabs (absorbed into group)
  12. renderTabs() + saveTabState()
```

```
BEFORE:                           AFTER (horizontal):
+---------------------------+     +-------------+-------------+
|        terminal-area      |     |   pane 1    |   pane 2    |
|        (sess-1 only)      |     |   sess-1    |   sess-2    |
+---------------------------+     +-------------+-------------+
                                        (gutter in between)
```

### Nested Split (existing split tree)

When splitting a pane that is already inside a split:

```
doSplit(sessionID1, sessionID2, direction)
        |
        v
   1. Find the .split-pane containing sessionID1 via DOM traversal
   2. Disconnect sessionID1 terminal
   3. Create a new container div (flex, direction)
   4. Create two child .split-pane divs inside container
   5. Replace the original pane with the container in the DOM
   6. Initialize Split.js on the two child panes
   7. loadPaneSession(pane1, sessionID1)
   8. loadPaneSession(pane2, sessionID2)
   9. In the split tree, find sessionID1's parent node
  10. Replace the leaf with a new split node containing both sessions
  11. renderTabs() + saveTabState()
```

```
BEFORE (2-pane):                 AFTER (3-pane, nested vertical in right):
+-------------+-------------+   +-------------+-------------+
|             |             |   |             |   sess-2    |
|   sess-1    |   sess-2    |   |   sess-1    +-------------+
|             |             |   |             |   sess-3    |
+-------------+-------------+   +-------------+-------------+
```

---

## Split Picker UI Flow

When a user clicks a split button on a pane header:

```
User clicks [split-h] or [split-v] button
        |
        v
splitPane(currentSessionID, direction)
        |
        v
   fetch('/api/sessions')          <-- get all active sessions
        |
        v
showSplitPicker(currentSessionID, direction, sessions)
        |
        v
   .-----------------------------------.
   | Open in split                     |
   |-----------------------------------|
   | [+ New Claude Session]            |  <-- opens new-session modal
   | [  New Terminal]                  |  <-- creates bare terminal session
   |-----------------------------------|
   | api-service   ~/code/api          |  <-- existing sessions
   | data-pipe     ~/code/data         |     (excludes sessions already
   |                                   |      in any split group)
   '-----------------------------------'

User picks a session (or creates a new one)
        |
        v
doSplit(currentSessionID, pickedSessionID, direction)
```

Sessions already used in any split group are excluded from the picker to prevent the same session from appearing in two places simultaneously.

---

## Pane Types

### Session Panes

Standard terminal panes connected to a Claude Code or shell session via WebSocket.

```
+-----------------------------------+
| pane-title    state    work_dir   |  <-- pane-header
| [delta] [|||] [===] [x] [stop]   |  <-- pane-actions
+-----------------------------------+
|                                   |
|         xterm.js terminal         |  <-- term-{sessionID}
|                                   |
+-----------------------------------+
```

Pane header buttons:
- Delta -- view git changes (diff modal)
- `|||` -- split horizontal (side by side)
- `===` -- split vertical (top/bottom)
- `x` -- close this pane (unsplit, session keeps running)
- Stop -- kill session (terminates the process)

Rendered by `templates.Terminal()` on the server, then `connectSession()` on the client creates the xterm.js instance and WebSocket connection.

### Iframe Panes

Embedded web pages, restricted to localhost URLs only (enforced server-side by `isLocalhostURL()`).

```
+-----------------------------------+
| pane-title    "iframe"    url     |  <-- pane-header
|                           [x]    |  <-- close button only
+-----------------------------------+
|                                   |
|        <iframe src=url>           |  <-- full-size iframe
|                                   |
+-----------------------------------+
```

Rendered by `templates.IframePane()`. The pane ID uses the format `iframe-{timestamp}`.

Opening an iframe tab:

```
openIframeTab(url, title)
        |
        v
   1. Generate ID: "iframe-" + Date.now()
   2. Push { id, name, state:"iframe", type:"iframe", url } to openTabs
   3. Set as active tab
   4. Clear terminal-area
   5. Create .split-pane div
   6. loadPaneIframe(pane, id, url, name)
   7. Save tab state
```

The server handler (`handleOpenIframe`) validates that the URL is localhost before rendering.

---

## Tree Helpers

Five functions manipulate the split tree data structure:

### isLeafNode(node)

Returns `true` if the node is not a split (i.e., it is a `session` or `iframe`).

### treeFind(node, id)

Recursively searches the tree for a leaf with the given ID. Returns the node or `null`.

```
treeFind(root, "sess-2")

        split
       /     \
   sess-1   sess-2  <-- found, returned
```

### treeFindParent(node, id)

Returns `{ parent: splitNode, index: childIndex }` for the leaf with the given ID, or `null` if not found.

```
treeFindParent(root, "sess-2")

        split        <-- parent
       /     \
   [0]       [1]     <-- index = 1
  sess-1    sess-2
```

### treeLeafIds(node)

Collects all leaf IDs in the tree (both session and iframe). Aliased as `treeSessionIds` for backward compatibility.

```
treeLeafIds(root) --> ["sess-1", "sess-2", "iframe-123"]
```

### treeRemove(node, id)

Removes a leaf from the tree. If the parent split node is left with only one child, the parent collapses -- it adopts the remaining child's properties (type, id, name, etc.).

```
BEFORE:                   AFTER treeRemove(root, "sess-2"):

      split                     sess-1
     /     \                    (collapsed -- parent becomes the leaf)
  sess-1  sess-2

BEFORE (3 panes):         AFTER treeRemove(root, "sess-3"):

      split                     split
     /     \                   /     \
  sess-1  split             sess-1  sess-2
          /    \                    (inner split collapsed)
       sess-2  sess-3
```

---

## Tab Persistence

Tab state is stored in two places for reliability: `localStorage` (fast, offline-capable) and server preferences via the `/api/preferences` endpoint (survives browser clears, works cross-device).

### What Gets Saved

| Key | Value | Example |
|-----|-------|---------|
| `open-tabs` | JSON array of tab objects | `[{"id":"sess-1","name":"my-project","state":"running","splitTree":{...}}]` |
| `active-tab` | Session ID string | `"sess-1"` |

### Save Flow

```
saveTabState()
        |
        +-- localStorage.setItem('ws-open-tabs', JSON.stringify(openTabs))
        +-- localStorage.setItem('ws-active-tab', activeTabId)
        |
        +-- PUT /api/preferences  { key: "open-tabs", value: JSON }
        +-- PUT /api/preferences  { key: "active-tab", value: ID }
        |
        v
   Returns Promise.all (used by callers that need to wait for persistence)
```

### Load Flow (Page Load)

```
1. loadTabState()                     <-- synchronous, from localStorage
   - Parse 'ws-open-tabs' into openTabs[]
   - Read 'ws-active-tab' into activeTabId
   - Provides instant tab bar on page load

2. DOMContentLoaded fires
   |
   v
3. syncTabStateFromServer()           <-- async, from /api/preferences
   |
   +-- Fetch server preferences
   +-- Merge: prefer local splitTree over server (server may be stale)
   +-- Overwrite openTabs with server list (with merged trees)
   +-- Push merged state back to server
   +-- renderTabs()
   +-- If activeTabId set, call openTab() to restore the view
```

### Server-Side Tab Groups

The server reads the persisted tab state in `applyTabGroups()` to annotate sidebar session views with group names:

```
applyTabGroups(views []SessionView)
        |
        v
   1. Read "open-tabs" preference from SQLite
   2. Parse each tab's splitTree
   3. extractTreeIDs() --> extractNodeIDs() recursively
   4. Build groupMap: sessionID -> tab name
   5. Set views[i].GroupName for sessions that belong to a split group
```

This allows the sidebar to show which sessions are grouped together.

---

## Dead Tab Pruning

Tabs can reference sessions that no longer exist (terminated, cleaned up). `pruneDeadTabs()` cleans these up:

```
pruneDeadTabs()
        |
        v
   fetch('/api/sessions')
        |
        v
   Build activeIds set from server response
        |
        v
   Filter openTabs:
        |
        +-- Iframe tabs: always keep (no server-side session)
        |
        +-- Split tabs: keep if at least one session in tree is alive
        |
        +-- Regular tabs: keep if session ID exists in activeIds
        |
        v
   If any tabs were removed:
        +-- Fix activeTabId if it was pruned (pick first remaining tab)
        +-- saveTabState()
        +-- renderTabs()
        +-- openTab() for the new active tab
```

Pruning runs during the initial page load sequence after tab state is synced.

---

## Focus Management

In a split layout, the focused pane gets a visual indicator (accent-colored bottom border on its header).

### How Focus Works

```
focusPane(sessionID)
        |
        v
   1. Set focusedSessionId = sessionID
   2. Remove .pane-focused from all .terminal-pane elements
   3. Add .pane-focused to the pane matching sessionID
```

### Focus Triggers

| Action | Focus target |
|--------|-------------|
| Click on pane header (not a button) | That pane |
| Mousedown on terminal container | That pane |
| Tab switch via openTab() | The target session (with 300ms delay for DOM) |
| Notification click | The notification's session |

### Visual Indicator

```
Unfocused pane header:
+-----------------------------------+
| my-project   running   ~/code     |  <-- normal border
+-----------------------------------+

Focused pane header:
+===================================+
| my-project   running   ~/code     |  <-- 2px accent bottom border
+===================================+
```

CSS rule: `.terminal-pane.pane-focused > .pane-header { border-bottom: 2px solid var(--accent); }`

---

## Drag-and-Drop Tab Reordering

Tabs support native HTML drag-and-drop for reordering and split-by-drag.

### Tab Reorder

```
User drags tab A over tab B
        |
        v
tabDragStart(e, tabId)
   - Set draggedTabId
   - Add .dragging class (opacity: 0.35)
   - Set dataTransfer to 'move'
   - After 100ms, show drop zones on terminal area
        |
        v
tabDragOver(e)  [on target tab]
   - preventDefault to allow drop
        |
        v
tabDrop(e, targetId)  [on target tab]
   - Hide drop zones
   - Find source and target indices in openTabs[]
   - Splice: remove from old position, insert at new position
   - saveTabState() + renderTabs()
        |
        v
tabDragEnd(e)
   - Remove .dragging class
   - Clear draggedTabId
   - Hide drop zones
```

### Split-by-Drag (Drop Zones)

When dragging a tab, four drop zones appear over the terminal area:

```
+-----------------------------------+
|              TOP (30%)            |
|         .......................   |
|         |                     |   |
| LEFT    |    (terminal area)  | RIGHT
| (25%)   |                     | (25%)
|         |                     |   |
|         '.....................'   |
|            BOTTOM (30%)           |
+-----------------------------------+
```

Dropping a tab on a zone triggers a split:

| Drop zone | Split direction | Dragged tab position |
|-----------|----------------|---------------------|
| Left | horizontal | First (left) |
| Right | horizontal | Second (right) |
| Top | vertical | First (top) |
| Bottom | vertical | Second (bottom) |

Drop zones only appear when there is an active tab different from the one being dragged.

---

## Tab Context Menu

Right-clicking a tab opens a context menu:

```
 .--------------------------------.
 | Close tab                      |
 | Close & stop session           |  <-- red text (ctx-danger)
 |................................|
 | Close other tabs               |
 | Close all tabs                 |
 '--------------------------------'
```

### Menu Actions

| Item | Behavior |
|------|----------|
| Close tab | Same as clicking the X button. Session keeps running. |
| Close & stop session | Kills the session process, then closes the tab. |
| Close other tabs | Disconnects and removes all tabs except this one. |
| Close all tabs | Disconnects and removes all tabs. Shows empty state. |

### Implementation

```
Right-click on tab
        |
        v
showTabContextMenu(e, tabId, tabName)
        |
        v
   1. Remove any existing context menu
   2. Create .tab-context-menu div at click coordinates (fixed position)
   3. Build menu items with click handlers
   4. Append to document.body
   5. On next click anywhere: closeTabContextMenu() removes the menu
```

The menu is styled as a floating panel with `position: fixed`, `z-index: 300`, and a drop shadow.

---

## Split.js Integration

Split.js provides the resizable gutter between panes. All Split.js instances are created through the `createSplit()` wrapper.

### Configuration

```
createSplit(panes, direction)
        |
        v
   Split(panes, {
     direction: splitDirection(direction),   // "horizontal" or "vertical"
     sizes: [50, 50],                        // equal distribution
     minSize: 80,                            // minimum pane size in pixels
     gutterSize: 4,                          // gutter width/height in pixels
     onDrag: debouncedRefit,                 // refit terminals during drag
     onDragEnd: refitAllTerminals,           // final refit after drag
   })
```

### Gutter Styling

```
Default:    4px | var(--border-subtle)
Hover:      4px | var(--accent)          <-- blue highlight
Horizontal: cursor: col-resize          <-- left/right drag
Vertical:   cursor: row-resize          <-- up/down drag
```

### Terminal Refitting

When a gutter is dragged, xterm.js terminals need to recalculate their dimensions:

```
Gutter drag
     |
     v
onDrag --> debouncedRefit()
     |
     +-- clearTimeout / setTimeout(16ms)
     |
     v
refitAllTerminals()
     |
     v
   For each terminal in terminals{}:
     fitAddon.fit()     <-- recalculate cols/rows
```

A debounce of 16ms (one frame) prevents excessive refit calls during continuous dragging. A final non-debounced `refitAllTerminals()` fires on `onDragEnd` to ensure the layout is correct after the drag completes.

### Rebuilding Split Layouts

When switching to a tab that has a split tree, `rebuildSplitLayout()` recursively reconstructs the entire DOM and Split.js instances:

```
rebuildSplitLayout(tab)
        |
        v
buildNode(tab.splitTree, terminal-area)
        |
        +-- Leaf "session"? --> create .split-pane, loadPaneSession()
        +-- Leaf "iframe"?  --> create .split-pane, loadPaneIframe()
        +-- Split node?
              |
              v
           1. Set container flex-direction from node.dir
           2. For each child: create .split-pane, recurse into buildNode()
           3. createSplit(childPanes, node.dir)
```

This means every tab switch that involves splits rebuilds the DOM from scratch. Previous terminal connections are disconnected before the rebuild starts.

---

## Complete Example Flow

```
1. User has session "api" open in a single tab
   openTabs = [{ id: "api", name: "api", state: "running", splitTree: null }]

2. User clicks split-horizontal button on the api pane
   --> splitPane("api", "horizontal")
   --> fetch /api/sessions
   --> showSplitPicker overlay appears

3. User picks "data-pipe" from the list
   --> doSplit("api", "data-pipe", "horizontal")
   --> terminal-area cleared, two .split-pane divs created
   --> Split.js initialized with 4px gutter
   --> Both panes load their Terminal HTML + connect WebSocket
   --> splitTree = { type: "split", dir: "horizontal",
         children: [{ type: "session", id: "api" },
                    { type: "session", id: "data-pipe" }] }
   --> "data-pipe" removed from top-level tabs
   --> Tab bar shows: [api  2]  (split badge = 2)
   --> saveTabState() writes to localStorage + server

4. User switches to another tab "frontend"
   --> api pane terminals disconnected
   --> terminal-area cleared
   --> "frontend" loaded as single session

5. User switches back to "api" tab
   --> rebuildSplitLayout() called
   --> buildNode() recurses the split tree
   --> Two panes recreated, Split.js re-initialized
   --> Both sessions reconnect via WebSocket
   --> Ring buffer replays recent output

6. User right-clicks "api" tab --> context menu appears
   --> "Close tab" --> closeTab("api")
   --> Both "api" and "data-pipe" terminals disconnected
   --> Tab removed, next tab activated

7. Page reload
   --> loadTabState() restores from localStorage (instant)
   --> syncTabStateFromServer() fetches from /api/preferences
   --> Merge: local splitTree preferred over server
   --> pruneDeadTabs() removes any sessions that no longer exist
   --> Active tab restored with full split layout
```
