package api

import (
	"net/http"

	"github.com/hamshoes/harness/internal/sessions"
)

func (s *Server) crewCreate(w http.ResponseWriter, r *http.Request) {
	var spec sessions.CrewSpec
	if !decodeBody(w, r, &spec) {
		return
	}
	if spec.Name == "" || spec.Dir == "" || spec.Goal == "" || spec.Lead == "" || len(spec.Members) == 0 {
		writeError(w, http.StatusBadRequest, "name, dir, goal, lead and members are required")
		return
	}
	c, err := s.Sessions.CreateCrew(spec)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) crewList(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Sessions.Crews()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cs)
}

func (s *Server) crewGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.Sessions.Crew(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) crewStop(w http.ResponseWriter, r *http.Request) {
	c, err := s.Sessions.StopCrew(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) crewMessages(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Sessions.CrewMessages(r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ms)
}

// crewSend is the crew MCP server's send tool: {from, to, text} between members of the crew.
// {result: "delivered" | "queued"}.
func (s *Server) crewSend(w http.ResponseWriter, r *http.Request) {
	var body struct{ From, To, Text string }
	if !decodeBody(w, r, &body) {
		return
	}
	if body.From == "" || body.To == "" {
		writeError(w, http.StatusBadRequest, "from, to and text are required")
		return
	}
	res, err := s.Sessions.Send(r.PathValue("name"), body.From, body.To, body.Text)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": res})
}
