package dispatcher

import (
	"log/slog"
	"time"

	"pwnmesh/internal/protocol"
)

// Emit one bounded outcome per local control attempt, after receipt recovery.
// Existing decision observations supply model/usage metrics by run ID. Wall time
// includes startup and cleanup, not just model work, and is not a billing estimate.
func controlOutcomeAttrs(t *task, outcome string, err error, started, ended time.Time) []slog.Attr {
	if !protocol.ControlJob(t.Job) {
		return nil
	}
	stale := outcome != "success" && (!t.staleInputAt.IsZero() || decisionStateChanged(err))
	attrs := []slog.Attr{
		slog.String("project", t.Job.Graph.Project.ID), slog.String("run", t.Job.RunID),
		slog.String("task", t.Job.Kind), slog.String("outcome", outcome),
		slog.Int64("generation", t.Job.Graph.Project.Generation), slog.Int("resumes", t.Execution.Resumes),
		slog.Bool("stale_input", stale), slog.Int64("attempt_wall_ms", max(0, ended.Sub(started).Milliseconds())),
	}
	if snapshot := t.Job.InputSnapshot; snapshot != nil {
		attrs = append(attrs, slog.Int64("input_revision", snapshot.Revision))
	} else if state := t.Job.State; state != nil {
		attrs = append(attrs, slog.Int64("input_revision", state.Revision))
	}
	// Neither error bodies nor input text belong in aggregate diagnostics.
	return attrs
}
