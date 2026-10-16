package board

import (
	"encoding/json"

	"pwnmesh/internal/board/internal/sqlstore"
)

// Fact and Candidate publication only appends one immutable metadata record.
// Keep null/empty markers and curation intact without encoding historical arrays.
func (t *Tx) saveAppendedState(project string, d stateData, op string, record []byte, revision, decision int64) error {
	header := map[string]any{
		"goals": d.Goals[:0], "steps": d.Steps[:0], "facts": d.Facts[:0],
		"findings": d.Findings[:0], "fact_relations": d.FactRelations[:0], "curation": d.Curation,
	}
	if len(d.Candidates) > 0 {
		header["candidates"] = []Candidate{}
	}
	if len(d.Disputes) > 0 {
		header["disputes"] = []Dispute{}
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return err
	}
	kind, position := "facts", len(d.Facts)-1
	if op == "candidate" {
		kind, position = "candidates", len(d.Candidates)-1
	}
	return sqlstore.AppendStateRecord(t, project, string(raw), kind, position, record, revision, decision)
}
