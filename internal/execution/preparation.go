package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	b "pwnmesh/internal/board"
	"pwnmesh/internal/protocol"
)

// Prepared identifies whether this transaction registered a new immutable run.
type Prepared struct {
	Execution b.Execution
	Created   bool
}

// PrepareRun freezes input and registers it atomically in the supplied transaction.
func PrepareRun(t *b.Tx, e b.Execution, fence b.ExecutionFence) (Prepared, error) {
	// Inspect presence before decoding into Go scalar fields: an explicit null
	// must not become false/0 and silently select a compatibility protocol.
	var protocolFields struct {
		GraphRPC              json.RawMessage `json:"graph_rpc"`
		ResultContractVersion json.RawMessage `json:"result_contract_version"`
	}
	if err := json.Unmarshal(e.Job, &protocolFields); err != nil {
		return Prepared{}, b.Err(422, "invalid execution template")
	}
	if err := validateExecutionProtocol(protocolFields.GraphRPC, protocolFields.ResultContractVersion); err != nil {
		return Prepared{}, err
	}
	var j protocol.Job
	if json.Unmarshal(e.Job, &j) != nil || j.Repair != nil || j.State != nil || j.InputSnapshot != nil || j.Decision != nil || len(j.InputView) != 0 || len(j.DependencyResults) != 0 || j.PreparationKey != "" || j.Graph.Project.ID != e.ProjectID || len(j.Graph.Facts)+len(j.Graph.Intents)+len(j.Graph.Hints) != 0 {
		return Prepared{}, b.Err(422, "prepare requires a small job template without graph data")
	}
	if err := j.Budget.ValidateTimeouts(); err != nil {
		return Prepared{}, b.Err(422, err.Error())
	}
	if (j.Kind == "reason" || j.Kind == "curate") && j.Intent != nil {
		return Prepared{}, b.Err(422, "Control preparation must not carry an intent")
	}
	if len(j.InputFiles) != 0 {
		return Prepared{}, b.Err(422, "input_files are assigned by the server")
	}
	if fence != e.Fence() {
		return Prepared{}, b.Err(403, "Execution preparation requires its lease")
	}
	canonical, err := json.Marshal(e)
	if err != nil {
		return Prepared{}, err
	}
	sum := sha256.Sum256(canonical)
	key := hex.EncodeToString(sum[:])
	prior, err := t.Execution(e.ProjectID, e.ID)
	if err == nil {
		var old protocol.Job
		if json.Unmarshal(prior.Job, &old) != nil || old.PreparationKey != key {
			return Prepared{}, b.Err(409, "Execution input is immutable")
		}
		return Prepared{Execution: prior}, nil
	}
	var ae *b.APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		return Prepared{}, err
	}
	state, err := t.State(e.ProjectID)
	if err != nil {
		return Prepared{}, err
	}
	if state.Graph.Project.Generation != j.Graph.Project.Generation {
		return Prepared{}, b.CodedErr(409, b.CodeStateChanged, "state_changed: project round changed before preparation")
	}
	if state.Graph.Project.OrchestrationVersion != 1 || (j.Kind != "reason" && j.Kind != "curate" && j.Kind != "explore") {
		return Prepared{}, b.Err(422, "Orchestration version 1 requires a live version 2 reason, curate or explore execution")
	}
	if err = state.Graph.RequireActive(); err != nil {
		return Prepared{}, err
	}
	if err = t.CheckExecution(state.Graph, e.Fence()); err != nil {
		return Prepared{}, err
	}
	e.RetryKey = e.Kind + ":" + e.Intent
	if e.Kind == "reason" {
		e.RetryKey = b.DecisionRetryKey(state.Graph, state.DecisionRevision)
	} else if e.Kind == "curate" {
		e.RetryKey = b.CurationRetryKey(state)
	}
	check, err := t.CheckExecutions(b.ExecutionCheckQuery{ProjectID: e.ProjectID, Namespace: e.Namespace, Generation: state.Graph.Project.Generation, Kind: e.Kind, Intent: e.Intent, RetryKey: e.RetryKey, StateVersion: b.DecisionStateVersion(state)})
	if err != nil {
		return Prepared{}, err
	}
	j.PreviousRunID = check.PreviousRunID
	j.Graph = b.Graph{Project: state.Graph.Project}
	j.InputFiles, err = t.InputFiles(e.ProjectID)
	if err != nil {
		return Prepared{}, err
	}
	j.DecisionRevision = state.DecisionRevision
	if e.Kind == "reason" {
		var cursor *b.DecisionCursor
		var events []b.StateEvent
		if previous := check.LatestDecision; previous != nil && previous.HasState {
			cursor = &b.DecisionCursor{ProjectID: previous.ProjectID, Generation: previous.Generation, Revision: previous.InputRevision}
			if cursor.Revision <= state.Revision {
				changes, changeErr := t.StateChanges(e.ProjectID, cursor.Revision, state.Revision)
				if changeErr != nil {
					return Prepared{}, changeErr
				}
				for _, change := range changes {
					events = append(events, b.StateEvent{Revision: change.Revision, Op: change.Op, ID: change.ID})
				}
			}
		}
		j.Decision, err = b.BuildDecisionContextFromCursor(state, cursor, events, b.DefaultContextViewBytes)
		if err != nil {
			return Prepared{}, b.Err(422, err.Error())
		}
		assessment, assessmentErr := t.CompletionAssessment(state)
		if assessmentErr != nil {
			return Prepared{}, assessmentErr
		}
		err = b.UseCompletionAssessment(state, j.Decision, assessment)
		if err != nil {
			return Prepared{}, err
		}
		if state.Graph.Project.OrchestrationVersion == 1 && b.HasCompletionEvidence(state) {
			j.Decision.ClosureProtocol = 1
		}
		j.DecisionRepeated = check.Repeated
		j.DecisionTriggers = preparedDecisionTriggers(state, check.LatestDecision, j.DecisionTrigger)
	} else if e.Kind == "curate" {
		j.InputView, err = b.CurationContextView(state, b.DefaultContextViewBytes)
		if err != nil {
			return Prepared{}, b.Err(422, err.Error())
		}
	} else {
		j.Intent = nil
		for _, i := range state.Graph.Intents {
			if i.ID == e.Intent {
				v := i
				j.Intent = &v
				break
			}
		}
		if j.Intent == nil {
			return Prepared{}, b.Err(404, "Step not found")
		}
		for _, current := range state.Steps {
			if current.ID == e.Intent {
				j.Repair = current.Repair
			}
		}
		j.DependencyResults, err = t.StepDependencyResults(e.ProjectID, e.Intent)
		if err != nil {
			return Prepared{}, err
		}
		step := ""
		if e.Kind == "explore" {
			step = e.Intent
		}
		j.InputView, err = b.ContextView(state, step, b.DefaultContextViewBytes)
		if err != nil {
			return Prepared{}, b.Err(422, err.Error())
		}
	}
	j.InputSnapshot, err = t.FreezeInput(state)
	if err != nil {
		return Prepared{}, err
	}
	j.PreparationKey = key
	e.Job, err = json.Marshal(j)
	if err != nil {
		return Prepared{}, err
	}
	if e.Kind == "curate" && len(e.Job) > b.MaxCurationInputBytes {
		return Prepared{}, b.Err(422, "Curation input exceeds the supported immutable input limit")
	}
	saved, err := Register(t, e, fence)
	return Prepared{Execution: saved, Created: err == nil}, err
}

func preparedDecisionTriggers(state b.State, previous *b.ExecutionSummary, trigger string) []string {
	if previous == nil {
		return []string{trigger}
	}
	causes := []string{}
	if trigger == "explicit_retry" {
		causes = append(causes, trigger)
	}
	if len(state.Graph.Facts) > previous.FactCount {
		causes = append(causes, "new_facts")
	}
	if len(state.Graph.Hints) > previous.HintCount {
		causes = append(causes, "new_hints")
	}
	if previous.OpenCount > 0 && state.Graph.OpenCount() == 0 {
		causes = append(causes, "intents_finished")
	}
	if state.DecisionRevision > previous.DecisionRevision {
		causes = append(causes, "state_changed")
	}
	if len(causes) == 0 {
		causes = append(causes, trigger)
	}
	return causes
}
