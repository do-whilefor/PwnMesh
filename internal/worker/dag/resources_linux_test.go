//go:build linux

package dag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/workergraph"
)

func TestDeclaredResourcePathsRequireOrderingBeforeEffects(t *testing.T) {
	for _, pair := range [][2]string{
		{"/workspace/report/../result", "/workspace/result"},
		{"/workspace/results", "/workspace/results/report.json"},
		{"./results", "path:results/report.json"},
		{"database", "database"},
	} {
		t.Run(pair[0]+"_"+pair[1], func(t *testing.T) {
			scope, dir := graphScope(t), t.TempDir()
			spec := Spec{Key: "conflict", Nodes: []Node{
				{ID: "a", Command: `touch "$PWNMESH_WORKSPACE/unexpected"`, Resources: []string{pair[0]}},
				{ID: "b", Command: "true", Resources: []string{pair[1]}},
			}}
			if _, err := commandGraphCall(t, context.Background(), scope, dir, spec); err == nil || !strings.Contains(err.Error(), "shared resource") {
				t.Fatalf("unordered resource conflict accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(scope.Workspace, "unexpected")); !os.IsNotExist(err) {
				t.Fatal("conflict reached execution")
			}
			spec.Nodes[1].DependsOn = []workergraph.Dependency{{ID: "a"}}
			if err := validateCommandGraph(&spec); err != nil {
				t.Fatalf("explicit ordering rejected: %v", err)
			}
		})
	}
}

func TestSharedResourceWorkspaceAliasesAndStepWriteScope(t *testing.T) {
	scope := scopeFor(graphScope(t), t.TempDir())
	scope.SharedWritesBound = true
	scope.WritePaths = []string{filepath.Join(scope.Workspace, "reports")}
	makeSpec := func(left, right string) Spec {
		return Spec{Key: "scoped", Nodes: []Node{
			{ID: "a", Command: "printf left", Resources: []string{left}},
			{ID: "b", Command: "printf right", Resources: []string{right}},
		}}
	}
	conflict := makeSpec("./reports", filepath.Join(scope.Workspace, "reports", "result.json"))
	if err := validateSharedResources(scope, conflict); err == nil {
		t.Fatal("workspace-relative alias escaped conflict check")
	}
	outside := makeSpec("./unassigned/result.json", "path:reports/right.json")
	if err := validateSharedResources(scope, outside); err == nil || !strings.Contains(err.Error(), "write_paths") {
		t.Fatalf("unassigned shared output accepted: %v", err)
	}
	independent := makeSpec("./reports/left.json", "path:reports/right.json")
	if _, err := commandGraphCall(t, context.Background(), scope, scope.RunDir, independent); err != nil {
		t.Fatalf("independent assigned outputs rejected: %v", err)
	}
	// A private node directory and a separate external resource do not enlarge
	// the shared deliverable scope or claim to restrict legitimate shell work.
	private := makeSpec(filepath.Join(scope.RunDir, "private"), "/tmp/external-resource")
	if err := validateSharedResources(scope, private); err != nil {
		t.Fatalf("private/external resources rejected: %v", err)
	}
	if resourcesConflict([]string{"/workspace/report"}, []string{"/workspace/reports"}, "") {
		t.Fatal("sibling path prefixes conflict")
	}
}
