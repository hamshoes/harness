// Package store keeps sessions and their events in SQLite, and large event strings as blob files.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Session statuses.
const (
	StatusRunning = "running"
	StatusIdle    = "idle"
	StatusStopped = "stopped"
	StatusFailed  = "failed"
)

// Statuses lists every session status.
var Statuses = []string{StatusRunning, StatusIdle, StatusStopped, StatusFailed}

// BlobThreshold is the size above which a string in an event's data is moved to a blob file.
const BlobThreshold = 32 << 10

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Session is one row of the sessions table.
type Session struct {
	Name            string          `json:"name"`
	Dir             string          `json:"dir"`
	Status          string          `json:"status"`
	Turns           int             `json:"turns"`
	LastSeq         int64           `json:"last_seq"`
	ClaudeSessionID string          `json:"claude_session_id"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	LastText        string          `json:"last_text"`        // newest assistant_text, first 200 characters
	LastResult      json.RawMessage `json:"last_result"`      // {duration_ms, total_cost_usd} of the newest result, or null
	WaitingOn       []Waiting       `json:"waiting_on"`       // pending approvals, oldest first
	Crew            string          `json:"crew,omitempty"`   // crew members only
	Member          string          `json:"member,omitempty"` // the name within the crew
	Agent           string          `json:"agent,omitempty"`  // the agent definition it runs
}

// Event is one row of the events table. Data is a JSON object.
type Event struct {
	Seq  int64           `json:"seq"`
	TS   string          `json:"ts"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Store is the SQLite database plus the blob directory.
type Store struct {
	w       *sql.DB // the one writer connection
	r       *sql.DB // readers
	blobDir string
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	name TEXT PRIMARY KEY,
	dir TEXT NOT NULL,
	status TEXT NOT NULL,
	claude_session_id TEXT,
	turns INTEGER NOT NULL DEFAULT 0,
	last_seq INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	last_text TEXT NOT NULL DEFAULT '',
	last_result TEXT
);
CREATE TABLE IF NOT EXISTS events (
	session TEXT NOT NULL,
	seq INTEGER NOT NULL,
	ts TEXT NOT NULL,
	type TEXT NOT NULL,
	data TEXT NOT NULL,
	PRIMARY KEY (session, seq)
);
CREATE TABLE IF NOT EXISTS approvals (
	ask_id TEXT PRIMARY KEY,
	session TEXT NOT NULL,
	kind TEXT NOT NULL,
	title TEXT NOT NULL,
	created_at TEXT NOT NULL,
	state TEXT NOT NULL,
	resolved_at TEXT
);
CREATE INDEX IF NOT EXISTS approvals_state ON approvals (state, created_at);
CREATE TABLE IF NOT EXISTS crews (
	name TEXT PRIMARY KEY,
	dir TEXT NOT NULL,
	goal TEXT NOT NULL,
	lead TEXT NOT NULL,
	status TEXT NOT NULL,
	turns INTEGER NOT NULL DEFAULT 0,
	max_turns INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS crew_queue (
	id INTEGER PRIMARY KEY,
	crew TEXT,
	to_member TEXT,
	from_member TEXT,
	text TEXT,
	at TEXT
);
CREATE INDEX IF NOT EXISTS crew_queue_to ON crew_queue (crew, to_member, id);
`

// Open opens (creating if needed) dataDir/harness.db and dataDir/blobs.
func Open(dataDir string) (*Store, error) {
	blobDir := filepath.Join(dataDir, "blobs")
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "harness.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	if err := addColumns(w, "sessions", map[string]string{
		"last_text":   `TEXT NOT NULL DEFAULT ''`,
		"last_result": `TEXT`,
		"crew":        `TEXT`,
		"member":      `TEXT`,
		"agent":       `TEXT`,
	}); err != nil {
		w.Close()
		return nil, fmt.Errorf("updating schema: %w", err)
	}
	r, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return &Store{w: w, r: r, blobDir: blobDir}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// Now is the timestamp format used everywhere: RFC 3339 UTC with fixed microseconds, so it sorts as text.
func Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
}

const sessionCols = `name, dir, status, COALESCE(claude_session_id, ''), turns, last_seq, created_at, updated_at, last_text, last_result, COALESCE(crew, ''), COALESCE(member, ''), COALESCE(agent, '')`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var x Session
	var result sql.NullString
	err := row.Scan(&x.Name, &x.Dir, &x.Status, &x.ClaudeSessionID, &x.Turns, &x.LastSeq, &x.CreatedAt, &x.UpdatedAt, &x.LastText, &result, &x.Crew, &x.Member, &x.Agent)
	if result.Valid {
		x.LastResult = json.RawMessage(result.String)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return x, err
}

// CreateSession inserts a new session with the given status. ErrExists if the name was ever used.
func (s *Store) CreateSession(name, dir, status string) (Session, error) {
	now := Now()
	_, err := s.w.Exec(`INSERT INTO sessions (name, dir, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		name, dir, status, now, now)
	var se *sqlite.Error
	if errors.As(err, &se) && (se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE) {
		return Session{}, ErrExists
	}
	if err != nil {
		return Session{}, err
	}
	return s.GetSession(name)
}

// GetSession returns one session, or ErrNotFound.
func (s *Store) GetSession(name string) (Session, error) {
	x, err := scanSession(s.r.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE name = ?`, name))
	if err != nil {
		return x, err
	}
	w, err := s.waiting(name)
	x.WaitingOn = w[name]
	if x.WaitingOn == nil {
		x.WaitingOn = []Waiting{}
	}
	return x, err
}

// ListSessions returns all sessions, newest first. With status set, only those in that status.
func (s *Store) ListSessions(status string) ([]Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	rows, err := s.r.Query(q+` ORDER BY created_at DESC, name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	w, err := s.waiting("")
	for i := range out {
		if out[i].WaitingOn = w[out[i].Name]; out[i].WaitingOn == nil {
			out[i].WaitingOn = []Waiting{}
		}
	}
	return out, err
}

// CountByStatus returns the number of sessions in each status (every status present, zero or not).
func (s *Store) CountByStatus() (map[string]int, error) {
	out := map[string]int{}
	for _, st := range Statuses {
		out[st] = 0
	}
	rows, err := s.r.Query(`SELECT status, COUNT(*) FROM sessions GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func (s *Store) exec1(q string, args ...any) error {
	res, err := s.w.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatus sets a session's status.
func (s *Store) SetStatus(name, status string) error {
	return s.exec1(`UPDATE sessions SET status = ?, updated_at = ? WHERE name = ?`, status, Now(), name)
}

// SetClaudeSessionID records the Claude Code session id that later turns resume.
func (s *Store) SetClaudeSessionID(name, id string) error {
	return s.exec1(`UPDATE sessions SET claude_session_id = ?, updated_at = ? WHERE name = ?`, id, Now(), name)
}

// BeginTurn marks the session running and bumps its turn count, returning the new turn number.
func (s *Store) BeginTurn(name string) (int, error) {
	var turn int
	err := s.w.QueryRow(`UPDATE sessions SET status = ?, turns = turns + 1, updated_at = ? WHERE name = ? RETURNING turns`,
		StatusRunning, Now(), name).Scan(&turn)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return turn, err
}

// AppendEvent stores an event with the session's next seq. Strings in data over BlobThreshold are
// moved to blob files first.
func (s *Store) AppendEvent(session, typ string, data map[string]any) (Event, error) {
	return s.AppendEventWith(session, typ, data, nil)
}

// AppendEventWith is AppendEvent with also, when set, run in the same transaction.
func (s *Store) AppendEventWith(session, typ string, data map[string]any, also func(*sql.Tx) error) (Event, error) {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	if len(raw) > BlobThreshold {
		if raw, err = s.externalize(raw); err != nil {
			return Event{}, err
		}
	}
	ev := Event{TS: Now(), Type: typ, Data: raw}
	ctx := context.Background()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`UPDATE sessions SET last_seq = last_seq + 1, updated_at = ? WHERE name = ? RETURNING last_seq`,
		ev.TS, session).Scan(&ev.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}
	if _, err := tx.Exec(`INSERT INTO events (session, seq, ts, type, data) VALUES (?, ?, ?, ?, ?)`,
		session, ev.Seq, ev.TS, ev.Type, string(ev.Data)); err != nil {
		return Event{}, err
	}
	if col, val, ok := summaryUpdate(typ, data); ok {
		if _, err := tx.Exec(`UPDATE sessions SET `+col+` = ? WHERE name = ?`, val, session); err != nil {
			return Event{}, err
		}
	}
	if also != nil {
		if err := also(tx); err != nil {
			return Event{}, err
		}
	}
	return ev, tx.Commit()
}

// Events returns up to limit events of a session with seq > after, in order.
func (s *Store) Events(session string, after int64, limit int) ([]Event, error) {
	rows, err := s.r.Query(`SELECT seq, ts, type, data FROM events WHERE session = ? AND seq > ? ORDER BY seq LIMIT ?`,
		session, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var data string
		if err := rows.Scan(&ev.Seq, &ev.TS, &ev.Type, &data); err != nil {
			return nil, err
		}
		ev.Data = json.RawMessage(data)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// externalize rewrites a JSON object, replacing every string longer than BlobThreshold with a blob reference.
func (s *Store) externalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	v, err := s.walk(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (s *Store) walk(v any) (any, error) {
	var err error
	switch x := v.(type) {
	case string:
		if len(x) > BlobThreshold {
			return s.writeBlob(x)
		}
	case map[string]any:
		for k, e := range x {
			if x[k], err = s.walk(e); err != nil {
				return nil, err
			}
		}
	case []any:
		for i, e := range x {
			if x[i], err = s.walk(e); err != nil {
				return nil, err
			}
		}
	}
	return v, nil
}

func (s *Store) writeBlob(text string) (map[string]any, error) {
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])
	ref := map[string]any{"blob": "sha256:" + h, "bytes": len(text)}
	path := filepath.Join(s.blobDir, h)
	if _, err := os.Stat(path); err == nil {
		return ref, nil
	}
	tmp, err := os.CreateTemp(s.blobDir, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(text)
	if err := errors.Join(werr, tmp.Close(), os.Rename(tmp.Name(), path)); err != nil {
		return nil, err
	}
	return ref, nil
}

// BlobPath returns the file of a blob given as "sha256:<hex>" or bare hex, or ErrNotFound if the hash is malformed.
func (s *Store) BlobPath(hash string) (string, error) {
	h := strings.TrimPrefix(hash, "sha256:")
	if len(h) != sha256.Size*2 || strings.Trim(h, "0123456789abcdef") != "" {
		return "", ErrNotFound
	}
	return filepath.Join(s.blobDir, h), nil
}

// addColumns adds columns that a database made by an older build lacks. Pre-alpha: this is the whole
// migration story.
func addColumns(db *sql.DB, table string, cols map[string]string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for name, def := range cols {
		if !have[name] {
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + def); err != nil {
				return err
			}
		}
	}
	return nil
}

// summaryUpdate returns the sessions-row update an event implies for the list fields, if any. It reads
// data as given, before large strings are moved to blobs.
func summaryUpdate(typ string, data map[string]any) (col string, val any, ok bool) {
	switch typ {
	case "assistant_text":
		text, isStr := data["text"].(string)
		if !isStr {
			return "", nil, false
		}
		if r := []rune(text); len(r) > 200 {
			text = string(r[:200])
		}
		return "last_text", text, true
	case "result":
		raw, err := json.Marshal(map[string]any{"duration_ms": data["duration_ms"], "total_cost_usd": data["total_cost_usd"]})
		if err != nil {
			return "", nil, false
		}
		return "last_result", string(raw), true
	}
	return "", nil, false
}
