package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"pwnmesh/internal/artifactcheck"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/contract"
	"pwnmesh/internal/protocol"
	"strings"
)

func validateRepairResult(jobRaw, resultRaw json.RawMessage) error {
	var job struct {
		Repair                *artifactcheck.Spec `json:"repair"`
		RunID                 string              `json:"run_id"`
		Workspace             string              `json:"workspace"`
		Kind                  string              `json:"kind"`
		GraphRPC              bool                `json:"graph_rpc"`
		ResultContractVersion int                 `json:"result_contract_version"`
	}
	var result struct {
		RepairCheck *protocol.RepairCheck `json:"repair_check"`
		Text        string                `json:"text"`
		Conclude    bool                  `json:"conclude"`
	}
	if json.Unmarshal(jobRaw, &job) != nil || json.Unmarshal(resultRaw, &result) != nil {
		return b.Err(422, "invalid repair result")
	}
	if job.Repair == nil {
		if result.RepairCheck != nil {
			return b.Err(422, "ordinary task cannot submit a repair receipt")
		}
		return nil
	}
	check := result.RepairCheck
	if check == nil || check.Path != job.Repair.Path || check.ExpectedSHA256 != job.Repair.SHA256 || !check.Satisfied || len(check.FailedRules) != 0 || (check.Outcome != "noop" && check.Outcome != "execute") {
		return b.Err(422, "repair success requires its matching satisfied runtime receipt")
	}
	copy := *job.Repair
	copy.SHA256 = check.SHA256
	if err := artifactcheck.Validate(copy); err != nil {
		return b.Err(422, "repair receipt has an invalid current SHA-256")
	}
	if job.RunID == "" || len(job.RunID) > 256 || strings.Trim(job.RunID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" ||
		!path.IsAbs(job.Workspace) || path.Clean(job.Workspace) != job.Workspace || strings.ContainsAny(job.Workspace, "\x00\\\r\n") ||
		!strings.HasPrefix(check.Path, strings.TrimSuffix(job.Workspace, "/")+"/") || job.Kind != "explore" || !job.GraphRPC || job.ResultContractVersion != 2 {
		return b.Err(422, "repair receipt requires its registered run and workspace")
	}
	parsed, err := contract.ParseWithPolicy(result.Text, job.Kind, result.Conclude, contract.Policy{Version: job.ResultContractVersion, GraphRPC: job.GraphRPC})
	if err != nil || parsed.Kind != "fact" || parsed.Outcome != "completed" || len(parsed.FactPayload) == 0 || parsed.FactID != "" {
		return b.Err(422, "repair success requires an inline fact with retained check evidence")
	}
	var fact struct {
		Evidence []b.EvidenceRef `json:"evidence"`
	}
	if json.Unmarshal(parsed.FactPayload, &fact) != nil || len(fact.Evidence) == 0 || len(fact.Evidence) > 32 {
		return b.Err(422, "repair success requires retained current-content and receipt evidence")
	}
	// The Worker is the filesystem authority. The service additionally binds
	// the accepted Fact to that run's content-addressed bytes and exact receipt;
	// a satisfied flag alone cannot certify an unrelated or source-only Fact.
	receipt, err := json.Marshal(check)
	if err != nil {
		return b.Err(422, "invalid canonical repair receipt")
	}
	evidenceDir := path.Join(job.Workspace, ".pwnmesh", "runs", job.RunID, "evidence")
	receiptPath := path.Join(evidenceDir, fmt.Sprintf("%x.raw", sha256.Sum256(receipt)))
	contentPath := path.Join(evidenceDir, check.SHA256+".raw")
	hasReceipt := false
	hasContent := check.BlankContent || check.SHA256 == fmt.Sprintf("%x", sha256.Sum256(nil))
	for _, ref := range fact.Evidence {
		name := path.Base(ref.Path)
		digest, decodeErr := hex.DecodeString(strings.TrimSuffix(name, ".raw"))
		if ref.RunID != job.RunID || path.Clean(ref.Path) != ref.Path || path.Dir(ref.Path) != evidenceDir ||
			decodeErr != nil || len(digest) != sha256.Size || name != hex.EncodeToString(digest)+".raw" ||
			ref.Excerpt == "" || len(ref.Excerpt) > 8192 || ref.StartLine < 0 || ref.EndLine < ref.StartLine || (ref.StartLine == 0 && ref.EndLine != 0) {
			return b.Err(422, "repair evidence must belong to this run at a canonical SHA-256 path")
		}
		hasContent = hasContent || ref.Path == contentPath
		hasReceipt = hasReceipt || (ref.Path == receiptPath && ref.Excerpt == string(receipt))
	}
	if !hasContent || !hasReceipt {
		return b.Err(422, "repair fact is missing the accepted content hash or canonical runtime receipt")
	}
	return nil
}
