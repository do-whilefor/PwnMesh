package server

import (
	"net/http"
	"net/url"
	"strconv"

	b "pwnmesh/internal/board"
)

func (s *Server) registerUIRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/executions", s.readOnly(s.projectExecutions))
	m.HandleFunc("GET /projects/{pid}/executions/changes", s.readOnly(s.projectExecutionChanges))
	m.HandleFunc("GET /projects/{pid}/identity", s.readOnly(s.projectIdentity))
}

func (s *Server) projectExecutionChanges(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	query := r.URL.Query()
	limit := int64(20)
	generation, after, cursor, through := int64(0), int64(0), int64(0), int64(0)
	if !query.Has("generation") {
		return 0, nil, b.Err(422, "execution changes require a project generation")
	}
	if err := parseExecutionPageQuery(query, "change", map[string]*int64{"generation": &generation, "after": &after, "cursor": &cursor, "through": &through, "limit": &limit}); err != nil {
		return 0, nil, err
	}
	page, err := t.ProjectExecutionChanges(r.PathValue("pid"), generation, after, cursor, through, int(limit))
	return http.StatusOK, page, err
}

func (s *Server) projectExecutions(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	query := r.URL.Query()
	limit := int64(20)
	cursor, through := int64(0), int64(0)
	if err := parseExecutionPageQuery(query, "page", map[string]*int64{"limit": &limit, "cursor": &cursor, "through": &through}); err != nil {
		return 0, nil, err
	}
	page, err := t.ProjectExecutionPage(r.PathValue("pid"), cursor, through, int(limit))
	return http.StatusOK, page, err
}

func parseExecutionPageQuery(query url.Values, view string, fields map[string]*int64) error {
	for key, target := range fields {
		if !query.Has(key) {
			continue
		}
		value, err := strconv.ParseInt(query.Get(key), 10, 64)
		if err != nil || value < 0 || (key == "limit" && (value == 0 || value > 100)) {
			return b.Err(422, "invalid execution "+view+" "+key)
		}
		*target = value
	}
	return nil
}

func (s *Server) projectIdentity(t *b.Tx, _ *request, r *http.Request) (int, any, error) {
	identity, err := t.ProjectIdentity(r.PathValue("pid"))
	return http.StatusOK, identity, err
}
