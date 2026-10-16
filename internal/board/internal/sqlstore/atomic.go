package sqlstore

import "errors"

// Atomic isolates one composed write inside the caller's transaction. Nested
// calls use SQLite's savepoint stack. Cleanup errors must propagate to Store.Do;
// callers may handle an operation error only after rollback succeeds.
func Atomic(q Queries, write func() error) (err error) {
	if _, err = q.Exec("SAVEPOINT pwnmesh_operation"); err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			// A panic or failed release must not retain the operation's writes.
			_, rollbackErr := q.Exec("ROLLBACK TO pwnmesh_operation")
			_, releaseErr := q.Exec("RELEASE pwnmesh_operation")
			err = errors.Join(err, rollbackErr, releaseErr)
		}
	}()
	if err = write(); err != nil {
		return err
	}
	_, err = q.Exec("RELEASE pwnmesh_operation")
	released = err == nil
	return err
}
