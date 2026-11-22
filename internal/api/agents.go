package api

import (
	"net/http"

	"github.com/hamshoes/harness/internal/agents"
	"github.com/hamshoes/harness/internal/sessions"
)

// agentList lists the agent definitions for ?dir= (the user's, then the project's overriding by name).
// Without dir, or for a dir that doesn't exist yet, only the user's.
func (s *Server) agentList(w http.ResponseWriter, r *http.Request) {
	dir := ""
	if q := r.URL.Query().Get("dir"); q != "" {
		d, exists, err := sessions.ExpandDir(q)
		if err != nil {
			s.fail(w, err)
			return
		}
		if exists {
			dir = d
		}
	}
	xs, err := agents.Load(dir, s.Log)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, xs)
}
