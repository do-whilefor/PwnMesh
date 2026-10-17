//go:build linux

package dag

import (
	"context"
	"os"
	"path/filepath"
)

// VisitAgentSessions enumerates original started Agent nodes under the current
// execution identity. Imported receipts and unstarted nodes add no model usage.
// It reads only retained definitions/checkpoints, without verifying output
// artifacts: a failed or subsequently changed artifact does not erase usage.
func VisitAgentSessions(ctx context.Context, scope Scope, consume func(AgentNode) error) error {
	root, err := existingCommandGraphDirectory(scope.RunDir, "graph-tools")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !commandGraphID.MatchString(entry.Name()) {
			continue
		}
		dir, err := existingCommandGraphDirectory(root, entry.Name())
		if err != nil {
			return err
		}
		checkpoint, specs, err := readCommandSnapshot(ctx, scope, entry.Name(), dir)
		if err != nil {
			return err
		}
		for _, node := range checkpoint.Nodes {
			spec := specs[node.ID]
			if spec.Kind != "agent" || node.InputSHA256 == "" || node.StartedAt.IsZero() {
				continue
			}
			nodeDir, err := existingCommandGraphDirectory(dir, "nodes", node.ID)
			if err != nil {
				return err
			}
			if err := consume(AgentNode{Key: entry.Name(), Dir: filepath.Clean(nodeDir), Spec: spec, InputSHA256: node.InputSHA256}); err != nil {
				return err
			}
		}
	}
	return nil
}
