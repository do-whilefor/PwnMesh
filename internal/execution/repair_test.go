package execution

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
	"strings"
	"testing"
)

func TestRepairSuccessRequiresBoundRuntimeReceipt(t *testing.T) {
	spec := &artifactcheck.Spec{Path: "/workspace/a", SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "json_valid"}}}
	job, _ := json.Marshal(protocol.Job{Repair: spec, RunID: "repair-run", Workspace: "/workspace", Kind: "explore", GraphRPC: true, ResultContractVersion: 2})
	valid := protocol.RepairCheck{Result: artifactcheck.Result{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("{}\n"))), Satisfied: true, FailedRules: []int{}}, Path: spec.Path, ExpectedSHA256: spec.SHA256, Outcome: "noop"}
	for _, name := range []string{"valid", "missing", "wrong path", "wrong binding", "failed check", "failed rule indexes", "invalid hash", "stale"} {
		t.Run(name, func(t *testing.T) {
			check := valid
			r := repairResultWithEvidence(check, "repair-run", "/workspace", true)
			r.RepairCheck = &check
			switch name {
			case "missing":
				r.RepairCheck = nil
			case "wrong path":
				check.Path = "/workspace/b"
			case "wrong binding":
				check.ExpectedSHA256 = strings.Repeat("c", 64)
			case "failed check":
				check.Satisfied = false
			case "failed rule indexes":
				check.FailedRules = []int{0}
			case "invalid hash":
				check.SHA256 = "invalid"
			case "stale":
				check.Outcome = "stale"
			}
			raw, _ := json.Marshal(r)
			if err := validateRepairResult(job, raw); (err == nil) != (name == "valid") {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}

func repairResultWithEvidence(check protocol.RepairCheck, run, workspace string, includeContent bool) protocol.Result {
	dir := path.Join(workspace, ".pwnmesh", "runs", run, "evidence")
	receipt, _ := json.Marshal(check)
	refs := []board.EvidenceRef{{RunID: run, Path: path.Join(dir, fmt.Sprintf("%x.raw", sha256.Sum256(receipt))), Excerpt: string(receipt)}}
	if includeContent {
		refs = append(refs, board.EvidenceRef{RunID: run, Path: path.Join(dir, check.SHA256+".raw"), Excerpt: "{}\n"})
	}
	fact := map[string]any{"description": "Deterministic repair accepted", "scope": check.Path, "observed_at": "2026-09-28T00:00:00Z", "evidence": refs}
	text, _ := json.Marshal(map[string]any{"accepted": true, "outcome": "completed", "data": map[string]any{"fact": fact}})
	return protocol.Result{Type: "result", Status: "success", Text: string(text), RepairCheck: &check}
}

func TestRepairReceiptMustBeBoundToCurrentRunEvidence(t *testing.T) {
	for _, name := range []string{"valid", "execute", "fact id", "missing evidence", "missing content", "missing receipt", "foreign run", "foreign directory", "traversal", "bad hash name", "uppercase hash", "receipt excerpt changed", "receipt hash changed", "blank excerpt", "invalid line selection", "missing run", "invalid run", "missing workspace", "noncanonical workspace", "target outside workspace", "wrong result contract", "wrong role"} {
		t.Run(name, func(t *testing.T) {
			spec := &artifactcheck.Spec{Path: "/workspace/a", SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "json_valid"}}}
			job := protocol.Job{Repair: spec, RunID: "repair-run", Workspace: "/workspace", Kind: "explore", GraphRPC: true, ResultContractVersion: 2}
			check := protocol.RepairCheck{Result: artifactcheck.Result{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("{}\n"))), Satisfied: true, FailedRules: []int{}}, Path: spec.Path, ExpectedSHA256: spec.SHA256, Outcome: "noop"}
			if name == "execute" {
				check.Outcome = "execute"
			}
			r := repairResultWithEvidence(check, job.RunID, job.Workspace, true)
			var envelope map[string]any
			_ = json.Unmarshal([]byte(r.Text), &envelope)
			data := envelope["data"].(map[string]any)
			fact := data["fact"].(map[string]any)
			refs := fact["evidence"].([]any)
			receipt := refs[0].(map[string]any)
			switch name {
			case "fact id":
				delete(data, "fact")
				data["fact_id"] = "another-fact"
			case "missing evidence":
				fact["evidence"] = []any{}
			case "missing content":
				fact["evidence"] = refs[:1]
			case "missing receipt":
				fact["evidence"] = refs[1:]
			case "foreign run":
				receipt["run_id"] = "other-run"
			case "foreign directory":
				receipt["path"] = strings.Replace(receipt["path"].(string), "/repair-run/", "/other-run/", 1)
			case "traversal":
				receipt["path"] = strings.Replace(receipt["path"].(string), "/evidence/", "/evidence/../evidence/", 1)
			case "bad hash name":
				receipt["path"] = path.Join(path.Dir(receipt["path"].(string)), "unverified.raw")
			case "uppercase hash":
				receipt["path"] = path.Join(path.Dir(receipt["path"].(string)), strings.ToUpper(path.Base(receipt["path"].(string))))
			case "receipt excerpt changed":
				receipt["excerpt"] = "satisfied"
			case "receipt hash changed":
				receipt["path"] = path.Join(path.Dir(receipt["path"].(string)), strings.Repeat("c", 64)+".raw")
			case "blank excerpt":
				refs[1].(map[string]any)["excerpt"] = ""
			case "invalid line selection":
				receipt["end_line"] = 1
			case "missing run":
				job.RunID = ""
			case "invalid run":
				job.RunID = "../repair-run"
			case "missing workspace":
				job.Workspace = ""
			case "noncanonical workspace":
				job.Workspace = "/workspace/../workspace"
			case "target outside workspace":
				job.Workspace = "/other"
			case "wrong result contract":
				job.ResultContractVersion = 0
			case "wrong role":
				job.Kind = "reason"
			}
			text, _ := json.Marshal(envelope)
			r.Text = string(text)
			jobRaw, _ := json.Marshal(job)
			resultRaw, _ := json.Marshal(r)
			if err := validateRepairResult(jobRaw, resultRaw); (err == nil) != (name == "valid" || name == "execute") {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}

func TestRepairEmptyTargetRequiresReceiptButNoEmptyExcerpt(t *testing.T) {
	spec := &artifactcheck.Spec{Path: "/workspace/a", SHA256: strings.Repeat("a", 64), Rules: []artifactcheck.Rule{{Kind: "text_not_contains", Text: "bad"}}}
	job, _ := json.Marshal(protocol.Job{Repair: spec, RunID: "repair-run", Workspace: "/workspace", Kind: "explore", GraphRPC: true, ResultContractVersion: 2})
	check := protocol.RepairCheck{Result: artifactcheck.Result{SHA256: fmt.Sprintf("%x", sha256.Sum256(nil)), Satisfied: true, FailedRules: []int{}}, Path: spec.Path, ExpectedSHA256: spec.SHA256, Outcome: "noop"}
	raw, _ := json.Marshal(repairResultWithEvidence(check, "repair-run", "/workspace", false))
	if err := validateRepairResult(job, raw); err != nil {
		t.Fatal(err)
	}
	check.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(" \n\t")))
	check.BlankContent = true
	blank, _ := json.Marshal(repairResultWithEvidence(check, "repair-run", "/workspace", false))
	if err := validateRepairResult(job, blank); err != nil {
		t.Fatalf("blank target lost its receipt: %v", err)
	}
	if err := validateRepairResult(json.RawMessage(`{}`), json.RawMessage(`{"status":"success","text":"ordinary legacy result"}`)); err != nil {
		t.Fatalf("ordinary non-repair result changed: %v", err)
	}
	if err := validateRepairResult(json.RawMessage(`{}`), raw); err == nil {
		t.Fatal("ordinary job accepted a repair receipt")
	}
}
