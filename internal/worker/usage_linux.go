//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/protocol"
	"pwnmesh/internal/worker/dag"
	eventlog "pwnmesh/internal/worker/internal/journal"
)

// Rebuild from original sessions, not graph tool results or previous totals.
// Recovery, repeated delivery and imported receipts therefore cannot add the
// same work twice. A damaged observation never changes business acceptance.
func withChildUsage(ctx context.Context, j Job, runDir string, parent DecisionMetrics) DecisionMetrics {
	parent.UsageScope = "run_and_children"
	children := protocol.ChildUsage{}
	err := dag.VisitAgentSessions(ctx, commandGraphScope(j, runDir), func(node dag.AgentNode) error {
		children.Sessions++
		metrics, incomplete, err := childUsage(ctx, j, node)
		if err != nil || incomplete {
			children.Incomplete = true
		}
		if err != nil {
			return nil
		}
		children.ModelCalls += metrics.ModelCalls
		children.UsageCalls += metrics.UsageCalls
		addUsage(&children.Usage, metrics.Usage)
		parent.ModelCalls += metrics.ModelCalls
		parent.SummaryCalls += metrics.SummaryCalls
		parent.CompletedCalls += metrics.CompletedCalls
		parent.FailedCalls += metrics.FailedCalls
		parent.UsageCalls += metrics.UsageCalls
		parent.ModelDurationMS += metrics.ModelDurationMS
		parent.TotalInputBytes += metrics.TotalInputBytes
		if parent.InputBytesBasis == "" {
			parent.InputBytesBasis = metrics.InputBytesBasis
		} else if metrics.InputBytesBasis != "" && parent.InputBytesBasis != metrics.InputBytesBasis {
			parent.InputBytesBasis = "mixed"
		}
		addUsage(&parent.Usage, metrics.Usage)
		return nil
	})
	if err != nil {
		children.Incomplete = true
	}
	if children.Sessions > 0 || children.Incomplete {
		parent.ChildUsage = &children
	}
	parent.UsageStatus = reportedUsageStatus(parent.ModelCalls, parent.UsageCalls)
	if children.Incomplete {
		parent.UsageStatus = "partial"
	}
	return parent
}

func childUsage(ctx context.Context, j Job, node dag.AgentNode) (DecisionMetrics, bool, error) {
	var state graphAgentSession
	raw, err := readEvidenceFile(ctx, filepath.Join(node.Dir, "session.json"), 32<<20)
	if err != nil {
		return DecisionMetrics{}, false, err
	}
	if json.Unmarshal(raw, &state) != nil || state.RunID != j.RunID || state.GraphKey != node.Key || state.NodeID != node.Spec.ID || state.GraphInputSHA256 != node.InputSHA256 {
		return DecisionMetrics{}, false, errors.New("child usage identity mismatch")
	}
	metrics := newDecisionMetrics()
	path := filepath.Join(node.Dir, "events.jsonl")
	err = eventlog.VisitCommittedEvents(ctx, path, state.Log, func(event agent.Event) error { metrics.observe(event); return nil })
	if err != nil {
		return DecisionMetrics{}, false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return DecisionMetrics{}, false, err
	}
	return metrics.DecisionMetrics, info.Size() != state.Log.Offset, nil
}
