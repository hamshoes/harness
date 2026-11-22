// Package sessions runs the session state machine: one turn at a time per session, stop, restart
// recovery, shutdown, and live fan-out of new events to followers.
//
//	create → running → idle ⇄ running
//	                 ↘ failed            stop (from any non-stopped state) → stopped
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hamshoes/harness/internal/runner"
	"github.com/hamshoes/harness/internal/store"
)

// StopGrace is how long a turn gets after SIGINT before SIGKILL.
const StopGrace = 10 * time.Second

// plainNameRE is what POST /sessions accepts. Crew members are named <crew>.<member>, which only
// crews create.
var plainNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Errors the API maps to status codes.
var (
	ErrInvalid  = errors.New("invalid")
	ErrNotFound = store.ErrNotFound
	ErrExists   = store.ErrExists
	ErrConflict = errors.New("conflict")
	ErrClosing  = errors.New("shutting down")
	ErrLimit    = store.ErrCrewLimit
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Options configures a Manager.
type Options struct {
	PermissionMode string // the --permission-mode of every turn, unless an agent sets its own
	DataDir        string // crew members' MCP config files go under DataDir/mcp
	Socket         string // the API socket, for the crew MCP server
	Exe            string // this binary, run as the crew MCP server
}

// Manager owns every session's live state.
type Manager struct {
	store          *store.Store
	permissionMode string
	opts           Options
	log            *slog.Logger

	crewMu sync.Mutex // serialises crew creation and stopping

	rateMu sync.Mutex
	rate   map[string][]time.Time // crew sends per receiver session, for the flood guard

	mu       sync.Mutex
	live     map[string]*session
	closing  bool
	turns    sync.WaitGroup // running turns
	shutdown bool           // set under mu; turns ending after it are interrupted, not ended
}

// session is the in-memory side of one session.
type session struct {
	name string

	mu       sync.Mutex // the state machine: held while changing status or starting/ending a turn
	proc     *runner.Proc
	ended    chan struct{} // closed by finishTurn once the running turn is fully written
	turn     int
	stopping bool
	claudeID string // last Claude session id seen; guarded by emitMu

	stopMu sync.Mutex // serialises Stop calls

	emitMu sync.Mutex // append + publish, so followers see events in seq order
	bc     *broadcaster
}

// New returns a manager. Call Recover before serving.
func New(st *store.Store, opts Options, log *slog.Logger) *Manager {
	return &Manager{store: st, permissionMode: opts.PermissionMode, opts: opts, log: log,
		live: map[string]*session{}, rate: map[string][]time.Time{}}
}

func (m *Manager) get(name string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.live[name]
	if !ok {
		s = &session{name: name, bc: newBroadcaster()}
		m.live[name] = s
	}
	return s
}

func (m *Manager) emit(s *session, typ string, data map[string]any) {
	if err := m.emitWith(s, typ, data, nil); err != nil {
		m.log.Error("storing event", "session", s.name, "type", typ, "err", err)
	}
}

// emitWith appends an event, running also in the same transaction, and publishes it once committed.
func (m *Manager) emitWith(s *session, typ string, data map[string]any, also func(*sql.Tx) error) error {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	ev, err := m.store.AppendEventWith(s.name, typ, data, also)
	if err != nil {
		return err
	}
	s.bc.publish(ev)
	return nil
}

// Append adds an event to an existing session from outside a turn (the approvals watcher), running
// also in the same transaction.
func (m *Manager) Append(name, typ string, data map[string]any, also func(*sql.Tx) error) error {
	if _, err := m.store.GetSession(name); err != nil {
		return err
	}
	return m.emitWith(m.get(name), typ, data, also)
}

func (m *Manager) setStatus(s *session, status string) {
	if err := m.store.SetStatus(s.name, status); err != nil {
		m.log.Error("setting status", "session", s.name, "status", status, "err", err)
	}
}

// Recover turns sessions left running by a previous server into idle ones with a turn_interrupted event.
func (m *Manager) Recover() error {
	running, err := m.store.ListSessions(store.StatusRunning)
	if err != nil {
		return err
	}
	for _, x := range running {
		s := m.get(x.Name)
		m.emit(s, "turn_interrupted", map[string]any{"turn": x.Turns, "reason": "server restarted"})
		m.setStatus(s, store.StatusIdle)
		m.log.Info("recovered interrupted turn", "session", x.Name, "turn", x.Turns)
	}
	return m.deliverQueued()
}

// ExpandDir expands a leading ~ and checks dir is absolute and, if it exists, a directory. It reports
// whether the directory exists.
func ExpandDir(dir string) (string, bool, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, err
		}
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		return "", false, invalid("dir must be absolute or start with ~/")
	}
	dir = filepath.Clean(dir)
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return dir, false, nil
	case err != nil:
		return "", false, invalid("dir %s: %v", dir, err)
	case !fi.IsDir():
		return "", false, invalid("dir %s exists and is not a directory", dir)
	}
	return dir, true, nil
}

// Create makes a session and starts its first turn. A missing dir is created first.
func (m *Manager) Create(name, dir, prompt string) (store.Session, error) {
	if !plainNameRE.MatchString(name) {
		return store.Session{}, invalid("name must match %s", plainNameRE)
	}
	dir, exists, err := ExpandDir(dir)
	if err != nil {
		return store.Session{}, err
	}
	if strings.TrimSpace(prompt) == "" {
		return store.Session{}, invalid("prompt is required")
	}
	s := m.get(name)
	s.mu.Lock() // every Create of this name goes through this lock, so the checks below hold
	defer s.mu.Unlock()
	if m.isClosing() {
		return store.Session{}, ErrClosing
	}
	if _, err := m.store.GetSession(name); err == nil {
		return store.Session{}, ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Session{}, err
	}
	if !exists {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return store.Session{}, invalid("creating dir: %v", err)
		}
	}
	if _, err := m.store.CreateSession(name, dir, store.StatusRunning); err != nil {
		return store.Session{}, err
	}
	m.log.Info("session created", "session", name, "dir", dir, "dir_created", !exists)
	if !exists {
		m.emit(s, "dir_created", map[string]any{"dir": dir})
	}
	x, err := m.store.GetSession(name)
	if err != nil {
		return store.Session{}, err
	}
	m.startTurn(s, x, prompt, nil)
	return m.store.GetSession(name)
}

// Message starts the next turn of an idle or failed session.
func (m *Manager) Message(name, text string) (store.Session, error) {
	if strings.TrimSpace(text) == "" {
		return store.Session{}, invalid("text is required")
	}
	x, err := m.store.GetSession(name)
	if err != nil {
		return store.Session{}, err
	}
	s := m.get(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.isClosing() {
		return store.Session{}, ErrClosing
	}
	if x, err = m.store.GetSession(name); err != nil { // fresh, under the lock
		return store.Session{}, err
	}
	if x.Crew != "" { // a crew member: the delivery rules, so it's queued while a turn runs
		if _, err := m.deliverLocked(s, x, SenderSam, "", text); err != nil {
			return store.Session{}, err
		}
		return m.store.GetSession(name)
	}
	if x.Status == store.StatusRunning || x.Status == store.StatusStopped || s.proc != nil {
		return store.Session{}, fmt.Errorf("%w: session is %s", ErrConflict, x.Status)
	}
	m.startTurn(s, x, text, nil)
	return m.store.GetSession(name)
}

func (m *Manager) isClosing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closing
}

// startTurn runs with s.mu held. x is the session's row; extra adds fields to the user_message event.
// For a crew member the turn is counted against the crew first: ErrLimit (and the crew stops itself)
// or a stopped crew means no turn starts and the error is returned. Every other failure is recorded
// in the log as a failed turn and nil is returned.
func (m *Manager) startTurn(s *session, x store.Session, prompt string, extra map[string]any) error {
	t := runner.Turn{
		Name:           s.name,
		Dir:            x.Dir,
		Prompt:         prompt,
		PermissionMode: m.permissionMode,
		ResumeID:       x.ClaudeSessionID,
		Emit:           func(typ string, data map[string]any) { m.emitTurn(s, typ, data) },
	}
	var cfgErr error
	if x.Crew != "" {
		if _, err := m.store.CrewTurn(x.Crew); err != nil {
			if errors.Is(err, store.ErrCrewLimit) {
				go m.crewLimit(x.Crew)
				return err
			}
			if errors.Is(err, store.ErrCrewStopped) {
				return fmt.Errorf("%w: crew %s is stopped", ErrConflict, x.Crew)
			}
			return err
		}
		cfgErr = m.memberTurn(x, &t)
	}

	turn, err := m.store.BeginTurn(s.name)
	if err != nil {
		m.log.Error("beginning turn", "session", s.name, "err", err)
		m.setStatus(s, store.StatusFailed)
		return nil
	}
	s.turn = turn
	s.emitMu.Lock()
	s.claudeID = x.ClaudeSessionID
	s.emitMu.Unlock()
	um := map[string]any{"text": prompt, "turn": turn}
	for k, v := range extra {
		um[k] = v
	}
	m.emit(s, "user_message", um)
	m.emit(s, "turn_started", map[string]any{"turn": turn})
	m.log.Info("turn started", "session", s.name, "turn", turn, "resume", x.ClaudeSessionID)
	m.log.Debug("turn prompt", "session", s.name, "turn", turn, "prompt", prompt)

	var p *runner.Proc
	if err = cfgErr; err == nil {
		p, err = runner.Start(t)
	}
	if err != nil {
		m.log.Error("starting turn", "session", s.name, "turn", turn, "err", err)
		m.emit(s, "stderr", map[string]any{"line": "harness: starting turn: " + err.Error()})
		m.emit(s, "turn_ended", map[string]any{"turn": turn, "exit_code": -1, "status": store.StatusFailed})
		m.setStatus(s, store.StatusFailed)
		return nil
	}
	s.proc = p
	s.ended = make(chan struct{})
	m.turns.Add(1)
	go m.finishTurn(s, p, turn, s.ended)
	return nil
}

// emitTurn stores a runner event, and records the Claude session id when it first appears or changes.
func (m *Manager) emitTurn(s *session, typ string, data map[string]any) {
	m.emit(s, typ, data)
	if typ != "claude_init" {
		return
	}
	id, _ := data["session_id"].(string)
	s.emitMu.Lock()
	changed := id != "" && id != s.claudeID
	if changed {
		s.claudeID = id
	}
	s.emitMu.Unlock()
	if changed {
		if err := m.store.SetClaudeSessionID(s.name, id); err != nil {
			m.log.Error("storing claude session id", "session", s.name, "err", err)
		}
	}
}

func (m *Manager) finishTurn(s *session, p *runner.Proc, turn int, ended chan struct{}) {
	defer m.turns.Done()
	res := p.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(ended)
	s.proc = nil
	m.mu.Lock()
	shutdown := m.shutdown
	m.mu.Unlock()

	switch {
	case s.stopping:
		// Stop writes session_stopped and the status once this returns.
		m.emit(s, "turn_ended", map[string]any{"turn": turn, "exit_code": res.ExitCode, "status": store.StatusStopped})
	case shutdown:
		m.emit(s, "turn_interrupted", map[string]any{"turn": turn, "exit_code": res.ExitCode, "reason": "server shut down"})
		m.setStatus(s, store.StatusIdle)
	default:
		status := store.StatusIdle
		if res.ExitCode != 0 || (turn == 1 && res.SessionID == "") {
			status = store.StatusFailed
		}
		m.emit(s, "turn_ended", map[string]any{"turn": turn, "exit_code": res.ExitCode, "status": status})
		m.setStatus(s, status)
		defer m.drainLocked(s) // crew members: queued messages start the next turn
	}
	m.log.Info("turn ended", "session", s.name, "turn", turn, "exit_code", res.ExitCode)
}

// Stop ends a session: SIGINT to a running turn, SIGKILL after StopGrace. Idempotent.
func (m *Manager) Stop(name string) (store.Session, error) {
	if _, err := m.store.GetSession(name); err != nil {
		return store.Session{}, err
	}
	s := m.get(name)
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	s.mu.Lock()
	x, err := m.store.GetSession(name)
	if err != nil || x.Status == store.StatusStopped {
		s.mu.Unlock()
		return x, err
	}
	if p, ended := s.proc, s.ended; p != nil {
		// While stopping, status stays running, so no new turn can start in between.
		s.stopping = true
		m.log.Info("stopping turn", "session", name, "turn", s.turn)
		p.Signal(syscall.SIGINT)
		s.mu.Unlock()
		select {
		case <-ended: // finishTurn wrote turn_ended
		case <-time.After(StopGrace):
			m.log.Warn("turn ignored SIGINT, killing", "session", name)
			p.Signal(syscall.SIGKILL)
			<-ended
		}
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	s.stopping = false
	m.emit(s, "session_stopped", map[string]any{})
	m.setStatus(s, store.StatusStopped)
	s.bc.close()
	m.log.Info("session stopped", "session", name)
	return m.store.GetSession(name)
}

// Shutdown refuses new turns, SIGINTs the running ones, and waits up to StopGrace before SIGKILL.
// Each interrupted turn gets a turn_interrupted event and leaves its session idle.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closing = true
	m.shutdown = true
	all := make([]*session, 0, len(m.live))
	for _, s := range m.live {
		all = append(all, s)
	}
	m.mu.Unlock()

	var procs []*runner.Proc
	for _, s := range all {
		s.mu.Lock()
		if s.proc != nil && !s.stopping {
			m.log.Info("interrupting turn", "session", s.name, "turn", s.turn)
			s.proc.Signal(syscall.SIGINT)
			procs = append(procs, s.proc)
		}
		s.mu.Unlock()
	}
	done := make(chan struct{})
	go func() { m.turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(StopGrace):
		for _, p := range procs {
			p.Signal(syscall.SIGKILL)
		}
		<-done
	}
	for _, s := range all {
		s.bc.close()
	}
}

// Follow streams a session's events with seq > after to fn: the backlog, then (with live) new events
// until the session is stopped or ctx ends. fn returning an error ends it too.
func (m *Manager) Follow(ctx context.Context, name string, after int64, live bool, fn func(store.Event) error) error {
	if _, err := m.store.GetSession(name); err != nil {
		return err
	}
	s := m.get(name)
	last := after
	backlog := func() error {
		for {
			evs, err := m.store.Events(name, last, 500)
			if err != nil {
				return err
			}
			for _, ev := range evs {
				if err := fn(ev); err != nil {
					return err
				}
				last = ev.Seq
			}
			if len(evs) < 500 {
				return nil
			}
		}
	}
	if !live {
		return backlog()
	}
	for {
		sub := s.bc.subscribe() // before the backlog, so nothing falls between the two
		if err := backlog(); err != nil {
			s.bc.unsubscribe(sub)
			return err
		}
	feed:
		for {
			select {
			case <-ctx.Done():
				s.bc.unsubscribe(sub)
				return nil
			case ev, ok := <-sub.C:
				if !ok {
					break feed
				}
				if ev.Seq <= last {
					continue
				}
				if err := fn(ev); err != nil {
					s.bc.unsubscribe(sub)
					return err
				}
				last = ev.Seq
			}
		}
		if !sub.Lagged { // stopped (or the server is shutting down): flush what's left and end
			return backlog()
		}
	}
}

// Health returns session counts by status.
func (m *Manager) Health() (map[string]int, error) {
	return m.store.CountByStatus()
}

// Get returns one session.
func (m *Manager) Get(name string) (store.Session, error) { return m.store.GetSession(name) }

// List returns all sessions, newest first.
func (m *Manager) List() ([]store.Session, error) { return m.store.ListSessions("") }
