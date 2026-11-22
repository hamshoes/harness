package store

import "database/sql"

// Approval states. pending until the spool shows an answer (allow, deny, cancelled) or the ask is gone
// without one (withdrawn).
const (
	ApprovalPending   = "pending"
	ApprovalAllow     = "allow"
	ApprovalDeny      = "deny"
	ApprovalCancelled = "cancelled"
	ApprovalWithdrawn = "withdrawn"
)

// Approval is one row of the approvals table: an ask found in the spool for a session.
type Approval struct {
	AskID      string  `json:"ask_id"`
	Session    string  `json:"session"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	State      string  `json:"state"`
	CreatedAt  string  `json:"created_at"`
	ResolvedAt *string `json:"resolved_at"`
}

// InsertApproval returns a transaction step that records a new pending approval.
func InsertApproval(a Approval) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO approvals (ask_id, session, kind, title, created_at, state) VALUES (?, ?, ?, ?, ?, ?)`,
			a.AskID, a.Session, a.Kind, a.Title, a.CreatedAt, ApprovalPending)
		return err
	}
}

// ResolveApproval returns a transaction step that sets a pending approval's final state.
func ResolveApproval(askID, state, resolvedAt string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE approvals SET state = ?, resolved_at = ? WHERE ask_id = ? AND state = ?`,
			state, resolvedAt, askID, ApprovalPending)
		return err
	}
}

// ApprovalIDs returns every ask id the table holds, in any state.
func (s *Store) ApprovalIDs() (map[string]bool, error) {
	rows, err := s.r.Query(`SELECT ask_id FROM approvals`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Approvals lists approvals, newest first. state "" means any state.
func (s *Store) Approvals(state string, limit int) ([]Approval, error) {
	q := `SELECT ask_id, session, kind, title, state, created_at, resolved_at FROM approvals`
	var args []any
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC, ask_id LIMIT ?`
	rows, err := s.r.Query(q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var a Approval
		var resolved sql.NullString
		if err := rows.Scan(&a.AskID, &a.Session, &a.Kind, &a.Title, &a.State, &a.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		if resolved.Valid {
			a.ResolvedAt = &resolved.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingApprovals lists every pending approval, oldest first.
func (s *Store) PendingApprovals() ([]Approval, error) {
	rows, err := s.r.Query(`SELECT ask_id, session, kind, title, state, created_at FROM approvals
		WHERE state = ? ORDER BY created_at, ask_id`, ApprovalPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.AskID, &a.Session, &a.Kind, &a.Title, &a.State, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Waiting is a pending approval as shown on its session.
type Waiting struct {
	AskID string `json:"ask_id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Since string `json:"since"`
}

// waiting returns pending approvals by session, oldest first; session "" means every session.
func (s *Store) waiting(session string) (map[string][]Waiting, error) {
	q := `SELECT session, ask_id, kind, title, created_at FROM approvals WHERE state = ?`
	args := []any{ApprovalPending}
	if session != "" {
		q += ` AND session = ?`
		args = append(args, session)
	}
	rows, err := s.r.Query(q+` ORDER BY created_at, ask_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Waiting{}
	for rows.Next() {
		var name string
		var w Waiting
		if err := rows.Scan(&name, &w.AskID, &w.Kind, &w.Title, &w.Since); err != nil {
			return nil, err
		}
		out[name] = append(out[name], w)
	}
	return out, rows.Err()
}

// CountPending returns the number of pending approvals.
func (s *Store) CountPending() (int, error) {
	var n int
	err := s.r.QueryRow(`SELECT COUNT(*) FROM approvals WHERE state = ?`, ApprovalPending).Scan(&n)
	return n, err
}
