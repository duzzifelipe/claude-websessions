package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/store"
	_ "modernc.org/sqlite"
)

func TestStore_SaveAndListSessions(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rec := store.SessionRecord{
		ID: "s1", Name: "my-session", ClaudeID: "claude-abc", Provider: "claude", ExternalSessionID: "claude-abc", WorkDir: "/home/user/project",
		StartTime: time.Now().Add(-5 * time.Minute), EndTime: time.Now(),
		ExitCode: 0, Status: "completed",
	}
	if err := s.SaveSession(rec); err != nil {
		t.Fatal(err)
	}
	rec2 := store.SessionRecord{
		ID: "s2", Name: "my-opencode-session", Provider: "opencode", ExternalSessionID: "opencode-xyz", WorkDir: "/home/user/project2",
		StartTime: time.Now().Add(-1 * time.Minute), EndTime: time.Now(),
		ExitCode: 0, Status: "completed",
	}
	if err := s.SaveSession(rec2); err != nil {
		t.Fatal(err)
	}
	records, err := s.ListSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	byID := make(map[string]store.SessionRecord)
	for _, record := range records {
		byID[record.ID] = record
	}
	if byID["s1"].ID != "s1" {
		t.Errorf("expected ID s1, got %s", byID["s1"].ID)
	}
	if byID["s1"].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", byID["s1"].Provider)
	}
	if byID["s1"].ExternalSessionID != "claude-abc" {
		t.Errorf("expected external_session_id claude-abc, got %s", byID["s1"].ExternalSessionID)
	}
	if byID["s2"].Provider != "opencode" {
		t.Errorf("expected provider opencode, got %s", byID["s2"].Provider)
	}
	if byID["s2"].ExternalSessionID != "opencode-xyz" {
		t.Errorf("expected external_session_id opencode-xyz, got %s", byID["s2"].ExternalSessionID)
	}

	loaded, err := s.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider != "claude" {
		t.Errorf("expected loaded provider claude, got %s", loaded.Provider)
	}
	if loaded.ExternalSessionID != "claude-abc" {
		t.Errorf("expected loaded external_session_id claude-abc, got %s", loaded.ExternalSessionID)
	}

	loaded2, err := s.GetSession("s2")
	if err != nil {
		t.Fatal(err)
	}
	if loaded2.Provider != "opencode" {
		t.Errorf("expected loaded provider opencode, got %s", loaded2.Provider)
	}
	if loaded2.ExternalSessionID != "opencode-xyz" {
		t.Errorf("expected loaded external_session_id opencode-xyz, got %s", loaded2.ExternalSessionID)
	}
}

func TestStore_SaveAndListNotifications(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	n := store.NotificationRecord{
		SessionID: "s1", EventType: "completed", Timestamp: time.Now(), Read: false,
	}
	if err := s.SaveNotification(n); err != nil {
		t.Fatal(err)
	}
	records, err := s.ListNotifications(10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(records))
	}
	if records[0].EventType != "completed" {
		t.Errorf("expected event completed, got %s", records[0].EventType)
	}
}

func TestStore_MarkNotificationRead(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	n := store.NotificationRecord{
		SessionID: "s1", EventType: "errored", Timestamp: time.Now(), Read: false,
	}
	if err := s.SaveNotification(n); err != nil {
		t.Fatal(err)
	}
	records, _ := s.ListNotifications(10, false)
	if err := s.MarkNotificationRead(records[0].ID); err != nil {
		t.Fatal(err)
	}
	unread, _ := s.ListNotifications(10, false)
	if len(unread) != 0 {
		t.Errorf("expected 0 unread, got %d", len(unread))
	}
}

func TestStore_SaveAuditLog(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	err = s.LogAudit("create_session", "s1", "192.168.1.1")
	if err != nil {
		t.Fatal(err)
	}
}

func TestStore_SaveSession_PreservesClaudeIDWhenExternalSessionIDDiffers(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	rec := store.SessionRecord{
		ID:                "s3",
		Name:              "mismatch-id",
		Provider:          "opencode",
		ExternalSessionID: "opencode-123",
		ClaudeID:          "claude-123",
		WorkDir:           "/tmp/project",
		StartTime:         time.Now().Add(-1 * time.Minute),
		EndTime:           time.Now(),
		Status:            "completed",
	}
	if err := s.SaveSession(rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.GetSession("s3")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ClaudeID != "claude-123" {
		t.Fatalf("expected claude_id claude-123, got %q", loaded.ClaudeID)
	}
	if loaded.ExternalSessionID != "opencode-123" {
		t.Fatalf("expected external_session_id opencode-123, got %q", loaded.ExternalSessionID)
	}
}

func TestStore_SaveSession_OpenCodeDoesNotFallbackExternalSessionIDFromClaudeID(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	rec := store.SessionRecord{
		ID:        "s4",
		Name:      "opencode-no-fallback",
		Provider:  "opencode",
		ClaudeID:  "claude-only-id",
		WorkDir:   "/tmp/project",
		StartTime: time.Now().Add(-1 * time.Minute),
		EndTime:   time.Now(),
		Status:    "completed",
	}
	if err := s.SaveSession(rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.GetSession("s4")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider != "opencode" {
		t.Fatalf("expected provider opencode, got %q", loaded.Provider)
	}
	if loaded.ExternalSessionID != "" {
		t.Fatalf("expected empty external_session_id for opencode fallback case, got %q", loaded.ExternalSessionID)
	}
	if loaded.ClaudeID != "claude-only-id" {
		t.Fatalf("expected claude_id claude-only-id, got %q", loaded.ClaudeID)
	}
}

func TestStore_MigrateLegacySessionsMissingProviderAndExternalSessionID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyDB.Close() //nolint:errcheck

	_, err = legacyDB.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			name TEXT,
			claude_id TEXT,
			work_dir TEXT,
			start_time DATETIME,
			end_time DATETIME,
			exit_code INTEGER,
			status TEXT,
			pid INTEGER
		);
		CREATE TABLE notifications (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT,
			event_type TEXT,
			timestamp DATETIME,
			read BOOLEAN DEFAULT FALSE
		);
		CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			action TEXT,
			session_id TEXT,
			client_ip TEXT,
			timestamp DATETIME
		);
		CREATE TABLE preferences (
			key TEXT PRIMARY KEY,
			value TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now().Add(-2 * time.Minute).UTC()
	end := time.Now().UTC()
	_, err = legacyDB.Exec(
		`INSERT INTO sessions (id, name, claude_id, work_dir, start_time, end_time, exit_code, status, pid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-1", "legacy", "claude-legacy", "/tmp/legacy", start, end, 0, "completed", 42,
	)
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	records, err := s.ListSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Provider != "claude" {
		t.Fatalf("expected provider claude fallback, got %q", records[0].Provider)
	}
	if records[0].ExternalSessionID != "claude-legacy" {
		t.Fatalf("expected external_session_id fallback to claude_id, got %q", records[0].ExternalSessionID)
	}
	if records[0].ClaudeID != "claude-legacy" {
		t.Fatalf("expected claude_id claude-legacy, got %q", records[0].ClaudeID)
	}

	loaded, err := s.GetSession("legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider != "claude" {
		t.Fatalf("expected loaded provider claude fallback, got %q", loaded.Provider)
	}
	if loaded.ExternalSessionID != "claude-legacy" {
		t.Fatalf("expected loaded external_session_id fallback to claude_id, got %q", loaded.ExternalSessionID)
	}
	if loaded.ClaudeID != "claude-legacy" {
		t.Fatalf("expected loaded claude_id claude-legacy, got %q", loaded.ClaudeID)
	}
}
