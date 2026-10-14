package sqlstore

import (
	"crypto/sha256"
	"encoding/hex"
)

// Archive writes share the caller's transaction with the live round reset.
// Board owns serialization; storage preserves and hashes the exact bytes.
func InsertRound(q Queries, project string, generation int64, archivedAt string) error {
	_, err := q.Exec("INSERT INTO pwnmesh_round_history(project_id,generation,archived_at) VALUES(?,?,?)", project, generation, archivedAt)
	return err
}

func InsertRoundEntry(q Queries, project string, generation int64, kind, id string, raw []byte) error {
	sum := sha256.Sum256(raw)
	_, err := q.Exec("INSERT INTO pwnmesh_round_entries(project_id,generation,kind,id,data,sha256) VALUES(?,?,?,?,?,?)", project, generation, kind, id, raw, hex.EncodeToString(sum[:]))
	return err
}

func RoundExists(q Queries, project string, generation int64) (exists bool, err error) {
	err = q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_round_history WHERE project_id=? AND generation=?)", project, generation).Scan(&exists)
	return
}

// Page visitors expose stored fields in SQL order. Board supplies the lookahead
// limit and derives public continuation cursors without materializing payloads.
func VisitRoundHistory(q Queries, project string, after int64, limit int, visit func(generation int64, archivedAt string) error) error {
	rows, err := q.Query("SELECT generation,archived_at FROM pwnmesh_round_history WHERE project_id=? AND generation>? ORDER BY generation LIMIT ?", project, after, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var generation int64
		var archivedAt string
		if err := rows.Scan(&generation, &archivedAt); err != nil {
			return err
		}
		if err := visit(generation, archivedAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

func VisitRoundEntries(q Queries, project string, generation, after int64, limit int, visit func(rowID int64, kind, id string, size int64, checksum string) error) error {
	rows, err := q.Query("SELECT rowid,kind,id,length(data),sha256 FROM pwnmesh_round_entries WHERE project_id=? AND generation=? AND rowid>? ORDER BY rowid LIMIT ?", project, generation, after, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID, size int64
		var kind, id, checksum string
		if err := rows.Scan(&rowID, &kind, &id, &size, &checksum); err != nil {
			return err
		}
		if err := visit(rowID, kind, id, size, checksum); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Offsets and lengths refer to BLOB bytes, including partial UTF-8 sequences.
// Missing-row and out-of-range policy remain with Board.
func ReadRoundEntry(q Queries, project string, generation, entry, offset int64, limit int) (data []byte, total int64, checksum string, err error) {
	err = q.QueryRow("SELECT substr(data,?,?),length(data),sha256 FROM pwnmesh_round_entries WHERE project_id=? AND generation=? AND rowid=?", offset+1, limit, project, generation, entry).Scan(&data, &total, &checksum)
	return
}
