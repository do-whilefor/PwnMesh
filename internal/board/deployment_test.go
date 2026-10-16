package board

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallationIdentitySurvivesReopenAndSeparatesDatabases(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var id string
		if err = store.Do(context.Background(), func(tx *Tx) error {
			var err error
			id, err = tx.InstallationID()
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 {
			t.Fatalf("invalid installation identity %q", id)
		}
		return id
	}
	first, second := filepath.Join(t.TempDir(), "first.db"), filepath.Join(t.TempDir(), "second.db")
	id := read(first)
	if read(first) != id {
		t.Fatal("installation changed on reopen")
	}
	if read(second) == id {
		t.Fatal("separate databases share container ownership")
	}
}

func TestDispatcherLeaseFencesCompetingAndExpiredOwners(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	var installation string
	if err := store.Do(context.Background(), func(tx *Tx) error {
		var err error
		installation, err = tx.InstallationID()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lease := func(namespace, holder, identity, op string, success bool) {
		t.Helper()
		err := store.Do(context.Background(), func(tx *Tx) error { return tx.DispatcherLease(namespace, holder, identity, op) })
		if (err == nil) != success {
			t.Fatalf("%s/%s %s: %v", namespace, holder, op, err)
		}
	}
	lease("test", a, installation, "claim", true)
	lease("test", b, installation, "claim", false)
	lease("test", b, installation, "heartbeat", false)
	lease("test", a, strings.Repeat("c", 32), "heartbeat", false)
	lease("other", b, installation, "claim", false)
	lease("other", a, installation, "claim", false)
	now = now.Add(DispatcherLeaseSeconds * time.Second)
	lease("other", b, installation, "claim", false)
	lease("test", a, installation, "heartbeat", false)
	lease("test", b, installation, "claim", true)
	lease("test", a, installation, "release", true)
	lease("test", b, installation, "heartbeat", true)
	lease("test", a, installation, "claim", false)
	lease("test", b, installation, "release", true)
	lease("other", b, installation, "claim", false)
	lease("test", a, installation, "claim", true)
}

func TestDispatcherLeaseValidationDoesNotChangeOwnership(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Do(context.Background(), func(tx *Tx) error {
		tx.Now = "2026-01-01T00:00:00Z"
		installation, err := tx.InstallationID()
		if err != nil {
			return err
		}
		holder := strings.Repeat("a", 32)
		if err := tx.DispatcherLease("test", holder, installation, "claim"); err != nil {
			return err
		}
		tx.Now = "2026-01-01T00:00:10Z"
		for _, invalid := range []struct {
			namespace, holder, identity, op string
			status                          int
		}{
			{"", holder, installation, "claim", 422},
			{"UPPER", holder, installation, "claim", 422},
			{strings.Repeat("a", 129), holder, installation, "claim", 422},
			{"test", "invalid", installation, "claim", 422},
			{"test", strings.Repeat("a", 30), installation, "claim", 422},
			{"test", holder, "different", "release", 409},
			{"test", holder, installation, "invalid", 422},
		} {
			err := tx.DispatcherLease(invalid.namespace, invalid.holder, invalid.identity, invalid.op)
			requireAPIStatus(t, err, invalid.status)
		}
		var count int
		if err := tx.transactionQueries.QueryRow("SELECT COUNT(*) FROM pwnmesh_dispatchers").Scan(&count); err != nil || count != 1 {
			t.Fatalf("rejected operation created a namespace: %d, %v", count, err)
		}
		var current, expires string
		if err := tx.transactionQueries.QueryRow("SELECT holder,expires_at FROM pwnmesh_dispatchers WHERE namespace='test'").Scan(&current, &expires); err != nil || current != holder || expires != "2026-01-01T00:00:30Z" {
			t.Fatalf("rejected operation changed the owner or expiry: %q, %q, %v", current, expires, err)
		}
		return nil // Rejections must be safe even when the caller handles them.
	}); err != nil {
		t.Fatal(err)
	}
}
