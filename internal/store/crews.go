package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Crew statuses.
const (
	CrewActive  = "active"
	CrewStopped = "stopped"
)

var (
	// ErrCrewLimit is returned by CrewTurn when the crew has used all its turns.
	ErrCrewLimit = errors.New("crew reached its turn limit")
	// ErrCrewStopped is returned by CrewTurn when the crew is stopped.
	ErrCrewStopped = errors.New("crew is stopped")
)

// Crew is one row of the crews table.
type Crew struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Goal      string `json:"goal"`
	Lead      string `json:"lead"`
	Status    string `json:"status"`
	Turns     int    `json:"turns"`
	MaxTurns  int    `json:"max_turns"`
	CreatedAt string `json:"created_at"`
}

// NewMember is a member session to create with its crew.
type NewMember struct {
	Session string // <crew>.<member>
	Member  string
	Agent   string
}

// Queued is a crew message waiting for its receiver's turn to end.
type Queued struct {
	ID   int64
	From string
	Text string
	At   string
}

// CrewMessage is a crew_message event, as the crew timeline shows it.
type CrewMessage struct {
	Session string          `json:"session"`
	Seq     int64           `json:"seq"`
	At      string          `json:"at"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	Text    json.RawMessage `json:"text"` // a string, or a blob reference
}

const crewCols = `name, dir, goal, lead, status, turns, max_turns, created_at`

func scanCrew(row interface{ Scan(...any) error }) (Crew, error) {
	var c Crew
	err := row.Scan(&c.Name, &c.Dir, &c.Goal, &c.Lead, &c.Status, &c.Turns, &c.MaxTurns, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}

func isConstraint(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && (se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}

// CreateCrew inserts a crew and its member sessions (idle, no turns) in one transaction. ErrExists if
// the crew name or any member's session name was ever used.
func (s *Store) CreateCrew(c Crew, members []NewMember) (Crew, error) {
	now := Now()
	tx, err := s.w.BeginTx(context.Background(), nil)
	if err != nil {
		return Crew{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO crews (`+crewCols+`) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		c.Name, c.Dir, c.Goal, c.Lead, CrewActive, c.MaxTurns, now); err != nil {
		if isConstraint(err) {
			return Crew{}, ErrExists
		}
		return Crew{}, err
	}
	for _, m := range members {
		if _, err := tx.Exec(`INSERT INTO sessions (name, dir, status, created_at, updated_at, crew, member, agent)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.Session, c.Dir, StatusIdle, now, now, c.Name, m.Member, m.Agent); err != nil {
			if isConstraint(err) {
				return Crew{}, ErrExists
			}
			return Crew{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Crew{}, err
	}
	return s.GetCrew(c.Name)
}

// GetCrew returns one crew, or ErrNotFound.
func (s *Store) GetCrew(name string) (Crew, error) {
	return scanCrew(s.r.QueryRow(`SELECT `+crewCols+` FROM crews WHERE name = ?`, name))
}

// ListCrews returns every crew, newest first.
func (s *Store) ListCrews() ([]Crew, error) {
	rows, err := s.r.Query(`SELECT ` + crewCols + ` FROM crews ORDER BY created_at DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Crew{}
	for rows.Next() {
		c, err := scanCrew(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CrewMembers returns a crew's member sessions in the order they were deployed.
func (s *Store) CrewMembers(crew string) ([]Session, error) {
	rows, err := s.r.Query(`SELECT `+sessionCols+` FROM sessions WHERE crew = ? ORDER BY rowid`, crew)
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

// StopCrewRow marks a crew stopped. It reports whether this call changed it (false if already stopped).
func (s *Store) StopCrewRow(name string) (bool, error) {
	res, err := s.w.Exec(`UPDATE crews SET status = ? WHERE name = ? AND status = ?`, CrewStopped, name, CrewActive)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CrewTurn counts one member turn against the crew's budget: ErrCrewStopped if the crew is stopped,
// ErrCrewLimit (nothing counted) once max_turns turns have been used.
func (s *Store) CrewTurn(name string) (Crew, error) {
	tx, err := s.w.BeginTx(context.Background(), nil)
	if err != nil {
		return Crew{}, err
	}
	defer tx.Rollback()
	c, err := scanCrew(tx.QueryRow(`SELECT `+crewCols+` FROM crews WHERE name = ?`, name))
	if err != nil {
		return Crew{}, err
	}
	if c.Status != CrewActive {
		return c, ErrCrewStopped
	}
	if c.Turns >= c.MaxTurns {
		return c, ErrCrewLimit
	}
	if _, err := tx.Exec(`UPDATE crews SET turns = turns + 1 WHERE name = ?`, name); err != nil {
		return Crew{}, err
	}
	c.Turns++
	return c, tx.Commit()
}

// Enqueue stores a message for a member that is mid-turn.
func (s *Store) Enqueue(crew, to, from, text string) error {
	_, err := s.w.Exec(`INSERT INTO crew_queue (crew, to_member, from_member, text, at) VALUES (?, ?, ?, ?, ?)`,
		crew, to, from, text, Now())
	return err
}

// QueuedFor returns a member's queued messages, oldest first.
func (s *Store) QueuedFor(crew, to string) ([]Queued, error) {
	rows, err := s.r.Query(`SELECT id, from_member, text, at FROM crew_queue WHERE crew = ? AND to_member = ? ORDER BY id`, crew, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Queued
	for rows.Next() {
		var q Queued
		if err := rows.Scan(&q.ID, &q.From, &q.Text, &q.At); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// DeleteQueued removes delivered messages.
func (s *Store) DeleteQueued(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := s.w.Exec(`DELETE FROM crew_queue WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	return err
}

// QueuedReceivers returns the member sessions that have queued messages, in active crews.
func (s *Store) QueuedReceivers() ([]string, error) {
	rows, err := s.r.Query(`SELECT DISTINCT s.name FROM crew_queue q
		JOIN crews c ON c.name = q.crew AND c.status = ?
		JOIN sessions s ON s.crew = q.crew AND s.member = q.to_member`, CrewActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CrewMessages returns every crew_message event in a crew's member logs, oldest first.
func (s *Store) CrewMessages(crew string) ([]CrewMessage, error) {
	rows, err := s.r.Query(`SELECT e.session, e.seq, e.ts, s.member, e.data FROM events e
		JOIN sessions s ON s.name = e.session
		WHERE s.crew = ? AND e.type = 'crew_message'
		ORDER BY e.ts, e.session, e.seq`, crew)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CrewMessage{}
	for rows.Next() {
		var m CrewMessage
		var data string
		if err := rows.Scan(&m.Session, &m.Seq, &m.At, &m.To, &data); err != nil {
			return nil, err
		}
		var d struct {
			From string          `json:"from"`
			Text json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			continue
		}
		m.From, m.Text = d.From, d.Text
		out = append(out, m)
	}
	return out, rows.Err()
}
