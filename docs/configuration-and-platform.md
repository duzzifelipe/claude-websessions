# Configuration and Platform Services

## Overview

websessions uses a layered configuration system with sensible defaults, an optional YAML config file, environment variable overrides, and CLI flags. Runtime state is persisted to a SQLite database. The Settings UI exposes all tunables in a single page, alongside integrations (Claude Code hooks, Plannotator), platform service management (systemd/launchd), self-update, and system health checks.

---

## Configuration File

The configuration lives at `~/.websessions/config.yaml`. It is created automatically when you first save settings from the UI. The full schema:

```yaml
server:
  port: 8080                  # HTTP listen port
  host: "0.0.0.0"             # Bind address ("0.0.0.0" = all interfaces)

sessions:
  scan_interval: "30s"        # How often to scan for new external provider processes
  output_buffer_size: "10MB"  # Per-session ring buffer for terminal output
  default_dir: "~/projects"   # Default working directory for new sessions

providers:
  default: claude              # Allowed: claude, opencode (invalid values fallback to claude)
  claude:
    command: claude            # Fallback if empty: "claude"
    args: []                   # Reserved for provider defaults
  opencode:
    command: opencode          # Fallback if empty: "opencode"
    args: []                   # Reserved for provider defaults

notifications:
  desktop: true               # Enable native OS notifications
  sound: true                 # Enable server-side audio playback
  audio_device: ""            # PulseAudio sink name (empty = system default)
  events:                     # Which event types trigger notifications
    - completed
    - errored
    - waiting
  reminder_minutes: 5         # Re-notify interval for waiting sessions (0 = disabled)

docker:
  copy_credentials: true      # Copy Claude credentials into sandbox VMs
```

### All Options With Defaults

| Section | Key | Default | Description |
|---------|-----|---------|-------------|
| `server.port` | `port` | `8080` | HTTP server listen port |
| `server.host` | `host` | `0.0.0.0` | Bind address |
| `sessions.scan_interval` | `scan_interval` | `30s` | Discovery scan interval (Go duration) |
| `sessions.output_buffer_size` | `output_buffer_size` | `10MB` | Ring buffer per session (supports KB, MB, GB) |
| `sessions.default_dir` | `default_dir` | `~/projects` | Pre-filled working directory in "New Session" |
| `providers.default` | `default` | `claude` | Normalized to `claude`/`opencode`; invalid values fallback to `claude` |
| `providers.claude.command` | `command` | `claude` | Claude provider executable; empty values fallback to default |
| `providers.claude.args` | `args` | `[]` | Reserved provider arg defaults (parsed from YAML) |
| `providers.opencode.command` | `command` | `opencode` | OpenCode provider executable; empty values fallback to default |
| `providers.opencode.args` | `args` | `[]` | Reserved provider arg defaults (parsed from YAML) |
| `notifications.desktop` | `desktop` | `true` | OS-level notifications (notify-send / osascript) |
| `notifications.sound` | `sound` | `true` | Server-side audio (paplay / afplay) |
| `notifications.audio_device` | `audio_device` | `""` | PulseAudio sink name or empty for default |
| `notifications.events` | `events` | `[completed, errored, waiting]` | Event types that fire notifications |
| `notifications.reminder_minutes` | `reminder_minutes` | `5` | Minutes between reminder pings for waiting sessions |
| `docker.copy_credentials` | `copy_credentials` | `true` | Auto-copy Claude creds into Docker sandboxes |

Provider config is loaded and normalized by `internal/config`. Session launch still resolves to `claude`/`opencode` command names in manager/handler logic; provider `args` are not auto-applied yet.

### Byte Size Parsing

The `output_buffer_size` field accepts human-readable sizes:

| Suffix | Multiplier |
|--------|------------|
| `B` | 1 |
| `KB` | 1,024 |
| `MB` | 1,048,576 |
| `GB` | 1,073,741,824 |

---

## Config Loading Priority

Configuration is resolved in the following order. Each layer overrides the previous:

```
  +------------------+
  |   Hard-coded     |    config.defaults()
  |   defaults       |    (always applied first)
  +--------+---------+
           |
           v
  +------------------+
  |   YAML file      |    ~/.websessions/config.yaml
  |                  |    (if --config flag or default path exists)
  +--------+---------+
           |
           v
  +------------------+
  |   Environment    |    applyEnvOverrides()
  |   variables      |    (reserved for future use)
  +--------+---------+
           |
           v
  +------------------+
  |   CLI flags      |    --config, --log-level, --gui
  +------------------+
```

### CLI Flags

| Flag | Description |
|------|-------------|
| `--config <path>` | Path to config YAML (overrides default `~/.websessions/config.yaml`) |
| `--log-level <level>` | Log verbosity: `debug`, `info` (default), `warn`, `error` |
| `--gui` | Launch in GUI mode (native window via WebKit/webview) |

If `--config` is not specified, the server checks for `~/.websessions/config.yaml` and loads it if present. If absent, pure defaults are used.

---

## Settings UI

The Settings page (`/settings`) exposes runtime server/session/notification options in a single form plus several integration panels.

Provider config (`providers.default`, `providers.*.command`, `providers.*.args`) is currently file-driven (`config.yaml`) and not editable from the Settings UI.

### What Can Be Changed at Runtime vs Requires Restart

| Setting | Applies Immediately | Requires Restart |
|---------|:------------------:|:----------------:|
| Discovery scan interval | | X |
| Output buffer size | | X |
| Default working directory | X | |
| Desktop notifications | X | |
| Notification sounds | X | |
| Audio device | X | |
| Notification events | X | |
| Reminder interval | | X |
| **Port** | | **X** |
| **Host** | | **X** |

When port or host are changed, the server automatically uninstalls Claude Code hooks (since they contain the old URL) and prompts reinstallation after restart.

### Settings Page Sections

```
 +------------------------------------------+
 | Settings                                  |
 |------------------------------------------|
 | [System Health]  Doctor checks grid       |
 |------------------------------------------|
 | [Server]         Port, Host               |
 |------------------------------------------|
 | [Sessions]       Scan interval, buffer,   |
 |                  default dir              |
 |------------------------------------------|
 | [Notifications]  Desktop, sound, events,  |
 |                  audio device, reminder   |
 |------------------------------------------|
 |              [Cancel] [Save Settings]     |
 |------------------------------------------|
 | [Claude Code Hooks]    Install/Uninstall  |
 |------------------------------------------|
 | [Integrations]         Plannotator on/off |
 |------------------------------------------|
 | [Background Service]   systemd/launchd    |
 |                        management         |
 |------------------------------------------|
 | [Updates]              Check / self-update|
 +------------------------------------------+
```

### Save Flow

```
User submits form
       |
       v
handleSaveSettings()
       |
       +-- Parse form values into cfg struct
       |
       +-- If port/host changed:
       |       uninstall Claude hooks (URL embedded in hook command)
       |       log warning to reinstall
       |
       +-- Update sound sink (enabled, device) immediately
       |
       +-- cfg.Save() --> writes ~/.websessions/config.yaml
       |
       v
Redirect to /settings?saved=1
```

---

## SQLite Store

All persistent state lives in `~/.websessions/websessions.db` (pure-Go SQLite via `modernc.org/sqlite`, no CGo dependency).

### Schema

```
sessions
+-------------+-----------+-----------------------------------------------+
| Column      | Type      | Description                                   |
+-------------+-----------+-----------------------------------------------+
| id          | TEXT PK   | Session ID (e.g., "sess-abc", "discovered-42")|
| name        | TEXT      | Display name                                  |
| provider    | TEXT      | Provider name (`claude`, `opencode`, or empty legacy row) |
| external_session_id | TEXT | Provider session ID used for resume/takeover |
| claude_id   | TEXT      | Claude session ID (legacy/claude compatibility field) |
| work_dir    | TEXT      | Working directory path                        |
| start_time  | DATETIME  | When the session started                      |
| end_time    | DATETIME  | When the session ended (if applicable)        |
| exit_code   | INTEGER   | Process exit code                             |
| status      | TEXT      | Last known state (running, completed, etc.)   |
| pid         | INTEGER   | OS process ID                                 |
| sandboxed   | BOOLEAN   | Whether session runs in Docker sandbox        |
| sandbox_name| TEXT      | Docker sandbox VM name                        |
+-------------+-----------+-----------------------------------------------+

notifications
+-------------+-----------+-----------------------------------------------+
| Column      | Type      | Description                                   |
+-------------+-----------+-----------------------------------------------+
| id          | INTEGER PK| Auto-increment                                |
| session_id  | TEXT      | Associated session                            |
| event_type  | TEXT      | "completed", "errored", "waiting"             |
| timestamp   | DATETIME  | When the notification was created              |
| read        | BOOLEAN   | Whether user has dismissed it                 |
+-------------+-----------+-----------------------------------------------+

audit_log
+-------------+-----------+-----------------------------------------------+
| Column      | Type      | Description                                   |
+-------------+-----------+-----------------------------------------------+
| id          | INTEGER PK| Auto-increment                                |
| action      | TEXT      | Action performed (create, kill, etc.)         |
| session_id  | TEXT      | Affected session                              |
| client_ip   | TEXT      | IP of the client that triggered the action    |
| timestamp   | DATETIME  | When the action occurred                      |
+-------------+-----------+-----------------------------------------------+

preferences
+-------------+-----------+-----------------------------------------------+
| Column      | Type      | Description                                   |
+-------------+-----------+-----------------------------------------------+
| key         | TEXT PK   | Preference name (e.g., "open-tabs")           |
| value       | TEXT      | JSON or plain text value                      |
+-------------+-----------+-----------------------------------------------+
```

### Preferences System

The `preferences` table is a key-value store used for UI state that persists across page reloads:

| Key | Usage |
|-----|-------|
| `open-tabs` | JSON array of open tab state (split trees, active sessions) |

The API endpoints for preferences:

- `GET /api/preferences` -- returns all preferences as a JSON object
- `POST /api/preferences` -- sets a single `{"key": "...", "value": "..."}` pair

### Database Initialization

On startup, `store.Open()` runs `migrate()` which:

1. Creates all four tables with `CREATE TABLE IF NOT EXISTS`
2. Runs `ALTER TABLE` migrations for columns added after initial schema (name, sandboxed, sandbox_name, provider, external_session_id) -- these silently succeed or fail if already present
3. Enables WAL journal mode for concurrent read/write performance

---

## Claude Code Hooks

Hooks let websessions know when Claude Code (running outside or inside websessions) hits a permission prompt that needs user attention.

### What They Are

A hook is an entry in `~/.claude/settings.json` under the `hooks` key. When Claude Code encounters a matching event, it executes the hook command with session context on stdin.

### The Hook Command

```
python3 -c "import sys,json; d=json.load(sys.stdin); \
  print(json.dumps({'event':'waiting', \
  'session_id':d.get('session_id',''), \
  'project':d.get('cwd',d.get('project',''))}))" \
  | curl -s -X POST http://localhost:8080/api/hook \
  -H "Content-Type: application/json" -d @- # websessions-hook
```

The trailing `# websessions-hook` comment is the **marker** used to identify and manage websessions hooks. The `containsMarker()` function searches for this string to detect installation.

### What Gets Written to settings.json

```json
{
  "hooks": {
    "Notification": [
      {
        "matcher": "permission_prompt",
        "hooks": [
          {
            "type": "command",
            "command": "python3 -c \"...\" | curl -s -X POST http://localhost:8080/api/hook ... # websessions-hook"
          }
        ]
      }
    ]
  }
}
```

Only the `Notification` event with `permission_prompt` matcher is hooked. The `Stop` event was considered but fires after every turn (too noisy).

### Install / Uninstall Flow

```
Install:

  hooks.Load()                    Read ~/.claude/settings.json
       |                          (create empty if missing)
       v
  IsInstalled()?
       |
  +----+----+
  |         |
  yes       no
  |         |
  v         v
  Update    Add hook entry
  URLs      to "Notification"
  |         |
  +----+----+
       |
       v
  settings.Save()                 Write back to settings.json


Uninstall:

  hooks.Load()
       |
       v
  For each event in hooks:
    Filter out entries where
    any hook command contains
    "websessions-hook" marker
       |
       v
  settings.Save()
```

The install/uninstall preserves all other hooks and settings in the file.

### API

- `POST /api/hooks` with `{"action": "install"}` or `{"action": "uninstall"}`

---

## Plannotator Integration

Plannotator is an external tool that generates plan review pages. When integration is enabled, those pages open as iframe panes inside websessions instead of in a separate browser tab.

### How It Works

```
  Enable:

  1. Write ws-open-url script to ~/.local/bin/ws-open-url
  2. Set PLANNOTATOR_BROWSER in ~/.claude/settings.json env block
     to point to the script path

  Disable:

  1. Remove ws-open-url script (best-effort)
  2. Delete PLANNOTATOR_BROWSER from settings.json env block
```

### The ws-open-url Script

```sh
#!/bin/sh
URL="$1"
WS_HOST="${WEBSESSIONS_HOST:-localhost:8080}"
curl -s -X POST "http://${WS_HOST}/api/panes/iframe" \
  -H "Content-Type: application/json" \
  -d "{\"url\":\"$URL\",\"title\":\"Plan Review\"}" \
  -o /dev/null -w "" 2>/dev/null || true
```

When Plannotator calls `$PLANNOTATOR_BROWSER <url>`, this script POSTs to the websessions API which broadcasts an `iframe-open` WebSocket message to all connected browsers. The browser then opens the URL in an iframe pane.

### API

- `POST /api/plannotator` with `{"action": "enable"}` or `{"action": "disable"}`

### Notes

- Takes effect for new Claude sessions only. Existing sessions must be restarted.
- The `WEBSESSIONS_HOST` environment variable can override the default `localhost:8080`.

---

## Platform Service Management

websessions can install itself as a background service that starts automatically on login.

### Architecture

```
  Settings UI
       |
       v
  POST /api/service  {"action": "install|uninstall|start|stop|enable|disable"}
       |
       v
  handleService()
       |
       +-- service.Install()   --> write unit/plist file
       +-- service.Enable()    --> enable autostart
       +-- service.Start()     --> start the service
       +-- service.Stop()      --> stop the service
       +-- service.Disable()   --> disable autostart
       +-- service.Uninstall() --> stop + disable + remove file
       |
       v
  JSON response: {ok, action, status, installed, active, enabled}
```

### Linux (systemd)

| Item | Value |
|------|-------|
| Unit name | `websessions.service` |
| Unit path | `~/.config/systemd/user/websessions.service` |
| Scope | User service (`systemctl --user`) |

Generated unit file:

```ini
[Unit]
Description=websessions -- Claude Code Session Manager
After=network.target

[Service]
Type=simple
ExecStart=/path/to/websessions --config ~/.websessions/config.yaml
Restart=on-failure
RestartSec=5
Environment=HOME=/home/user

[Install]
WantedBy=default.target
```

Operations:

| Action | Command |
|--------|---------|
| Install | Write unit file + `systemctl --user daemon-reload` |
| Uninstall | Stop + disable + remove file + daemon-reload |
| Enable | `systemctl --user enable websessions.service` |
| Disable | `systemctl --user disable websessions.service` |
| Start | `systemctl --user start websessions.service` |
| Stop | `systemctl --user stop websessions.service` |
| Status check | `systemctl --user is-active` + `is-enabled` |

### macOS (launchd)

| Item | Value |
|------|-------|
| Plist name | `com.websessions.plist` |
| Plist path | `~/Library/LaunchAgents/com.websessions.plist` |

Generated plist:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.websessions</string>
    <key>ProgramArguments</key>
    <array>
        <string>/path/to/websessions</string>
        <string>--config</string>
        <string>~/.websessions/config.yaml</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>~/.websessions/websessions.log</string>
    <key>StandardErrorPath</key>
    <string>~/.websessions/websessions.log</string>
</dict>
</plist>
```

Operations:

| Action | Command |
|--------|---------|
| Install | Write plist file |
| Uninstall | `launchctl unload` + remove plist |
| Enable/Start | `launchctl load` (installs if needed) |
| Disable/Stop | `launchctl unload` |
| Status check | `launchctl list` (grep for `com.websessions`) |

### Status Display

The `statusString()` helper produces human-readable status:

| State | Display |
|-------|---------|
| Not installed | `not installed` |
| Running + enabled | `running (enabled)` |
| Running + not enabled | `running (not enabled)` |
| Stopped + enabled | `stopped (enabled)` |
| Stopped + disabled | `stopped (disabled)` |

---

## Self-Update

websessions can check for and install updates from GitHub releases without leaving the UI.

### Update Check Flow

```
  User clicks "Check for updates"
         |
         v
  GET /api/update/check
         |
         v
  updater.CheckForUpdate(currentVersion)
         |
         v
  GET https://api.github.com/repos/IgorDeo/claude-websessions/releases/latest
         |
         v
  Parse Release JSON
         |
         +-- Compare tag_name with current version
         |   (strip "v" prefix, compare strings)
         |   ("dev" version always shows update available)
         |
         +-- Find matching asset by platform suffix
         |   suffix = runtime.GOOS + "-" + runtime.GOARCH
         |   e.g., "linux-amd64", "darwin-arm64"
         |   (skip assets with "checksums" in name)
         |
         v
  Return UpdateInfo {
    CurrentVersion, LatestVersion, UpdateAvail,
    ReleaseURL, ReleaseNotes, DownloadURL, AssetName, AssetSize
  }
```

### Self-Update (Binary Replacement) Flow

```
  User clicks "Update now"
         |
         v
  POST /api/update/apply
         |
         v
  updater.CheckForUpdate() again (re-verify)
         |
         +-- If not UpdateAvail: return "already up to date"
         |
         v
  updater.SelfUpdate(downloadURL)
         |
         v
  1. HTTP GET downloadURL --> stream to <binary>.update
  2. Rename current binary --> <binary>.bak
  3. Rename <binary>.update --> current binary path
  4. Remove <binary>.bak
         |
         v
  If step 3 fails: restore <binary>.bak --> original path
         |
         v
  Return "Updated to vX.Y.Z. Restart the server to use the new version."
```

The update is atomic from the filesystem perspective: the old binary is backed up before the new one is moved into place, and restored on failure.

---

## Docker Sandbox

Sessions can run inside Docker Desktop sandbox VMs for isolation. This requires Docker Desktop (not just Docker Engine) with the `docker sandbox` feature.

### When It Is Used

Sandbox mode is opt-in per session. The "New Session" dialog shows a "Sandbox" checkbox if Docker Desktop is available (detected via `docker sandbox version`).

### Provisioning Flow

```
  User checks "Sandbox" + creates session
         |
         v
  docker.IsAvailable()?
         |
    +----+----+
    |         |
    yes       no --> return error "Docker Desktop not available"
    |
    v
  docker.SandboxCreate(workDir)
         |
         v
  docker sandbox create claude <workDir>
         |
         v
  docker.FindSandboxForWorkDir(workDir)
    (looks up the sandbox name from listing)
         |
         v
  docker.SandboxCopyCredentials(name)
    (if config docker.copy_credentials = true)
         |
         +-- Copy ~/.claude/.credentials.json
         +-- Copy ~/.claude.json
         +-- Copy ~/.claude/.settings.json
         |   (each via: docker sandbox exec <name> bash -c "mkdir -p ... && cat > ...")
         |
         v
  Session starts inside sandbox VM
  session.Sandboxed = true
  session.SandboxName = "claude-<basename>"
```

### Sandbox Naming

Docker Desktop uses the pattern `claude-<basename>` where basename is the last component of the workspace directory path.

### Available Operations

| Function | Docker Command |
|----------|----------------|
| `IsAvailable()` | `docker sandbox version` (5s timeout) |
| `ListSandboxes()` | `docker sandbox ls --json` |
| `SandboxCreate(workDir)` | `docker sandbox create claude <workDir>` |
| `SandboxStop(name)` | `docker sandbox stop <name>` |
| `SandboxRemove(name)` | `docker sandbox rm <name>` |
| `FindSandboxForWorkDir(workDir)` | List + filter by workspace match |
| `SandboxExists(name)` | List + filter by name |
| `SandboxCopyCredentials(name)` | `docker sandbox exec` for each credential file |

### Cleanup on Shutdown

On server shutdown (`SIGINT`/`SIGTERM`), all sandboxed sessions are stopped:

```
for each session:
    if session.Sandboxed && session.SandboxName != "":
        docker.SandboxStop(session.SandboxName)
```

Sandboxes are stopped but not removed, so a server restart can reattach to them.

---

## Doctor Checks

The System Health section on the Settings page runs `doctor.RunChecks()` to verify the host environment. Each check returns a status of `ok`, `missing`, or `warning`.

### Checks Performed

| Check | What It Verifies | Status if Missing |
|-------|-----------------|-------------------|
| `tmux` | `tmux` binary in PATH, version | `missing` -- required for session management |
| `claude` | `claude` CLI in PATH, version | `missing` -- required for Claude sessions |
| `docker sandbox` | `docker` in PATH + `docker sandbox version` | `missing` (no docker) or `warning` (engine only, no Desktop) |
| `git` | `git` in PATH, version | `warning` -- optional, needed for git diff viewer |
| `shell` | `$SHELL` environment variable resolves | `warning` -- falls back to bash |
| `sqlite (embedded)` | Always OK (pure Go, no external dep) | n/a |
| `platform` | `runtime.GOOS/GOARCH` + Go version | Always `ok` |
| `config directory` | `~/.websessions/` exists, config.yaml present, DB size | `warning` if dir missing (created on first run) |
| `claude hooks` | `~/.claude/settings.json` exists + contains `websessions-hook` | `warning` if not installed |

---

## Startup Sequence

The `main()` function wires all components in this order:

```
  1. Parse CLI flags (--config, --log-level, --gui)
  2. Configure slog logger
  3. Resolve config path
     (explicit --config OR default ~/.websessions/config.yaml)
  4. config.Load(path) --> defaults + YAML + env overrides
  5. Create ~/.websessions/ directory
  6. store.Open(~/.websessions/websessions.db)
  7. Print ASCII banner to stderr
  8. session.NewManager(outputBufferSize)
  9. notification.NewBus()
 10. notification.NewInAppSink(100)
 11. Register OnStateChange callback
     (publishes events, saves to SQLite)
 12. Check tmux availability (exit if missing)
 13. mgr.RecoverTmuxSessions()
     (reattach to surviving tmux sessions from previous run)
 14. Restore offline sessions from SQLite history
     (running/waiting/created, skip discovered/killed/completed)
  15. Initial discovery scan (synchronous)
      (scan for external provider processes, add as discovered)
 16. Start background discovery ticker (if scan_interval > 0)
 17. Start auto-cleanup ticker (remove stale completed/errored after 5 min)
 18. Start waiting reminder ticker (if reminder_minutes > 0)
 19. server.New(cfg, mgr, bus, sink, store)
 20. srv.SetVersion(version)
 21. srv.SetSnoozeFunc(...)
 22. httpServer.ListenAndServe() (in goroutine)
 23. If --gui: launch native window (in goroutine)
 24. Block on SIGINT/SIGTERM signal
```

---

## Shutdown Sequence

```
  SIGINT or SIGTERM received
  (or GUI window closed in --gui mode)
         |
         v
  Print "Shutting down gracefully..."
         |
         v
  For each active session:
    +-- If sandboxed: docker.SandboxStop(sandboxName)
    +-- Save final state to SQLite
         |
         v
  httpServer.Shutdown(5s timeout)
    (drains in-flight HTTP requests,
     closes WebSocket connections)
         |
         v
  store.Close() (deferred from main)
         |
         v
  Print "All sessions saved. Goodbye!"
```

Key behaviors during shutdown:

- Active tmux sessions survive the server shutdown (they are independent processes). On next server start, `RecoverTmuxSessions()` reattaches to them.
- Docker sandboxes are stopped but not removed, allowing reattachment on restart.
- All session state is persisted to SQLite so offline sessions can be restored.
- The HTTP server gets a 5-second grace period for request draining.

---

## Install Script

The `install.sh` script provides a one-line install via curl:

```sh
curl -LsSf https://raw.githubusercontent.com/IgorDeo/claude-websessions/main/install.sh | sh
```

### Install Flow

```
  1. Detect OS (linux / darwin)
  2. Detect architecture (amd64 / arm64)
  3. Check for GUI dependencies (WebKit2GTK on Linux)
     - macOS: always available (WebKit built-in)
     - Linux: check pkg-config, ldconfig, or direct .so search
  4. Fetch latest version tag from GitHub API
     (or use WEBSESSIONS_VERSION env var)
  5. Build asset name:
     - GUI: websessions-gui-<os>-<arch>
     - Standard: websessions-<os>-<arch>
  6. Download binary to temp directory
  7. chmod +x
  8. Move to install directory:
     - WEBSESSIONS_INSTALL_DIR (if set)
     - /usr/local/bin (if writable)
     - ~/.local/bin (fallback)
  9. Install ws-open-url helper script alongside binary
 10. Check if install dir is in PATH, warn if not
```

### Options

| Option / Env Var | Description |
|------------------|-------------|
| `--no-gui` | Force standard binary even if GUI deps are available |
| `WEBSESSIONS_VERSION` | Pin a specific version (e.g., `v0.5.0`) |
| `WEBSESSIONS_INSTALL_DIR` | Override install location |

### GUI Detection

On Linux, the installer checks for `libwebkit2gtk-4.1` via three methods:

1. `pkg-config --exists webkit2gtk-4.1`
2. `ldconfig -p` grep
3. Direct `.so` file search in standard lib paths

If GUI deps are missing, the installer prints distro-specific install commands:

| Distro | Command |
|--------|---------|
| Ubuntu/Debian | `sudo apt install libwebkit2gtk-4.1-0 libgtk-3-0` |
| Fedora/RHEL | `sudo dnf install webkit2gtk4.1 gtk3` |
| Arch/Manjaro | `sudo pacman -S webkit2gtk-4.1 gtk3` |
| openSUSE | `sudo zypper install libwebkit2gtk-4_1-0 gtk3` |
