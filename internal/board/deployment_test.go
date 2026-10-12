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
