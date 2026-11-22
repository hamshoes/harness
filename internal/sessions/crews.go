package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hamshoes/harness/internal/agents"
	"github.com/hamshoes/harness/internal/runner"
	"github.com/hamshoes/harness/internal/store"
)

// Crew messaging limits and defaults.
const (
	SenderSam       = "sam" // the sender of messages from the page or the API
	MaxSendText     = 8000  // characters per send
	DefaultMaxTurns = 60
	maxMaxTurns     = 1000
	maxMembers      = 12
	rateWindow      = 10 * time.Minute
	rateMax         = 30 // sends per receiver per rateWindow
)

// CrewTools are the harness MCP server's tools, allowed for every crew member whatever its agent says.
var CrewTools = []string{"mcp__harness__send", "mcp__harness__members"}

// ErrTooMany is the flood guard's error.
var ErrTooMany = errors.New("too many messages")

// crewNameRE is a crew or member name.
var crewNameRE = agents.NameRE

// MemberSpec is one member to deploy: an agent, and optionally its name in the crew.
type MemberSpec struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
}

// CrewSpec is what POST /crews takes.
type CrewSpec struct {
	Name     string       `json:"name"`
	Dir      string       `json:"dir"`
	Goal     string       `json:"goal"`
	Lead     string       `json:"lead"`
	Members  []MemberSpec `json:"members"`
	MaxTurns int          `json:"max_turns"`
}

// Member is a member session plus its agent's description.
type Member struct {
	store.Session
	Description string `json:"description"`
}

// CrewView is a crew with its members.
type CrewView struct {
	store.Crew
	Members []Member `json:"members"`
}

func memberSession(crew, member string) string { return crew + "." + member }

// CreateCrew validates the spec, creates the crew and one idle session per member, then sends the goal
// to the lead as its first message, from sam.
func (m *Manager) CreateCrew(spec CrewSpec) (CrewView, error) {
	if !crewNameRE.MatchString(spec.Name) {
		return CrewView{}, invalid("crew name must match %s", crewNameRE)
	}
	if strings.TrimSpace(spec.Goal) == "" {
		return CrewView{}, invalid("goal is required")
	}
	if len(spec.Members) == 0 || len(spec.Members) > maxMembers {
		return CrewView{}, invalid("a crew has 1 to %d members", maxMembers)
	}
	if spec.MaxTurns == 0 {
		spec.MaxTurns = DefaultMaxTurns
	}
	if spec.MaxTurns < 1 || spec.MaxTurns > maxMaxTurns {
		return CrewView{}, invalid("max_turns must be 1 to %d", maxMaxTurns)
	}
	dir, exists, err := ExpandDir(spec.Dir)
	if err != nil {
		return CrewView{}, err
	}
	agentDir := ""
	if exists {
		agentDir = dir
	}
	defs, err := agents.Load(agentDir, m.log)
	if err != nil {
		return CrewView{}, err
	}
	known := map[string]bool{}
	for _, a := range defs {
		known[a.Name] = true
	}

	// Member names: explicit ones first, so defaults (coder, coder-2, …) go around them.
	taken := map[string]bool{}
	for _, ms := range spec.Members {
		if ms.Name == "" {
			continue
		}
		if !crewNameRE.MatchString(ms.Name) {
			return CrewView{}, invalid("member name %q must match %s", ms.Name, crewNameRE)
		}
		if ms.Name == SenderSam {
			return CrewView{}, invalid("member name %q is reserved", SenderSam)
		}
		if taken[ms.Name] {
			return CrewView{}, invalid("member name %q is used twice", ms.Name)
		}
		taken[ms.Name] = true
	}
	var news []store.NewMember
	for _, ms := range spec.Members {
		if !known[ms.Agent] {
			return CrewView{}, invalid("no agent %q for %s (put agent files in ~/.config/harness/agents or <dir>/.claude/agents)", ms.Agent, dir)
		}
		name := ms.Name
		if name == "" {
			name = ms.Agent
			if name == SenderSam {
				name = "sam-agent"
			}
			for i := 2; taken[name]; i++ {
				name = ms.Agent + "-" + strconv.Itoa(i)
			}
			if !crewNameRE.MatchString(name) {
				return CrewView{}, invalid("can't name a second %s member; give it a name", ms.Agent)
			}
			taken[name] = true
		}
		news = append(news, store.NewMember{Session: memberSession(spec.Name, name), Member: name, Agent: ms.Agent})
	}
	if !taken[spec.Lead] {
		return CrewView{}, invalid("lead %q is not a member", spec.Lead)
	}

	m.crewMu.Lock()
	if m.isClosing() {
		m.crewMu.Unlock()
		return CrewView{}, ErrClosing
	}
	if !exists {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			m.crewMu.Unlock()
			return CrewView{}, invalid("creating dir: %v", err)
		}
	}
	_, err = m.store.CreateCrew(store.Crew{Name: spec.Name, Dir: dir, Goal: spec.Goal, Lead: spec.Lead, MaxTurns: spec.MaxTurns}, news)
	m.crewMu.Unlock()
	if errors.Is(err, store.ErrExists) {
		return CrewView{}, fmt.Errorf("%w: crew %s or one of its member session names was used before", ErrExists, spec.Name)
	}
	if err != nil {
		return CrewView{}, err
	}
	m.log.Info("crew created", "crew", spec.Name, "dir", dir, "members", len(news), "lead", spec.Lead)

	lead := m.get(memberSession(spec.Name, spec.Lead))
	lead.mu.Lock()
	if !exists {
		m.emit(lead, "dir_created", map[string]any{"dir": dir})
	}
	x, err := m.store.GetSession(lead.name)
	if err == nil {
		_, err = m.deliverLocked(lead, x, SenderSam, "", spec.Goal)
	}
	lead.mu.Unlock()
	if err != nil {
		m.log.Error("sending the goal to the lead", "crew", spec.Name, "err", err)
	}
	return m.Crew(spec.Name)
}

// Crew returns a crew with its members.
func (m *Manager) Crew(name string) (CrewView, error) {
	c, err := m.store.GetCrew(name)
	if err != nil {
		return CrewView{}, crewErr(err)
	}
	return m.view(c)
}

// Crews returns every crew with its members, newest first.
func (m *Manager) Crews() ([]CrewView, error) {
	cs, err := m.store.ListCrews()
	if err != nil {
		return nil, err
	}
	out := make([]CrewView, 0, len(cs))
	for _, c := range cs {
		v, err := m.view(c)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *Manager) view(c store.Crew) (CrewView, error) {
	xs, err := m.store.CrewMembers(c.Name)
	if err != nil {
		return CrewView{}, err
	}
	desc := map[string]string{}
	if defs, err := agents.Load(c.Dir, m.log); err == nil {
		for _, a := range defs {
			desc[a.Name] = a.Description
		}
	}
	v := CrewView{Crew: c, Members: make([]Member, 0, len(xs))}
	for _, x := range xs {
		v.Members = append(v.Members, Member{Session: x, Description: desc[x.Agent]})
	}
	return v, nil
}

// ErrCrewNotFound is ErrNotFound for crews.
var ErrCrewNotFound = fmt.Errorf("%w: crew", store.ErrNotFound)

func crewErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrCrewNotFound
	}
	return err
}

// CrewMessages returns every crew message, oldest first.
func (m *Manager) CrewMessages(name string) ([]store.CrewMessage, error) {
	if _, err := m.store.GetCrew(name); err != nil {
		return nil, crewErr(err)
	}
	return m.store.CrewMessages(name)
}

// StopCrew marks the crew stopped and stops every member. Idempotent.
func (m *Manager) StopCrew(name string) (CrewView, error) {
	if _, err := m.store.GetCrew(name); err != nil {
		return CrewView{}, crewErr(err)
	}
	if _, err := m.store.StopCrewRow(name); err != nil {
		return CrewView{}, err
	}
	m.log.Info("stopping crew", "crew", name)
	m.stopMembers(name)
	return m.Crew(name)
}

func (m *Manager) stopMembers(crew string) {
	xs, err := m.store.CrewMembers(crew)
	if err != nil {
		m.log.Error("listing crew members", "crew", crew, "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, x := range xs {
		wg.Go(func() {
			if _, err := m.Stop(x.Name); err != nil {
				m.log.Error("stopping crew member", "session", x.Name, "err", err)
			}
		})
	}
	wg.Wait()
}

// crewLimit stops a crew that has used all its turns, with a crew_limit event in every member's log.
// It runs on its own goroutine: the caller holds a member's lock.
func (m *Manager) crewLimit(crew string) {
	changed, err := m.store.StopCrewRow(crew)
	if err != nil || !changed {
		return
	}
	c, _ := m.store.GetCrew(crew)
	xs, err := m.store.CrewMembers(crew)
	if err != nil {
		m.log.Error("listing crew members", "crew", crew, "err", err)
	}
	for _, x := range xs {
		m.emit(m.get(x.Name), "crew_limit", map[string]any{"turns": c.Turns, "max_turns": c.MaxTurns})
	}
	m.log.Warn("crew reached its turn limit", "crew", crew, "max_turns", c.MaxTurns)
	m.stopMembers(crew)
}

// Send delivers a message from one member of a crew to another (the MCP server's send tool). It
// returns "delivered" when it started the receiver's turn, "queued" when the receiver is mid-turn.
func (m *Manager) Send(crew, from, to, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", invalid("text is required")
	}
	if utf8.RuneCountInString(text) > MaxSendText {
		return "", invalid("text is over %d characters", MaxSendText)
	}
	c, err := m.store.GetCrew(crew)
	if err != nil {
		return "", crewErr(err)
	}
	if c.Status != store.CrewActive {
		return "", fmt.Errorf("%w: crew %s is stopped", ErrConflict, crew)
	}
	fx, err := m.store.GetSession(memberSession(crew, from))
	if err != nil || fx.Crew != crew {
		return "", invalid("%s is not a member of crew %s", from, crew)
	}
	if to == from {
		return "", invalid("can't send to yourself")
	}
	tx, err := m.store.GetSession(memberSession(crew, to))
	if err != nil || tx.Crew != crew {
		return "", invalid("no member %q in crew %s", to, crew)
	}
	if tx.Status == store.StatusStopped {
		return "", fmt.Errorf("%w: %s is stopped", ErrConflict, to)
	}
	if !m.allowSend(tx.Name) {
		return "", fmt.Errorf("%w to %s; batch them", ErrTooMany, to)
	}
	s := m.get(tx.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.isClosing() {
		return "", ErrClosing
	}
	x, err := m.store.GetSession(tx.Name) // fresh, under the lock
	if err != nil {
		return "", err
	}
	return m.deliverLocked(s, x, from, fx.Name, text)
}

// allowSend is the flood guard: at most rateMax sends to one receiver per rateWindow.
func (m *Manager) allowSend(receiver string) bool {
	m.rateMu.Lock()
	defer m.rateMu.Unlock()
	cut := time.Now().Add(-rateWindow)
	ts := slices.DeleteFunc(m.rate[receiver], func(t time.Time) bool { return t.Before(cut) })
	if len(ts) >= rateMax {
		m.rate[receiver] = ts
		return false
	}
	m.rate[receiver] = append(ts, time.Now())
	return true
}

// deliverLocked runs with s.mu held, s being the receiver x. It writes crew_sent to the sender's log
// (fromSession; empty for sam) and crew_message to the receiver's, queues the message, and, unless the
// receiver is mid-turn, starts its next turn with everything queued for it.
func (m *Manager) deliverLocked(s *session, x store.Session, from, fromSession, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", invalid("text is required")
	}
	if x.Status == store.StatusStopped {
		return "", fmt.Errorf("%w: %s is stopped", ErrConflict, x.Member)
	}
	c, err := m.store.GetCrew(x.Crew)
	if err != nil {
		return "", crewErr(err)
	}
	if c.Status != store.CrewActive {
		return "", fmt.Errorf("%w: crew %s is stopped", ErrConflict, c.Name)
	}
	idle := x.Status != store.StatusRunning && s.proc == nil && !s.stopping
	if idle && c.Turns >= c.MaxTurns { // it would need a turn there's no budget for
		go m.crewLimit(c.Name)
		return "", ErrLimit
	}
	if err := m.store.Enqueue(c.Name, x.Member, from, text); err != nil {
		return "", err
	}
	if fromSession != "" {
		m.emit(m.get(fromSession), "crew_sent", map[string]any{"to": x.Member, "text": text})
	}
	m.emit(s, "crew_message", map[string]any{"from": from, "text": text})
	m.log.Info("crew message", "crew", c.Name, "from", from, "to", x.Member)
	m.log.Debug("crew message text", "crew", c.Name, "from", from, "to", x.Member, "text", text)
	if !idle {
		return "queued", nil
	}
	if err := m.startQueued(s, x); err != nil {
		return "", err
	}
	return "delivered", nil
}

// drainLocked runs with s.mu held: an idle or failed crew member with queued messages starts its next
// turn with all of them.
func (m *Manager) drainLocked(s *session) {
	if m.isClosing() || s.proc != nil || s.stopping {
		return
	}
	x, err := m.store.GetSession(s.name)
	if err != nil || x.Crew == "" || (x.Status != store.StatusIdle && x.Status != store.StatusFailed) {
		return
	}
	if err := m.startQueued(s, x); err != nil && !errors.Is(err, ErrLimit) && !errors.Is(err, ErrConflict) {
		m.log.Error("delivering queued crew messages", "session", s.name, "err", err)
	}
}

// startQueued starts a member's turn with every message queued for it, oldest first, and removes them
// from the queue once the turn is under way. Nothing queued: nothing happens.
func (m *Manager) startQueued(s *session, x store.Session) error {
	qs, err := m.store.QueuedFor(x.Crew, x.Member)
	if err != nil || len(qs) == 0 {
		return err
	}
	parts := make([]string, 0, len(qs))
	from := make([]string, 0, len(qs))
	ids := make([]int64, 0, len(qs))
	for _, q := range qs {
		parts = append(parts, fmt.Sprintf("Message from %s (crew %s):\n\n%s", q.From, x.Crew, q.Text))
		from = append(from, q.From)
		ids = append(ids, q.ID)
	}
	if err := m.startTurn(s, x, strings.Join(parts, "\n\n"), map[string]any{"from": from}); err != nil {
		return err
	}
	return m.store.DeleteQueued(ids)
}

// deliverQueued starts the turns that queued messages are waiting for: after a restart, members that
// were mid-turn are idle again.
func (m *Manager) deliverQueued() error {
	names, err := m.store.QueuedReceivers()
	if err != nil {
		return err
	}
	for _, name := range names {
		s := m.get(name)
		s.mu.Lock()
		m.drainLocked(s)
		s.mu.Unlock()
	}
	return nil
}

// memberTurn adds a crew member's agent settings to its turn: model, tools (plus the crew tools),
// permission mode, the agent's prompt and a crew note as the appended system prompt, and the harness
// MCP server.
func (m *Manager) memberTurn(x store.Session, t *runner.Turn) error {
	c, err := m.store.GetCrew(x.Crew)
	if err != nil {
		return err
	}
	defs, err := agents.Load(c.Dir, m.log)
	if err != nil {
		return err
	}
	byName := map[string]agents.Agent{}
	for _, a := range defs {
		byName[a.Name] = a
	}
	a, ok := byName[x.Agent]
	if !ok {
		return fmt.Errorf("agent %q has no definition for %s any more", x.Agent, c.Dir)
	}
	members, err := m.store.CrewMembers(c.Name)
	if err != nil {
		return err
	}
	t.Model = a.Model
	if a.PermissionMode != "" {
		t.PermissionMode = a.PermissionMode
	}
	t.AllowedTools = append(slices.Clone(a.Tools), CrewTools...)
	note := crewNote(c, x, members, byName)
	if a.Prompt != "" {
		t.AppendSystemPrompt = a.Prompt + "\n\n" + note
	} else {
		t.AppendSystemPrompt = note
	}
	t.MCPConfig, err = m.writeMCPConfig(x.Name)
	return err
}

// crewNote tells a member about its crew: name, goal, members and how messaging works.
func crewNote(c store.Crew, me store.Session, members []store.Session, defs map[string]agents.Agent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Crew %s\n\n", c.Name)
	fmt.Fprintf(&b, "You are %q, one member of crew %q: Claude Code agents working together in %s.\n\n", me.Member, c.Name, c.Dir)
	fmt.Fprintf(&b, "Goal (from sam, the human who deployed the crew):\n%s\n\n", strings.TrimSpace(c.Goal))
	b.WriteString("Members:\n")
	for _, x := range members {
		var tags []string
		if x.Member == me.Member {
			tags = append(tags, "you")
		}
		if x.Member == c.Lead {
			tags = append(tags, "lead")
		}
		line := fmt.Sprintf("- %s (agent %s", x.Member, x.Agent)
		if len(tags) > 0 {
			line += ", " + strings.Join(tags, ", ")
		}
		line += ")"
		if d := strings.TrimSpace(defs[x.Agent].Description); d != "" {
			line += ": " + strings.ReplaceAll(d, "\n", " ")
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, `
Messaging:
- Use the harness MCP tools: send(to, text) messages another member by name; members() lists them with their status.
- A message becomes the receiver's next turn: it starts at once if they are idle, or waits until their current turn ends. Nobody blocks waiting for a reply, so send what you need and end your turn; replies arrive as a new turn starting "Message from <name> (crew %s):".
- Messages from "sam" are from the human. Your final text in a turn is not seen by other members: send anything they need.
- Make each message self-contained, at most %d characters. At most %d messages to one member per 10 minutes: batch them.
- The crew has %d turns in total across all members; it stops itself when they are used.
`, c.Name, MaxSendText, rateMax, c.MaxTurns)
	return b.String()
}

// writeMCPConfig writes the member's MCP config, pointing the harness server at this binary, and
// returns its path.
func (m *Manager) writeMCPConfig(session string) (string, error) {
	if m.opts.Exe == "" || m.opts.DataDir == "" {
		return "", errors.New("crew MCP server is not configured")
	}
	dir := filepath.Join(m.opts.DataDir, "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	args := []string{"mcp", "--session", session}
	if m.opts.Socket != "" {
		args = append(args, "--socket", m.opts.Socket)
	}
	b, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"harness": map[string]any{"type": "stdio", "command": m.opts.Exe, "args": args},
		},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, session+".json")
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(b)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close(), os.Rename(tmp.Name(), path)); err != nil {
		return "", err
	}
	return path, nil
}
