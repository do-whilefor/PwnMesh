package board

import "pwnmesh/internal/board/internal/sqlstore"

// RevokeRuns invalidates execution identities while retaining worker and
// heartbeat metadata on concluded intents. A resumed project must reject
// delayed writes from revoked workers.
func (t *Tx) RevokeRuns(project string) error {
	return sqlstore.RevokeProjectRuns(t.transactionQueries, project)
}

func (t *Tx) RunRevoked(project, worker string) (bool, error) {
	return sqlstore.RunRevoked(t.transactionQueries, project, worker)
}

// ExpireProject limits lease housekeeping to the project being scheduled or
// mutated. Global mutations pass an empty project and expire all leases,
// including those whose workers have crashed. Observational reads never expire.
func (t *Tx) ExpireProject(project string) error {
	s, err := t.Settings()
	if err != nil {
		return err
	}
	return sqlstore.ExpireLeases(t.transactionQueries, project, t.Now, 1, s.IntentTimeout, s.ReasonTimeout)
}
