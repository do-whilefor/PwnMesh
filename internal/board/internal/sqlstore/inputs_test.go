package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"pwnmesh/internal/board/model"
)

type inputMetadataQueries struct {
	Queries
	t *testing.T
}

func (q inputMetadataQueries) check(query string) {
	q.t.Helper()
	columns, _, _ := strings.Cut(strings.ToLower(query), " from ")
	if strings.Contains(columns, "data") || strings.Contains(columns, "*") {
		q.t.Fatalf("metadata read selected file contents: %s", query)
	}
}

func (q inputMetadataQueries) Query(query string, args ...any) (*sql.Rows, error) {
	q.check(query)
	return q.Queries.Query(query, args...)
}

func (q inputMetadataQueries) QueryRow(query string, args ...any) *sql.Row {
	q.check(query)
	return q.Queries.QueryRow(query, args...)
}

func TestInputStoragePreservesMetadataOrderIsolationAndBinaryBytes(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	files := []model.InputFile{
		{ID: "c", Name: "capture.bin", Size: 6, SHA256: "original digest", CreatedAt: "later"},
		{ID: "b", Name: "empty.bin", SHA256: "empty digest", CreatedAt: "earlier"},
		{ID: "a", Name: "first.bin", Size: 2, SHA256: "first digest", CreatedAt: "earlier"},
	}
	contents := [][]byte{{'P', 'K', 0, 255, '\r', '\n'}, {}, {128, 0}}
	if err := s.Do(ctx, func(q Queries) error {
		for _, project := range []string{"p", "other"} {
			if err := SaveGraph(q, model.Graph{Project: model.Project{ID: project, Title: "fixture", Status: "active", CreatedAt: "now"}}); err != nil {
				return err
			}
		}
		for i, file := range files {
			file.Path = "/caller-projection-is-not-stored"
			if err := InsertInputFile(q, "p", file, contents[i]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(q Queries) error {
		metadata := inputMetadataQueries{Queries: q, t: t}
		got, err := InputFiles(metadata, "p")
		if err != nil || !reflect.DeepEqual(got, []model.InputFile{files[2], files[1], files[0]}) {
			t.Fatalf("metadata ordering or path ownership changed: %+v, %v", got, err)
		}
		for i, file := range files {
			got, err := InputFile(metadata, "p", file.ID)
			if err != nil || got != file {
				t.Fatalf("metadata changed: %+v, %v", got, err)
			}
			data, err := InputData(q, "p", file.ID)
			if err != nil || !bytes.Equal(data, contents[i]) {
				t.Fatalf("binary contents changed: %v, %v", data, err)
			}
			if _, err := InputFile(metadata, "other", file.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cross-project metadata read: %v", err)
			}
			if _, err := InputData(q, "other", file.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cross-project content read: %v", err)
			}
		}
		for _, project := range []string{"other", "missing"} {
			got, err := InputFiles(metadata, project)
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("empty metadata list changed: %+v, %v", got, err)
			}
			count, size, err := InputUsage(q, project)
			if err != nil || count != 0 || size != 0 {
				t.Fatalf("empty usage changed: %d, %d, %v", count, size, err)
			}
		}
		count, size, err := InputUsage(q, "p")
		if err != nil || count != len(files) || size != 8 {
			t.Fatalf("usage changed: %d, %d, %v", count, size, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInputStorageReceiptsAndFilesStayInCallerTransaction(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Do(ctx, func(q Queries) error {
		return SaveGraph(q, model.Graph{Project: model.Project{ID: "p", Title: "fixture", Status: "active", CreatedAt: "now"}})
	}); err != nil {
		t.Fatal(err)
	}
	file := model.InputFile{ID: "file", Name: "capture.bin", Size: 2, SHA256: "digest", CreatedAt: "now"}
	rejected := errors.New("outer operation rejected")
	responses := []string{"null", "[]", "[ {\"name\":\"\\u2603.bin\",\"size\":2} ]\n"}
	write := func(q Queries) error {
		if err := InsertInputFile(q, "p", file, []byte{0, 255}); err != nil {
			return err
		}
		for _, raw := range responses {
			if err := InsertInputBatchReceipt(q, "p", raw, "request digest", raw); err != nil {
				return err
			}
			digest, response, err := InputBatchReceipt(q, "p", raw)
			if err != nil || digest != "request digest" || response != raw {
				t.Fatalf("receipt bytes changed: %q, %q, %v", digest, response, err)
			}
		}
		return nil
	}
	if err := s.Do(ctx, func(q Queries) error {
		if err := write(q); err != nil {
			return err
		}
		return rejected
	}); !errors.Is(err, rejected) {
		t.Fatalf("outer transaction result = %v", err)
	}
	if err := s.Do(ctx, func(q Queries) error {
		if _, err := InputFile(q, "p", file.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("file outlived rejected transaction: %v", err)
		}
		for _, raw := range responses {
			if _, _, err := InputBatchReceipt(q, "p", raw); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("receipt outlived rejected transaction: %v", err)
			}
		}
		return write(q)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(q Queries) error {
		for _, raw := range responses {
			digest, response, err := InputBatchReceipt(q, "p", raw)
			if err != nil || digest != "request digest" || response != raw {
				t.Fatalf("committed receipt changed: %q, %q, %v", digest, response, err)
			}
			if _, _, err := InputBatchReceipt(q, "other", raw); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cross-project receipt read: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
