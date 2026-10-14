package model

import (
	"errors"
	"testing"
)

func TestGraphValidationPreservesStatusAndSourceErrorPriority(t *testing.T) {
	graph := Graph{Project: Project{Status: "active"}, Facts: []Fact{{ID: "origin"}, {ID: "goal"}, {ID: "f001"}}}
	if err := graph.RequireActive(); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"stopped", "completed", "terminated", ""} {
		graph.Project.Status = status
		requireError(t, graph.RequireActive(), 403, "Project is "+status)
	}
	for _, test := range []struct {
		name   string
		from   []string
		status int
		detail string
	}{
		{name: "empty remains valid"},
		{name: "original input", from: []string{"origin"}},
		{name: "legacy duplicate sources", from: []string{"f001", "f001"}},
		{name: "unknown fact", from: []string{"missing"}, status: 404, detail: "Fact missing not found"},
		{name: "root goal", from: []string{"goal"}, status: 400, detail: "goal cannot be used in from"},
		{name: "missing wins before goal restriction", from: []string{"goal", "missing"}, status: 404, detail: "Fact missing not found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := graph.ValidateSources(test.from)
			if test.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			requireError(t, err, test.status, test.detail)
		})
	}
}

func TestGraphOpenCountRetainsLegacyConclusionSemantics(t *testing.T) {
	graph := Graph{Intents: []Intent{
		{ID: "open"},
		{ID: "leased", Worker: Ptr("worker")},
		{ID: "abandoned", ConcludedAt: Ptr("now")},
		{ID: "completed", To: Ptr("f001")},
	}}
	if got := graph.OpenCount(); got != 3 {
		t.Fatalf("legacy open count changed: %d", got)
	}
	if Value(nil) != "" || Value(Ptr("worker")) != "worker" {
		t.Fatal("optional graph value semantics changed")
	}
}

func requireError(t *testing.T, err error, status int, detail string) {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Status != status || api.Detail != detail || api.Error() != detail {
		t.Fatalf("got %#v, want status %d and detail %q", err, status, detail)
	}
}
