package sqlstore

import "pwnmesh/internal/board/model"

// ProjectSummaries reads the list projection without materializing graph
// descriptions, evidence or intent sources. Aggregate each table once rather
// than loading every project's graph separately. Counts retain their legacy
// meaning: concluded_at, not to_fact_id, determines open work.
func ProjectSummaries(q Queries) ([]model.Summary, error) {
	return projectSummaries(q, "", 0, 0, 0)
}

// ObservedProjectSummaries projects the same lease visibility as ExpireLeases,
// including its supported version and valid-date predicates, without writes.
func ObservedProjectSummaries(q Queries, now string, version, intentTimeout, reasonTimeout int) ([]model.Summary, error) {
	return projectSummaries(q, now, version, intentTimeout, reasonTimeout)
}

func projectSummaries(q Queries, now string, version, intentTimeout, reasonTimeout int) ([]model.Summary, error) {
	rows, err := q.Query(`WITH
lease_clock AS (SELECT ? AS now,? AS version,? AS intent_timeout,? AS reason_timeout),
visible_intents AS (
 SELECT i.project_id,i.concluded_at,
 CASE WHEN o.version=c.version AND i.to_fact_id IS NULL AND
  (julianday(c.now)-julianday(i.last_heartbeat_at))*86400>c.intent_timeout
 THEN NULL ELSE i.worker END AS worker
 FROM intents i LEFT JOIN pwnmesh_project_orchestration o ON o.project_id=i.project_id CROSS JOIN lease_clock c
),
fact_counts AS (SELECT project_id,COUNT(*) AS total FROM facts GROUP BY project_id),
intent_counts AS (
 SELECT project_id,COUNT(*) AS total,
 SUM(concluded_at IS NULL AND worker IS NOT NULL) AS working,
 SUM(concluded_at IS NULL AND worker IS NULL) AS unclaimed
 FROM visible_intents GROUP BY project_id
),
hint_counts AS (SELECT project_id,COUNT(*) AS total FROM hints GROUP BY project_id)
SELECT p.id,p.title,p.status,p.bootstrap_enabled,p.created_at,
 CASE WHEN o.version=c.version AND (julianday(c.now)-julianday(p.reason_last_heartbeat_at))*86400>c.reason_timeout
 THEN NULL ELSE p.reason_worker END,p.reason_trigger,p.reason_started_at,p.reason_last_heartbeat_at,
 COALESCE(m.scenario,''),COALESCE(round.generation,0),COALESCE(round.restarted_at,''),COALESCE(termination.terminated_at,''),
 COALESCE(f.total,0),COALESCE(i.total,0),COALESCE(i.working,0),COALESCE(i.unclaimed,0),COALESCE(h.total,0),
 COALESCE(o.version,0),
 CASE WHEN o.version=c.version AND (julianday(c.now)-julianday(o.curator_heartbeat))*86400>c.reason_timeout
 THEN NULL ELSE o.curator_worker END,o.curator_trigger,o.curator_started_at,o.curator_heartbeat
FROM projects p CROSS JOIN lease_clock c
LEFT JOIN pwnmesh_project_metadata m ON m.project_id=p.id
LEFT JOIN pwnmesh_project_rounds round ON round.project_id=p.id
LEFT JOIN pwnmesh_project_termination termination ON termination.project_id=p.id
LEFT JOIN pwnmesh_project_orchestration o ON o.project_id=p.id
LEFT JOIN fact_counts f ON f.project_id=p.id
LEFT JOIN intent_counts i ON i.project_id=p.id
LEFT JOIN hint_counts h ON h.project_id=p.id
ORDER BY p.created_at,p.rowid`, now, version, intentTimeout, reasonTimeout)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Summary{}
	for rows.Next() {
		var summary model.Summary
		var worker, trigger, started, heartbeat *string
		var curator, curatorTrigger, curatorStarted, curatorHeartbeat *string
		if err := rows.Scan(&summary.ID, &summary.Title, &summary.Status, &summary.Bootstrap, &summary.CreatedAt,
			&worker, &trigger, &started, &heartbeat, &summary.Scenario, &summary.Generation, &summary.RestartedAt, &summary.TerminatedAt,
			&summary.FactCount, &summary.IntentCount, &summary.Working, &summary.Unclaimed, &summary.HintCount,
			&summary.OrchestrationVersion, &curator, &curatorTrigger, &curatorStarted, &curatorHeartbeat); err != nil {
			return nil, err
		}
		if worker != nil {
			summary.Reason = &model.Reason{Worker: *worker, Trigger: model.Value(trigger), StartedAt: model.Value(started), Heartbeat: model.Value(heartbeat)}
		}
		if curator != nil {
			summary.Curator = &model.Reason{Worker: *curator, Trigger: model.Value(curatorTrigger), StartedAt: model.Value(curatorStarted), Heartbeat: model.Value(curatorHeartbeat)}
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}
