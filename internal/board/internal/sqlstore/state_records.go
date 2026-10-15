package sqlstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

var stateRecordKinds = [...]string{"goals", "steps", "findings", "fact_relations", "candidates", "disputes"}

// Go accepts case-folded fields and merges repeated struct fields in input
// order. Preserve these historical headers instead of collapsing them to a map.
func keepStateHeaderInline(raw string) bool {
	decoder := json.NewDecoder(strings.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return true
	}
	known := append(stateRecordKinds[:], "facts", "curation")
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return true
		}
		name, ok := token.(string)
		if !ok {
			return true
		}
		for _, kind := range known {
			if strings.EqualFold(name, kind) {
				if name != kind || seen[kind] {
					return true
				}
				seen[kind] = true
				break
			}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return true
		}
	}
	return false
}

// Empty array markers distinguish present arrays from null and omitted fields.
// Their payloads move into ordered records; small scalar metadata stays inline.
func splitStateRecords(raw string) (string, map[string][]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields == nil || keepStateHeaderInline(raw) {
		return raw, nil, nil
	}
	records := make(map[string][]json.RawMessage, len(stateRecordKinds))
	changed := false
	for _, kind := range stateRecordKinds {
		value := fields[kind]
		if value == nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil {
			return "", nil, fmt.Errorf("invalid %s metadata: %w", kind, err)
		}
		for n, item := range items {
			var compact bytes.Buffer
			if err := json.Compact(&compact, item); err != nil {
				return "", nil, err
			}
			items[n] = append(json.RawMessage(nil), compact.Bytes()...)
		}
		records[kind] = items
		if len(items) != 0 {
			fields[kind] = json.RawMessage("[]")
			changed = true
		}
	}
	if !changed {
		return raw, records, nil
	}
	header, err := json.Marshal(fields)
	return string(header), records, err
}

func restoreStateRecords(q Queries, project string, fields map[string]json.RawMessage) (bool, error) {
	rows, err := q.Query("SELECT kind,data FROM pwnmesh_state_records WHERE project_id=? ORDER BY kind,position", project)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	records := map[string][]json.RawMessage{}
	for rows.Next() {
		var kind string
		var raw []byte
		if err := rows.Scan(&kind, &raw); err != nil {
			return false, err
		}
		records[kind] = append(records[kind], raw)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	changed := false
	for kind, items := range records {
		// Legacy inline arrays remain readable until their next normalized write.
		if !emptyStateArray(fields[kind]) {
			continue
		}
		fields[kind], err = json.Marshal(items)
		if err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func emptyStateArray(raw []byte) bool {
	var items []json.RawMessage
	return json.Unmarshal(raw, &items) == nil && items != nil && len(items) == 0
}

func saveStateRecords(q Queries, project string, records map[string][]json.RawMessage) error {
	type key struct {
		kind     string
		position int
	}
	rows, err := q.Query("SELECT kind,position,data FROM pwnmesh_state_records WHERE project_id=? ORDER BY kind,position", project)
	if err != nil {
		return err
	}
	previous := map[key]string{}
	for rows.Next() {
		var entry key
		var data string
		if err := rows.Scan(&entry.kind, &entry.position, &data); err != nil {
			rows.Close()
			return err
		}
		previous[entry] = data
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, kind := range stateRecordKinds {
		items := records[kind]
		for position, item := range items {
			if previous[key{kind, position}] == string(item) {
				continue
			}
			var identity struct {
				ID string `json:"id"`
			}
			if kind == "steps" {
				// Go's field matching and duplicate-key order define the identity,
				// including retained ID/Id spellings that SQLite JSON paths miss.
				if err := json.Unmarshal(item, &identity); err != nil {
					return fmt.Errorf("invalid Step identity: %w", err)
				}
			}
			if _, err := q.Exec("INSERT INTO pwnmesh_state_records(project_id,kind,position,data,record_id) VALUES(?,?,?,?,?) ON CONFLICT(project_id,kind,position) DO UPDATE SET data=excluded.data,record_id=excluded.record_id", project, kind, position, string(item), identity.ID); err != nil {
				return err
			}
		}
		if _, err := q.Exec("DELETE FROM pwnmesh_state_records WHERE project_id=? AND kind=? AND position>=?", project, kind, len(items)); err != nil {
			return err
		}
	}
	return nil
}
