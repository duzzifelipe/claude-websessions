package session

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/discovery"
	"github.com/IgorDeo/claude-websessions/internal/docker"
	"github.com/creack/pty"
)

type StateChangeFunc func(s *Session, from, to State)
type OutputFunc func(sessionID string, data []byte)

type Manager struct {
	mu            sync.RWMutex
	sessions      map[string]*Session
	bufferSize    int64
	onStateChange StateChangeFunc
	onOutput      OutputFunc
	stopReaders   map[string]chan struct{} // signal to stop reading for a session
}

func NewManager(bufferSize int64) *Manager {
	return &Manager{
		sessions:    make(map[string]*Session),
		bufferSize:  bufferSize,
		stopReaders: make(map[string]chan struct{}),
	}
}

// CreateOptions holds optional parameters for session creation.
type CreateOptions struct {
	Sandboxed         bool
	Provider          string
	ExternalSessionID string
}

const defaultProvider = "claude"

func normalizeProvider(provider string) string {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return defaultProvider
	}
	return provider
}

func extractFlagValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return strings.Trim(args[i+1], "'\"")
		}
	}
	return ""
}

func opencodeArgs(args []string, externalSessionID string) []string {
	out := make([]string, 0, len(args))
	sessionID := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				i++
			}
		case "--resume", "--session":
			if i+1 < len(args) {
				sessionID = args[i+1]
				i++
			}
		default:
			out = append(out, args[i])
		}
	}

	if externalSessionID != "" {
		sessionID = externalSessionID
	}
	if sessionID != "" {
		out = append(out, "--session", sessionID)
	}

	return out
}

func resolveProviderCommandAndArgs(provider, command string, args []string, externalSessionID string) (string, []string, string) {
	provider = normalizeProvider(provider)
	resolvedCommand := strings.TrimSpace(command)
	resolvedArgs := append([]string(nil), args...)
	resolvedExternalSessionID := externalSessionID

	switch provider {
	case "opencode":
		if resolvedCommand == "" || resolvedCommand == "claude" {
			resolvedCommand = "opencode"
		}
		resolvedArgs = opencodeArgs(resolvedArgs, resolvedExternalSessionID)
		if resolvedExternalSessionID == "" {
			resolvedExternalSessionID = extractFlagValue(resolvedArgs, "--session")
		}
	default:
		if resolvedCommand == "" {
			resolvedCommand = defaultProvider
		}
		if resolvedExternalSessionID == "" {
			resolvedExternalSessionID = extractFlagValue(resolvedArgs, "--resume")
		}
	}

	return resolvedCommand, resolvedArgs, resolvedExternalSessionID
}

func providerCreateArgs(provider, name, externalSessionID string) []string {
	provider = normalizeProvider(provider)
	if provider == "opencode" {
		args := []string{}
		if externalSessionID != "" {
			args = append(args, "--session", externalSessionID)
		}
		return args
	}

	args := []string{"--name", name}
	if externalSessionID != "" {
		args = append(args, "--resume", externalSessionID)
	}
	return args
}

func RecoverProviderMetadataFromCommand(startCommand, fallbackClaudeID string) (string, string, string) {
	provider := defaultProvider
	externalSessionID := fallbackClaudeID
	claudeID := fallbackClaudeID

	fields := strings.Fields(startCommand)
	providerIndex := -1
	for i, field := range fields {
		cmd := strings.Trim(filepath.Base(strings.Trim(field, "'\"")), "'\"")
		if cmd == "opencode" || cmd == defaultProvider {
			provider = cmd
			providerIndex = i
			break
		}
	}

	if providerIndex >= 0 {
		args := fields[providerIndex+1:]
		switch provider {
		case "opencode":
			externalSessionID = extractFlagValue(args, "--session")
			if externalSessionID == "" {
				externalSessionID = extractFlagValue(args, "--resume")
			}
			claudeID = ""
		default:
			externalSessionID = extractFlagValue(args, "--resume")
			if externalSessionID == "" {
				externalSessionID = extractFlagValue(args, "--session-id")
			}
			if externalSessionID == "" {
				externalSessionID = fallbackClaudeID
			}
			claudeID = externalSessionID
		}
	}

	if provider == defaultProvider && claudeID == "" {
		claudeID = externalSessionID
	}
	return provider, externalSessionID, claudeID
}

func (m *Manager) OnStateChange(fn StateChangeFunc) { m.onStateChange = fn }
func (m *Manager) OnOutput(fn OutputFunc)           { m.onOutput = fn }

// Create creates a new session inside a tmux session.
// When opts.Sandboxed is true, the session runs inside a Docker Desktop sandbox VM.
func (m *Manager) Create(id, workDir, command string, args []string, opts ...*CreateOptions) (*Session, error) {
	var opt *CreateOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
	}
	sandboxed := opt != nil && opt.Sandboxed
	provider := defaultProvider
	externalSessionID := ""
	if opt != nil {
		provider = normalizeProvider(opt.Provider)
		externalSessionID = opt.ExternalSessionID
	}
	resolvedCommand, resolvedArgs, resolvedExternalSessionID := resolveProviderCommandAndArgs(provider, command, args, externalSessionID)
	provider = normalizeProvider(provider)
	claudeID := ""
	if provider == defaultProvider {
		claudeID = resolvedExternalSessionID
	}

	// Expand ~ in workDir
	if len(workDir) > 0 && workDir[0] == '~' {
		home, _ := os.UserHomeDir()
		workDir = home + workDir[1:]
	}

	// Validate directory exists
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("working directory does not exist: %s", workDir)
	}

	if sandboxed {
		// Sandbox mode: return session immediately in "starting" state,
		// then provision the sandbox VM asynchronously to avoid blocking the UI.
		s := &Session{
			ID:                id,
			Provider:          provider,
			ExternalSessionID: resolvedExternalSessionID,
			ClaudeID:          claudeID,
			Name:              id,
			WorkDir:           workDir,
			State:             StateStarting,
			StartTime:         time.Now(),
			Owned:             true,
			Sandboxed:         true,
			output:            NewRingBuf(int(m.bufferSize)),
		}

		m.mu.Lock()
		m.sessions[id] = s
		m.mu.Unlock()

		if m.onStateChange != nil {
			m.onStateChange(s, StateCreated, StateStarting)
		}

		go m.provisionSandbox(s, workDir, resolvedCommand, resolvedArgs)
		return s, nil
	}

	// Non-sandbox path: resolve command and create tmux session synchronously
	var resolvedCmd string
	resolvedCmd, err := exec.LookPath(resolvedCommand)
	if err != nil {
		return nil, fmt.Errorf("command not found: %s", resolvedCommand)
	}

	tmuxName := TmuxSessionName(id)

	// Kill any existing tmux session with this name
	if tmuxSessionExists(tmuxName) {
		_ = tmuxKillSession(tmuxName)
	}

	// Create tmux session
	if err := tmuxCreateSession(tmuxName, workDir, resolvedCmd, resolvedArgs); err != nil {
		return nil, fmt.Errorf("creating tmux session: %w", err)
	}

	s := &Session{
		ID:                id,
		Provider:          provider,
		ExternalSessionID: resolvedExternalSessionID,
		ClaudeID:          claudeID,
		Name:              id,
		WorkDir:           workDir,
		State:             StateRunning,
		StartTime:         time.Now(),
		Owned:             true,
		TmuxSession:       tmuxName,
		output:            NewRingBuf(int(m.bufferSize)),
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	if m.onStateChange != nil {
		m.onStateChange(s, StateCreated, StateRunning)
	}

	// Start reading output from tmux via a PTY attached to the session
	m.startReader(s)

	return s, nil
}

// provisionSandbox runs Docker sandbox setup asynchronously, then starts the tmux session.
func (m *Manager) provisionSandbox(s *Session, workDir, command string, args []string) {
	var sandboxName string

	existing, err := docker.FindSandboxForWorkDir(workDir)
	if err != nil {
		slog.Error("sandbox: failed to check existing", "id", s.ID, "error", err)
		m.failSession(s, fmt.Sprintf("checking sandbox: %v", err))
		return
	}
	if existing != nil {
		sandboxName = existing.Name
	} else {
		name, err := docker.SandboxCreate(workDir)
		if err != nil {
			slog.Error("sandbox: failed to create", "id", s.ID, "error", err)
			m.failSession(s, fmt.Sprintf("creating sandbox: %v", err))
			return
		}
		sandboxName = name
		if err := docker.SandboxCopyCredentials(sandboxName); err != nil {
			slog.Warn("sandbox: failed to copy credentials", "id", s.ID, "error", err)
		}
	}

	s.mu.Lock()
	s.SandboxName = sandboxName
	s.mu.Unlock()

	// Build the tmux command: docker sandbox run <name> -- <command> <args...>
	fullArgs := []string{"sandbox", "run", sandboxName, "--", command}
	fullArgs = append(fullArgs, args...)
	tmuxName := TmuxSessionName(s.ID)

	if tmuxSessionExists(tmuxName) {
		_ = tmuxKillSession(tmuxName)
	}
	if err := tmuxCreateSession(tmuxName, workDir, "docker", fullArgs); err != nil {
		slog.Error("sandbox: failed to create tmux session", "id", s.ID, "error", err)
		m.failSession(s, fmt.Sprintf("creating tmux session: %v", err))
		return
	}

	s.mu.Lock()
	s.TmuxSession = tmuxName
	s.State = StateRunning
	s.mu.Unlock()

	if m.onStateChange != nil {
		m.onStateChange(s, StateStarting, StateRunning)
	}

	m.startReader(s)
}

// failSession transitions a session to errored state.
func (m *Manager) failSession(s *Session, errMsg string) {
	from := s.GetState()
	s.mu.Lock()
	s.State = StateErrored
	s.Error = errMsg
	s.EndTime = time.Now()
	s.mu.Unlock()
	if m.onStateChange != nil {
		m.onStateChange(s, from, StateErrored)
	}
}

// startReader attaches to the tmux session and reads output.
// Uses `tmux pipe-pane` to stream output, or attaches a reader PTY.
func (m *Manager) startReader(s *Session) {
	stop := make(chan struct{})
	m.mu.Lock()
	m.stopReaders[s.ID] = stop
	m.mu.Unlock()

	go func() {
		// Attach to the tmux session in read-only mode.
		cmd := exec.Command("tmux", "attach-session", "-t", s.TmuxSession, "-r")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 200})
		if err != nil {
			slog.Error("failed to attach to tmux session", "session", s.ID, "error", err)
			return
		}
		s.SetReaderPTY(ptmx)
		defer func() {
			s.SetReaderPTY(nil)
			_ = ptmx.Close()
			_ = cmd.Process.Kill()
		}()

		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}

			n, err := ptmx.Read(buf)
			if n > 0 {
				_, _ = s.Output().Write(buf[:n])
				if m.onOutput != nil {
					m.onOutput(s.ID, buf[:n])
				}
				m.checkWaitingState(s, buf[:n])
			}
			if err != nil {
				break
			}
		}

		// Reader stopped — check if tmux session is still alive
		if !tmuxSessionExists(s.TmuxSession) {
			from := s.GetState()
			if s.Killed {
				return
			}
			s.mu.Lock()
			s.State = StateCompleted
			s.EndTime = time.Now()
			s.mu.Unlock()
			if m.onStateChange != nil {
				m.onStateChange(s, from, StateCompleted)
			}
			m.Remove(s.ID)
		}
	}()
}

// stopReader stops the output reader for a session.
func (m *Manager) stopReader(id string) {
	m.mu.Lock()
	if ch, ok := m.stopReaders[id]; ok {
		close(ch)
		delete(m.stopReaders, id)
	}
	m.mu.Unlock()
}

// Patterns that indicate Claude is waiting for user input.
var waitingPatterns = []string{
	"Do you want to proceed?",
	"[Y/n]", "[y/N]",
	"? (y/n)",
	"(yes/no)",
}

func (m *Manager) checkWaitingState(s *Session, data []byte) {
	line := string(data)
	currentState := s.GetState()

	if currentState == StateWaiting {
		if len(data) > 10 {
			from := currentState
			s.mu.Lock()
			s.State = StateRunning
			s.mu.Unlock()
			if m.onStateChange != nil {
				m.onStateChange(s, from, StateRunning)
			}
		}
		return
	}

	if currentState != StateRunning {
		return
	}

	for _, pattern := range waitingPatterns {
		if strings.Contains(line, pattern) {
			from := currentState
			s.mu.Lock()
			s.State = StateWaiting
			s.mu.Unlock()
			if m.onStateChange != nil {
				m.onStateChange(s, from, StateWaiting)
			}
			return
		}
	}
}

// Wait is kept for compatibility but is a no-op for tmux sessions.
// The reader goroutine handles state transitions.
func (m *Manager) Wait(id string) {
	// For tmux sessions, the reader goroutine handles completion detection.
	// This is kept for compatibility with the kill handler.
	time.Sleep(500 * time.Millisecond) // brief wait for tmux cleanup
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	sort.SliceStable(result, func(i, j int) bool {
		pi, pj := statePriority(result[i].GetState()), statePriority(result[j].GetState())
		if pi != pj {
			return pi < pj
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func statePriority(s State) int {
	switch s {
	case StateRunning, StateWaiting:
		return 0
	case StateCreated:
		return 1
	case StateDiscovered:
		return 2
	case StateOffline:
		return 3
	case StateCompleted:
		return 4
	case StateErrored:
		return 5
	default:
		return 6
	}
}

// Kill kills a session's tmux session and removes it from the manager.
func (m *Manager) Kill(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	m.stopReader(id)
	if s.TmuxSession != "" {
		_ = tmuxKillSession(s.TmuxSession)
	}
	// Stop sandbox VM asynchronously to avoid blocking the UI
	if s.Sandboxed && s.SandboxName != "" {
		sandboxName := s.SandboxName
		go func() {
			if err := docker.SandboxStop(sandboxName); err != nil {
				slog.Warn("failed to stop sandbox", "name", sandboxName, "error", err)
			}
			if err := docker.SandboxRemove(sandboxName); err != nil {
				slog.Warn("failed to remove sandbox", "name", sandboxName, "error", err)
			}
		}()
	}
	// Set terminal state
	from := s.GetState()
	s.mu.Lock()
	s.EndTime = time.Now()
	if s.Killed {
		s.State = StateErrored // will be saved as "killed" by the handler
	} else {
		s.State = StateErrored
	}
	s.mu.Unlock()
	if m.onStateChange != nil {
		m.onStateChange(s, from, s.GetState())
	}
	m.Remove(id)
	return nil
}

func (m *Manager) AddDiscovered(id, claudeID, workDir string, pid int, startTime time.Time, opts ...*CreateOptions) *Session {
	var opt *CreateOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
	}
	provider := defaultProvider
	externalSessionID := ""
	if opt != nil {
		provider = normalizeProvider(opt.Provider)
		if opt.ExternalSessionID != "" {
			externalSessionID = opt.ExternalSessionID
		}
	}
	if provider == defaultProvider && externalSessionID == "" {
		externalSessionID = claudeID
	}
	if provider == defaultProvider && claudeID == "" {
		claudeID = externalSessionID
	}

	name := filepath.Base(workDir)
	if name == "" || name == "." {
		name = workDir
	}
	s := &Session{
		ID: id, Provider: provider, ExternalSessionID: externalSessionID, ClaudeID: claudeID,
		Name: name, WorkDir: workDir,
		State: StateDiscovered, PID: pid, StartTime: startTime, Owned: false,
		output: NewRingBuf(int(m.bufferSize)),
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

// AddOffline adds a session from a previous server run (loaded from SQLite).
func (m *Manager) AddOffline(id, name, claudeID, workDir string, opts ...*CreateOptions) *Session {
	var opt *CreateOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
	}
	provider := defaultProvider
	externalSessionID := ""
	if opt != nil {
		provider = normalizeProvider(opt.Provider)
		if opt.ExternalSessionID != "" {
			externalSessionID = opt.ExternalSessionID
		}
	}
	if provider == defaultProvider && externalSessionID == "" {
		externalSessionID = claudeID
	}
	if provider == defaultProvider && claudeID == "" {
		claudeID = externalSessionID
	}

	s := &Session{
		ID: id, Provider: provider, ExternalSessionID: externalSessionID, ClaudeID: claudeID,
		Name: name, WorkDir: workDir,
		State: StateOffline, Owned: false,
		output: NewRingBuf(int(m.bufferSize)),
	}
	if name == "" {
		s.Name = filepath.Base(workDir)
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

// Reattach reconnects to an existing tmux session (e.g., after server restart).
func (m *Manager) Reattach(id, name, claudeID, workDir, tmuxName string, opts ...*CreateOptions) *Session {
	var opt *CreateOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
	}
	provider := defaultProvider
	externalSessionID := claudeID
	if opt != nil {
		provider = normalizeProvider(opt.Provider)
		if opt.ExternalSessionID != "" {
			externalSessionID = opt.ExternalSessionID
		}
	}
	if provider == defaultProvider && claudeID == "" {
		claudeID = externalSessionID
	}

	s := &Session{
		ID:                id,
		Provider:          provider,
		ExternalSessionID: externalSessionID,
		ClaudeID:          claudeID,
		Name:              name,
		WorkDir:           workDir,
		State:             StateRunning,
		StartTime:         time.Now(),
		Owned:             true,
		TmuxSession:       tmuxName,
		output:            NewRingBuf(int(m.bufferSize)),
	}
	if name == "" {
		s.Name = filepath.Base(workDir)
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	// Start reading output
	m.startReader(s)

	return s
}

// Restart creates a new provider session in the same directory, replacing an offline session.
func (m *Manager) Restart(id string, opts ...*CreateOptions) (*Session, error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if s.GetState() != StateOffline {
		return nil, fmt.Errorf("session %s is not offline", id)
	}

	name := s.Name
	workDir := s.WorkDir
	provider := normalizeProvider(s.Provider)
	externalSessionID := s.ExternalSessionID
	claudeID := s.ClaudeID
	sandboxed := s.Sandboxed
	if provider == defaultProvider && externalSessionID == "" {
		externalSessionID = claudeID
	}

	// Merge explicit opts with stored sandbox flag
	var opt *CreateOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
		if opt.Provider != "" {
			provider = normalizeProvider(opt.Provider)
		}
		if opt.ExternalSessionID != "" {
			externalSessionID = opt.ExternalSessionID
		}
		if !opt.Sandboxed && sandboxed {
			opt.Sandboxed = true
		}
	} else if sandboxed {
		opt = &CreateOptions{Sandboxed: true, Provider: provider, ExternalSessionID: externalSessionID}
	} else {
		opt = &CreateOptions{Provider: provider, ExternalSessionID: externalSessionID}
	}

	if provider == defaultProvider && externalSessionID == "" {
		externalSessionID = claudeID
	}

	if provider == defaultProvider && claudeID == "" && workDir != "" {
		claudeID = discovery.ResolveClaudeSessionID(workDir)
		if externalSessionID == "" {
			externalSessionID = claudeID
			opt.ExternalSessionID = externalSessionID
		}
	}

	m.Remove(id)

	args := providerCreateArgs(provider, name, externalSessionID)
	opt.Provider = provider
	opt.ExternalSessionID = externalSessionID

	newSess, err := m.Create(id, workDir, "", args, opt)
	if err != nil {
		return nil, err
	}
	newSess.Name = name
	return newSess, nil
}

func (m *Manager) Remove(id string) {
	m.stopReader(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// isTerminalResponse checks if data is a terminal response sequence
// that should be filtered out (e.g., DA responses from xterm.js).
func isTerminalResponse(data []byte) bool {
	s := string(data)
	// DA1 response: ESC [ ? ... c
	if strings.HasPrefix(s, "\033[?") && strings.HasSuffix(s, "c") {
		return true
	}
	// DA2 response: ESC [ > ... c
	if strings.HasPrefix(s, "\033[>") && strings.HasSuffix(s, "c") {
		return true
	}
	// Raw DA response without ESC (sometimes sent as plain text)
	if strings.HasPrefix(s, ">0;") && strings.HasSuffix(s, "c") {
		return true
	}
	// DSR response: ESC [ ... R (cursor position report)
	if strings.HasPrefix(s, "\033[") && strings.HasSuffix(s, "R") {
		// Check it's digits and semicolons between
		inner := s[2 : len(s)-1]
		for _, ch := range inner {
			if ch != ';' && (ch < '0' || ch > '9') {
				return false
			}
		}
		return true
	}
	return false
}

// WriteInput sends input to the session's tmux pane.
func (m *Manager) WriteInput(id string, data []byte) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	if s.TmuxSession == "" {
		return fmt.Errorf("session %s has no tmux session", id)
	}
	// Filter out terminal capability responses from xterm.js
	if isTerminalResponse(data) {
		return nil
	}
	return tmuxSendKeys(s.TmuxSession, string(data))
}

// RecoverTmuxSessions finds existing ws-* tmux sessions and reattaches to them.
func (m *Manager) RecoverTmuxSessions() int {
	sessions, err := tmuxListSessions()
	if err != nil || len(sessions) == 0 {
		return 0
	}

	count := 0
	for _, tmuxName := range sessions {
		// Extract session ID from tmux name: "ws-myproject" -> "myproject"
		id := strings.TrimPrefix(tmuxName, tmuxPrefix)

		// Skip if already tracked
		if _, ok := m.Get(id); ok {
			continue
		}

		// Try to get the pane's current directory
		workDir, _ := tmuxRun("display-message", "-t", tmuxName, "-p", "#{pane_current_path}")

		name := filepath.Base(workDir)
		if name == "" || name == "." {
			name = id
		}

		claudeID := ""
		if workDir != "" {
			claudeID = discovery.ResolveClaudeSessionID(workDir)
		}
		startCommand, _ := tmuxRun("display-message", "-t", tmuxName, "-p", "#{pane_start_command}")
		provider, externalSessionID, recoveredClaudeID := RecoverProviderMetadataFromCommand(startCommand, claudeID)

		m.Reattach(
			id,
			name,
			recoveredClaudeID,
			workDir,
			tmuxName,
			&CreateOptions{Provider: provider, ExternalSessionID: externalSessionID},
		)
		slog.Info("reattached to tmux session", "id", id, "tmux", tmuxName, "dir", workDir)
		count++
	}
	return count
}
