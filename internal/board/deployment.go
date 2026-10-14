package board

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const DispatcherLeaseSeconds = 30

// InstallationID belongs to the database, not its URL or a Compose project name.
// A restarted dispatcher must recover the same owned containers; another database
// must never adopt or clean them up just because its project IDs happen to match.
func (t *Tx) InstallationID() (string, error) {
	var id string
	err := t.QueryRow("SELECT identity FROM pwnmesh_installation WHERE singleton=1").Scan(&id)
	return id, err
}

// The dispatcher owns all process cleanup in its namespace. A lease is taken
// before startup cleanup; a second dispatcher must not stop the first one's work.
// Keep the namespace binding after release: changing it would strand existing
// workspaces and prevent startup cleanup from stopping their old executions.
func (t *Tx) DispatcherLease(namespace, holder, installation, op string) error {
	if namespace == "" || len(namespace) > 128 || strings.Trim(namespace, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return Err(422, "invalid dispatcher namespace")
	}
	raw, err := hex.DecodeString(holder)
	if err != nil || len(raw) != 16 {
		return Err(422, "invalid dispatcher holder")
	}
	id, err := t.InstallationID()
	if err != nil {
		return err
	}
	if id != installation {
		return Err(409, "server installation changed")
	}
	if op == "release" {
		_, err := t.Exec("UPDATE pwnmesh_dispatchers SET expires_at=? WHERE namespace=? AND holder=?", t.Now, namespace, holder)
		return err
	}
	var current, expires string
	err = t.QueryRow("SELECT holder,expires_at FROM pwnmesh_dispatchers WHERE namespace=?", namespace).Scan(&current, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if op == "heartbeat" {
		if current != holder || expires <= t.Now {
			return Err(409, "dispatcher lease lost")
		}
	} else if op != "claim" {
		return Err(422, "invalid dispatcher lease operation")
	} else if current != holder && expires > t.Now {
		return Err(409, "another dispatcher owns this installation namespace")
	}
	if op == "claim" {
		var boundElsewhere bool
		if err := t.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_dispatchers WHERE namespace<>?)", namespace).Scan(&boundElsewhere); err != nil {
			return err
		}
		if boundElsewhere {
			return Err(409, "installation is bound to a different container namespace; migrate existing workspaces before changing it")
		}
	}
	now, err := time.Parse(time.RFC3339, t.Now)
	if err != nil {
		return err
	}
	expires = now.Add(DispatcherLeaseSeconds * time.Second).Format(time.RFC3339)
	_, err = t.Exec(`INSERT INTO pwnmesh_dispatchers(namespace,holder,expires_at) VALUES(?,?,?)
ON CONFLICT(namespace) DO UPDATE SET holder=excluded.holder,expires_at=excluded.expires_at`, namespace, holder, expires)
	return err
}
