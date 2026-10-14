package board_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/board"
)

func TestTransactionCallbacksHideSQLLifecycleMethods(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	var before board.Settings
	check := func(tx *board.Tx) error {
		for _, method := range []string{"Commit", "Rollback", "Stmt", "StmtContext", "Prepare", "PrepareContext"} {
			if _, exists := reflect.TypeOf(tx).MethodByName(method); exists {
				t.Errorf("callback exposes transaction ownership method %s", method)
			}
		}
		for _, field := range reflect.VisibleFields(reflect.TypeOf(*tx)) {
			if field.IsExported() && field.Type.String() == "*sql.Tx" {
				t.Errorf("callback exposes raw transaction through %s", field.Name)
			}
		}
		var err error
		before, err = tx.Settings()
		return err
	}
	if err := store.View(ctx, check); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("later operation failed")
	if err := store.Do(ctx, func(tx *board.Tx) error {
		if err := check(tx); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE settings SET intent_timeout=intent_timeout+1 WHERE rowid=1"); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	if err := store.View(ctx, func(tx *board.Tx) error {
		after, err := tx.Settings()
		if err == nil && after != before {
			t.Fatalf("failed operation committed settings: got %+v, want %+v", after, before)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
