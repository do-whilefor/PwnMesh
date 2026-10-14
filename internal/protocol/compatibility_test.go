package protocol_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

// These bytes were captured from Worker before moving its wire definitions.
// Exact field order matters: persisted session identity hashes typed Job JSON.
func TestWireEncodingPreservesExistingExecutions(t *testing.T) {
	for name, fixture := range wireFixtures() {
		t.Run(name, func(t *testing.T) {
			expected, err := os.ReadFile(filepath.Join("testdata", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			expected = bytes.TrimSuffix(expected, []byte("\n"))
			actual, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, expected) {
				t.Fatalf("wire encoding changed\ngot: %s\nwant: %s", actual, expected)
			}
			restored := reflect.New(reflect.TypeOf(fixture))
			if err := json.Unmarshal(expected, restored.Interface()); err != nil {
				t.Fatal(err)
			}
			replay, err := json.Marshal(restored.Elem().Interface())
			if err != nil || !bytes.Equal(replay, expected) {
				t.Fatalf("retained wire data changed on replay: %s, %v", replay, err)
			}
		})
	}
}

func TestTraceBindingUsesTypedJobIdentity(t *testing.T) {
	job := wireFixtures()["job"].(protocol.Job)
	job.Workspace = t.TempDir()
	typed, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(typed, &fields); err != nil {
		t.Fatal(err)
	}
	registered, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(registered, typed) {
		t.Fatal("fixture did not reorder the registered job")
	}
	run := board.TraceRun{ProjectID: job.Graph.Project.ID, Generation: job.Graph.Project.Generation, RunID: job.RunID, StepID: job.Intent.ID, Workspace: job.Workspace, RegisteredJob: registered}
	bound, err := protocol.BindTraceRun(run)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(typed)
	if bound.JobSHA256 != hex.EncodeToString(digest[:]) || bound.RegisteredJob != nil {
		t.Fatalf("trace lost typed job identity or retained private job: %+v", bound)
	}
	run.StepID = "foreign"
	if _, err := protocol.BindTraceRun(run); err == nil {
		t.Fatal("trace accepted a different execution identity")
	}
}

func TestTransportConsumersDoNotImportWorker(t *testing.T) {
	for _, name := range []string{"protocol", "dispatcher", "docker", "server"} {
		directory := filepath.Join("..", name)
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, dependency := range file.Imports {
				imported, err := strconv.Unquote(dependency.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if imported == "pwnmesh/internal/worker" {
					t.Errorf("%s imports Worker instead of the shared protocol", path)
				}
			}
		}
	}
}
