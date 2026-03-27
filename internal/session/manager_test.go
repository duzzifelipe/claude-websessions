package session_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/session"
)

func createFakeCommand(t *testing.T, dir, name, logPath string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s %s\\n' \"" + name + "\" \"$*\" >> " + strconv.Quote(logPath) + "\nsleep 10\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake %s command: %v", name, err)
	}
}

func waitForCommandLine(t *testing.T, logPath string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(logPath)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
				return lines[0]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for command log at %s", logPath)
	return ""
}

func TestManager_CreateSession(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	mgr := session.NewManager(10 * 1024 * 1024)

	// Use bash -c so tmux has a real shell to run
	s, err := mgr.Create("test-create", "/tmp", "bash", []string{"-c", "echo hello && sleep 2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = mgr.Kill(s.ID) }()

	if s.ID != "test-create" {
		t.Errorf("expected ID test-create, got %s", s.ID)
	}
	if s.GetState() != session.StateRunning {
		t.Errorf("expected state running, got %s", s.GetState())
	}
	if s.TmuxSession == "" {
		t.Error("expected non-empty TmuxSession")
	}
}

func TestManager_CreateSession_DefaultProviderUsesClaudeCommand(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "claude", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	mgr := session.NewManager(10 * 1024 * 1024)
	s, err := mgr.Create("test-create-default-provider", "/tmp", "", []string{"--name", "default-provider"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = mgr.Kill(s.ID) }()

	if s.Provider != "claude" {
		t.Fatalf("expected provider claude, got %q", s.Provider)
	}

	line := waitForCommandLine(t, logPath)
	if !strings.HasPrefix(line, "claude ") {
		t.Fatalf("expected claude command, got %q", line)
	}
	if !strings.Contains(line, "--name default-provider") {
		t.Fatalf("expected claude command args to include name, got %q", line)
	}
}

func TestManager_CreateSession_OpenCodeUsesProviderCommandAndArgs(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "claude", logPath)
	createFakeCommand(t, tempDir, "opencode", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	mgr := session.NewManager(10 * 1024 * 1024)
	s, err := mgr.Create(
		"test-create-opencode-provider",
		"/tmp",
		"claude",
		[]string{"--name", "opencode-session", "--resume", "oc-123"},
		&session.CreateOptions{Provider: "opencode", ExternalSessionID: "oc-123"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = mgr.Kill(s.ID) }()

	if s.Provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", s.Provider)
	}
	if s.ExternalSessionID != "oc-123" {
		t.Fatalf("expected external session id oc-123, got %q", s.ExternalSessionID)
	}

	line := waitForCommandLine(t, logPath)
	if !strings.HasPrefix(line, "opencode ") {
		t.Fatalf("expected opencode command, got %q", line)
	}
	if strings.Contains(line, "--name") {
		t.Fatalf("expected opencode args not to contain --name, got %q", line)
	}
	if strings.Contains(line, "--resume") {
		t.Fatalf("expected opencode args not to contain --resume, got %q", line)
	}
	if !strings.Contains(line, "--session oc-123") {
		t.Fatalf("expected opencode args to include --session oc-123, got %q", line)
	}
}

func TestManager_RestartAndTakeoverPreserveProvider(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "opencode", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	mgr := session.NewManager(10 * 1024 * 1024)

	offline := mgr.AddOffline("test-restart-provider", "restart-opencode", "", "/tmp")
	offline.Provider = "opencode"
	offline.ExternalSessionID = "oc-restart"

	restarted, err := mgr.Restart(offline.ID)
	if err != nil {
		t.Fatalf("restart error: %v", err)
	}
	defer func() { _ = mgr.Kill(restarted.ID) }()

	if restarted.Provider != "opencode" {
		t.Fatalf("expected restarted provider opencode, got %q", restarted.Provider)
	}
	if restarted.ExternalSessionID != "oc-restart" {
		t.Fatalf("expected restarted external session id oc-restart, got %q", restarted.ExternalSessionID)
	}

	discovered := mgr.AddDiscovered("test-takeover-provider", "", "/tmp", 999999, time.Now())
	discovered.Provider = "opencode"
	discovered.ExternalSessionID = "oc-takeover"

	mgr.Remove(discovered.ID)
	takeover, err := mgr.Create(
		discovered.ID,
		discovered.WorkDir,
		"claude",
		[]string{"--name", discovered.Name, "--resume", discovered.ExternalSessionID},
		&session.CreateOptions{Provider: discovered.Provider, ExternalSessionID: discovered.ExternalSessionID},
	)
	if err != nil {
		t.Fatalf("takeover create error: %v", err)
	}
	defer func() { _ = mgr.Kill(takeover.ID) }()

	if takeover.Provider != "opencode" {
		t.Fatalf("expected takeover provider opencode, got %q", takeover.Provider)
	}
	if takeover.ExternalSessionID != "oc-takeover" {
		t.Fatalf("expected takeover external session id oc-takeover, got %q", takeover.ExternalSessionID)
	}
}

func TestRecoverProviderMetadataFromCommand(t *testing.T) {
	tests := []struct {
		name                  string
		startCommand          string
		fallbackClaudeID      string
		wantProvider          string
		wantExternalSessionID string
		wantClaudeID          string
	}{
		{
			name:                  "opencode command uses session flag",
			startCommand:          "opencode --session oc-123",
			fallbackClaudeID:      "",
			wantProvider:          "opencode",
			wantExternalSessionID: "oc-123",
			wantClaudeID:          "",
		},
		{
			name:                  "claude command uses resume flag",
			startCommand:          "claude --resume cl-123",
			fallbackClaudeID:      "",
			wantProvider:          "claude",
			wantExternalSessionID: "cl-123",
			wantClaudeID:          "cl-123",
		},
		{
			name:                  "legacy fallback uses claude id",
			startCommand:          "",
			fallbackClaudeID:      "legacy-claude-id",
			wantProvider:          "claude",
			wantExternalSessionID: "legacy-claude-id",
			wantClaudeID:          "legacy-claude-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProvider, gotExternalSessionID, gotClaudeID := session.RecoverProviderMetadataFromCommand(tt.startCommand, tt.fallbackClaudeID)
			if gotProvider != tt.wantProvider {
				t.Fatalf("provider mismatch: got %q want %q", gotProvider, tt.wantProvider)
			}
			if gotExternalSessionID != tt.wantExternalSessionID {
				t.Fatalf("external session id mismatch: got %q want %q", gotExternalSessionID, tt.wantExternalSessionID)
			}
			if gotClaudeID != tt.wantClaudeID {
				t.Fatalf("claude id mismatch: got %q want %q", gotClaudeID, tt.wantClaudeID)
			}
		})
	}
}

func TestManager_AddDiscovered_OpenCodeDoesNotFallbackExternalSessionIDFromClaudeID(t *testing.T) {
	mgr := session.NewManager(1024)
	s := mgr.AddDiscovered(
		"discovered-opencode-no-fallback",
		"legacy-claude-id",
		"/tmp",
		123,
		time.Now(),
		&session.CreateOptions{Provider: "opencode"},
	)

	if s.Provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", s.Provider)
	}
	if s.ExternalSessionID != "" {
		t.Fatalf("expected empty external session id for opencode fallback, got %q", s.ExternalSessionID)
	}
	if s.ClaudeID != "legacy-claude-id" {
		t.Fatalf("expected claude id preserved, got %q", s.ClaudeID)
	}
}

func TestManager_AddOffline_OpenCodeDoesNotFallbackExternalSessionIDFromClaudeID(t *testing.T) {
	mgr := session.NewManager(1024)
	s := mgr.AddOffline(
		"offline-opencode-no-fallback",
		"offline",
		"legacy-claude-id",
		"/tmp",
		&session.CreateOptions{Provider: "opencode"},
	)

	if s.Provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", s.Provider)
	}
	if s.ExternalSessionID != "" {
		t.Fatalf("expected empty external session id for opencode fallback, got %q", s.ExternalSessionID)
	}
	if s.ClaudeID != "legacy-claude-id" {
		t.Fatalf("expected claude id preserved, got %q", s.ClaudeID)
	}
}

func TestManager_Restart_OpenCodeDoesNotFallbackExternalSessionIDFromClaudeID(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "opencode", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	mgr := session.NewManager(10 * 1024 * 1024)
	offline := mgr.AddOffline(
		"restart-opencode-no-fallback",
		"restart-opencode-no-fallback",
		"legacy-claude-id",
		"/tmp",
		&session.CreateOptions{Provider: "opencode"},
	)

	restarted, err := mgr.Restart(offline.ID)
	if err != nil {
		t.Fatalf("restart error: %v", err)
	}
	defer func() { _ = mgr.Kill(restarted.ID) }()

	if restarted.Provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", restarted.Provider)
	}
	if restarted.ExternalSessionID != "" {
		t.Fatalf("expected empty external session id, got %q", restarted.ExternalSessionID)
	}
	if restarted.ClaudeID != "" {
		t.Fatalf("expected empty claude id for opencode session, got %q", restarted.ClaudeID)
	}

	line := waitForCommandLine(t, logPath)
	if line != "opencode" {
		t.Fatalf("expected opencode command, got %q", line)
	}
	if strings.Contains(line, "--session") {
		t.Fatalf("expected opencode args not to include --session fallback, got %q", line)
	}
}

func TestManager_ListSessions(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	mgr := session.NewManager(10 * 1024 * 1024)

	_, _ = mgr.Create("test-list-1", "/tmp", "bash", []string{"-c", "sleep 5"})
	_, _ = mgr.Create("test-list-2", "/tmp", "bash", []string{"-c", "sleep 5"})
	defer func() { _ = mgr.Kill("test-list-1") }()
	defer func() { _ = mgr.Kill("test-list-2") }()

	sessions := mgr.List()
	if len(sessions) < 2 {
		t.Errorf("expected at least 2 sessions, got %d", len(sessions))
	}
}

func TestManager_GetSession(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	mgr := session.NewManager(10 * 1024 * 1024)

	_, _ = mgr.Create("test-get", "/tmp", "bash", []string{"-c", "sleep 5"})
	defer func() { _ = mgr.Kill("test-get") }()

	s, ok := mgr.Get("test-get")
	if !ok {
		t.Fatal("expected to find session")
	}
	if s.ID != "test-get" {
		t.Errorf("expected ID test-get, got %s", s.ID)
	}
	_, ok = mgr.Get("nonexistent")
	if ok {
		t.Error("expected not to find nonexistent session")
	}
}

func TestManager_KillSession(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}
	mgr := session.NewManager(10 * 1024 * 1024)

	s, err := mgr.Create("test-kill", "/tmp", "bash", []string{"-c", "sleep 60"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = mgr.Kill(s.ID)
	if err != nil {
		t.Fatalf("kill error: %v", err)
	}

	// Give tmux a moment to clean up
	time.Sleep(500 * time.Millisecond)
}
