//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	board "pwnmesh/internal/board/model"
)

func TestFinishStepPreservesFactDerivationThroughEvidenceAndRecovery(t *testing.T) {
	j := graphWrapperJob(t)
	j.ResultContractVersion = 2
	dir := t.TempDir()
	path := filepath.Join(j.Workspace, "calculated.txt")
	if err := os.WriteFile(path, []byte("calculated from retained input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(finishStepInput(t, path), &data); err != nil {
		t.Fatal(err)
	}
	var fact map[string]json.RawMessage
	if err := json.Unmarshal(data["fact"], &fact); err != nil {
		t.Fatal(err)
	}
	fact["derived_from"] = json.RawMessage(`["input-fact","calibration-fact"]`)
	data["fact"], _ = json.Marshal(fact)
	raw, _ := json.Marshal(data)
	finish := &stepFinish{job: j, runDir: dir}
	if _, err := finish.tool().Execute(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	text, ok := finish.result()
	if !ok {
		t.Fatal("valid derived fact did not reach handoff")
	}
	prepared, err := prepareFinalEvidence(context.Background(), j, dir, Result{Status: "success", Text: text}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseOutput(j, false, prepared.Text)
	if err != nil {
		t.Fatal(err)
	}
	var retained board.FactRecord
	if json.Unmarshal(parsed.FactPayload, &retained) != nil || !reflect.DeepEqual(retained.DerivedFrom, []string{"input-fact", "calibration-fact"}) || len(retained.Evidence) != 1 || retained.Evidence[0].Path == path {
		t.Fatalf("freezing evidence lost derivation: %s", parsed.FactPayload)
	}
	recovered := &stepFinish{job: j, runDir: dir}
	if err = recovered.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := recovered.result(); !ok || got != text {
		t.Fatal("terminal recovery rewrote fact derivation")
	}
}
