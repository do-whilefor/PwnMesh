package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"pwnmesh/internal/artifactcheck"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/contract"
	"pwnmesh/internal/worker"
	"strings"
)

func (s *Server) registerExecutionRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /projects/{pid}/scheduling", s.wrap(s.schedulingInput))
	m.HandleFunc("POST /projects/{pid}/executions/prepare", s.wrap(s.prepareExecution))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/input/read", s.wrap(s.snapshotRead))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/updates", s.wrap(s.executionUpdates))
	m.HandleFunc("POST /projects/{pid}/executions/{rid}/traces/read", s.wrap(s.executionTraces))
	m.HandleFunc("GET /executions/pending", s.wrap(s.pendingExecutions))
	m.HandleFunc("GET /projects/{pid}/executions/check", s.wrap(s.executionCheck))
	m.HandleFunc("GET /projects/{pid}/executions/{rid}", s.wrap(s.executionDetail))
	m.HandleFunc("GET /projects/{pid}/executions/{rid}/identity", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("execution_identity", "true")
		s.wrap(s.executionDetail)(w, r)
	})
	for _, op := range []string{"status", "resume", "apply", "retry"} {
		m.HandleFunc("POST /projects/{pid}/executions/{rid}/"+op, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("execution_op", op)
			s.wrap(s.executionAction)(w, r)
		})
	}
}
func decodeFields(q *request, v any) error {
	raw, err := json.Marshal(q.fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// Execution inputs always use the live evidence protocol.
func validateExecutionProtocol(graphRPC, resultVersion json.RawMessage) error {
	var enabled bool
	if json.Unmarshal(graphRPC, &enabled) != nil || !enabled {
		return b.Err(422, "graph_rpc must be true")
	}
	var version int
	if json.Unmarshal(resultVersion, &version) != nil || version != 2 {
		return b.Err(422, "result_contract_version must be 2")
	}
	return nil
}

func requireCurrentExecution(e b.Execution) error {
	var job struct {
		Kind                  string `json:"kind"`
		GraphRPC              bool   `json:"graph_rpc"`
		ResultContractVersion int    `json:"result_contract_version"`
		Graph                 struct {
			Project struct {
				OrchestrationVersion int `json:"orchestration_version"`
			} `json:"project"`
		} `json:"graph"`
		Decision *struct {
			Version int `json:"version"`
		} `json:"decision"`
	}
	if json.Unmarshal(e.Job, &job) != nil || job.Graph.Project.OrchestrationVersion != 1 || !job.GraphRPC || job.ResultContractVersion != 2 || job.Kind != e.Kind || (e.Kind != "reason" && e.Kind != "curate" && e.Kind != "explore") || (e.Kind == "reason" && (job.Decision == nil || job.Decision.Version != 2)) {
		return b.Err(409, "Historical execution protocols are read-only; create a new project to continue")
	}
	return nil
}

// Production registrations receive the job built by server-side preparation.
func (s *Server) registerExecution(t *b.Tx, e b.Execution, r *http.Request) (int, any, error) {
	var job struct {
		RunID                 string          `json:"run_id"`
		Kind                  string          `json:"kind"`
		Graph                 b.Graph         `json:"graph"`
		Intent                *b.Intent       `json:"intent"`
		Workspace             string          `json:"workspace"`
		GraphRPC              json.RawMessage `json:"graph_rpc"`
		ResultContractVersion json.RawMessage `json:"result_contract_version"`
	}
	if json.Unmarshal(e.Job, &job) != nil || e.ID == "" || e.Namespace == "" || e.Backend == "" || e.Lease == "" || e.RetryKey == "" || job.RunID != e.ID || job.Kind != e.Kind || job.Graph.Project.ID != e.ProjectID || job.Workspace == "" {
		return 0, nil, b.Err(422, "Invalid execution identity")
	}
	if err := validateExecutionProtocol(job.GraphRPC, job.ResultContractVersion); err != nil {
		return 0, nil, err
	}
	if !b.ValidExecutionID(e.ID) || len(e.Namespace) > 128 || len(e.Backend) > 256 || len(e.RetryKey) > 1024 || e.Lease != e.Backend+"@"+e.ID {
		return 0, nil, b.Err(422, "Invalid execution identity")
	}
	if e.Kind != "reason" && e.Kind != "curate" && e.Kind != "explore" {
		return 0, nil, b.Err(422, "Invalid execution kind")
	}
	control := e.Kind == "reason" || e.Kind == "curate"
	if (control && (e.Intent != "" || job.Intent != nil)) || (!control && (job.Intent == nil || job.Intent.ID != e.Intent)) {
		return 0, nil, b.Err(422, "Invalid execution step")
	}
	if r.Header.Get("X-PwnMesh-Run") != e.Lease || r.Header.Get("X-PwnMesh-Lease") != e.Kind || r.Header.Get("X-PwnMesh-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Execution registration requires its lease")
	}
	if err := t.RegisterExecution(e); err != nil {
		return 0, nil, err
	}
	saved, err := t.Execution(e.ProjectID, e.ID)
	return 201, saved, err
}
func (s *Server) executionAction(t *b.Tx, q *request, r *http.Request) (int, any, error) {
	op := r.PathValue("execution_op")
	if op == "retry" {
		if r.Header.Get("X-PwnMesh-Run") != "" {
			return 0, nil, b.Err(403, "retry authorization is a project-management operation")
		}
		if value, exists := q.fields["automatic"]; exists {
			automatic, ok := value.(bool)
			if !ok {
				return 0, nil, b.Err(422, "automatic must be a boolean")
			}
			if automatic {
				e := b.Execution{ProjectID: r.PathValue("pid"), ID: r.PathValue("rid")}
				return 200, map[string]string{"previous_run_id": e.ID}, t.RequestAutomaticDecisionRetry(e)
			}
		}
	}
	e, err := t.Execution(r.PathValue("pid"), r.PathValue("rid"))
	if err != nil {
		return 0, nil, err
	}
	if err = requireCurrentExecution(e); err != nil {
		return 0, nil, err
	}
	// Retry is a project-management operation, never a Worker tool. Automatic
	// Decide and curator infrastructure recovery have a server-enforced bound;
	// human retry remains explicit. Both retain the old record and enable one
	// subsequent attempt.
	if op == "retry" {
		if e.Status != "failed" && e.Status != "rejected" && e.Status != "cancelled" {
			return 0, nil, b.Err(409, "Only failed, rejected or cancelled executions can be retried")
		}
		if err := t.CheckRepairRetry(e.ProjectID, e.ID); err != nil {
			return 0, nil, err
		}
		g, err := t.Load(e.ProjectID)
		if err != nil {
			return 0, nil, err
		}
		if err = g.RequireActive(); err != nil {
			return 0, nil, err
		}
		if e.Kind != "reason" && e.Kind != "curate" {
			if err = t.StepAvailable(e.ProjectID, e.Intent); err != nil {
				return 0, nil, err
			}
			for _, intent := range g.Intents {
				if intent.ID == e.Intent && (intent.To != nil || intent.ConcludedAt != nil) {
					return 0, nil, b.Err(409, "An ended step cannot authorize a retry")
				}
			}
		}
		return 200, map[string]string{"previous_run_id": e.ID}, t.ExecutionStatus(e, "retry_requested", e.Result)
	}
	if r.Header.Get("X-PwnMesh-Run") != e.Lease || r.Header.Get("X-PwnMesh-Lease") != e.Kind || r.Header.Get("X-PwnMesh-Intent") != e.Intent {
		return 0, nil, b.Err(403, "Execution lease mismatch")
	}
	if op == "resume" {
		if err = t.ResumeExecution(e); err != nil {
			return 0, nil, err
		}
		e, err = t.Execution(e.ProjectID, e.ID)
		return 200, e, err
	}
	if op == "apply" {
		return applyExecution(t, e)
	}
	status := q.text("status")
	switch status {
	case "running", "retryable", "result_pending", "failed", "cancelled", "rejected":
	default:
		return 0, nil, b.Err(422, "Invalid execution status")
	}
	if !e.Pending() {
		return 0, nil, b.Err(409, "Execution is terminal")
	}
	if e.Status == "result_pending" && (status == "running" || status == "retryable") {
		return 0, nil, b.Err(409, "Pending result cannot return to execution")
	}
	var result json.RawMessage
	if v, ok := q.fields["result"]; ok {
		result, err = json.Marshal(v)
		if err != nil {
			return 0, nil, err
		}
	}
	if status == "running" || status == "retryable" || status == "result_pending" {
		g, err := t.Load(e.ProjectID)
		if err != nil {
			return 0, nil, err
		}
		if err = g.RequireActive(); err != nil {
			return 0, nil, err
		}
		if err = t.CheckExecution(g, e.Fence()); err != nil {
			return 0, nil, err
		}
	}
	if status == "result_pending" {
		var rr struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(result, &rr) != nil || rr.Status != "success" {
			return 0, nil, b.Err(422, "Pending result must be successful")
		}
		if err := validateRepairResult(e.Job, result); err != nil {
			return 0, nil, err
		}
	}
	if err = t.ExecutionStatus(e, status, result); err != nil {
		return 0, nil, err
	}
	e, err = t.Execution(e.ProjectID, e.ID)
	return 200, e, err
}

// Final business mutations and their receipt commit in the same SQLite
// transaction. Retrying after a lost HTTP response cannot append duplicates.
func applyExecution(t *b.Tx, e b.Execution) (int, any, error) {
	if err := requireCurrentExecution(e); err != nil {
		return 0, nil, err
	}
	if e.Status == "succeeded" || e.Status == "rejected" {
		return 200, map[string]string{"status": e.Status}, nil
	}
	if e.Status != "result_pending" {
		return 0, nil, b.Err(409, "Execution has no pending result")
	}
	if err := validateRepairResult(e.Job, e.Result); err != nil {
		return 0, nil, err
	}
	g, err := t.Load(e.ProjectID)
	if err != nil {
		return 0, nil, err
	}
	if err = g.RequireActive(); err != nil {
		return 0, nil, err
	}
	if err = t.CheckExecution(g, e.Fence()); err != nil {
		return 0, nil, err
	}
	var result struct {
		Text         string `json:"text"`
		Conclude     bool   `json:"conclude"`
		StateVersion string `json:"state_version"`
	}
	if err = json.Unmarshal(e.Result, &result); err != nil {
		return 0, nil, err
	}
	if e.Kind == "reason" {
		initialVersion, err := b.DecisionJobVersion(e.Job)
		if err != nil {
			return 0, nil, err
		}
		if initialVersion != "" || result.StateVersion != "" {
			state, err := t.State(e.ProjectID)
			if err != nil {
				return 0, nil, err
			}
			if result.StateVersion == "" || result.StateVersion != b.DecisionStateVersion(state) {
				return 0, nil, b.Err(409, "state_changed: decision result does not match current shared state")
			}
		}
	}
	// The only executable protocol is fixed by registration, never by apply input.
	parsed, err := contract.ParseWithPolicy(result.Text, e.Kind, result.Conclude, contract.Policy{Version: 2, GraphRPC: true})
	if err != nil {
		return 0, nil, b.Err(422, err.Error())
	}
	if parsed.Outcome == "continue" || parsed.Outcome == "incomplete" {
		return 0, nil, b.Err(422, "Continuing or incomplete worker output cannot be applied as a successful result")
	}
	if e.Kind == "reason" && parsed.Kind != "rejected" {
		// Only a refusal can finish without publishing a plan. A successful
		// batch already wrote its receipt; final JSON cannot replace that commit.
		return 0, nil, b.Err(409, "Decide version 2 requires a committed decision batch")
	}
	status := "succeeded"
	if parsed.Kind == "rejected" {
		status = "rejected"
	} else if e.Kind == "curate" {
		if parsed.Kind != "curated" {
			return 0, nil, b.Err(422, "Invalid curation result")
		}
		receipt, receiptErr := t.CurationReceipt(e.ProjectID, e.Lease)
		if receiptErr != nil {
			var apiError *b.APIError
			if errors.As(receiptErr, &apiError) && apiError.Status == http.StatusNotFound {
				return 0, nil, b.Err(409, "Curator has no committed curation receipt")
			}
			return 0, nil, receiptErr
		}
		if !receipt.Committed || receipt.Op != "curate" {
			return 0, nil, b.Err(409, "Curator has no committed curation receipt")
		}

	} else {
		if _, err = t.ConcludeEvidenceStep(e.ProjectID, e.Fence(), parsed.FactID, parsed.FactPayload); err != nil {
			return 0, nil, err
		}
	}
	if err = t.ExecutionStatus(e, status, e.Result); err != nil {
		return 0, nil, err
	}
	if e.Kind == "curate" {
		if err = t.ReleaseCurator(e.ProjectID, e.Lease); err != nil {
			return 0, nil, err
		}
	}
	return 200, map[string]string{"status": status}, nil
}

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
		RepairCheck *worker.RepairCheck `json:"repair_check"`
		Text        string              `json:"text"`
		Conclude    bool                `json:"conclude"`
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
