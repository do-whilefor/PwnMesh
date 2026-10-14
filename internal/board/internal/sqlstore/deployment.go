package sqlstore

import (
	"database/sql"
	"errors"
)

type DispatcherLease struct {
	Holder, ExpiresAt string
}

func InstallationID(q Queries) (string, error) {
	var id string
	err := q.QueryRow("SELECT identity FROM pwnmesh_installation WHERE singleton=1").Scan(&id)
	return id, err
}

// An absent lease has no holder or expiry. Board decides whether an operation
// may create it, renew it or replace its owner within the caller's transaction.
func LoadDispatcherLease(q Queries, namespace string) (DispatcherLease, error) {
	var lease DispatcherLease
	err := q.QueryRow("SELECT holder,expires_at FROM pwnmesh_dispatchers WHERE namespace=?", namespace).Scan(&lease.Holder, &lease.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DispatcherLease{}, nil
	}
	return lease, err
}

func DispatcherNamespaceBoundElsewhere(q Queries, namespace string) (bool, error) {
	var bound bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_dispatchers WHERE namespace<>?)", namespace).Scan(&bound)
	return bound, err
}

func SaveDispatcherLease(q Queries, namespace, holder, expires string) error {
	_, err := q.Exec(`INSERT INTO pwnmesh_dispatchers(namespace,holder,expires_at) VALUES(?,?,?)
ON CONFLICT(namespace) DO UPDATE SET holder=excluded.holder,expires_at=excluded.expires_at`, namespace, holder, expires)
	return err
}

// Release expires only this holder and retains the installation's namespace.
func ReleaseDispatcherLease(q Queries, namespace, holder, now string) error {
	_, err := q.Exec("UPDATE pwnmesh_dispatchers SET expires_at=? WHERE namespace=? AND holder=?", now, namespace, holder)
	return err
}
