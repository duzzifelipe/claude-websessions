package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/discovery"
	"github.com/IgorDeo/claude-websessions/internal/docker"
	"github.com/IgorDeo/claude-websessions/internal/doctor"
	"github.com/IgorDeo/claude-websessions/internal/hooks"
	"github.com/IgorDeo/claude-websessions/internal/notification"
	"github.com/IgorDeo/claude-websessions/internal/service"
	"github.com/IgorDeo/claude-websessions/internal/session"
	"github.com/IgorDeo/claude-websessions/internal/store"
	"github.com/IgorDeo/claude-websessions/internal/updater"
	"github.com/IgorDeo/claude-websessions/web/templates"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sessions := s.mgr.List()
	views := make([]templates.SessionView, len(sessions))
	for i, sess := range sessions {
		views[i] = sessionToView(sess)
	}
	s.applyTabGroups(views)
	data := templates.PageData{Sessions: views, UnreadCount: s.sink.UnreadCount()}
	data.History = s.loadHistory(views)
	if err := templates.Index(data).Render(r.Context(), w); err != nil {
		slog.Error("failed to render index", "error", err)
	}
}

func (s *Server) handleSidebar(w http.ResponseWriter, r *http.Request) {
	sessions := s.mgr.List()
	views := make([]templates.SessionView, len(sessions))
	for i, sess := range sessions {
		views[i] = sessionToView(sess)
	}
	s.applyTabGroups(views)
	if err := templates.Sidebar(views, s.loadHistory(views)).Render(r.Context(), w); err != nil {
		slog.Error("failed to render sidebar", "error", err)
	}
}

// applyTabGroups reads saved tab state and sets GroupName for sessions in split groups.
func (s *Server) applyTabGroups(views []templates.SessionView) {
	if s.store == nil {
		return
	}
	tabsJSON, err := s.store.GetPreference("open-tabs")
	if err != nil || tabsJSON == "" {
		return
	}
	var tabs []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		SplitTree *struct {
			Type string `json:"type"`
		} `json:"splitTree"`
	}
	if json.Unmarshal([]byte(tabsJSON), &tabs) != nil {
		return
	}

	// Parse the full JSON to extract session IDs from split trees
	var rawTabs []json.RawMessage
	if json.Unmarshal([]byte(tabsJSON), &rawTabs) != nil {
		return
	}

	groupMap := make(map[string]string) // sessionID -> group name
	for i, tab := range tabs {
		if tab.SplitTree == nil {
			continue
		}
		// Extract all session IDs from the tree
		ids := extractTreeIDs(rawTabs[i])
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			groupMap[id] = tab.Name
		}
	}

	for i := range views {
		if gn, ok := groupMap[views[i].ID]; ok {
			views[i].GroupName = gn
		}
	}
}

func extractTreeIDs(raw json.RawMessage) []string {
	var node struct {
		SplitTree *json.RawMessage `json:"splitTree"`
	}
	if json.Unmarshal(raw, &node) == nil && node.SplitTree != nil {
		return extractNodeIDs(*node.SplitTree)
	}
	return nil
}

func extractNodeIDs(raw json.RawMessage) []string {
	var node struct {
		Type     string            `json:"type"`
		ID       string            `json:"id"`
		Children []json.RawMessage `json:"children"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return nil
	}
	if node.Type != "split" && node.ID != "" {
		return []string{node.ID}
	}
	var ids []string
	for _, child := range node.Children {
		ids = append(ids, extractNodeIDs(child)...)
	}
	return ids
}

func (s *Server) loadHistory(activeViews []templates.SessionView) []templates.SessionView {
	if s.store == nil {
		return nil
	}
	records, _ := s.store.ListSessions(20)
	activeIDs := make(map[string]bool)
	for _, v := range activeViews {
		activeIDs[v.ID] = true
	}
	var history []templates.SessionView
	for _, rec := range records {
		if activeIDs[rec.ID] {
			continue
		}
		name := rec.Name
		if name == "" {
			name = rec.ID
		}
		history = append(history, templates.SessionView{
			ID:        rec.ID,
			Name:      name,
			WorkDir:   rec.WorkDir,
			State:     rec.Status,
			Type:      sessionType(rec.ID, rec.Provider),
			Sandboxed: rec.Sandboxed,
		})
	}
	return history
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	workDir := r.FormValue("work_dir")
	prompt := r.FormValue("prompt")
	provider, err := s.parseRequestedProvider(r.FormValue("provider"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if name == "" || workDir == "" {
		http.Error(w, "name and work_dir required", http.StatusBadRequest)
		return
	}
	args := s.providerBaseArgs(provider)
	if provider == "claude" {
		args = append(args, "--name", name)
	}
	resumeID := r.FormValue("resume_id")
	if resumeID != "" {
		if provider == "opencode" {
			args = append(args, "--session", resumeID)
		} else {
			args = append(args, "--resume", resumeID)
		}
	}
	if prompt != "" {
		args = append(args, "-p", prompt)
	}

	// Sandbox support
	opts := &session.CreateOptions{Provider: provider, ExternalSessionID: resumeID}
	if r.FormValue("sandbox") == "true" {
		available, _, _ := docker.IsAvailable()
		if !available {
			http.Error(w, "Docker Desktop is not available for sandbox mode", http.StatusBadRequest)
			return
		}
		opts.Sandboxed = true
	}

	sess, err := s.mgr.Create(name, workDir, s.providerCommand(provider), args, opts)
	if err != nil {
		slog.Error("failed to create session", "error", err)
		http.Error(w, "failed to create session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Persist to SQLite immediately so it survives restarts
	if s.store != nil {
		_ = s.store.SaveSession(store.SessionRecord{
			ID: sess.ID, Name: sess.Name, Provider: sess.Provider, ExternalSessionID: sess.ExternalSessionID,
			ClaudeID: sess.ClaudeID, WorkDir: sess.WorkDir,
			StartTime: sess.StartTime, Status: "running", PID: sess.PID,
			Sandboxed: sess.Sandboxed, SandboxName: sess.SandboxName,
		})
	}
	// Tell the client to auto-open this session and refresh the sidebar
	w.Header().Set("X-Session-ID", sess.ID)
	w.Header().Set("X-Session-Name", sess.Name)
	w.Header().Set("X-Session-Type", sessionType(sess.ID, sess.Provider))
	w.Header().Set("HX-Trigger", "refreshSidebar")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCreateTerminal(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	workDir := r.FormValue("work_dir")
	if workDir == "" {
		workDir = s.cfg.Sessions.DefaultDir
	}

	// Count existing terminals to generate a friendly name
	termCount := 0
	for _, sess := range s.mgr.List() {
		if strings.HasPrefix(sess.ID, "term-") {
			termCount++
		}
	}
	name := fmt.Sprintf("Terminal %d", termCount+1)
	id := fmt.Sprintf("term-%d", time.Now().UnixMilli())

	// Use the user's default shell
	shell := os.Getenv("SHELL")
	if shell == "" {
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		} else {
			shell = "/bin/bash"
		}
	}

	sess, err := s.mgr.Create(id, workDir, shell, nil)
	if err != nil {
		slog.Error("failed to create terminal", "error", err)
		http.Error(w, "failed to create terminal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sess.Name = name
	if s.store != nil {
		_ = s.store.SaveSession(store.SessionRecord{
			ID: sess.ID, Name: sess.Name, WorkDir: sess.WorkDir,
			StartTime: sess.StartTime, Status: "running",
		})
	}
	w.Header().Set("X-Session-ID", sess.ID)
	w.Header().Set("X-Session-Name", sess.Name)
	w.Header().Set("X-Session-Type", sessionType(sess.ID, sess.Provider))
	s.handleSidebar(w, r)
}

func (s *Server) handleOpenSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, ok := s.mgr.Get(sessionID)
	if ok {
		v := sessionToView(sess)
		w.Header().Set("X-Session-ID", v.ID)
		w.Header().Set("X-Session-Name", v.Name)
		w.Header().Set("X-Session-Type", v.Type)
		if err := templates.Terminal(v.ID, v.Name, v.WorkDir, v.State).Render(r.Context(), w); err != nil {
			slog.Error("failed to render terminal", "error", err)
		}
		return
	}
	// Session not in active manager — check SQLite for history
	if s.store != nil {
		if rec, err := s.store.GetSession(sessionID); err == nil {
			name := rec.Name
			if name == "" {
				name = sessionID
			}
			w.Header().Set("X-Session-ID", rec.ID)
			w.Header().Set("X-Session-Name", name)
			w.Header().Set("X-Session-Type", sessionType(rec.ID, rec.Provider))
			if err := templates.Terminal(rec.ID, name, rec.WorkDir, rec.Status).Render(r.Context(), w); err != nil {
				slog.Error("failed to render terminal", "error", err)
			}
			return
		}
	}
	http.Error(w, "session not found", http.StatusNotFound)
}

func isLocalhostURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1"
}

func (s *Server) handleOpenIframe(w http.ResponseWriter, r *http.Request) {
	iframeURL := r.URL.Query().Get("url")
	title := r.URL.Query().Get("title")
	if iframeURL == "" {
		http.Error(w, "url parameter required", http.StatusBadRequest)
		return
	}
	if !isLocalhostURL(iframeURL) {
		http.Error(w, "only localhost URLs are allowed", http.StatusBadRequest)
		return
	}
	if title == "" {
		title = "Iframe"
	}
	paneID := fmt.Sprintf("iframe-%d", time.Now().UnixMilli())
	_ = templates.IframePane(paneID, title, iframeURL).Render(r.Context(), w)
}

func (s *Server) handleCreateIframePane(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.URL == "" {
		http.Error(w, "url field required", http.StatusBadRequest)
		return
	}
	if !isLocalhostURL(payload.URL) {
		http.Error(w, "only localhost URLs are allowed", http.StatusBadRequest)
		return
	}
	if payload.Title == "" {
		payload.Title = "Iframe"
	}

	msg, _ := json.Marshal(map[string]string{
		"type":  "iframe-open",
		"url":   payload.URL,
		"title": payload.Title,
	})
	s.hub.broadcastNotification(msg)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

func (s *Server) handleNewSessionModal(w http.ResponseWriter, r *http.Request) {
	var recentDirs []string
	if s.store != nil {
		recentDirs, _ = s.store.RecentDirs(10)
	}
	// Also add working dirs from currently discovered/running sessions
	for _, sess := range s.mgr.List() {
		found := false
		for _, d := range recentDirs {
			if d == sess.WorkDir {
				found = true
				break
			}
		}
		if !found && sess.WorkDir != "" {
			recentDirs = append(recentDirs, sess.WorkDir)
		}
	}
	dockerAvailable, _, _ := docker.IsAvailable()
	if err := templates.NewSessionModal(s.cfg.Sessions.DefaultDir, recentDirs, dockerAvailable).Render(r.Context(), w); err != nil {
		slog.Error("failed to render new session modal", "error", err)
	}
}

func (s *Server) handleRecentProjects(w http.ResponseWriter, r *http.Request) {
	var dirs []string
	if s.store != nil {
		dirs, _ = s.store.RecentDirs(10)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(dirs); err != nil {
		slog.Error("failed to encode recent projects", "error", err)
	}
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	var views []templates.NotificationView
	if s.store != nil {
		records, _ := s.store.ListNotifications(30, false)
		for _, rec := range records {
			v := templates.NotificationView{
				ID:        rec.ID,
				SessionID: rec.SessionID,
				EventType: rec.EventType,
				Timestamp: rec.Timestamp.Format("15:04:05"),
				TimeAgo:   timeAgo(rec.Timestamp),
				Message:   eventMessage(rec.EventType),
			}
			// Enrich with session details
			if sess, ok := s.mgr.Get(rec.SessionID); ok {
				v.SessionName = sess.Name
				v.WorkDir = sess.WorkDir
			} else if sessRec, err := s.store.GetSession(rec.SessionID); err == nil {
				v.SessionName = sessRec.Name
				v.WorkDir = sessRec.WorkDir
			}
			if v.SessionName == "" {
				v.SessionName = rec.SessionID
			}
			views = append(views, v)
		}
	}
	_ = templates.Notifications(views).Render(r.Context(), w)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d min ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

func eventMessage(eventType string) string {
	switch eventType {
	case "completed":
		return "Session finished successfully"
	case "errored":
		return "Session exited with an error"
	case "waiting":
		return "Waiting for your input (tool approval)"
	case "killed":
		return "Session was terminated"
	default:
		return eventType
	}
}

func sessionType(id, provider string) string {
	if strings.HasPrefix(id, "term-") {
		return "terminal"
	}
	if strings.HasPrefix(id, "discovered-") {
		return "discovered"
	}
	provider = coerceProvider(provider)
	if provider == "opencode" {
		return "opencode"
	}
	return "claude"
}

func sessionToView(s *session.Session) templates.SessionView {
	return templates.SessionView{
		ID: s.ID, Name: s.Name, WorkDir: s.WorkDir,
		State: string(s.GetState()), Type: sessionType(s.ID, s.Provider), Owned: s.Owned,
		Sandboxed: s.Sandboxed,
	}
}

func normalizeProviderName(provider string) string {
	provider = strings.TrimSpace(strings.ToLower(provider))
	return provider
}

func isSupportedProvider(provider string) bool {
	switch normalizeProviderName(provider) {
	case "claude", "opencode":
		return true
	default:
		return false
	}
}

func (s *Server) defaultProvider() string {
	if s.cfg == nil {
		return "claude"
	}
	provider := normalizeProviderName(s.cfg.Providers.Default)
	if !isSupportedProvider(provider) {
		return "claude"
	}
	return provider
}

func (s *Server) parseRequestedProvider(provider string) (string, error) {
	provider = normalizeProviderName(provider)
	if provider == "" {
		provider = s.defaultProvider()
	}
	if !isSupportedProvider(provider) {
		return "", fmt.Errorf("invalid provider %q", provider)
	}
	return provider, nil
}

func coerceProvider(provider string) string {
	provider = normalizeProviderName(provider)
	if !isSupportedProvider(provider) {
		return "claude"
	}
	return provider
}

func resolveProviderMetadata(provider, externalSessionID, claudeID string) (string, string, string) {
	provider = coerceProvider(provider)
	externalSessionID = strings.TrimSpace(externalSessionID)
	claudeID = strings.TrimSpace(claudeID)
	if provider == "claude" && externalSessionID == "" {
		externalSessionID = claudeID
	}
	if provider == "claude" && claudeID == "" {
		claudeID = externalSessionID
	}
	return provider, externalSessionID, claudeID
}

func (s *Server) providerCommand(provider string) string {
	provider = coerceProvider(provider)
	if s.cfg == nil {
		if provider == "opencode" {
			return "opencode"
		}
		return "claude"
	}
	if provider == "opencode" {
		cmd := strings.TrimSpace(s.cfg.Providers.OpenCode.Command)
		if cmd != "" {
			return cmd
		}
		return "opencode"
	}
	cmd := strings.TrimSpace(s.cfg.Providers.Claude.Command)
	if cmd != "" {
		return cmd
	}
	return "claude"
}

func (s *Server) providerBaseArgs(provider string) []string {
	provider = coerceProvider(provider)
	if s.cfg == nil {
		return nil
	}
	if provider == "opencode" {
		return append([]string{}, s.cfg.Providers.OpenCode.Args...)
	}
	return append([]string{}, s.cfg.Providers.Claude.Args...)
}

func (s *Server) providerResumeCommand(provider, name, externalSessionID string) (string, []string) {
	provider = normalizeProviderName(provider)
	args := s.providerBaseArgs(provider)
	if provider == "opencode" {
		if externalSessionID != "" {
			args = append(args, "--session", externalSessionID)
		}
		return s.providerCommand(provider), args
	}

	args = append(args, "--name", name)
	if externalSessionID != "" {
		args = append(args, "--resume", externalSessionID)
	}
	return s.providerCommand(provider), args
}

func (s *Server) handleTakeover(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, ok := s.mgr.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if sess.GetState() != session.StateDiscovered {
		http.Error(w, "session is not in discovered state", http.StatusBadRequest)
		return
	}
	provider, externalSessionID, claudeID := resolveProviderMetadata(sess.Provider, sess.ExternalSessionID, sess.ClaudeID)
	workDir := sess.WorkDir
	pid := sess.PID
	name := sess.Name

	// Try to resolve session ID if not already known
	if provider == "claude" && externalSessionID == "" {
		claudeID = discovery.ResolveClaudeSessionID(workDir)
		externalSessionID = claudeID
	}

	if err := discovery.KillProcess(pid, 5*time.Second); err != nil {
		slog.Error("takeover kill failed", "session", sessionID, "error", err)
		http.Error(w, "failed to kill process: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mgr.Remove(sessionID)

	command, args := s.providerResumeCommand(provider, name, externalSessionID)
	if externalSessionID != "" {
		slog.Info("takeover resuming session", "session", sessionID, "provider", provider, "external_session_id", externalSessionID)
	}
	newSess, err := s.mgr.Create(sessionID, workDir, command, args, &session.CreateOptions{Provider: provider, ExternalSessionID: externalSessionID})
	if err != nil {
		slog.Error("takeover resume failed", "session", sessionID, "error", err)
		http.Error(w, "failed to resume session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	v := sessionToView(newSess)
	w.Header().Set("X-Session-ID", v.ID)
	w.Header().Set("X-Session-Name", v.Name)
	w.Header().Set("X-Session-Type", v.Type)
	_ = templates.Terminal(v.ID, v.Name, v.WorkDir, v.State).Render(r.Context(), w)
}

// handleListSessions returns a JSON list of all sessions for the split picker.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.mgr.List()
	type sessionJSON struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		WorkDir string `json:"work_dir"`
		State   string `json:"state"`
		Type    string `json:"type"`
	}
	var result []sessionJSON
	for _, sess := range sessions {
		result = append(result, sessionJSON{
			ID: sess.ID, Name: sess.Name, WorkDir: sess.WorkDir,
			State: string(sess.GetState()), Type: sessionType(sess.ID, sess.Provider),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Error("failed to encode sessions list", "error", err)
	}
}

// handleRenameSession renames a session.
func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	sess, ok := s.mgr.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess.Name = name
	// Persist rename to SQLite
	if s.store != nil {
		_ = s.store.SaveSession(store.SessionRecord{
			ID: sess.ID, Name: name, Provider: sess.Provider, ExternalSessionID: sess.ExternalSessionID,
			ClaudeID: sess.ClaudeID, WorkDir: sess.WorkDir,
			StartTime: sess.StartTime, EndTime: sess.EndTime,
			ExitCode: sess.ExitCode, Status: string(sess.GetState()), PID: sess.PID,
		})
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, ok := s.mgr.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess.Killed = true

	// Save to SQLite as killed
	if s.store != nil {
		_ = s.store.SaveSession(store.SessionRecord{
			ID: sess.ID, Name: sess.Name, Provider: sess.Provider, ExternalSessionID: sess.ExternalSessionID,
			ClaudeID: sess.ClaudeID, WorkDir: sess.WorkDir,
			StartTime: sess.StartTime, EndTime: time.Now(),
			ExitCode: -1, Status: "killed", PID: sess.PID,
		})
	}

	// Kill handles: stop reader, kill tmux, fire state change, remove from manager
	if err := s.mgr.Kill(sessionID); err != nil {
		// For sessions without tmux (offline/discovered), just remove
		s.mgr.Remove(sessionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	// Try in-memory first (for offline sessions)
	newSess, err := s.mgr.Restart(sessionID)
	if err != nil {
		// Not in memory — try loading from SQLite history
		if s.store != nil {
			records, _ := s.store.ListSessions(100)
			for _, rec := range records {
				if rec.ID == sessionID {
					// Create directly from history record
					name := rec.Name
					if name == "" {
						name = sessionID
					}
					provider, externalSessionID, claudeID := resolveProviderMetadata(rec.Provider, rec.ExternalSessionID, rec.ClaudeID)
					if provider == "claude" && externalSessionID == "" {
						claudeID = discovery.ResolveClaudeSessionID(rec.WorkDir)
						externalSessionID = claudeID
					}
					command, args := s.providerResumeCommand(provider, name, externalSessionID)
					// Preserve sandbox flag from history
					opts := &session.CreateOptions{Sandboxed: rec.Sandboxed, Provider: provider, ExternalSessionID: externalSessionID}
					newSess, err = s.mgr.Create(sessionID, rec.WorkDir, command, args, opts)
					if err == nil {
						newSess.Name = name
					}
					break
				}
			}
		}
		if err != nil || newSess == nil {
			slog.Error("restart failed", "session", sessionID, "error", err)
			http.Error(w, "failed to restart session: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// Save to DB
	if s.store != nil {
		_ = s.store.SaveSession(store.SessionRecord{
			ID: newSess.ID, Name: newSess.Name, Provider: newSess.Provider, ExternalSessionID: newSess.ExternalSessionID,
			ClaudeID: newSess.ClaudeID, WorkDir: newSess.WorkDir,
			StartTime: newSess.StartTime, Status: "running", PID: newSess.PID,
			Sandboxed: newSess.Sandboxed, SandboxName: newSess.SandboxName,
		})
	}
	v := sessionToView(newSess)
	w.Header().Set("X-Session-ID", v.ID)
	w.Header().Set("X-Session-Name", v.Name)
	w.Header().Set("X-Session-Type", v.Type)
	if err := templates.Terminal(v.ID, v.Name, v.WorkDir, v.State).Render(r.Context(), w); err != nil {
		slog.Error("failed to render terminal", "error", err)
	}
}

// handleGitDiff returns git diff and status for a session's working directory.
func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, ok := s.mgr.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	workDir := sess.WorkDir

	type gitDiffResponse struct {
		Diff    string   `json:"diff"`
		Status  string   `json:"status"`
		Files   []string `json:"files"`
		WorkDir string   `json:"work_dir"`
	}

	// git status
	statusCmd := exec.Command("git", "status", "--short")
	statusCmd.Dir = workDir
	statusOut, err := statusCmd.Output()
	if err != nil {
		slog.Warn("git status failed", "dir", workDir, "error", err)
	}

	// git diff (staged + unstaged)
	diffCmd := exec.Command("git", "diff", "HEAD", "--no-color")
	diffCmd.Dir = workDir
	diffOut, err := diffCmd.Output()
	if err != nil {
		// Maybe no HEAD yet, try just git diff
		diffCmd2 := exec.Command("git", "diff", "--no-color")
		diffCmd2.Dir = workDir
		diffOut, err = diffCmd2.Output()
		if err != nil {
			slog.Warn("git diff failed", "dir", workDir, "error", err)
		}
	}

	// Also get untracked/new files diff
	statusLines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	var files []string
	for _, line := range statusLines {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}

	resp := gitDiffResponse{
		Diff:    string(diffOut),
		Status:  string(statusOut),
		Files:   files,
		WorkDir: workDir,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("failed to encode git diff response", "error", err)
	}
}

type providerSessionResponse struct {
	Provider          string `json:"provider"`
	ExternalSessionID string `json:"external_session_id"`
	ID                string `json:"id"`
	Date              string `json:"date"`
	Summary           string `json:"summary"`
	SizeKB            int64  `json:"size_kb"`
	modTime           time.Time
}

func normalizeWorkDir(workDir string) string {
	if strings.HasPrefix(workDir, "~") {
		home, _ := os.UserHomeDir()
		workDir = home + workDir[1:]
	}
	return strings.TrimSuffix(workDir, "/")
}

func claudeSessionsDir(workDir string) string {
	projectName := strings.ReplaceAll(workDir, "/", "-")
	projectName = strings.ReplaceAll(projectName, ".", "-")
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", projectName)
}

func claudeSessionSummaryFromFile(fpath string) string {
	summary := ""
	f, err := os.Open(fpath)
	if err != nil {
		return summary
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*64), 1024*64)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var obj map[string]interface{}
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		if obj["type"] != "user" {
			continue
		}
		msg, ok := obj["message"].(map[string]interface{})
		if !ok {
			continue
		}
		if content, ok := msg["content"].(string); ok {
			summary = content
		} else if contentList, ok := msg["content"].([]interface{}); ok {
			for _, c := range contentList {
				cm, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if text, ok := cm["text"].(string); ok {
					summary = text
					break
				}
			}
		}
		if len(summary) > 100 {
			summary = summary[:100]
		}
		if summary != "" {
			return summary
		}
	}

	return summary
}

func listStoredProviderSessions(st *store.Store, provider, workDir string) []providerSessionResponse {
	if st == nil {
		return []providerSessionResponse{}
	}

	records, err := st.ListSessions(500)
	if err != nil {
		return []providerSessionResponse{}
	}

	provider = coerceProvider(provider)
	workDir = normalizeWorkDir(workDir)
	sessions := make([]providerSessionResponse, 0)
	for _, rec := range records {
		if coerceProvider(rec.Provider) != provider {
			continue
		}
		if normalizeWorkDir(rec.WorkDir) != workDir {
			continue
		}
		externalSessionID := rec.ExternalSessionID
		if externalSessionID == "" {
			continue
		}
		summary := rec.Name
		if summary == "" {
			summary = externalSessionID
		}
		sessions = append(sessions, providerSessionResponse{
			Provider:          provider,
			ExternalSessionID: externalSessionID,
			ID:                rec.ID,
			Date:              rec.StartTime.Format("2006-01-02 15:04"),
			Summary:           summary,
			SizeKB:            0,
			modTime:           rec.StartTime,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})
	return sessions
}

func listProviderSessions(st *store.Store, provider, workDir string) []providerSessionResponse {
	provider = coerceProvider(provider)
	if provider == "opencode" {
		return listStoredProviderSessions(st, provider, workDir)
	}

	workDir = normalizeWorkDir(workDir)
	sessionsDir := claudeSessionsDir(workDir)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return []providerSessionResponse{}
	}

	sessions := make([]providerSessionResponse, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		info, err := entry.Info()
		if err != nil || info == nil {
			continue
		}

		sessions = append(sessions, providerSessionResponse{
			Provider:          "claude",
			ExternalSessionID: sessionID,
			ID:                sessionID,
			Date:              info.ModTime().Format("2006-01-02 15:04"),
			Summary:           claudeSessionSummaryFromFile(filepath.Join(sessionsDir, entry.Name())),
			SizeKB:            info.Size() / 1024,
			modTime:           info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})
	return sessions
}

func (s *Server) handleProviderSessions(w http.ResponseWriter, r *http.Request) {
	provider, err := s.parseRequestedProvider(r.URL.Query().Get("provider"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workDir := r.URL.Query().Get("work_dir")
	if workDir == "" {
		http.Error(w, "work_dir required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(listProviderSessions(s.store, provider, workDir)); err != nil {
		slog.Error("failed to encode provider sessions", "provider", provider, "error", err)
	}
}

// handleClaudeSessions lists claude sessions available for a given project directory.
func (s *Server) handleClaudeSessions(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("dir")
	if workDir == "" {
		http.Error(w, "dir required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(listProviderSessions(s.store, "claude", workDir)); err != nil {
		slog.Error("failed to encode claude sessions", "error", err)
	}
}

// handleHookCallback receives notifications from Claude Code hooks.
func (s *Server) handleHookCallback(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Event     string `json:"event"`
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Only process "waiting" (permission prompts). Ignore everything else —
	// "completed" fires too often (every turn), "tool_use" is informational.
	if payload.Event != "waiting" {
		w.WriteHeader(http.StatusOK)
		return
	}
	eventType := notification.EventWaiting

	// Map the hook's project path to a websessions session ID.
	// The hook sends Claude's internal UUID as session_id and the cwd as project.
	var resolvedSessionID string
	var resolvedSessionName string
	var managed bool

	// First, try to find an active session matching the working directory
	for _, sess := range s.mgr.List() {
		if sess.WorkDir == payload.Project {
			resolvedSessionID = sess.ID
			resolvedSessionName = sess.Name
			managed = true
			break
		}
	}

	// If not managed, auto-discover: scan for the process and add it
	if !managed && payload.Project != "" {
		processes, err := discovery.Scan()
		if err == nil {
			for _, p := range processes {
				if p.WorkDir == payload.Project {
					// Check it's not already tracked
					alreadyTracked := false
					for _, sess := range s.mgr.List() {
						if sess.PID == p.PID {
							alreadyTracked = true
							resolvedSessionID = sess.ID
							resolvedSessionName = sess.Name
							managed = true
							break
						}
					}
					if !alreadyTracked {
						id := fmt.Sprintf("discovered-%d", p.PID)
						claudeID := p.ClaudeID
						if claudeID == "" {
							claudeID = payload.SessionID
						}
						sess := s.mgr.AddDiscovered(id, claudeID, p.WorkDir, p.PID, p.StartTime)
						if s.store != nil {
							_ = s.store.SaveSession(store.SessionRecord{
								ID: id, Name: sess.Name, ClaudeID: claudeID, WorkDir: p.WorkDir,
								StartTime: p.StartTime, Status: "discovered", PID: p.PID,
							})
						}
						resolvedSessionID = id
						resolvedSessionName = sess.Name
						managed = true
						slog.Info("auto-discovered session from hook", "id", id, "dir", p.WorkDir)
					}
					break
				}
			}
		}

		// If still not found (process might have exited), use project name
		if !managed {
			resolvedSessionID = "external-" + filepath.Base(payload.Project)
			resolvedSessionName = filepath.Base(payload.Project)
		}
	}

	if resolvedSessionName == "" {
		resolvedSessionName = filepath.Base(payload.Project)
	}

	event := notification.SessionEvent{
		SessionID: resolvedSessionID,
		Type:      eventType,
		Timestamp: time.Now(),
		Message:   resolvedSessionName + ": " + eventMessage(string(eventType)),
	}
	s.bus.Publish(event)

	// Persist to store
	if s.store != nil {
		_ = s.store.SaveNotification(store.NotificationRecord{
			SessionID: resolvedSessionID,
			EventType: string(eventType),
			Timestamp: time.Now(),
		})
	}

	w.WriteHeader(http.StatusOK)
}

// handleService manages the background service (systemd on Linux, launchd on macOS).
func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var err error
	switch payload.Action {
	case "install":
		err = service.Install()
		if err == nil {
			err = service.Enable()
		}
	case "uninstall":
		err = service.Uninstall()
	case "start":
		if !service.IsInstalled() {
			err = service.Install()
			if err == nil {
				err = service.Enable()
			}
		}
		if err == nil {
			err = service.Start()
		}
	case "stop":
		err = service.Stop()
	case "enable":
		err = service.Enable()
	case "disable":
		err = service.Disable()
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	if err != nil {
		slog.Error("service action failed", "action", payload.Action, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"action":    payload.Action,
		"status":    service.Status(),
		"installed": service.IsInstalled(),
		"active":    service.IsActive(),
		"enabled":   service.IsEnabled(),
	})
}

// handleCheckUpdate checks for a newer version on GitHub.
func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := updater.CheckForUpdate(s.version)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// handleSelfUpdate downloads and replaces the current binary.
func (s *Server) handleSelfUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := updater.CheckForUpdate(s.version)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if !info.UpdateAvail {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "already up to date"})
		return
	}
	if err := updater.SelfUpdate(info.DownloadURL); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"version": info.LatestVersion,
		"message": "Updated to " + info.LatestVersion + ". Restart the server to use the new version.",
	})
}

// handleInstallHooks installs/uninstalls websessions hooks in Claude settings.
func (s *Server) handleInstallHooks(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	baseURL := fmt.Sprintf("http://%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	if s.cfg.Server.Host == "0.0.0.0" {
		baseURL = fmt.Sprintf("http://localhost:%d", s.cfg.Server.Port)
	}

	var err error
	switch payload.Action {
	case "install":
		err = hooks.Install(baseURL)
	case "uninstall":
		err = hooks.Uninstall()
	default:
		http.Error(w, "action must be 'install' or 'uninstall'", http.StatusBadRequest)
		return
	}

	if err != nil {
		slog.Error("hook action failed", "action", payload.Action, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Check new status
	installed := false
	claudeSettings, loadErr := hooks.Load()
	if loadErr == nil {
		installed = claudeSettings.IsInstalled()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"action":    payload.Action,
		"installed": installed,
	})
}

func (s *Server) handlePlannotatorIntegration(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	var err error
	switch payload.Action {
	case "enable":
		err = hooks.SetPlannotatorIntegration(true)
	case "disable":
		err = hooks.SetPlannotatorIntegration(false)
	default:
		http.Error(w, "action must be 'enable' or 'disable'", http.StatusBadRequest)
		return
	}

	if err != nil {
		slog.Error("plannotator integration failed", "action", payload.Action, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	enabled := false
	claudeSettings, loadErr := hooks.Load()
	if loadErr == nil {
		enabled = claudeSettings.IsPlannotatorIntegrated()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"action":  payload.Action,
		"enabled": enabled,
	})
}

func (s *Server) setupNotificationBridge() {
	desktopSink := notification.NewDesktopSink()
	s.bus.Subscribe(func(e notification.SessionEvent) {
		_ = s.sink.Send(e)
		_ = desktopSink.Send(e)
		_ = s.sound.Send(e)
		// Push to all connected notification WebSocket clients
		msg, _ := json.Marshal(map[string]string{
			"type":      "notification",
			"event":     string(e.Type),
			"sessionID": e.SessionID,
			"message":   e.Message,
			"timestamp": e.Timestamp.Format("15:04:05"),
		})
		s.hub.broadcastNotification(msg)
	})
}

func (s *Server) handleKillAll(w http.ResponseWriter, r *http.Request) {
	killed := 0
	for _, sess := range s.mgr.List() {
		state := sess.GetState()
		if state == session.StateRunning || state == session.StateWaiting || state == session.StateCreated {
			sess.Killed = true
			if s.store != nil {
				_ = s.store.SaveSession(store.SessionRecord{
					ID: sess.ID, Name: sess.Name, Provider: sess.Provider, ExternalSessionID: sess.ExternalSessionID,
					ClaudeID: sess.ClaudeID, WorkDir: sess.WorkDir,
					StartTime: sess.StartTime, EndTime: time.Now(),
					ExitCode: -1, Status: "killed", PID: sess.PID,
				})
			}
			if err := s.mgr.Kill(sess.ID); err != nil {
				s.mgr.Remove(sess.ID)
			}
			killed++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"killed": killed})
}

func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.store == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{})
		return
	}
	prefs, err := s.store.GetAllPreferences()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(prefs)
}

func (s *Server) handleSetPreference(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if s.store != nil {
		_ = s.store.SetPreference(payload.Key, payload.Value)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAudioDevices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(notification.ListAudioDevices())
}

func (s *Server) handleSetAudioDevice(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	s.sound.SetAudioDevice(payload.Device)
	s.cfg.Notifications.AudioDevice = payload.Device
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true", "device": payload.Device})
}

func (s *Server) handleTestSound(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Event string `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	_ = s.sound.Send(notification.SessionEvent{Type: notification.EventType(payload.Event)})
	w.WriteHeader(http.StatusOK)
}

// handleListDirs returns a JSON list of directories for the file finder autocomplete.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	data := templates.SettingsData{
		Port:             s.cfg.Server.Port,
		Host:             s.cfg.Server.Host,
		ScanInterval:     s.cfg.Sessions.ScanIntervalRaw,
		OutputBufferSize: s.cfg.Sessions.OutputBufferRaw,
		DefaultDir:       s.cfg.Sessions.DefaultDir,
		DesktopNotifs:    s.cfg.Notifications.Desktop,
		ReminderMinutes:  s.cfg.Notifications.ReminderMinutes,
		SoundEnabled:     s.cfg.Notifications.Sound,
		AudioDevice:      s.cfg.Notifications.AudioDevice,
		Version:          s.version,
	}
	// Populate audio devices
	for _, d := range notification.ListAudioDevices() {
		data.AudioDevices = append(data.AudioDevices, templates.AudioDeviceView{Name: d.Name, Description: d.Description})
	}
	// Check which events are enabled
	for _, e := range s.cfg.Notifications.Events {
		switch e {
		case "completed":
			data.NotifCompleted = true
		case "errored":
			data.NotifErrored = true
		case "waiting":
			data.NotifWaiting = true
		}
	}
	// Check if hooks are installed
	claudeSettings, err := hooks.Load()
	if err == nil {
		data.HooksInstalled = claudeSettings.IsInstalled()
		data.PlannotatorIntegration = claudeSettings.IsPlannotatorIntegrated()
	}
	// Check service status (systemd on Linux, launchd on macOS)
	data.ServiceInstalled = service.IsInstalled()
	data.ServiceActive = service.IsActive()
	data.ServiceEnabled = service.IsEnabled()
	data.ServiceStatus = service.Status()
	data.ServiceName = service.Name()
	// Run doctor checks
	checks := doctor.RunChecks()
	for _, c := range checks {
		data.Doctor = append(data.Doctor, templates.DoctorCheck{
			Name: c.Name, Status: c.Status, Version: c.Version, Detail: c.Detail,
		})
	}
	_ = templates.Settings(data).Render(r.Context(), w)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	oldPort := s.cfg.Server.Port
	oldHost := s.cfg.Server.Host

	s.cfg.Server.Port, _ = strconv.Atoi(r.FormValue("port"))
	s.cfg.Server.Host = r.FormValue("host")
	s.cfg.Sessions.ScanIntervalRaw = r.FormValue("scan_interval")
	s.cfg.Sessions.OutputBufferRaw = r.FormValue("output_buffer_size")
	s.cfg.Sessions.DefaultDir = r.FormValue("default_dir")
	s.cfg.Notifications.Desktop = r.FormValue("desktop_notifs") == "on"

	var events []string
	if r.FormValue("notif_completed") == "on" {
		events = append(events, "completed")
	}
	if r.FormValue("notif_errored") == "on" {
		events = append(events, "errored")
	}
	if r.FormValue("notif_waiting") == "on" {
		events = append(events, "waiting")
	}
	s.cfg.Notifications.Events = events
	s.cfg.Notifications.ReminderMinutes, _ = strconv.Atoi(r.FormValue("reminder_minutes"))
	s.cfg.Notifications.Sound = r.FormValue("sound_enabled") == "on"
	s.cfg.Notifications.AudioDevice = r.FormValue("audio_device")
	s.sound.SetEnabled(s.cfg.Notifications.Sound)
	s.sound.SetAudioDevice(s.cfg.Notifications.AudioDevice)

	// If port or host changed, uninstall hooks (they contain the old URL)
	if s.cfg.Server.Port != oldPort || s.cfg.Server.Host != oldHost {
		claudeSettings, err := hooks.Load()
		if err == nil && claudeSettings.IsInstalled() {
			_ = hooks.Uninstall()
			slog.Info("hooks uninstalled due to server address change — reinstall from settings")
		}
	}

	// Save config to disk
	if err := s.cfg.Save(); err != nil {
		slog.Error("failed to save config", "error", err)
		http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) handleListDirs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		query = "~"
	}

	// Expand ~
	if strings.HasPrefix(query, "~") {
		home, _ := os.UserHomeDir()
		query = home + query[1:]
	}

	// If query doesn't end with /, list parent dir filtered by prefix
	dir := query
	prefix := ""
	if !strings.HasSuffix(query, "/") {
		dir = filepath.Dir(query)
		prefix = filepath.Base(query)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]string{})
		return
	}

	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(entry.Name()), strings.ToLower(prefix)) {
			continue
		}
		dirs = append(dirs, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(dirs)

	// Limit results
	if len(dirs) > 20 {
		dirs = dirs[:20]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dirs)
}
