//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"pwnmesh/internal/board"
)

func finishAssetInput(t *testing.T, path string, assets []board.AssetSpec) json.RawMessage {
	t.Helper()
	var data map[string]json.RawMessage
	if err := json.Unmarshal(finishStepInput(t, path), &data); err != nil {
		t.Fatal(err)
	}
	var fact map[string]json.RawMessage
	if err := json.Unmarshal(data["fact"], &fact); err != nil {
		t.Fatal(err)
	}
	fact["assets"], _ = json.Marshal(assets)
	data["fact"], _ = json.Marshal(fact)
	raw, _ := json.Marshal(data)
	return raw
}

func TestFinishStepRejectsInvalidAssetsBeforeHandoffAndAllowsCorrection(t *testing.T) {
	for name, spec := range map[string]board.AssetSpec{
		"missing method": {Kind: "endpoint", Value: "https://example.test/a"},
		"service path":   {Kind: "service", Value: "https://example.test/a"},
		"userinfo":       {Kind: "endpoint", Value: "https://user@example.test/a", Method: "GET"},
	} {
		t.Run(name, func(t *testing.T) {
			j := graphWrapperJob(t)
			j.ResultContractVersion = 2
			dir := t.TempDir()
			path := filepath.Join(j.Workspace, "result.txt")
			if err := os.WriteFile(path, []byte("observed response\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f := &stepFinish{job: j, runDir: dir}
			if _, err := f.tool().Execute(context.Background(), finishAssetInput(t, path, []board.AssetSpec{spec})); err == nil {
				t.Fatal("invalid assets accepted as a terminal handoff")
			}
			if _, accepted := f.result(); accepted {
				t.Fatal("rejected observation ended Worker")
			}
			for _, name := range []string{"finish-step.json", "evidence"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("invalid assets wrote durable %s: %v", name, err)
				}
			}
			valid := []board.AssetSpec{{Kind: "endpoint", Value: "https://EXAMPLE.test:443/a?b=1&a=2", Method: "GET"}, {Kind: "host", Value: "Example.test."}}
			if _, err := f.tool().Execute(context.Background(), finishAssetInput(t, path, valid)); err != nil {
				t.Fatalf("correction could not finish: %v", err)
			}
			text, accepted := f.result()
			if !accepted {
				t.Fatal("valid correction was not accepted")
			}
			prepared, err := prepareFinalEvidence(context.Background(), j, dir, Result{Status: "success", Text: text}, nil)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseOutput(j, false, prepared.Text)
			if err != nil {
				t.Fatal(err)
			}
			var fact struct {
				Assets []board.AssetSpec `json:"assets"`
			}
			if err := json.Unmarshal(parsed.FactPayload, &fact); err != nil || !reflect.DeepEqual(fact.Assets, valid) {
				t.Fatalf("evidence preparation rewrote asset observation: %+v, %v", fact, err)
			}
		})
	}
}
