# Discovery and Takeover System

## Overview

The discovery and takeover system finds supported provider CLIs (`claude` and `opencode`) running outside websessions, displays them in the UI, and lets users "take over" those sessions -- killing the external process and resuming the same conversation inside a managed tmux session. This enables websessions to act as a command center for managed and externally-started provider sessions on the machine.

The system has four stages: **scan** (find provider processes), **discover** (track them in the session manager), **resolve** (recover provider/session metadata), and **takeover** (kill + resume with provider-specific args).

---

## Architecture

```
                              PROCESS SCANNING
    .-----------------------------------------------------------.
    |                                                           |
    |  Linux: read /proc/<pid>/cmdline + /proc/<pid>/cwd       |
    |  macOS: ps -eo pid,comm  +  ps -o command=  +  lsof cwd  |
    |                                                           |
    '----------------------------+------------------------------'
                                 |
                                 v
                       discovery.Scan()
                       returns []ProcessInfo
                                 |
                .----------------+-----------------.
                |                                  |
                v                                  v
      Initial scan (sync)               Background ticker
      at server startup                 (every scan_interval)
      cmd/websessions/main.go           cmd/websessions/main.go
                |                                  |
                '----------------+-----------------'
                                 |
                                 v
                          DEDUP FILTER
                 skip if PID already tracked
                 skip if WorkDir owned by managed session
                                 |
                                 v
                     mgr.AddDiscovered(id, ...)
                     state = "discovered"
                     Owned = false
                                 |
                                 v
                 .-------------------------------.
                 | Session visible in UI sidebar |
                 | with "Takeover" button         |
                 '---------------+---------------'
                                 |
                          user clicks takeover
                                 |
                                 v
                     POST /session/{id}/takeover
                                 |
                                 v
                       handleTakeover()
                                 |
                 .---------------+----------------.
                 |                                |
                 v                                v
         discovery.KillProcess()          mgr.Create(id, ...)
         SIGTERM -> wait -> SIGKILL       provider resume command
                                                  |
                                                  v
                                     New owned tmux session
                                     state = "running"
```

---

## Process Scanning

The `discovery.Scan()` function dispatches to a platform-specific scanner. Both platforms produce the same `[]ProcessInfo` result.

### Linux: /proc Scanning

```
/proc/
  1234/
    cmdline    "claude\x00--resume\x00abc-123"    <- NUL-separated args
    cwd        -> /home/user/myproject             <- symlink to working dir
  5678/
    cmdline    "node\x00server.js"                 <- not claude, skipped
    cwd        -> /home/user/webapp
```

Steps:

1. Read all entries in `/proc`
2. Filter to numeric directories (PIDs)
3. Read `/proc/<pid>/cmdline` -- bytes are NUL-separated, converted to space-separated string
4. Call `ParseCmdline()` -- skip if binary basename is not `claude` or `opencode`
5. Read `/proc/<pid>/cwd` symlink for the working directory
6. If provider is `claude` and no `--resume`/`--session-id`/`--session` flag is found, resolve session ID from `~/.claude/projects/`

### macOS: ps + lsof Scanning

macOS lacks `/proc`, so the scanner uses standard Unix tools:

```
Step 1:  ps -eo pid,comm
         filter lines where comm basename is a supported provider binary
         collect candidate PIDs

Step 2:  For each PID:
         ps -o command= -p <pid>
         full command line for ParseCmdline()

Step 3:  lsof -a -p <pid> -d cwd -Fn
         extract line starting with "n" -> working directory
```

The `lsof` output format uses tagged fields. Lines starting with `n` contain the file name (the cwd path). The `-Fn` flag requests only the name field.

### Platform Support

| Platform | Scanner | CWD Source | Full Cmdline Source |
|----------|---------|------------|---------------------|
| Linux | `/proc` filesystem | `/proc/<pid>/cwd` symlink | `/proc/<pid>/cmdline` |
| macOS | `ps` + `lsof` | `lsof -a -p <pid> -d cwd -Fn` | `ps -o command= -p <pid>` |
| Other | Not supported | -- | -- |

---

## What Gets Scanned

### Binary Detection

The scanner looks for processes whose binary basename maps to a supported provider:

```go
func ProviderForBinary(path string) (string, bool) {
    switch filepath.Base(path) {
    case "claude":
        return "claude", true
    case "opencode":
        return "opencode", true
    default:
        return "", false
    }
}
```

This matches `/usr/local/bin/claude`, `/usr/local/bin/opencode`, and basename invocations (`claude`, `opencode`). It does not match `claude-code`, `node`, or wrapper scripts.

### Command Line Parsing

`ParseCmdline()` extracts structured information from the command line string:

```
Input:  "opencode --session oc-123"

Parsed:
  Binary:            "opencode"
  Provider:          "opencode"
  Args:              ["--session", "oc-123"]
  ExternalSessionID: "oc-123"
```

Flag precedence is provider-specific:

| Provider | Priority order | Notes |
|----------|----------------|-------|
| `claude` | `--resume`, then `--session-id`, then `--session` | `ClaudeID` mirrors resolved external session ID |
| `opencode` | `--session`, then `--session-id`, then `--resume` | Session ID is tracked in `ExternalSessionID`; no `.claude` fallback |

If no supported flag is present, only `claude` attempts fallback resolution from project files.

### The ProcessInfo Struct

```go
type ProcessInfo struct {
    PID               int       // OS process ID
    Binary            string    // full path or basename of the provider binary
    Provider          string    // "claude" or "opencode"
    WorkDir           string    // current working directory of the process
    Args              []string  // command line arguments (excluding binary)
    ExternalSessionID string    // provider session ID from CLI flags/fallbacks
    ClaudeID          string    // claude-only mirror of ExternalSessionID
    StartTime         time.Time // process start time (used for claude ID resolution)
}
```

---

## Discovery Loop

### Initial Scan (Startup)

When websessions starts, it runs a synchronous scan before printing the banner:

```
Server startup
      |
      v
discovery.Scan()
      |
      v
Build dedup sets from already-recovered tmux sessions:
  existingPIDs = { pid of each recovered session }
  existingDirs = { workDir of each OWNED session }
      |
      v
For each discovered process:
  - Skip if PID in existingPIDs
  - Skip if WorkDir in existingDirs (and session is Owned)
  - ID = "discovered-<PID>"
  - mgr.AddDiscovered(id, claudeID, workDir, pid, startTime)
  - Save to SQLite with status "discovered"
      |
      v
Print: "Discovered N Claude session(s)"
```

### Background Ticker

If `scan_interval > 0`, a goroutine runs periodic scans:

```
ticker fires (every scan_interval, default 30s)
      |
      +--- Phase 1: Health check
      |    For each session in StateDiscovered:
      |      if process is dead (signal 0 fails):
      |        save as "completed" in SQLite
      |        mgr.Remove(id)
      |
      +--- Phase 2: New discovery
           discovery.Scan()
           Build fresh dedup sets
           Add new processes as "discovered-<PID>"
```

### Dedup Logic

The dedup filter prevents duplicates using two checks:

| Check | Condition | Why |
|-------|-----------|-----|
| PID match | `existingPIDs[p.PID]` | Same process already tracked |
| WorkDir match | `existingDirs[p.WorkDir]` when Owned | An owned session already manages this directory |

The WorkDir check only applies to **owned** sessions. Discovered sessions do not block other discovered sessions for the same directory (though in practice this is unlikely).

### Session Naming

Discovered sessions are named after the basename of their working directory:

```go
name := filepath.Base(workDir)   // "/home/user/myproject" -> "myproject"
```

Their ID is always `discovered-<PID>`, e.g., `discovered-12345`.

---

## Claude Project File Scanning (Claude Provider Only)

Claude Code stores conversation history in `~/.claude/projects/`. The directory structure uses a mangled version of the working directory path:

```
~/.claude/projects/
  -home-user-myproject/
    abc-123-def-456.jsonl         <- session transcript
    ghi-789-jkl-012.jsonl         <- another session
  -home-user-other-project/
    mno-345-pqr-678.jsonl
```

### Path Mangling

The working directory is converted to a project folder name by replacing `/` and `.` with `-`:

```
/home/user.name/myproject  ->  -home-user-name-myproject
```

### Session ID Resolution

`ResolveClaudeSessionID()` finds the active session ID for a given working directory:

```
ResolveClaudeSessionID("/home/user/myproject")
      |
      v
Compute project name: "-home-user-myproject"
      |
      v
Read ~/.claude/projects/-home-user-myproject/
      |
      v
Collect all .jsonl files with their modification times
      |
      v
Pick the most recently modified file
      |
      v
Return filename without .jsonl extension as session ID
```

### Process-Aware Resolution

When a process start time is available, `ResolveClaudeSessionIDForProcess()` uses a smarter heuristic:

```
Given: workDir, processStartTime
      |
      v
Collect .jsonl files in the project directory
      |
      v
Filter: keep only files modified AFTER processStartTime
  (files not written since the process started are stale)
      |
      v
Among remaining files: pick the one with mod time
  closest to processStartTime (smallest delta)
      |
      v
Fallback: if no files modified after start time,
  pick the most recently modified file overall
```

This handles the case where multiple Claude instances share the same working directory -- each will have a different `.jsonl` file, and the one whose modification time is closest to the process start belongs to that process.

### Where Resolution Is Used

| Caller | When | Function Used |
|--------|------|---------------|
| `scanLinux()` / `scanDarwin()` | During process scan, when provider=`claude` and no external session ID in args | `ResolveClaudeSessionIDForProcess()` |
| `handleTakeover()` | Before killing, when provider=`claude` and no external session ID | `ResolveClaudeSessionID()` |
| `OnStateChange` callback | State change for `claude` sessions missing `ClaudeID` | `ResolveClaudeSessionID()` |
| `RecoverTmuxSessions()` | Reattaching tmux sessions with claude provider metadata | `ResolveClaudeSessionID()` |
| `Restart()` | Restarting offline claude sessions with missing external session ID | `ResolveClaudeSessionID()` |

---

## Session Resolution from Hooks

The `handleHookCallback()` endpoint receives notifications from Claude Code hooks (configured in `~/.claude/settings.json`). When a hook fires, it must map the external event to a websessions session.

### Hook Payload

```json
{
  "event": "waiting",
  "session_id": "claude-internal-uuid",
  "project": "/home/user/myproject"
}
```

Only `"waiting"` events are processed (permission prompts). Other events like `"completed"` or `"tool_use"` are ignored to avoid noise.

### Resolution Strategy

```
handleHookCallback()
      |
      v
Step 1: Match by WorkDir
  For each active session:
    if session.WorkDir == payload.Project:
      FOUND -> use this session
      |
      v
Step 2: Auto-discover (if not found)
  discovery.Scan()
  For each process:
    if process.WorkDir == payload.Project:
      if process PID already tracked:
        FOUND -> use existing session
      else:
        mgr.AddDiscovered("discovered-<PID>", ...)
        FOUND -> use new discovered session
      |
      v
Step 3: External placeholder (if still not found)
  ID = "external-<basename>"
  name = basename of project path
  (no session in manager, notification only)
```

The three-step cascade ensures that hook notifications always produce a notification, even if the process has already exited by the time the hook callback arrives.

---

## Takeover Flow

When a user clicks the "Takeover" button on a discovered session:

```
Browser: POST /session/discovered-12345/takeover
                      |
                      v
              handleTakeover()
                      |
                      v
         Validate: state must be "discovered"
                      |
                      v
         Resolve external session ID if needed
         (claude only: ResolveClaudeSessionID from project files)
                      |
                      v
         discovery.KillProcess(pid, 5s timeout)
                      |
         .------------+-------------.
         |                          |
         v                          v
  Send SIGTERM              Wait up to 5 seconds
  to external process       polling with signal 0
         |                          |
         |                          +-- Process exited? -> done
         |                          |
         |                          +-- Timeout reached?
         |                                    |
         |                                    v
         |                            Send SIGKILL (force)
         |                            Wait 200ms
         |                            proc.Wait()
         |
         v
  mgr.Remove("discovered-12345")
  (remove discovered session from manager)
         |
         v
  mgr.Create("discovered-12345", workDir, providerCommand, args)
          |
          v
  if provider == "claude":
    args = ["--name", "<project-name>"]
    if externalSessionID != "": args += ["--resume", "<session-id>"]
  if provider == "opencode":
    args = []
    if externalSessionID != "": args += ["--session", "<session-id>"]
         |
         v
  New tmux session created
  State: running, Owned: true
         |
         v
  Render terminal view in browser
```

### KillProcess Details

The kill sequence is deliberately graceful:

```
SIGTERM (graceful shutdown request)
    |
    v
Poll loop (100ms intervals, up to timeout):
    signal 0 -> process still alive? keep waiting
    signal 0 -> error? process exited, success
    |
    v
If timeout reached:
    SIGKILL (force kill, cannot be caught)
    wait 200ms
    proc.Wait() (reap zombie)
```

This gives Claude Code time to save state before being forcefully terminated.

### Provider Resume Flags

Takeover uses provider-specific resume arguments:

- `claude`: starts `claude --name <name> --resume <session-id>` when an ID is available
- `opencode`: starts `opencode --session <session-id>` when an ID is available

For Claude, `--resume` maps to `~/.claude/projects/<project>/<session-id>.jsonl`. For OpenCode, websessions only reuses IDs it observed from process args or stored records.

---

## Provider Session Listing

The UI uses `GET /api/provider-sessions?provider=<claude|opencode>&work_dir=<path>` to list resumable sessions:

- `provider=claude`: reads `~/.claude/projects/<mangled-workdir>/*.jsonl` (same behavior as legacy `GET /api/claude-sessions?dir=...`)
- `provider=opencode`: reads session records from SQLite (`provider`, `external_session_id`, `work_dir`)

```
GET /api/claude-sessions?dir=/home/user/myproject
      |
      v
Compute project folder:
  /home/user/myproject -> -home-user-myproject
      |
      v
Read ~/.claude/projects/-home-user-myproject/
      |
      v
For each .jsonl file:
  - Extract session ID from filename
  - Read first "user" message as summary (up to 100 chars)
  - Collect modification date and file size
      |
      v
Return JSON sorted by date (most recent first):
[
  {
    "id": "abc-123-def-456",
    "date": "2026-03-26 14:32",
    "summary": "Fix the authentication bug in...",
    "size_kb": 42
  }
]
```

---

## Configuration

```yaml
sessions:
  scan_interval: 30s    # How often to scan for new external provider processes
                        # Default: 30s
                        # Set to 0 to disable background scanning
                        # (initial startup scan always runs)
  output_buffer_size: 1048576   # Ring buffer size for session output
```

| Setting | Default | Description |
|---------|---------|-------------|
| `scan_interval` | `30s` | Interval between background discovery scans. Accepts Go duration strings (`10s`, `1m`, `0`). |

When `scan_interval` is 0, only the initial synchronous scan at startup runs. The background ticker goroutine is never started.

---

## Edge Cases

### Process Already Tracked

If a discovered process has the same PID as an existing session, it is silently skipped. This prevents duplicate entries when the background ticker rescans and finds the same processes.

### WorkDir Already Owned

If a discovered process has the same working directory as an **owned** session, it is skipped. This prevents showing a "discovered" entry for a process that websessions itself spawned. Note: this check only applies to owned sessions -- two discovered sessions can share a WorkDir.

### PID Reuse

When a discovered session's process dies, the health-check phase of the background ticker removes it:

```
Health check: signal 0 -> process not found
  -> save as "completed"
  -> mgr.Remove(id)
```

If the OS reuses that PID for a new claude process, the next scan will discover it as a new session with a fresh `discovered-<PID>` entry. Since the old entry was already removed, there is no conflict.

### Session Without WorkDir Match

When `handleHookCallback()` cannot find a matching session and the process scan also fails to find a match (e.g., process exited between the hook firing and the callback arriving), it falls back to an external placeholder:

```
ID:   "external-myproject"
Name: "myproject"
```

The notification still fires, but there is no session in the manager to click on. The user sees the notification but cannot navigate to the session.

### No External Session ID Available

If no session flag is present in the command line, the session ID may remain empty. For Claude, websessions also attempts `.jsonl` fallback resolution; for OpenCode there is no filesystem fallback. Takeover still works, but it starts without a resume/session flag so history continuity depends on provider behavior.

### Multiple Claude Instances in Same Directory

When multiple Claude processes share the same working directory, `ResolveClaudeSessionIDForProcess()` uses the process start time to disambiguate. It picks the `.jsonl` file whose modification time is closest to (but after) the process start time. This is a heuristic -- it works well when processes start at different times but may pick the wrong file if two processes start within the same second.

### Auto-Cleanup of Stale Sessions

Completed and errored sessions are automatically removed from the active session list after 5 minutes. This applies to all sessions, including discovered ones whose processes died. A separate goroutine checks every 60 seconds:

```
For each session in completed or errored state:
  if EndTime was more than 5 minutes ago:
    mgr.Remove(id)
```

---

## Complete Example Flow

The detailed walkthrough below uses Claude; OpenCode follows the same scan/dedup/takeover mechanics but resumes with `opencode --session <id>`.

```
1. User starts "claude" in terminal at ~/myproject
   PID 12345 begins running

2. websessions background ticker fires (every 30s)
   discovery.Scan() finds PID 12345
     - /proc/12345/cmdline -> "claude"
     - /proc/12345/cwd -> /home/user/myproject
     - No --resume flag in args
     - Resolves session ID from ~/.claude/projects/-home-user-myproject/
       finds abc-123.jsonl (most recent)
     - ProcessInfo{PID:12345, WorkDir:"/home/user/myproject", ClaudeID:"abc-123"}

3. Dedup check passes (PID not tracked, WorkDir not owned)
   mgr.AddDiscovered("discovered-12345", "abc-123", "/home/user/myproject", 12345, ...)
   Session appears in UI sidebar as "myproject" with state "discovered"

4. User clicks "Takeover" button in the UI
   POST /session/discovered-12345/takeover

5. handleTakeover():
   - Validates state is "discovered"
   - ClaudeID = "abc-123" (already resolved)
   - KillProcess(12345, 5s):
     - SIGTERM sent to PID 12345
     - Polls every 100ms... process exits after 800ms
   - mgr.Remove("discovered-12345")
   - mgr.Create("discovered-12345", "/home/user/myproject", "claude",
       ["--name", "myproject", "--resume", "abc-123"])
   - New tmux session "ws-discovered-12345" created
   - Claude resumes conversation abc-123

6. User now sees a live terminal in the browser
   Session state: "running", Owned: true
   Full terminal output streamed via WebSocket
```
