package sqlstore

import (
	"context"
	"errors"
	"testing"
)

func TestDispatcherStorageRetainsBindingAndCallerTransaction(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	const namespace, holder = "test", "owner"
	original := DispatcherLease{Holder: holder, ExpiresAt: "2026-01-01T00:00:30Z"}
	var identity string
	if err := store.Do(ctx, func(q Queries) error {
		var err error
		identity, err = InstallationID(q)
		if err != nil {
			return err
		}
		lease, err := LoadDispatcherLease(q, namespace)
		if err != nil || lease != (DispatcherLease{}) {
			t.Fatalf("missing lease = %+v, %v", lease, err)
		}
		return SaveDispatcherLease(q, namespace, original.Holder, original.ExpiresAt)
	}); err != nil {
		t.Fatal(err)
	}
	check := func(want DispatcherLease) {
		t.Helper()
		if err := store.View(ctx, func(q Queries) error {
			lease, err := LoadDispatcherLease(q, namespace)
			if err != nil || lease != want {
				t.Fatalf("lease = %+v, want %+v: %v", lease, want, err)
			}
			for _, candidate := range []string{namespace, "other"} {
				bound, err := DispatcherNamespaceBoundElsewhere(q, candidate)
				if err != nil || bound != (candidate != namespace) {
					t.Fatalf("binding for %s = %v, %v", candidate, bound, err)
				}
			}
			id, err := InstallationID(q)
			if err != nil || id != identity {
				t.Fatalf("dispatcher write changed installation: %q, %v", id, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Do(ctx, func(q Queries) error {
		if err := ReleaseDispatcherLease(q, namespace, "different", "expired"); err != nil {
			return err
		}
		return ReleaseDispatcherLease(q, "other", holder, "expired")
	}); err != nil {
		t.Fatal(err)
	}
	check(original)
	rejected := errors.New("outer operation rejected")
	if err := store.Do(ctx, func(q Queries) error {
		if err := SaveDispatcherLease(q, namespace, "replacement", "replacement-expiry"); err != nil {
			return err
		}
		if err := ReleaseDispatcherLease(q, namespace, "replacement", "expired"); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("outer rejection = %v", err)
	}
	check(original)
	if err := store.Do(ctx, func(q Queries) error {
		return ReleaseDispatcherLease(q, namespace, holder, "2026-01-01T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	check(DispatcherLease{Holder: holder, ExpiresAt: "2026-01-01T00:00:00Z"})
}
