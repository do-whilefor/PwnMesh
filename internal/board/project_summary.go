package board

import "pwnmesh/internal/board/internal/sqlstore"

// ProjectSummaries returns list metadata without loading project graphs.
func (t *Tx) ProjectSummaries() ([]Summary, error) {
	return sqlstore.ProjectSummaries(t)
}
