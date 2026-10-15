package sqlstore

import "pwnmesh/internal/board/model"

// Only runtime identity is needed for the current graph. Keep archived Job and
// Result bodies out of this read, including when a retained body is malformed.
// Registration order selects the latest attempt across namespaces/generations;
// the projection decides whether that attempt can support the current state.
func LatestStepExecutions(q Queries, project string) ([]model.ExecutionSummary, error) {
	rows, err := q.Query(`SELECT e.id,e.intent,e.lease,e.status,e.generation FROM intents i JOIN pwnmesh_executions e ON e.rowid=(
		SELECT rowid FROM pwnmesh_executions WHERE project_id=i.project_id AND intent=i.id AND kind!='reason' ORDER BY rowid DESC LIMIT 1
	) WHERE i.project_id=?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ExecutionSummary
	for rows.Next() {
		var run model.ExecutionSummary
		if err := rows.Scan(&run.ID, &run.Intent, &run.Lease, &run.Status, &run.Generation); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// Delivery failures retain their diagnostic in an event, without overwriting an
// immutable pending result. Board requests this only for projected failed Steps.
func ExecutionFailureReason(q Queries, project, lease string) (string, error) {
	var detail string
	err := q.QueryRow("SELECT json_extract(event,'$.payload.reason') FROM pwnmesh_state_events WHERE project_id=? AND json_extract(event,'$.op')='execution_failed' AND json_extract(event,'$.run_id')=? ORDER BY revision DESC LIMIT 1", project, lease).Scan(&detail)
	return detail, err
}

func ProjectAssets(q Queries, project string, generation int64) ([]model.Asset, []model.AssetAnchor, error) {
	rows, err := q.Query(`SELECT a.id,a.kind,a.value,a.method,n.node_kind,n.node_id FROM pwnmesh_asset_anchors n JOIN pwnmesh_assets a ON a.project_id=n.project_id AND a.id=n.asset_id WHERE n.project_id=? AND n.generation=? ORDER BY a.id,n.node_kind,n.node_id`, project, generation)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var assets []model.Asset
	var anchors []model.AssetAnchor
	seen := map[string]bool{}
	for rows.Next() {
		var asset model.Asset
		var anchor model.AssetAnchor
		if err := rows.Scan(&asset.ID, &asset.Kind, &asset.Value, &asset.Method, &anchor.NodeKind, &anchor.NodeID); err != nil {
			return nil, nil, err
		}
		anchor.AssetID = asset.ID
		if !seen[asset.ID] {
			assets = append(assets, asset)
			seen[asset.ID] = true
		}
		anchors = append(anchors, anchor)
	}
	return assets, anchors, rows.Err()
}
