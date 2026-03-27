package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/config"
	"github.com/IgorDeo/claude-websessions/internal/notification"
	"github.com/IgorDeo/claude-websessions/internal/session"
	"github.com/IgorDeo/claude-websessions/internal/store"
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

func newProviderTestServer(t *testing.T) (*Server, *store.Store, *session.Manager) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "websessions.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		Server:   config.ServerConfig{Port: 0, Host: "127.0.0.1"},
		Sessions: config.SessionsConfig{OutputBufferSize: 1024, DefaultDir: "/tmp"},
	}
	mgr := session.NewManager(1024)
	bus := notification.NewBus()
	sink := notification.NewInAppSink(100)

	return New(cfg, mgr, bus, sink, st), st, mgr
}

func saveProviderRecord(t *testing.T, st *store.Store, sess *session.Session, status string) {
	t.Helper()
	err := st.SaveSession(store.SessionRecord{
		ID:                sess.ID,
		Name:              sess.Name,
		Provider:          sess.Provider,
		ExternalSessionID: sess.ExternalSessionID,
		ClaudeID:          sess.ClaudeID,
		WorkDir:           sess.WorkDir,
		StartTime:         time.Now().Add(-1 * time.Minute),
		Status:            status,
		PID:               sess.PID,
	})
	if err != nil {
		t.Fatalf("seed session record: %v", err)
	}
}

func assertStoredProviderMetadata(t *testing.T, st *store.Store, sessionID, wantProvider, wantExternalID string) {
	t.Helper()
	rec, err := st.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session record: %v", err)
	}
	if rec.Provider != wantProvider {
		t.Fatalf("provider mismatch: got %q want %q", rec.Provider, wantProvider)
	}
	if rec.ExternalSessionID != wantExternalID {
		t.Fatalf("external session id mismatch: got %q want %q", rec.ExternalSessionID, wantExternalID)
	}
}

func TestResolveProviderMetadata(t *testing.T) {
	tests := []struct {
		name                  string
		provider              string
		externalSessionID     string
		claudeID              string
		wantProvider          string
		wantExternalSessionID string
		wantClaudeID          string
	}{
		{
			name:                  "provider metadata preserved",
			provider:              "opencode",
			externalSessionID:     "oc-123",
			claudeID:              "",
			wantProvider:          "opencode",
			wantExternalSessionID: "oc-123",
			wantClaudeID:          "",
		},
		{
			name:                  "legacy claude fallback",
			provider:              "",
			externalSessionID:     "",
			claudeID:              "cl-123",
			wantProvider:          "claude",
			wantExternalSessionID: "cl-123",
			wantClaudeID:          "cl-123",
		},
		{
			name:                  "opencode does not fallback to claude id",
			provider:              "opencode",
			externalSessionID:     "",
			claudeID:              "cl-123",
			wantProvider:          "opencode",
			wantExternalSessionID: "",
			wantClaudeID:          "cl-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProvider, gotExternalSessionID, gotClaudeID := resolveProviderMetadata(tt.provider, tt.externalSessionID, tt.claudeID)
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

func TestProviderResumeCommand(t *testing.T) {
	srv, _, _ := newProviderTestServer(t)

	tests := []struct {
		name              string
		provider          string
		sessionName       string
		externalSessionID string
		wantCommand       string
		wantArgs          []string
	}{
		{
			name:              "opencode uses opencode command",
			provider:          "opencode",
			sessionName:       "example",
			externalSessionID: "oc-123",
			wantCommand:       "opencode",
			wantArgs:          []string{"--session", "oc-123"},
		},
		{
			name:              "claude uses resume",
			provider:          "claude",
			sessionName:       "example",
			externalSessionID: "cl-123",
			wantCommand:       "claude",
			wantArgs:          []string{"--name", "example", "--resume", "cl-123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCommand, gotArgs := srv.providerResumeCommand(tt.provider, tt.sessionName, tt.externalSessionID)
			if gotCommand != tt.wantCommand {
				t.Fatalf("command mismatch: got %q want %q", gotCommand, tt.wantCommand)
			}
			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args length mismatch: got %d want %d", len(gotArgs), len(tt.wantArgs))
			}
			for i := range gotArgs {
				if gotArgs[i] != tt.wantArgs[i] {
					t.Fatalf("arg %d mismatch: got %q want %q", i, gotArgs[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestHandleRenameSession_PreservesProviderMetadata(t *testing.T) {
	srv, st, mgr := newProviderTestServer(t)
	sess := mgr.AddOffline(
		"rename-opencode",
		"old-name",
		"",
		"/tmp",
		&session.CreateOptions{Provider: "opencode", ExternalSessionID: "oc-rename"},
	)
	saveProviderRecord(t, st, sess, "running")

	req := httptest.NewRequest(http.MethodPost, "/sessions/rename-opencode/rename", strings.NewReader("name=new-name"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	srv.handleRenameSession(w, req, sess.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	assertStoredProviderMetadata(t, st, sess.ID, "opencode", "oc-rename")
}

func TestHandleKillSession_PreservesProviderMetadata(t *testing.T) {
	srv, st, mgr := newProviderTestServer(t)
	sess := mgr.AddOffline(
		"kill-opencode",
		"kill-name",
		"",
		"/tmp",
		&session.CreateOptions{Provider: "opencode", ExternalSessionID: "oc-kill"},
	)
	saveProviderRecord(t, st, sess, "running")

	req := httptest.NewRequest(http.MethodPost, "/sessions/kill-opencode/kill", nil)
	w := httptest.NewRecorder()

	srv.handleKillSession(w, req, sess.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	assertStoredProviderMetadata(t, st, sess.ID, "opencode", "oc-kill")
}

func TestHandleKillAll_PreservesProviderMetadata(t *testing.T) {
	srv, st, mgr := newProviderTestServer(t)
	sess := mgr.AddOffline(
		"killall-opencode",
		"kill-all-name",
		"",
		"/tmp",
		&session.CreateOptions{Provider: "opencode", ExternalSessionID: "oc-killall"},
	)
	sess.State = session.StateRunning
	saveProviderRecord(t, st, sess, "running")

	req := httptest.NewRequest(http.MethodPost, "/api/kill-all", nil)
	w := httptest.NewRecorder()

	srv.handleKillAll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	assertStoredProviderMetadata(t, st, sess.ID, "opencode", "oc-killall")
}

func TestCreateSessionProvider_OpenCodeLaunchesOpenCode(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}

	srv, _, mgr := newProviderTestServer(t)
	workDir := t.TempDir()
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "claude", logPath)
	createFakeCommand(t, tempDir, "opencode", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	body := url.Values{
		"name":     []string{"provider-opencode"},
		"work_dir": []string{workDir},
		"provider": []string{"opencode"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	srv.handleCreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Session-Type"); got != "opencode" {
		t.Fatalf("expected X-Session-Type opencode, got %q", got)
	}

	sess, ok := mgr.Get("provider-opencode")
	if !ok {
		t.Fatal("expected created session to exist")
	}
	defer func() { _ = mgr.Kill(sess.ID) }()

	if sess.Provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", sess.Provider)
	}

	line := waitForCommandLine(t, logPath)
	if !strings.HasPrefix(line, "opencode") {
		t.Fatalf("expected opencode command, got %q", line)
	}
}

func TestCreateSessionProvider_DefaultsToClaude(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}

	srv, _, mgr := newProviderTestServer(t)
	workDir := t.TempDir()
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "claude", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	body := url.Values{
		"name":     []string{"provider-default"},
		"work_dir": []string{workDir},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	srv.handleCreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Session-Type"); got != "claude" {
		t.Fatalf("expected X-Session-Type claude, got %q", got)
	}

	sess, ok := mgr.Get("provider-default")
	if !ok {
		t.Fatal("expected created session to exist")
	}
	defer func() { _ = mgr.Kill(sess.ID) }()

	if sess.Provider != "claude" {
		t.Fatalf("expected provider claude, got %q", sess.Provider)
	}

	line := waitForCommandLine(t, logPath)
	if !strings.HasPrefix(line, "claude") {
		t.Fatalf("expected claude command, got %q", line)
	}
}

func TestCreateSessionProvider_PersistsProviderMetadataInStore(t *testing.T) {
	if !session.TmuxIsAvailable() {
		t.Skip("tmux not available")
	}

	srv, st, mgr := newProviderTestServer(t)
	workDir := t.TempDir()
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "cmd.log")
	createFakeCommand(t, tempDir, "opencode", logPath)
	t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))

	body := url.Values{
		"name":      []string{"provider-persist"},
		"work_dir":  []string{workDir},
		"provider":  []string{"opencode"},
		"resume_id": []string{"oc-persist"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	srv.handleCreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	sess, ok := mgr.Get("provider-persist")
	if !ok {
		t.Fatal("expected created session to exist")
	}
	defer func() { _ = mgr.Kill(sess.ID) }()

	assertStoredProviderMetadata(t, st, sess.ID, "opencode", "oc-persist")
}

func TestCreateSessionProvider_InvalidProviderReturnsBadRequest(t *testing.T) {
	srv, _, mgr := newProviderTestServer(t)
	body := url.Values{
		"name":     []string{"provider-invalid"},
		"work_dir": []string{t.TempDir()},
		"provider": []string{"unknown"},
	}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	srv.handleCreateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}
	if len(mgr.List()) != 0 {
		t.Fatalf("expected no sessions created, got %d", len(mgr.List()))
	}
}

func TestProviderSessionsRoute_ClaudeAndCompatibility(t *testing.T) {
	srv, _, _ := newProviderTestServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	workDir := filepath.Join(home, "project.with.dots")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("create work dir: %v", err)
	}

	projectName := strings.ReplaceAll(workDir, "/", "-")
	projectName = strings.ReplaceAll(projectName, ".", "-")
	sessionsDir := filepath.Join(home, ".claude", "projects", projectName)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("create sessions dir: %v", err)
	}

	sessionFile := filepath.Join(sessionsDir, "claude-session-1.jsonl")
	content := "{\"type\":\"user\",\"message\":{\"content\":\"hello from provider sessions\"}}\n"
	if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	newRouteReq := httptest.NewRequest(http.MethodGet, "/api/provider-sessions?provider=claude&work_dir="+url.QueryEscape(workDir), nil)
	newRouteW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(newRouteW, newRouteReq)
	if newRouteW.Code != http.StatusOK {
		t.Fatalf("expected provider-sessions status 200, got %d", newRouteW.Code)
	}

	var providerSessions []map[string]interface{}
	if err := json.NewDecoder(newRouteW.Body).Decode(&providerSessions); err != nil {
		t.Fatalf("decode provider-sessions response: %v", err)
	}
	if len(providerSessions) != 1 {
		t.Fatalf("expected one provider session, got %d", len(providerSessions))
	}
	if got := providerSessions[0]["provider"]; got != "claude" {
		t.Fatalf("expected provider claude, got %v", got)
	}
	if got := providerSessions[0]["external_session_id"]; got != "claude-session-1" {
		t.Fatalf("expected external_session_id claude-session-1, got %v", got)
	}

	compatReq := httptest.NewRequest(http.MethodGet, "/api/claude-sessions?dir="+url.QueryEscape(workDir), nil)
	compatW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(compatW, compatReq)
	if compatW.Code != http.StatusOK {
		t.Fatalf("expected claude-sessions status 200, got %d", compatW.Code)
	}

	var compatSessions []map[string]interface{}
	if err := json.NewDecoder(compatW.Body).Decode(&compatSessions); err != nil {
		t.Fatalf("decode claude-sessions response: %v", err)
	}
	if len(compatSessions) != 1 {
		t.Fatalf("expected one compatibility session, got %d", len(compatSessions))
	}
	if got := compatSessions[0]["id"]; got != "claude-session-1" {
		t.Fatalf("expected id claude-session-1, got %v", got)
	}
}

func TestProviderSessionsRoute_OpenCodeUsesStoreHistory(t *testing.T) {
	srv, st, _ := newProviderTestServer(t)
	workDir := t.TempDir()

	now := time.Now()
	records := []store.SessionRecord{
		{
			ID:                "opencode-recent",
			Name:              "Recent OpenCode",
			Provider:          "opencode",
			ExternalSessionID: "oc-recent",
			WorkDir:           workDir,
			StartTime:         now,
			Status:            "exited",
		},
		{
			ID:                "opencode-old",
			Name:              "Old OpenCode",
			Provider:          "opencode",
			ExternalSessionID: "oc-old",
			WorkDir:           workDir,
			StartTime:         now.Add(-1 * time.Hour),
			Status:            "exited",
		},
		{
			ID:                "opencode-other-dir",
			Name:              "Other Dir",
			Provider:          "opencode",
			ExternalSessionID: "oc-other",
			WorkDir:           t.TempDir(),
			StartTime:         now,
			Status:            "exited",
		},
		{
			ID:                "opencode-missing-external-id",
			Name:              "Missing External",
			Provider:          "opencode",
			ExternalSessionID: "",
			WorkDir:           workDir,
			StartTime:         now.Add(-2 * time.Hour),
			Status:            "exited",
		},
		{
			ID:                "claude-same-dir",
			Name:              "Claude",
			Provider:          "claude",
			ExternalSessionID: "cl-1",
			ClaudeID:          "cl-1",
			WorkDir:           workDir,
			StartTime:         now,
			Status:            "exited",
		},
	}

	for _, rec := range records {
		if err := st.SaveSession(rec); err != nil {
			t.Fatalf("save session record %s: %v", rec.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/provider-sessions?provider=opencode&work_dir="+url.QueryEscape(workDir), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var sessions []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode provider-sessions response: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected two opencode sessions, got %d", len(sessions))
	}
	if got := sessions[0]["external_session_id"]; got != "oc-recent" {
		t.Fatalf("expected most recent external_session_id oc-recent, got %v", got)
	}
	if got := sessions[1]["external_session_id"]; got != "oc-old" {
		t.Fatalf("expected second external_session_id oc-old, got %v", got)
	}
	if got := sessions[0]["id"]; got == "opencode-missing-external-id" {
		t.Fatalf("did not expect session with missing external_session_id to be returned")
	}
	if got := sessions[1]["id"]; got == "opencode-missing-external-id" {
		t.Fatalf("did not expect session with missing external_session_id to be returned")
	}
	if got := sessions[0]["provider"]; got != "opencode" {
		t.Fatalf("expected provider opencode, got %v", got)
	}
}
