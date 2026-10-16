package board

import "pwnmesh/internal/board/internal/sqlstore"

// ProjectSummaries returns list metadata without loading project graphs.
func (t *Tx) ProjectSummaries() ([]Summary, error) {
	return sqlstore.ProjectSummaries(t.transactionQueries)
}

// ObservedProjectSummaries hides expired leases while retaining their stored
// values. Scheduling and mutations remain responsible for actual housekeeping.
func (t *Tx) ObservedProjectSummaries() ([]Summary, error) {
	settings, err := t.Settings()
	if err != nil {
		return nil, err
	}
	return sqlstore.ObservedProjectSummaries(t.transactionQueries, t.Now, 1, settings.IntentTimeout, settings.ReasonTimeout)
}
