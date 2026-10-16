package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"pwnmesh/internal/board/internal/sqlstore"
	"strconv"
)

// Archive the prior round before removing its current projections. The archive
// and reset share the caller's transaction; any failure preserves the live round.
// These records have no update path and never grant authority to an old Worker.
func (t *Tx) archiveRound(project string) error {
	state, err := t.State(project)
	if err != nil {
		return err
	}
	generation := state.Graph.Project.Generation
	if err = sqlstore.InsertRound(t.transactionQueries, project, generation, t.Now); err != nil {
		return err
	}
	save := func(kind, id string, raw []byte) error {
		return sqlstore.InsertRoundEntry(t.transactionQueries, project, generation, kind, id, raw)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = save("state", "state", raw); err != nil {
		return err
	}
	// Capture membership under the same write transaction as the round. Upload
	// timestamps have only second precision and cannot identify this boundary.
	inputs, err := t.InputFiles(project)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(inputs)
	if err != nil {
		return err
	}
	if err = save("inputs", "inputs", raw); err != nil {
		return err
	}
	// Stream records one by one rather than materializing every historical Job
	// or all evidence-bearing events in an archive-sized in-memory slice.
	err = sqlstore.VisitStateEvents(t.transactionQueries, project, nil, 0, func(revision int64, event []byte) error {
		return save("event", strconv.FormatInt(revision, 10), event)
	})
	if err != nil {
		return err
	}
	err = sqlstore.VisitActionReceipts(t.transactionQueries, project, func(id, request, response string) error {
		// Keep even legacy request/response text byte-for-byte.
		data, err := json.Marshal(map[string]string{"request": request, "response": response})
		if err != nil {
			return err
		}
		return save("action", id, data)
	})
	if err != nil {
		return err
	}
	return sqlstore.VisitExecutions(t.transactionQueries, project, func(execution Execution) error {
		raw, err := json.Marshal(execution)
		if err != nil {
			return err
		}
		return save("execution", execution.ID, raw)
	})
}

type RoundSummary struct {
	Generation int64  `json:"generation"`
	ArchivedAt string `json:"archived_at"`
}

type RoundPage struct {
	Items      []RoundSummary `json:"items"`
	NextCursor *int64         `json:"next_cursor,omitempty"`
}

func (t *Tx) RoundHistory(project string, after int64, limit int) (RoundPage, error) {
	out := RoundPage{Items: []RoundSummary{}}
	if after < -1 || limit < 1 || limit > 100 {
		return out, Err(422, "round history requires cursor >= -1 and limit between 1 and 100")
	}
	exists, err := sqlstore.ProjectExists(t.transactionQueries, project)
	if err != nil {
		return out, err
	}
	if !exists {
		return out, Err(404, "Project not found")
	}
	err = sqlstore.VisitRoundHistory(t.transactionQueries, project, after, limit+1, func(generation int64, archivedAt string) error {
		if len(out.Items) == limit {
			cursor := out.Items[len(out.Items)-1].Generation
			out.NextCursor = &cursor
		} else {
			out.Items = append(out.Items, RoundSummary{Generation: generation, ArchivedAt: archivedAt})
		}
		return nil
	})
	return out, err
}

type RoundEntry struct {
	Cursor int64  `json:"cursor"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type RoundEntryPage struct {
	Items      []RoundEntry `json:"items"`
	NextCursor *int64       `json:"next_cursor,omitempty"`
}

func (t *Tx) requireRound(project string, generation int64) error {
	exists, err := sqlstore.RoundExists(t.transactionQueries, project, generation)
	if err != nil {
		return err
	}
	if !exists {
		return Err(404, "Archived round not found")
	}
	return nil
}

func (t *Tx) RoundEntries(project string, generation, after int64, limit int) (RoundEntryPage, error) {
	out := RoundEntryPage{Items: []RoundEntry{}}
	if generation < 0 || after < 0 || limit < 1 || limit > 100 {
		return out, Err(422, "invalid archived round cursor or limit")
	}
	if err := t.requireRound(project, generation); err != nil {
		return out, err
	}
	err := sqlstore.VisitRoundEntries(t.transactionQueries, project, generation, after, limit+1, func(rowID int64, kind, id string, size int64, checksum string) error {
		if len(out.Items) == limit {
			cursor := out.Items[len(out.Items)-1].Cursor
			out.NextCursor = &cursor
		} else {
			out.Items = append(out.Items, RoundEntry{Cursor: rowID, Kind: kind, ID: id, Bytes: size, SHA256: checksum})
		}
		return nil
	})
	return out, err
}

type RoundEntryChunk struct {
	Data       []byte `json:"data"` // JSON encodes byte chunks as base64, including split UTF-8.
	Encoding   string `json:"encoding"`
	Offset     int64  `json:"offset"`
	TotalBytes int64  `json:"total_bytes"`
	NextOffset *int64 `json:"next_offset,omitempty"`
	SHA256     string `json:"sha256"`
}

func (t *Tx) ReadRoundEntry(project string, generation, entry, offset int64, limit int) (RoundEntryChunk, error) {
	out := RoundEntryChunk{Encoding: "base64", Offset: offset}
	if generation < 0 || entry < 1 || offset < 0 || limit < 1 || limit > 32<<10 {
		return out, Err(422, "invalid archive entry; byte limit must be between 1 and 32768")
	}
	var err error
	out.Data, out.TotalBytes, out.SHA256, err = sqlstore.ReadRoundEntry(t.transactionQueries, project, generation, entry, offset, limit)
	if errors.Is(err, sql.ErrNoRows) {
		return out, Err(404, "Archived entry not found")
	}
	if err != nil {
		return out, err
	}
	if offset > out.TotalBytes {
		return out, Err(422, "archive offset exceeds entry size")
	}
	if next := offset + int64(len(out.Data)); next < out.TotalBytes {
		out.NextOffset = &next
	}
	return out, nil
}
