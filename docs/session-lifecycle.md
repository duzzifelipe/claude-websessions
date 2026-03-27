# Session Lifecycle

## Overview

The session lifecycle system manages the complete lifetime of provider-backed sessions (`claude`, `opencode`) and terminal sessions within websessions. It handles session creation, state transitions, output streaming, tmux integration, process discovery, takeover of external processes, and graceful shutdown with offline recovery.

All sessions run inside tmux, which provides process persistence independent of the websessions server. This means sessions survive server restarts and can be reattached automatically.

---

## Session Types

Sessions enter the system through five distinct paths depending on their origin:

| Type | ID Pattern | Owned | How Created |
|------|-----------|-------|-------------|
| **claude/opencode** | User-provided name (e.g. `myproject`) | Yes | User clicks "New Session" in UI, chooses provider, runs provider CLI inside tmux |
| **terminal** | `term-{timestamp}` | Yes | User clicks "New Terminal", runs user's `$SHELL` inside tmux |
| **discovered** | `discovered-{pid}` | No | Process scanner finds running supported provider processes via `/proc` or `ps` |
| **external** | `external-{basename}` | No | Claude Code hook fires from a standalone CLI session (via `/api/hook`) |
| **offline** | Original session ID | No | Loaded from SQLite on server restart for sessions that were running |

### Provider Sessions

Created through `POST /sessions` with a name, working directory, provider (`claude` or `opencode`), and optional prompt or resume ID. The handler validates/coerces provider metadata before calling `manager.Create()`.

Provider-specific creation args:

| Provider | Session args built by handler/manager |
|----------|---------------------------------------|
| `claude` | `--name <name>` and optional `--resume <id>` |
| `opencode` | optional `--session <id>` (drops `--name` and converts `--resume` to `--session`) |

### Terminal Sessions

Created through `POST /sessions/terminal`. Uses the user's `$SHELL` (defaults to `/bin/zsh` on macOS, `/bin/bash` on Linux). Named sequentially: "Terminal 1", "Terminal 2", etc.

### Discovered Sessions

The process scanner (`internal/discovery`) runs on a configurable interval (default 30s). It scans for running supported provider processes (`claude`, `opencode`), deduplicates against already-tracked sessions by PID and provider+working-directory, and adds new ones in `StateDiscovered`. Dead discovered processes are removed on the next scan tick.

### Offline Sessions

On server startup, websessions loads the last 50 session records from SQLite. Sessions that were in `running`, `waiting`, or `created` state (and not `discovered-*` prefixed) are added as offline placeholders. These can be restarted with provider-specific resume args (`--resume` for Claude, `--session` for OpenCode).

---

## State Machine

### States

| State | Description |
|-------|-------------|
| `discovered` | External provider process found by scanner; not yet managed |
| `takeover` | Transitional: external process being killed for takeover |
| `created` | Session object exists but process not yet started |
| `starting` | Sandbox provisioning in progress (Docker sessions only) |
| `running` | Process is actively running inside tmux |
| `waiting` | Claude is waiting for user input (permission prompt detected) |
| `completed` | Process exited normally |
| `errored` | Process exited with error or was killed |
| `offline` | Loaded from history; original tmux session no longer exists |

### Transition Diagram

```
                                         +----------+
                                         | offline  |  (loaded from SQLite
                                         +----+-----+   on server restart)
                                              |
                                              | Restart()
                                              v
  +------------+         +---------+     +---------+     +-----------+
  | discovered | ------> | takeover| --> | running | --> | completed |
  +------------+         +---------+     +----+----+     +-----------+
                              |               |
                              |               +--------> +---------+
                              +--------------------->    | errored |
                                              |          +---------+
                                              |               ^
  +---------+     +----------+                |               |
  | created | --> | starting | ---------------+               |
  +---------+     +-----+----+                |               |
       |                |                     |               |
       |                +------ (fail) -------+---------------+
       |                                      |
       +--------------------------------------+
                                              |
                                         +----+----+
                                         | waiting |
                                         +---------+
                                              |
                                              | (output resumes)
                                              v
                                         +---------+
                                         | running |
                                         +---------+
```

### Transition Rules

The `validTransitions` map enforces which state changes are legal:

```
  From State    -->  Allowed Target States
  -----------------------------------------------
  discovered    -->  takeover
  takeover      -->  running, errored
  created       -->  running, starting
  starting      -->  running, errored
  running       -->  waiting, completed, errored
  waiting       -->  running, completed, errored
```

Any transition not in this table returns an error from `Session.Transition()`. Terminal states (`completed`, `errored`, `offline`) have no outgoing transitions.

### Waiting State Detection

The manager monitors PTY output for patterns that indicate Claude is waiting for user input:

```
Patterns checked:
  - "Do you want to proceed?"
  - "[Y/n]"
  - "[y/N]"
  - "? (y/n)"
  - "(yes/no)"
```

When a pattern matches and the session is in `running` state, it transitions to `waiting`. When subsequent output exceeds 10 bytes (indicating the process resumed), it transitions back to `running`.

---

## Session Creation Flow

### Standard (Non-Sandbox) Path

```
handleCreateSession (HTTP POST)
        |
        v
manager.Create(id, workDir, providerCommand, args, &CreateOptions{Provider: provider, ExternalSessionID: resumeID})
        |
        |-- 1. Expand ~ in workDir
        |-- 2. Validate directory exists
        |-- 3. Resolve provider command+args (`claude` or `opencode`)
        |-- 4. exec.LookPath(providerCommand) to resolve binary
        |-- 5. TmuxSessionName(id) -> "ws-{id}"
        |-- 6. Kill any existing tmux session with that name
        |-- 7. tmuxCreateSession(name, workDir, cmd, args)
        |       |
        |       |-- tmux new-session -d -s ws-{id} -x 200 -y 50 -c {workDir}
        |       |-- tmux set-option status off
        |       |-- tmux set-option mouse off
        |       |-- tmux set-option default-terminal xterm-256color
        |       |-- tmux set-window-option aggressive-resize on
        |       v
        |-- 8. Build Session{Provider, ExternalSessionID, ClaudeID?, State: running, Owned: true}
        |-- 9. Fire onStateChange(created -> running)
        |-- 10. startReader(session)
        |
        v
   Return session to handler
        |
        v
   Save to SQLite, set response headers (X-Session-ID, HX-Trigger)
```

### Sandbox (Docker Desktop) Path

```
handleCreateSession (HTTP POST, sandbox=true)
        |
        v
manager.Create(id, workDir, providerCommand, args, &CreateOptions{Sandboxed: true, Provider: provider, ExternalSessionID: resumeID})
        |
        |-- 1. Build Session{State: starting, Sandboxed: true}
        |-- 2. Fire onStateChange(created -> starting)
        |-- 3. Launch goroutine: provisionSandbox()
        |       |
        |       |-- docker.FindSandboxForWorkDir(workDir)
        |       |     |
        |       |     +-- Found? Reuse existing sandbox
        |       |     +-- Not found? docker.SandboxCreate(workDir)
        |       |           |
        |       |           +-- docker sandbox create claude {workDir}
        |       |           +-- docker.SandboxCopyCredentials(name)
        |       |                 |
        |       |                 +-- Copies ~/.claude/.credentials.json
        |       |                 +-- Copies ~/.claude.json
        |       |                 +-- Copies ~/.claude/.settings.json
        |       |
        |       |-- tmuxCreateSession(name, workDir, "docker",
        |       |       ["sandbox", "run", {name}, "--", ...args])
        |       |
        |       |-- Transition: starting -> running
        |       |-- startReader(session)
        |       |
        |       +-- On any failure: failSession() -> errored
        |
        v
   Return session immediately (UI shows "starting" spinner)
```

### Terminal Creation Path

```
handleCreateTerminal (HTTP POST)
        |
        |-- Count existing term-* sessions for naming
        |-- id = "term-{timestamp}", name = "Terminal N"
        |-- Resolve $SHELL (or /bin/bash default)
        |
        v
manager.Create(id, workDir, shell, nil)
        |
        (same as standard path but runs shell instead of claude)
```

---

## PTY & Ring Buffer

### Output Pipeline

```
+-------------------+     +-------------------+     +------------------+
| tmux session      |     | reader goroutine  |     | RingBuf          |
| (claude / shell)  |     | (PTY attach)      |     | (circular buffer)|
|                   |     |                   |     |                  |
| stdout/stderr ----|---->| ptmx.Read(4096) --|---->| .Write(data)     |
|                   |     |                   |     |                  |
+-------------------+     +--------+----------+     +--------+---------+
                                   |                         |
                                   |                         | .Bytes()
                                   v                         v
                          +------------------+     +-------------------+
                          | onOutput(id,data)|     | New WS client     |
                          |                  |     | connects: replay  |
                          +--------+---------+     | full buffer       |
                                   |               +-------------------+
                                   v
                          +------------------+
                          | WebSocket hub    |
                          | broadcast to all |
                          | connected clients|
                          +------------------+
```

### Reader Goroutine

Each session gets a dedicated reader goroutine started by `startReader()`:

1. Attaches to tmux in read-only mode: `tmux attach-session -t {name} -r`
2. Starts the attach command inside a PTY (50 rows x 200 cols initial size)
3. Stores the PTY file descriptor for resize operations
4. Reads in a loop (4096-byte buffer), writing to both the ring buffer and the output callback
5. Checks each chunk for waiting-state patterns
6. On read error (PTY closed): checks if tmux session still exists
   - If tmux session is gone and not intentionally killed: transitions to `completed`
   - If intentionally killed (`s.Killed == true`): returns silently

The reader can be stopped via a channel signal from `stopReader()`, which is called during kill and remove operations.

### Ring Buffer

The `RingBuf` is a thread-safe circular byte buffer that retains the most recent output:

```
RingBuf struct:
  buf     []byte   -- fixed-size backing array
  size    int      -- capacity (default 10MB)
  w       int      -- next write position (wraps around)
  written int64    -- total bytes ever written

Write behavior:
  - Data smaller than buffer: copy with wrap-around at boundary
  - Data larger than buffer: keep only the last `size` bytes

Read behavior (Bytes()):
  - Buffer not full (written < size): return buf[0:w]
  - Buffer full: return buf[w:] + buf[:w]  (oldest-first order)
```

When a new WebSocket client connects to a session, the full ring buffer contents are replayed so the client sees recent output history.

### Input Path

```
WebSocket client (browser xterm.js)
        |
        | binary WS frame
        v
WriteInput(sessionID, data)
        |
        |-- Filter terminal response sequences (DA1, DA2, DSR)
        |
        v
tmuxSendKeys(session.TmuxSession, data)
        |
        v
tmux send-keys -t {name} -l {data}
```

Terminal responses (device attribute queries from xterm.js) are filtered out to prevent feedback loops.

---

## Tmux Integration

### Why Tmux

Tmux provides three critical capabilities:

1. **Process persistence** -- Sessions survive server restarts. The provider CLI continues running inside tmux even when websessions is stopped and restarted.
2. **Output multiplexing** -- Multiple browser tabs/clients can view the same session. The reader attaches in read-only mode while the original process runs independently.
3. **Resize isolation** -- The tmux window size can be adjusted independently via `resize-window`, and the reader PTY is resized separately for proper terminal rendering.

### Session Naming Convention

```
websessions ID:    myproject
tmux session name: ws-myproject

Sanitization rules:
  - Prefix with "ws-"
  - Replace "." with "_"
  - Replace ":" with "_"
  - Replace " " with "_"
```

### Tmux Session Configuration

Each tmux session is created with:

```
tmux new-session -d -s {name} -x 200 -y 50 -c {workDir} {command}
```

Then configured:

| Option | Value | Purpose |
|--------|-------|---------|
| `status` | `off` | Hide tmux status bar (UI provides its own) |
| `mouse` | `off` | Prevent tmux from capturing mouse events |
| `default-terminal` | `xterm-256color` | Proper color support |
| `aggressive-resize` | `on` | Use largest client size, not smallest |

### Recovery on Restart

```
Server starts
     |
     v
RecoverTmuxSessions()
     |
     |-- tmux ls -F "#{session_name}"
     |-- Filter sessions starting with "ws-"
     |
     |-- For each ws-* session not already tracked:
     |     |
     |     |-- Extract ID: strip "ws-" prefix
     |     |-- Get workDir: tmux display-message "#{pane_current_path}"
     |     |-- Recover provider + external session ID from pane start command
     |     |-- For recovered claude sessions, resolve claudeID from workDir fallback
     |     |
     |     v
     |   Reattach(id, name, recoveredClaudeID, workDir, tmuxName, CreateOptions{Provider, ExternalSessionID})
     |     |
     |     |-- Create Session{State: running, Owned: true}
     |     |-- startReader() to resume output streaming
     |
     v
Return count of reattached sessions
```

---

## Session Kill Flow

```
handleKillSession (HTTP DELETE)
        |
        |-- Set sess.Killed = true
        |-- Save to SQLite with status "killed"
        |
        v
manager.Kill(sessionID)
        |
        |-- stopReader(id)          -- close stop channel, reader goroutine exits
        |-- tmuxKillSession(name)   -- tmux kill-session -t {name}
        |
        |-- If sandboxed:
        |     go func():
        |       docker.SandboxStop(name)
        |       docker.SandboxRemove(name)
        |
        |-- Set EndTime = now, State = errored
        |-- Fire onStateChange(from -> errored)
        |     |
        |     +-- Because s.Killed == true, main.go callback
        |         saves with status "killed" and skips notification
        |
        v
   manager.Remove(id) -- delete from sessions map
```

### Kill All

`handleKillAll` iterates all sessions and kills each one sequentially, skipping sessions not in `running` or `waiting` state.

---

## Restart Flow

```
handleRestartSession (HTTP POST)
        |
        v
manager.Restart(sessionID)
        |
        |-- Get session, verify state == offline
        |-- Save name, workDir, provider, externalSessionID, sandboxed flag
        |-- For provider=claude only: resolve session ID from project files if missing
        |-- manager.Remove(old session)
        |
        v
manager.Create(id, workDir, providerCommand, providerResumeArgs,
    &CreateOptions{Provider: provider, ExternalSessionID: externalSessionID})
        |
        (follows standard or sandbox creation path)
        |
        v
Return new session (same ID, same name, resumed conversation)

---

If session not in memory (already cleaned up):
        |
        v
Fallback: load from SQLite history
        |-- Find matching record
        |-- Build provider-specific resume args (`--resume` for claude, `--session` for opencode)
        |-- Preserve sandbox flag from history record
        |
        v
manager.Create(sessionID, rec.WorkDir, providerCommand, args, opts)
```

Resume behavior is provider-aware: Claude uses `--resume`, OpenCode uses `--session`.

---

## Takeover Flow

Takeover converts a discovered (externally-running) provider process into a managed session:

```
handleTakeover (HTTP POST)
        |
        |-- Verify session state == discovered
        |-- Save provider, externalSessionID, workDir, PID, name
        |-- For provider=claude only: resolve session ID from project files if missing
        |
        v
discovery.KillProcess(pid, 5s timeout)
        |
        |-- Send SIGTERM
        |-- Wait up to 5 seconds for process to exit
        |-- Send SIGKILL if still alive
        |
        v
manager.Remove(sessionID)  -- remove discovered placeholder
        |
        v
manager.Create(sessionID, workDir, providerCommand,
    providerResumeArgs, &CreateOptions{Provider: provider, ExternalSessionID: externalSessionID})
        |
        |-- Creates tmux session with provider-specific resume args
        |-- Provider CLI picks up the conversation where possible
        |   when an external session ID is known
        |
        v
Return terminal view to browser (auto-opens session)
```

The key insight: websessions preserves provider/session metadata and relaunches with provider-appropriate resume flags after killing the external process.

---

## Auto-Cleanup

A background goroutine runs every 60 seconds and archives stale sessions:

```
Ticker (every 60 seconds)
        |
        v
For each session in manager:
        |
        +-- State == completed or errored?
        |     |
        |     +-- EndTime set and > 5 minutes ago?
        |           |
        |           +-- YES: manager.Remove(id)
        |           |        (session data already persisted
        |           |         in SQLite by onStateChange)
        |           |
        |           +-- NO: keep in active list
        |
        +-- State == discovered?
              |
              (handled by scan ticker, not cleanup ticker)
```

Sessions in `offline` state are NOT auto-cleaned; they persist until explicitly restarted or killed by the user.

---

## Offline Recovery on Server Restart

The full startup sequence for session recovery:

```
Server main() starts
        |
        v
1. RecoverTmuxSessions()
   |-- Find ws-* tmux sessions still running
   |-- Reattach each one (State: running, start reader)
   |-- Result: N sessions recovered from tmux
        |
        v
2. Load from SQLite (last 50 records)
   |-- Filter: only status in {running, waiting, created}
   |-- Skip: discovered-* prefixed sessions
   |-- Skip: sessions already recovered from tmux in step 1
   |-- AddOffline() for each remaining record
   |-- Result: M offline sessions loaded
        |
        v
3. Initial discovery scan
   |-- discovery.Scan() for running provider processes
   |-- Deduplicate by PID and provider+workDir against steps 1+2
   |-- AddDiscovered() for new processes
   |-- Result: K discovered sessions
        |
        v
4. Start periodic scan ticker (every scan_interval)
5. Start auto-cleanup ticker (every 60s)
6. Start HTTP server
```

### Session State After Restart

| Scenario | Result |
|----------|--------|
| tmux session still alive | Reattached as `running`, output streaming resumes |
| tmux gone, was `running` in DB | Added as `offline`, user can restart with provider resume args |
| tmux gone, was `killed`/`completed`/`errored` | Not restored (terminal state) |
| External process still running | Added as `discovered`, user can take over |

---

## Graceful Shutdown

```
SIGINT or SIGTERM received
        |
        v
For each session in manager:
        |
        |-- If sandboxed: docker.SandboxStop(name)
        |-- Save to SQLite with current state
        |       (preserves ID, name, claudeID, workDir, state, etc.)
        |
        v
HTTP server shutdown (5s timeout)
        |
        v
Exit
```

Tmux sessions are intentionally NOT killed on shutdown. They continue running so they can be reattached on the next server start.

---

## Key Data Structures

### Session Struct

```go
type Session struct {
    mu sync.RWMutex

    ID                string     // Unique identifier (user-provided or generated)
    Provider          string     // "claude" or "opencode"
    ExternalSessionID string     // Provider session ID used for resume/takeover
    ClaudeID          string     // Claude conversation session ID (legacy/claude-specific)
    Name              string     // Display name (shown in sidebar)
    WorkDir           string     // Working directory
    State             State      // Current state (see state machine)
    PID               int        // OS process ID (for discovered sessions)
    StartTime         time.Time  // When session was created
    EndTime           time.Time  // When session ended (completed/errored)
    ExitCode          int        // Process exit code
    Error             string     // Error message (if errored)
    Owned             bool       // true = created by websessions; false = discovered/offline
    Killed            bool       // true = intentionally killed by user (suppresses notification)
    TmuxSession       string     // tmux session name (e.g. "ws-myproject")
    Sandboxed         bool       // Running inside Docker Desktop sandbox VM
    SandboxName       string     // Docker sandbox name (e.g. "claude-myproject")

    readerPTY *os.File      // PTY fd for tmux attach reader (used for resize)
    output    *RingBuf      // Circular output buffer
}
```

### Manager Struct

```go
type Manager struct {
    mu            sync.RWMutex
    sessions      map[string]*Session  // Active sessions by ID
    bufferSize    int64                // Ring buffer size per session
    onStateChange StateChangeFunc      // Callback: func(s, from, to)
    onOutput      OutputFunc           // Callback: func(sessionID, data)
    stopReaders   map[string]chan struct{} // Signals to stop reader goroutines
}
```

### Callback Types

```go
type StateChangeFunc func(s *Session, from, to State)
type OutputFunc func(sessionID string, data []byte)
```

The `onStateChange` callback wired in `main.go` handles:
- Resolving `ClaudeID` for claude-provider sessions when not yet known
- Publishing notification events to the bus
- Persisting session state to SQLite
- Skipping notifications for intentionally killed sessions

### Session List Ordering

Sessions are sorted by priority for sidebar display:

| Priority | States |
|----------|--------|
| 0 (top) | `running`, `waiting` |
| 1 | `created` |
| 2 | `discovered` |
| 3 | `offline` |
| 4 | `completed` |
| 5 | `errored` |

Within the same priority, sessions are sorted alphabetically by name, then by ID.

---

## Configuration Options

Relevant settings in `~/.websessions/config.yaml`:

```yaml
sessions:
  scan_interval: "30s"           # How often to scan for external provider processes
                                  # Set to 0 to disable discovery scanning
  output_buffer_size: "10MB"     # Ring buffer size per session
                                  # Determines how much scrollback new clients see
  default_dir: "~/projects"      # Default working directory for new sessions
providers:
  default: claude
  claude:
    command: claude
  opencode:
    command: opencode
```

| Setting | Default | Description |
|---------|---------|-------------|
| `scan_interval` | `30s` | Discovery scan frequency. Also controls health-check of discovered processes. |
| `output_buffer_size` | `10MB` | Per-session circular buffer. Larger values retain more scrollback but use more memory. |
| `default_dir` | `~/projects` | Pre-filled directory in the new session dialog. |
| `providers.default` | `claude` | Default provider value in config (currently parsed/validated but not yet used by session creation UI). |

---

## Manual Smoke Checklist

Use this quick checklist after provider-related changes:

1. Create a Claude session from the New Session modal and verify the sidebar type badge shows `claude`.
2. Create an OpenCode session from the same modal and verify the sidebar/tab show OpenCode labeling (`opencode` / `OC`).
3. Restart one Claude session and one OpenCode session from history/offline state and confirm provider-specific resume args are used (`--resume` for Claude, `--session` for OpenCode).
4. Run takeover for discovered provider sessions and confirm replacement sessions keep the original provider metadata.
5. Verify `/api/provider-sessions` returns provider-filtered resume candidates and `/api/claude-sessions` compatibility route still works.
