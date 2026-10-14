//go:build linux

package integration

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

const graphBridgeDecideTools = "graph_action,read_evidence,read_graph,read_snapshot"
const graphBridgeAssessRootTools = "assess_root," + graphBridgeDecideTools
const graphBridgeExploreTools = "bash,edit,find,finish_step,graph_action,grep,ls,read,read_evidence,read_graph,read_snapshot,read_worker_trace,run_graph,write"

// Keep the scripted Docker model's exact capability checks covered even when
// container acceptance is not enabled in an ordinary offline test run.
func TestGraphBridgeScriptedCapabilitiesMatchRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, kind, want string
		closureProtocol  int
	}{
		{"decide", "reason", graphBridgeDecideTools, 0},
		{"assess-root", "reason", graphBridgeAssessRootTools, 1},
		{"explore", "explore", graphBridgeExploreTools, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := strings.Repeat("a", 64)
			job := worker.Job{Kind: tc.kind, RunID: "capability-test", Workspace: t.TempDir(), GraphRPC: true, ResultContractVersion: 2,
				Graph: board.Graph{Project: board.Project{ID: "capability-test", OrchestrationVersion: 1}}, InputSnapshot: &board.InputSnapshot{StateVersion: version}}
			if tc.kind == "reason" {
				job.Decision = &board.DecisionContext{Version: 2, StateVersion: version, ClosureProtocol: tc.closureProtocol}
			} else {
				job.Intent = &board.Intent{ID: "step"}
			}
			options := worker.Options{RunDir: filepath.Join(job.Workspace, ".pwnmesh", "runs", job.RunID)}
			if err := worker.ConfigureRuntimeTools(job, &options); err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(options.Tools))
			for _, tool := range options.Tools {
				names = append(names, tool.Name)
			}
			sort.Strings(names)
			if got := strings.Join(names, ","); got != tc.want {
				t.Fatalf("scripted %s capabilities differ from runtime: got %s, want %s", tc.kind, got, tc.want)
			}
		})
	}
}
