//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/artifactcheck"
)

func repairJob(t *testing.T, content string) Job {
	t.Helper()
	j := outcomeJob(t, "explore")
	j.Graph.Project.OrchestrationVersion = 1
	j.GraphRPC, j.ResultContractVersion = true, 2
	target := filepath.Join(j.Workspace, "target.json")
	if err := os.WriteFile(target, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`{"ok":false}`))
	j.Repair = &artifactcheck.Spec{Path: target, SHA256: hex.EncodeToString(sum[:]), Rules: []artifactcheck.Rule{{Kind: "json_equals", Pointer: "/ok", Value: json.RawMessage(`true`)}}}
	return j
}

func repairTestBridge(dir string) *draftTestBridge {
	return &draftTestBridge{dir: dir, handle: func(GraphRequest) (any, error) { return nil, errors.New("fixture: no unrelated graph updates") }}
}

func TestStaleRepairNoopRetainsProofAndUsesNoModel(t *testing.T) {
	j := repairJob(t, `{"ok":true}`)
	dir := t.TempDir()
	p := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("already satisfied repair called model")
		return agent.Message{}, nil
	})
	r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: p})
	if err != nil || r.Status != "success" || r.RepairCheck == nil || r.RepairCheck.Outcome != "noop" || !r.RepairCheck.Satisfied {
		t.Fatalf("%+v %v", r, err)
	}
	refs := finalEvidenceRefs(t, j, r)
	if len(refs) != 2 || refs[0].Path == j.Repair.Path {
		t.Fatalf("missing retained proof: %+v", refs)
	}
	again, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: p})
	if err != nil || again.Text != r.Text {
		t.Fatalf("resume changed result: %+v %v", again, err)
	}
	if err := os.WriteFile(j.Repair.Path, []byte(`{"ok":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	again, err = runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: p})
	if err == nil && again.Status == "success" {
		t.Fatal("reused successful graph despite changed target")
	}
}

func TestStaleRepairChangedBrokenTargetFailsBeforeModel(t *testing.T) {
	j := repairJob(t, `{"ok":false,"new":1}`)
	p := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		t.Fatal("stale repair called model")
		return agent.Message{}, nil
	})
	r, err := runTestWorker(context.Background(), j, Options{RunDir: t.TempDir(), Provider: p})
	if err != nil || r.Status != "failed" || r.Retryable || r.FailureKind != "repair_stale" || r.RepairCheck.Outcome != "stale" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRepairExecutionMustPassFinalRules(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{true: "fixed", false: "false success"}[repair], func(t *testing.T) {
			j := repairJob(t, `{"ok":false}`)
			calls := 0
			p := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
				calls++
				if repair {
					if err := os.WriteFile(j.Repair.Path, []byte(`{"ok":true}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return agent.Text("assistant", finalEvidenceOutput(j.Repair.Path)), nil
			})
			dir := t.TempDir()
			r, err := runTestWorker(context.Background(), j, Options{RunDir: dir, Provider: p, Tools: []agent.Tool{}, Output: repairTestBridge(dir)})
			if err != nil || calls != 1 || (r.Status == "success") != repair {
				t.Fatalf("%+v %v calls=%d", r, err, calls)
			}
			if repair && (r.RepairCheck == nil || r.RepairCheck.Outcome != "execute") {
				t.Fatalf("missing execution receipt: %+v", r)
			}
			if !repair && r.FailureKind != "repair_acceptance" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestRepairReadFailsClosed(t *testing.T) {
	for _, kind := range []string{"outside", "parent symlink", "file symlink", "fifo", "missing", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			j := repairJob(t, `{"ok":true}`)
			outside := filepath.Join(t.TempDir(), "outside.json")
			if err := os.WriteFile(outside, []byte(`{"ok":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "outside":
				j.Repair.Path = outside
			case "parent symlink":
				link := filepath.Join(j.Workspace, "linked")
				if err := os.Symlink(filepath.Dir(outside), link); err != nil {
					t.Fatal(err)
				}
				j.Repair.Path = filepath.Join(link, "outside.json")
			case "file symlink":
				j.Repair.Path = filepath.Join(j.Workspace, "linked.json")
				if err := os.Symlink(outside, j.Repair.Path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				j.Repair.Path = filepath.Join(j.Workspace, "pipe")
				if err := unix.Mkfifo(j.Repair.Path, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				j.Repair.Path = filepath.Join(j.Workspace, "missing")
			case "oversize":
				if err := os.WriteFile(j.Repair.Path, []byte(strings.Repeat("x", artifactcheck.MaxFileBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := inspectRepair(context.Background(), j); err == nil {
				t.Fatal("accepted invalid file")
			}
		})
	}
}

func TestRepairRecoveryPreservesAnExecutedAttempt(t *testing.T) {
	j := repairJob(t, `{"ok":false}`)
	dir := t.TempDir()
	ctx, cancel := context.WithCancelCause(context.Background())
	calls := 0
	p := scenarioProvider(func(context.Context, []agent.Message, []agent.Definition, agent.Emit) (agent.Message, error) {
		calls++
		if err := os.WriteFile(j.Repair.Path, []byte(`{"ok":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		cancel(ErrInterrupted)
		return agent.Message{}, context.Canceled
	})
	if _, err := runSession(ctx, j, Options{RunDir: dir, Provider: p, Tools: []agent.Tool{}, Output: repairTestBridge(dir)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interruption, got %v", err)
	}
	r, err := runSession(context.Background(), j, Options{RunDir: dir, Provider: p, Tools: []agent.Tool{}, Output: repairTestBridge(dir)})
	if err != nil || calls != 1 || r.Status != "success" || r.RepairCheck == nil || r.RepairCheck.Outcome != "execute" || strings.Contains(r.Text, "no model call") {
		t.Fatalf("recovery mislabeled prior execution: %+v %v calls=%d", r, err, calls)
	}
}

func TestRepairDraftPreservesExactNumericPredicate(t *testing.T) {
	payload := `{"action":"add","from":["origin"],"description":"repair","repair":{"path":"/workspace/a.json","sha256":"` + strings.Repeat("a", 64) + `","rules":[{"kind":"json_equals","pointer":"/n","value":9007199254740993}]}}`
	d := &decisionDraft{orchestration: true}
	if _, err := d.action(context.Background(), draftTestAction("step", "repair", payload), "version"); err != nil {
		t.Fatal(err)
	}
	if len(d.actions) != 1 || !strings.Contains(string(d.actions[0].Payload), "9007199254740993") {
		t.Fatalf("predicate lost precision: %+v", d.actions)
	}
	large := &decisionDraft{orchestration: true}
	if _, err := large.action(context.Background(), draftTestAction("step", "large", strings.Replace(payload, "9007199254740993", "1e309", 1)), "version"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(large.actions[0].Payload), "1e309") {
		t.Fatal("large exponent lost in draft")
	}
	for _, bad := range []string{strings.Replace(payload, "json_equals", "arbitrary_script", 1), strings.Replace(payload, `"action":"add"`, `"action":"priority"`, 1)} {
		invalid := &decisionDraft{orchestration: true}
		if _, err := invalid.action(context.Background(), draftTestAction("step", "repair", bad), "version"); err == nil || len(invalid.actions) > 0 {
			t.Fatal("invalid repair staged")
		}
	}
}

func TestRepairPreservesBlankAndLeadingWhitespaceArtifacts(t *testing.T) {
	for _, raw := range []string{"", " \n\t", strings.Repeat(" ", 5000) + `{"ok":true}`} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			j := repairJob(t, raw)
			if strings.TrimSpace(raw) == "" {
				j.Repair.Rules = []artifactcheck.Rule{{Kind: "text_not_contains", Text: "broken"}}
			}
			dir := t.TempDir()
			r, err := runTestWorker(context.Background(), j, Options{RunDir: dir})
			if err != nil || r.Status != "success" || r.RepairCheck == nil {
				t.Fatalf("%+v %v", r, err)
			}
			bytes, err := os.ReadFile(filepath.Join(dir, "evidence", r.RepairCheck.SHA256+".raw"))
			if err != nil || string(bytes) != raw {
				t.Fatalf("original blank bytes not retained: %v", err)
			}
		})
	}
}
