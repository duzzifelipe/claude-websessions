package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type SessionRecord struct {
	ID                string
	Name              string
	Provider          string
	ExternalSessionID string
	ClaudeID          string
	WorkDir           string
	StartTime         time.Time
	EndTime           time.Time
	ExitCode          int
	Status            string
	PID               int
	Sandboxed         bool
	SandboxName       string
}

type NotificationRecord struct {
	ID        int64
	SessionID string
	EventType string
	Timestamp time.Time
	Read      bool
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	_, _ = db.Exec("PRAGMA journal_mode=WAL")
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY, name TEXT, provider TEXT, external_session_id TEXT, claude_id TEXT, work_dir TEXT,
		start_time DATETIME, end_time DATETIME,
		exit_code INTEGER, status TEXT, pid INTEGER
	);
	CREATE TABLE IF NOT EXISTS notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT, event_type TEXT,
		timestamp DATETIME, read BOOLEAN DEFAULT FALSE
	);
	CREATE TABLE IF NOT EXISTS audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		action TEXT, session_id TEXT, client_ip TEXT, timestamp DATETIME
	);
	CREATE TABLE IF NOT EXISTS preferences (
		key TEXT PRIMARY KEY,
		value TEXT
	);`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// Migration: add name column if it doesn't exist (for existing DBs)
	_, _ = db.Exec("ALTER TABLE sessions ADD COLUMN name TEXT DEFAULT ''")
	// Migration: add sandbox columns
	_, _ = db.Exec("ALTER TABLE sessions ADD COLUMN sandboxed BOOLEAN DEFAULT FALSE")
	_, _ = db.Exec("ALTER TABLE sessions ADD COLUMN sandbox_name TEXT DEFAULT ''")
	// Migration: add provider columns
	_, _ = db.Exec("ALTER TABLE sessions ADD COLUMN provider TEXT DEFAULT ''")
	_, _ = db.Exec("ALTER TABLE sessions ADD COLUMN external_session_id TEXT DEFAULT ''")
	return nil
}

func (s *Store) SaveSession(r SessionRecord) error {
	provider := strings.ToLower(strings.TrimSpace(r.Provider))
	externalSessionID := r.ExternalSessionID
	claudeID := r.ClaudeID
	if provider == "" && claudeID != "" {
		provider = "claude"
	}
	if provider == "claude" {
		if claudeID == "" {
			claudeID = externalSessionID
		}
		if externalSessionID == "" {
			externalSessionID = claudeID
		}
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO sessions (id, name, provider, external_session_id, claude_id, work_dir, start_time, end_time, exit_code, status, pid, sandboxed, sandbox_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, provider, externalSessionID, claudeID, r.WorkDir, r.StartTime, r.EndTime, r.ExitCode, r.Status, r.PID, r.Sandboxed, r.SandboxName,
	)
	return err
}

func (s *Store) ListSessions(limit int) ([]SessionRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, COALESCE(name, ''), COALESCE(provider, ''), COALESCE(external_session_id, ''), COALESCE(claude_id, ''), work_dir, start_time, end_time, exit_code, status, pid, COALESCE(sandboxed, 0), COALESCE(sandbox_name, '') FROM sessions ORDER BY start_time DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var records []SessionRecord
	for rows.Next() {
		var r SessionRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.Provider, &r.ExternalSessionID, &r.ClaudeID, &r.WorkDir, &r.StartTime, &r.EndTime, &r.ExitCode, &r.Status, &r.PID, &r.Sandboxed, &r.SandboxName); err != nil {
			return nil, err
		}
		r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
		if r.Provider == "" && r.ClaudeID != "" {
			r.Provider = "claude"
		}
		if r.Provider == "claude" {
			if r.ExternalSessionID == "" {
				r.ExternalSessionID = r.ClaudeID
			}
			if r.ClaudeID == "" {
				r.ClaudeID = r.ExternalSessionID
			}
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) SaveNotification(n NotificationRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO notifications (session_id, event_type, timestamp, read) VALUES (?, ?, ?, ?)`,
		n.SessionID, n.EventType, n.Timestamp, n.Read,
	)
	return err
}

func (s *Store) ListNotifications(limit int, includeRead bool) ([]NotificationRecord, error) {
	query := `SELECT id, session_id, event_type, timestamp, read FROM notifications`
	if !includeRead {
		query += ` WHERE read = FALSE`
	}
	query += ` ORDER BY timestamp DESC LIMIT ?`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var records []NotificationRecord
	for rows.Next() {
		var r NotificationRecord
		if err := rows.Scan(&r.ID, &r.SessionID, &r.EventType, &r.Timestamp, &r.Read); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) GetSession(id string) (*SessionRecord, error) {
	var r SessionRecord
	err := s.db.QueryRow(
		`SELECT id, COALESCE(name, ''), COALESCE(provider, ''), COALESCE(external_session_id, ''), COALESCE(claude_id, ''), work_dir, start_time, end_time, exit_code, status, pid, COALESCE(sandboxed, 0), COALESCE(sandbox_name, '') FROM sessions WHERE id = ?`, id,
	).Scan(&r.ID, &r.Name, &r.Provider, &r.ExternalSessionID, &r.ClaudeID, &r.WorkDir, &r.StartTime, &r.EndTime, &r.ExitCode, &r.Status, &r.PID, &r.Sandboxed, &r.SandboxName)
	if err != nil {
		return nil, err
	}
	r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
	if r.Provider == "" && r.ClaudeID != "" {
		r.Provider = "claude"
	}
	if r.Provider == "claude" {
		if r.ExternalSessionID == "" {
			r.ExternalSessionID = r.ClaudeID
		}
		if r.ClaudeID == "" {
			r.ClaudeID = r.ExternalSessionID
		}
	}
	return &r, nil
}

func (s *Store) MarkNotificationRead(id int64) error {
	_, err := s.db.Exec(`UPDATE notifications SET read = TRUE WHERE id = ?`, id)
	return err
}

func (s *Store) MarkAllNotificationsRead() error {
	_, err := s.db.Exec(`UPDATE notifications SET read = TRUE WHERE read = FALSE`)
	return err
}

// RecentDirs returns distinct working directories from recent sessions, most recent first.
func (s *Store) RecentDirs(limit int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT work_dir FROM sessions WHERE work_dir != '' ORDER BY start_time DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var dirs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		dirs = append(dirs, d)
	}
	return dirs, rows.Err()
}

func (s *Store) GetPreference(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM preferences WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) SetPreference(key, value string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO preferences (key, value) VALUES (?, ?)`,
		key, value,
	)
	return err
}

func (s *Store) GetAllPreferences() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM preferences`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		prefs[k] = v
	}
	return prefs, rows.Err()
}

func (s *Store) LogAudit(action, sessionID, clientIP string) error {
	_, err := s.db.Exec(
		`INSERT INTO audit_log (action, session_id, client_ip, timestamp) VALUES (?, ?, ?, ?)`,
		action, sessionID, clientIP, time.Now(),
	)
	return err
}
