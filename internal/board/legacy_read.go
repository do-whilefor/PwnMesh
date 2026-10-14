package board

import "encoding/json"

// Older executions may have no failure event. Read only bounded diagnostic
// fields without rewriting retained results or their immutable job inputs.
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
