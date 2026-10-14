package board

import "encoding/json"

// Compatibility readers for unversioned state snapshots and pre-failure-event
// execution results. These formats have no release marker; do not assign them
// invented versions. Retention and removal requirements: docs/compatibility.md.

// Omit curation from pre-orchestration-v1 wire snapshots. The exact field order
// and omission rules preserve decision hashes from the unversioned protocol.
func (s State) MarshalJSON() ([]byte, error) {
	type stateAlias State
	view := struct {
		*stateAlias
		Curation *CurationProgress `json:"curation,omitempty"`
	}{stateAlias: (*stateAlias)(&s)}
	if s.Graph.Project.OrchestrationVersion == 1 {
		view.Curation = &s.Curation
	}
	return json.Marshal(view)
}

// Read bounded diagnostics when an execution predates failure events, without
// rewriting retained Result bytes or immutable Job inputs.
func (t *Tx) legacyExecutionFailure(project string, execution Execution) (string, error) {
	var raw []byte
	err := t.QueryRow(`WITH failure AS (
		SELECT CASE WHEN json_valid(result) THEN result ELSE '{}' END AS body,
		char(9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288) AS whitespace
		FROM xloom_executions WHERE project_id=? AND id=?)
		SELECT json_object(
		'error',substr(trim(json_extract(body,'$.error'),whitespace),1,2048),
		'failure_kind',substr(json_extract(body,'$.failure_kind'),1,2048),
		'text',json_object('reason',substr(trim(json_extract(CASE WHEN json_valid(json_extract(body,'$.text')) THEN json_extract(body,'$.text') ELSE '{}' END,'$.reason'),whitespace),1,2048)))
		FROM failure`, project, execution.ID).Scan(&raw)
	if err != nil {
		return "", err
	}
	// The legacy result's text field is a JSON string, not an embedded object.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", err
	}
	fields["text"], _ = json.Marshal(string(fields["text"]))
	raw, err = json.Marshal(fields)
	return executionFailureDescription(execution.Status, raw), err
}
