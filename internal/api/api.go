// Package api serves the session API: JSON in and out, errors as {"error":"…"}, event logs as NDJSON.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/hamshoes/harness/internal/sessions"
	"github.com/hamshoes/harness/internal/store"
)

const maxBody = 4 << 20

// Server holds what the handlers need.
type Server struct {
	Sessions      *sessions.Manager
	Store         *store.Store
	Version       string
	ClaudeVersion string
	Spool         string
	Log           *slog.Logger
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", s.create)
	mux.HandleFunc("GET /sessions", s.list)
	mux.HandleFunc("GET /sessions/{name}", s.get)
	mux.HandleFunc("GET /sessions/{name}/events", s.events)
	mux.HandleFunc("POST /sessions/{name}/messages", s.message)
	mux.HandleFunc("POST /sessions/{name}/stop", s.stop)
	mux.HandleFunc("GET /blobs/{hash}", s.blob)
	mux.HandleFunc("GET /approvals", s.approvals)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such route")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail maps a manager or store error to a status code.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessions.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "session name already used")
	case errors.Is(err, sessions.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, sessions.ErrClosing):
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
	default:
		s.Log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Dir, Prompt string }
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Name == "" || body.Dir == "" || body.Prompt == "" {
		writeError(w, http.StatusBadRequest, "name, dir and prompt are required")
		return
	}
	x, err := s.Sessions.Create(body.Name, body.Dir, body.Prompt)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, x)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	xs, err := s.Sessions.List()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, xs)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	x, err := s.Sessions.Get(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	var body struct{ Text string }
	if !decodeBody(w, r, &body) {
		return
	}
	x, err := s.Sessions.Message(r.PathValue("name"), body.Text)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, x)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	x, err := s.Sessions.Stop(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	follow := false
	if v := q.Get("follow"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "follow must be 1 or 0")
			return
		}
		follow = b
	}
	if _, err := s.Sessions.Get(name); err != nil {
		s.fail(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if follow {
		rc.Flush() // the client sees the headers before the first event
	}
	enc := json.NewEncoder(w)
	err := s.Sessions.Follow(r.Context(), name, after, follow, func(ev store.Event) error {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if follow {
			return rc.Flush()
		}
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		s.Log.Warn("event stream ended", "session", name, "err", err)
	}
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	path, err := s.Store.BlobPath(r.PathValue("hash"))
	if err != nil {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	counts, err := s.Sessions.Health()
	if err != nil {
		s.fail(w, err)
		return
	}
	pending, err := s.Store.CountPending()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"version":           s.Version,
		"claude_version":    s.ClaudeVersion,
		"sessions":          counts,
		"pending_approvals": pending,
		"spool":             s.Spool,
	})
}

// approvals lists approvals, newest first, at most 200: ?state=pending (default), all, or one state.
func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	switch state {
	case "":
		state = store.ApprovalPending
	case "all":
		state = ""
	case store.ApprovalPending, store.ApprovalAllow, store.ApprovalDeny, store.ApprovalCancelled, store.ApprovalWithdrawn:
	default:
		writeError(w, http.StatusBadRequest, "state must be pending, all, allow, deny, cancelled or withdrawn")
		return
	}
	xs, err := s.Store.Approvals(state, 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, xs)
}
