// Package approvals reports agentic's approval holds in session logs. It polls the notify spool
// read-only: it never writes, renames, deletes or chmods anything there, and never logs an ask's detail.
//
// Spool contract, read from devbox-tools at commit 2750d0f868ac745d5a84d8c876261cd0ff038649
// (tools/agentic/spool.go, pairs/notify/vm/main.go, pkg/platform/platform.go):
//   - The spool is $BOX_NOTIFY_SPOOL, else the BOX_NOTIFY_SPOOL line of /etc/box/platform.env.
//   - agentic writes asks/<ask_id>.json atomically (asks/.tmp-<id>, then rename). ask_id matches
//     ^[a-z0-9]{8,40}$. Fields: ask_id, kind, title, detail, cwd, pid, run_id, run_name, created_at
//     (RFC 3339 UTC). run_name is AGENTIC_RUN_NAME, which `agentic run --name` sets.
//   - notify-vm writes answers/<ask_id>.json atomically: {decision, note, answered_at}, decision
//     allow | deny | cancelled. agentic treats anything but allow or cancelled as deny.
//   - An interrupted agentic touches cancel/<ask_id>; notify-vm answers it cancelled, then agentic
//     removes the ask (also after 3 s if notify-vm doesn't answer). notify-vm removes answers older
//     than an hour, removing the ask first.
package approvals

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hamshoes/harness/internal/store"
)

const (
	EnvSpool     = "BOX_NOTIFY_SPOOL"
	PlatformFile = "/etc/box/platform.env"
	pollEvery    = time.Second
)

var idPattern = regexp.MustCompile(`^[a-z0-9]{8,40}$`)

// FindSpool returns the spool directory: flag, else $BOX_NOTIFY_SPOOL, else PlatformFile.
func FindSpool(flag string) (string, error) {
	dir := flag
	if dir == "" {
		dir = os.Getenv(EnvSpool)
	}
	if dir == "" {
		v, err := platformValue(PlatformFile, EnvSpool)
		if err != nil {
			return "", fmt.Errorf("spool: %s is not set and %w", EnvSpool, err)
		}
		dir = v
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("spool %q is not an absolute path", dir)
	}
	return filepath.Clean(dir), nil
}

// platformValue reads KEY=value (optionally `export`ed, optionally quoted) from a platform env file.
func platformValue(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				v = v[1 : end+1]
			}
		} else if i := strings.IndexAny(v, " \t#"); i >= 0 {
			v = v[:i]
		}
		if v != "" {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no %s", path, key)
}

// CheckSpool fails if asks/ or answers/ can't be listed. The harness never creates them.
func CheckSpool(dir string) error {
	for _, sub := range []string{"asks", "answers"} {
		if _, err := os.ReadDir(filepath.Join(dir, sub)); err != nil {
			return fmt.Errorf("spool %s/ is not readable: %w", sub, err)
		}
	}
	return nil
}

// Sessions is what the watcher needs from the session manager.
type Sessions interface {
	Get(name string) (store.Session, error)
	Append(name, typ string, data map[string]any, also func(*sql.Tx) error) error
}

// Watcher polls the spool and turns asks of running sessions into approval events.
type Watcher struct {
	dir      string
	store    *store.Store
	sessions Sessions
	log      *slog.Logger

	known   map[string]bool // ask ids in the approvals table
	ignored map[string]bool // asks of no running session; not looked at again
}

// New returns a watcher for the spool at dir.
func New(dir string, st *store.Store, sessions Sessions, log *slog.Logger) *Watcher {
	return &Watcher{dir: dir, store: st, sessions: sessions, log: log, ignored: map[string]bool{}}
}

// Dir is the spool directory.
func (w *Watcher) Dir() string { return w.dir }

// Run polls until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		w.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watcher) tick() {
	if w.known == nil {
		ids, err := w.store.ApprovalIDs()
		if err != nil {
			w.log.Error("approvals: loading known asks", "err", err)
			return
		}
		w.known = ids
	}
	w.scanAsks()
	w.scanResolutions()
}

type askFile struct {
	AskID     string `json:"ask_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Cwd       string `json:"cwd"`
	RunName   string `json:"run_name"`
	CreatedAt string `json:"created_at"`
}

func (w *Watcher) scanAsks() {
	ents, err := os.ReadDir(filepath.Join(w.dir, "asks"))
	if err != nil {
		w.log.Warn("approvals: listing asks", "err", err)
		return
	}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) || w.known[id] || w.ignored[id] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(w.dir, "asks", e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue // gone before we read it
		}
		var a askFile
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		if err != nil {
			w.log.Debug("approvals: skipping unreadable ask for now", "ask_id", id, "err", err)
			continue
		}
		if a.RunName == "" {
			w.ignored[id] = true
			continue
		}
		x, err := w.sessions.Get(a.RunName)
		if err != nil || x.Status != store.StatusRunning {
			w.ignored[id] = true // some other agent run on the box
			continue
		}
		if a.CreatedAt == "" {
			a.CreatedAt = store.Now()
		}
		err = w.sessions.Append(x.Name, "approval_pending", map[string]any{
			"ask_id": id, "kind": a.Kind, "title": a.Title, "detail": a.Detail, "cwd": a.Cwd, "created_at": a.CreatedAt,
		}, store.InsertApproval(store.Approval{AskID: id, Session: x.Name, Kind: a.Kind, Title: a.Title, CreatedAt: a.CreatedAt}))
		if err != nil {
			w.log.Error("approvals: recording ask", "ask_id", id, "session", x.Name, "err", err)
			continue
		}
		w.known[id] = true
		w.log.Info("approval pending", "ask_id", id, "session", x.Name, "kind", a.Kind)
	}
}

func (w *Watcher) scanResolutions() {
	pending, err := w.store.PendingApprovals()
	if err != nil {
		w.log.Error("approvals: listing pending", "err", err)
		return
	}
	for _, p := range pending {
		state, note, at, ok := w.resolution(p.AskID)
		if !ok {
			continue
		}
		err := w.sessions.Append(p.Session, "approval_resolved",
			map[string]any{"ask_id": p.AskID, "decision": state, "note": note},
			store.ResolveApproval(p.AskID, state, at))
		if err != nil {
			w.log.Error("approvals: recording resolution", "ask_id", p.AskID, "session", p.Session, "err", err)
			continue
		}
		w.log.Info("approval resolved", "ask_id", p.AskID, "session", p.Session, "decision", state)
	}
}

// resolution reports an ask's final state, if it has one yet.
func (w *Watcher) resolution(id string) (state, note, at string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(w.dir, "answers", id+".json"))
	if err == nil {
		var a struct {
			Decision   string `json:"decision"`
			Note       string `json:"note"`
			AnsweredAt string `json:"answered_at"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			w.log.Debug("approvals: skipping unreadable answer for now", "ask_id", id, "err", err)
			return "", "", "", false
		}
		switch a.Decision {
		case store.ApprovalAllow, store.ApprovalCancelled:
			state = a.Decision
		default: // as agentic reads it
			state = store.ApprovalDeny
		}
		if a.AnsweredAt == "" {
			a.AnsweredAt = store.Now()
		}
		return state, a.Note, a.AnsweredAt, true
	}
	if !errors.Is(err, os.ErrNotExist) {
		w.log.Warn("approvals: reading answer", "ask_id", id, "err", err)
		return "", "", "", false
	}
	if _, err := os.Stat(filepath.Join(w.dir, "asks", id+".json")); errors.Is(err, os.ErrNotExist) {
		return store.ApprovalWithdrawn, "", store.Now(), true
	}
	return "", "", "", false
}
