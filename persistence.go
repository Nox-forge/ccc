package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides SQLite-backed persistence for sessions, messages, and context snapshots.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// Message represents a persisted message.
type Message struct {
	ID        int64
	Session   string
	Role      string // "user", "assistant", "system"
	Content   string
	Channel   string // "telegram", "signal", "websocket", etc.
	Timestamp time.Time
}

// ContextSnapshot stores a summary of context at the time of a compact event.
type ContextSnapshot struct {
	ID        int64
	Session   string
	Summary   string
	Timestamp time.Time
}

var (
	store     *Store
	storeOnce sync.Once
	storeErr  error
)

// getDBPath returns the path to the SQLite database.
func getDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc", "sessions.db")
}

// OpenStore opens (or creates) the SQLite database and initializes tables.
func OpenStore() (*Store, error) {
	dbPath := getDBPath()
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Enable WAL mode explicitly
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	// Create tables
	if err := createTables(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	return &Store{db: db}, nil
}

func createTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			name       TEXT PRIMARY KEY,
			topic_id   INTEGER NOT NULL DEFAULT 0,
			path       TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT (datetime('now')),
			updated_at DATETIME DEFAULT (datetime('now'))
		);

		CREATE TABLE IF NOT EXISTS messages (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session    TEXT NOT NULL,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			channel    TEXT NOT NULL DEFAULT 'telegram',
			timestamp  DATETIME DEFAULT (datetime('now')),
			FOREIGN KEY (session) REFERENCES sessions(name) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_messages_session_ts ON messages(session, timestamp);

		CREATE TABLE IF NOT EXISTS context_snapshots (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session    TEXT NOT NULL,
			summary    TEXT NOT NULL,
			timestamp  DATETIME DEFAULT (datetime('now')),
			FOREIGN KEY (session) REFERENCES sessions(name) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_snapshots_session ON context_snapshots(session, timestamp DESC);
	`)
	return err
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// --- Session operations ---

// UpsertSession inserts or updates a session record.
func (s *Store) UpsertSession(name string, topicID int64, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO sessions (name, topic_id, path, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(name) DO UPDATE SET
			topic_id = excluded.topic_id,
			path = excluded.path,
			updated_at = datetime('now')
	`, name, topicID, path)
	return err
}

// DeleteSession removes a session and all its messages/snapshots (CASCADE).
func (s *Store) DeleteSession(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Delete in order since modernc/sqlite may not support FK cascading by default
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tx.Exec("DELETE FROM messages WHERE session = ?", name)
	tx.Exec("DELETE FROM context_snapshots WHERE session = ?", name)
	tx.Exec("DELETE FROM sessions WHERE name = ?", name)

	return tx.Commit()
}

// --- Message operations ---

// SaveMessage persists a message.
func (s *Store) SaveMessage(session, role, content, channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO messages (session, role, content, channel)
		VALUES (?, ?, ?, ?)
	`, session, role, content, channel)
	return err
}

// GetMessages returns recent messages for a session, ordered by timestamp ascending.
// limit=0 returns all messages.
func (s *Store) GetMessages(session string, limit int) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []interface{}

	if limit > 0 {
		// Subquery to get last N, then order ascending
		query = `
			SELECT id, session, role, content, channel, timestamp FROM (
				SELECT id, session, role, content, channel, timestamp
				FROM messages WHERE session = ?
				ORDER BY timestamp DESC LIMIT ?
			) sub ORDER BY timestamp ASC
		`
		args = []interface{}{session, limit}
	} else {
		query = `
			SELECT id, session, role, content, channel, timestamp
			FROM messages WHERE session = ?
			ORDER BY timestamp ASC
		`
		args = []interface{}{session}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var ts string
		if err := rows.Scan(&m.ID, &m.Session, &m.Role, &m.Content, &m.Channel, &ts); err != nil {
			return nil, err
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// GetMessagesSince returns messages after a given timestamp.
func (s *Store) GetMessagesSince(session string, since time.Time) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, session, role, content, channel, timestamp
		FROM messages WHERE session = ? AND timestamp > ?
		ORDER BY timestamp ASC
	`, session, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		var ts string
		if err := rows.Scan(&m.ID, &m.Session, &m.Role, &m.Content, &m.Channel, &ts); err != nil {
			return nil, err
		}
		m.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// MessageCount returns the total number of messages for a session.
func (s *Store) MessageCount(session string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session = ?", session).Scan(&count)
	return count, err
}

// --- Context snapshot operations ---

// SaveSnapshot saves a context snapshot (typically from a /compact event).
func (s *Store) SaveSnapshot(session, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO context_snapshots (session, summary)
		VALUES (?, ?)
	`, session, summary)
	return err
}

// GetLatestSnapshot returns the most recent context snapshot for a session.
func (s *Store) GetLatestSnapshot(session string) (*ContextSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var snap ContextSnapshot
	var ts string
	err := s.db.QueryRow(`
		SELECT id, session, summary, timestamp
		FROM context_snapshots
		WHERE session = ?
		ORDER BY timestamp DESC LIMIT 1
	`, session).Scan(&snap.ID, &snap.Session, &snap.Summary, &ts)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
	return &snap, nil
}

// GetSnapshots returns all context snapshots for a session, newest first.
func (s *Store) GetSnapshots(session string, limit int) ([]ContextSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, session, summary, timestamp
		FROM context_snapshots
		WHERE session = ?
		ORDER BY timestamp DESC LIMIT ?
	`, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []ContextSnapshot
	for rows.Next() {
		var snap ContextSnapshot
		var ts string
		if err := rows.Scan(&snap.ID, &snap.Session, &snap.Summary, &ts); err != nil {
			return nil, err
		}
		snap.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		snaps = append(snaps, snap)
	}
	return snaps, rows.Err()
}

// --- Utility ---

// CheckIntegrity runs SQLite integrity check.
func (s *Store) CheckIntegrity() (string, error) {
	var result string
	err := s.db.QueryRow("PRAGMA integrity_check").Scan(&result)
	return result, err
}

// Stats returns basic database statistics.
func (s *Store) Stats() (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make(map[string]int)

	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err == nil {
		stats["sessions"] = count
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err == nil {
		stats["messages"] = count
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM context_snapshots").Scan(&count); err == nil {
		stats["snapshots"] = count
	}

	return stats, nil
}

// initStore initializes the global store using sync.Once for thread safety.
// Safe to call from multiple goroutines — only opens the DB once.
func initStore() error {
	storeOnce.Do(func() {
		store, storeErr = OpenStore()
	})
	return storeErr
}
