package sqlstore

// Receipt storage retains request and response text without interpreting it.
// Board owns canonicalization, replay validation and missing-receipt policy.
func ActionReceipt(q Queries, project, key string) (request, response string, err error) {
	err = q.QueryRow("SELECT request,response FROM pwnmesh_state_actions WHERE project_id=? AND idempotency_key=?", project, key).Scan(&request, &response)
	return
}

func InsertActionReceipt(q Queries, project, key, request, response string) error {
	_, err := q.Exec("INSERT INTO pwnmesh_state_actions(project_id,idempotency_key,request,response) VALUES(?,?,?,?)", project, key, request, response)
	return err
}

func CurationResponse(q Queries, project, run string) (response string, err error) {
	err = q.QueryRow(`SELECT a.response FROM pwnmesh_state_actions a WHERE a.project_id=? AND json_extract(a.request,'$.Run')=? AND json_extract(a.request,'$.Op')='curate' LIMIT 1`, project, run).Scan(&response)
	return
}

func HasCurationReceipt(q Queries, project, run string) (exists bool, err error) {
	err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM pwnmesh_state_actions WHERE project_id=? AND json_extract(request,'$.Run')=? AND json_extract(request,'$.Op')='curate')`, project, run).Scan(&exists)
	return
}

// VisitActionReceipts streams insertion order in the caller's transaction.
// The visitor owns archival encoding, including support for legacy text.
func VisitActionReceipts(q Queries, project string, visit func(key, request, response string) error) error {
	rows, err := q.Query("SELECT idempotency_key,request,response FROM pwnmesh_state_actions WHERE project_id=? ORDER BY rowid", project)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, request, response string
		if err := rows.Scan(&key, &request, &response); err != nil {
			return err
		}
		if err := visit(key, request, response); err != nil {
			return err
		}
	}
	return rows.Err()
}

func DeleteActionReceipts(q Queries, project string) error {
	_, err := q.Exec("DELETE FROM pwnmesh_state_actions WHERE project_id=?", project)
	return err
}
