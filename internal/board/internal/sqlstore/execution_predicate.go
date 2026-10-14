package sqlstore

// PendingExecutionSQL is shared by the pending index and its runtime queries.
const PendingExecutionSQL = `status IN ('prepared','running','retryable','result_pending')`
