package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	board "pwnmesh/internal/board/model"
	"pwnmesh/internal/protocol"
)

func TestControlOutcomeDiagnosticsDistinguishStaleFromCommittedRecovery(t *testing.T) {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, kind, outcome string
		err                 error
		confirmed, stale    bool
	}{
		{"cas-rejection", "reason", "failed", &ProtocolError{Status: 409, Code: board.CodeStateChanged, Detail: `{"detail":"state_changed: private-input-secret"}`}, false, true},
		{"heartbeat-confirmed", "curate", "failed", errors.New("private-input-secret"), true, true},
		{"receipt-won", "reason", "success", &ProtocolError{Status: 409, Code: board.CodeStateChanged, Detail: `{"detail":"state_changed: private-input-secret"}`}, false, false},
		{"shutdown", "curate", "interrupted", context.Canceled, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &task{Job: protocol.Job{RunID: "run", Kind: tc.kind, Graph: board.Graph{Project: board.Project{ID: "p", Generation: 4}}, InputSnapshot: &board.InputSnapshot{Revision: 12}}, Execution: board.Execution{Resumes: 2}}
			if tc.confirmed {
				task.staleInputAt = started.Add(time.Second)
			}
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			logger.LogAttrs(context.Background(), slog.LevelInfo, "control task finished", controlOutcomeAttrs(task, tc.outcome, tc.err, started, started.Add(1500*time.Millisecond))...)
			var event map[string]any
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["stale_input"] != tc.stale || event["outcome"] != tc.outcome || event["attempt_wall_ms"] != float64(1500) || event["input_revision"] != float64(12) || event["resumes"] != float64(2) || event["generation"] != float64(4) {
				t.Fatalf("incomplete or misclassified control diagnostic: %s", output.Bytes())
			}
			if strings.Contains(output.String(), "private-input-secret") || event["error"] != nil {
				t.Fatal("diagnostics leaked error contents")
			}
		})
	}
	if attrs := controlOutcomeAttrs(&task{Job: protocol.Job{Kind: "explore"}}, "failed", nil, started, started); attrs != nil {
		t.Fatal("Execute was counted as a control attempt")
	}
}
