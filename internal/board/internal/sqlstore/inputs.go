package sqlstore

import "pwnmesh/internal/board/model"

// Input reads keep file contents separate from metadata. Board projects the
// execution path and owns identities, quota checks and upload instructions.
func scanInputFile(row interface{ Scan(...any) error }) (model.InputFile, error) {
	var file model.InputFile
	err := row.Scan(&file.ID, &file.Name, &file.Size, &file.SHA256, &file.CreatedAt)
	return file, err
}

func InputFiles(q Queries, project string) ([]model.InputFile, error) {
	rows, err := q.Query("SELECT id,name,size,sha256,created_at FROM pwnmesh_input_files WHERE project_id=? ORDER BY created_at,id", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []model.InputFile{}
	for rows.Next() {
		file, err := scanInputFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func InputFile(q Queries, project, id string) (model.InputFile, error) {
	return scanInputFile(q.QueryRow("SELECT id,name,size,sha256,created_at FROM pwnmesh_input_files WHERE project_id=? AND id=?", project, id))
}

func InputData(q Queries, project, id string) ([]byte, error) {
	var data []byte
	err := q.QueryRow("SELECT data FROM pwnmesh_input_files WHERE project_id=? AND id=?", project, id).Scan(&data)
	return data, err
}

func InputUsage(q Queries, project string) (count int, size int64, err error) {
	err = q.QueryRow("SELECT COUNT(*),COALESCE(SUM(size),0) FROM pwnmesh_input_files WHERE project_id=?", project).Scan(&count, &size)
	return
}

func InsertInputFile(q Queries, project string, file model.InputFile, data []byte) error {
	_, err := q.Exec("INSERT INTO pwnmesh_input_files(project_id,id,name,size,sha256,created_at,data) VALUES(?,?,?,?,?,?,?)", project, file.ID, file.Name, file.Size, file.SHA256, file.CreatedAt, data)
	return err
}

// Upload receipts retain request digests and response JSON verbatim. Board
// decides whether a key can be replayed and how to decode its response.
func InputBatchReceipt(q Queries, project, key string) (digest, response string, err error) {
	err = q.QueryRow("SELECT request_sha256,response FROM pwnmesh_input_batches WHERE project_id=? AND idempotency_key=?", project, key).Scan(&digest, &response)
	return
}

func InsertInputBatchReceipt(q Queries, project, key, digest, response string) error {
	_, err := q.Exec("INSERT INTO pwnmesh_input_batches(project_id,idempotency_key,request_sha256,response) VALUES(?,?,?,?)", project, key, digest, response)
	return err
}
