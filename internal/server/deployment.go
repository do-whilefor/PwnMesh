package server

import (
	"net/http"

	b "pwnmesh/internal/board"
)

func (s *Server) registerDeploymentRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /deployment", func(w http.ResponseWriter, r *http.Request) {
		var id string
		err := s.Store.Do(r.Context(), func(t *b.Tx) error {
			var err error
			id, err = t.InstallationID()
			return err
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		modelJSON(w, map[string]string{"installation_id": id})
	})
	m.HandleFunc("POST /dispatchers/{namespace}/{op}", s.wrap(func(t *b.Tx, q *request, r *http.Request) (int, any, error) {
		if r.Header.Get("X-PwnMesh-Run") != "" {
			return 0, nil, b.Err(403, "Dispatcher ownership is a management operation")
		}
		var input struct {
			Holder       string `json:"holder"`
			Installation string `json:"installation_id"`
		}
		if err := decodeStrictFields(q, &input); err != nil {
			return 0, nil, b.Err(422, "invalid dispatcher lease")
		}
		return http.StatusNoContent, nil, t.DispatcherLease(r.PathValue("namespace"), input.Holder, input.Installation, r.PathValue("op"))
	}))
}
