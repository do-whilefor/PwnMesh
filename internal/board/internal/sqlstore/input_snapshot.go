package sqlstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const snapshotChunkPrefix = "PwnMesh snapshot chunks v1\n"
const snapshotChunkMin = 1 << 10
const snapshotChunkMax = 16 << 10
const snapshotReadPageChunks = 128

type snapshotManifest struct {
	Bytes  int      `json:"bytes"`
	Chunks []string `json:"chunks"`
}

// Content-defined boundaries resynchronize after inserted observations or lease
// changes. A fixed-size split would shift and duplicate every later chunk.
// The table is deterministic and has no cryptographic role; SHA-256 binds each
// retained chunk and the existing snapshot ID binds the reconstructed bytes.
var snapshotGear = func() [256]uint64 {
	var table [256]uint64
	for i := range table {
		x := uint64(i) + 0x9e3779b97f4a7c15
		x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
		x = (x ^ (x >> 27)) * 0x94d049bb133111eb
		table[i] = x ^ (x >> 31)
	}
	return table
}()

func snapshotChunks(raw []byte) [][]byte {
	var chunks [][]byte
	start, rolling := 0, uint64(0)
	for n, b := range raw {
		rolling = (rolling << 1) + snapshotGear[b]
		size := n + 1 - start
		if size >= snapshotChunkMin && ((rolling>>32)&4095 == 0 || size == snapshotChunkMax) {
			chunks = append(chunks, raw[start:n+1])
			start, rolling = n+1, 0
		}
	}
	if start < len(raw) {
		chunks = append(chunks, raw[start:])
	}
	return chunks
}

func encodeInputSnapshot(q Queries, project string, raw []byte) ([]byte, error) {
	// Small inputs cost less than a manifest and its chunk lookups. Preserve
	// their exact inline representation.
	if len(raw) <= snapshotChunkMax {
		return raw, nil
	}
	manifest := snapshotManifest{Bytes: len(raw)}
	for _, chunk := range snapshotChunks(raw) {
		digest := sha256.Sum256(chunk)
		id := hex.EncodeToString(digest[:])
		if _, err := q.Exec("INSERT OR IGNORE INTO pwnmesh_snapshot_chunks(project_id,sha256,data) VALUES(?,?,?)", project, id, chunk); err != nil {
			return nil, err
		}
		manifest.Chunks = append(manifest.Chunks, id)
	}
	encoded, err := json.Marshal(manifest)
	return append([]byte(snapshotChunkPrefix), encoded...), err
}

func decodeInputSnapshot(q Queries, project string, stored []byte) ([]byte, error) {
	if !bytes.HasPrefix(stored, []byte(snapshotChunkPrefix)) {
		return stored, nil // Current small snapshots store their exact JSON inline.
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(stored[len(snapshotChunkPrefix):], &manifest); err != nil {
		return nil, errors.New("invalid input snapshot chunk manifest")
	}
	// Validate the declared expansion before reading chunks; never allocate
	// from an unchecked byte count in a corrupt or partially restored database.
	count := len(manifest.Chunks)
	if count == 0 || count > 1<<20 || manifest.Bytes <= 0 || int64(manifest.Bytes) > int64(count)*snapshotChunkMax || int64(manifest.Bytes) < int64(count-1)*snapshotChunkMin+1 {
		return nil, errors.New("invalid input snapshot chunk bounds")
	}
	var raw bytes.Buffer
	for start := 0; start < count; start += snapshotReadPageChunks {
		if err := appendSnapshotChunkPage(q, project, manifest, start, &raw); err != nil {
			return nil, err
		}
	}
	if raw.Len() != manifest.Bytes {
		return nil, errors.New("input snapshot chunk length mismatch")
	}
	return raw.Bytes(), nil
}

// Bound each ordered join to 128 chunks, limiting chunk payloads in any SQLite
// sort buffer to 2 MiB. Manifest positions retain duplicate identities and order;
// a left join makes missing and oversized chunks visible instead of skipping them.
func appendSnapshotChunkPage(q Queries, project string, manifest snapshotManifest, start int, raw *bytes.Buffer) error {
	ids := manifest.Chunks[start:min(start+snapshotReadPageChunks, len(manifest.Chunks))]
	digests := make([][]byte, len(ids))
	for n, id := range ids {
		digest, err := hex.DecodeString(id)
		if err != nil || len(digest) != sha256.Size {
			return errors.New("invalid input snapshot chunk identity")
		}
		digests[n] = digest
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := q.Query(`SELECT entry.key,chunk.data FROM json_each(?) AS entry
LEFT JOIN pwnmesh_snapshot_chunks AS chunk ON chunk.project_id=? AND chunk.sha256=entry.value AND length(chunk.data)<=?
ORDER BY entry.key`, string(encoded), project, snapshotChunkMax)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var position int
		var chunk []byte
		if err := rows.Scan(&position, &chunk); err != nil {
			return err
		}
		if position != n || n >= len(ids) || chunk == nil {
			return errors.New("input snapshot chunk is missing or oversized")
		}
		actual := sha256.Sum256(chunk)
		if !bytes.Equal(digests[n], actual[:]) || len(chunk) == 0 || start+n < len(manifest.Chunks)-1 && len(chunk) < snapshotChunkMin || len(chunk) > manifest.Bytes-raw.Len() {
			return errors.New("input snapshot chunk digest or size mismatch")
		}
		raw.Write(chunk)
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != len(ids) {
		return errors.New("input snapshot chunk is missing or oversized")
	}
	return nil
}

// SaveInputSnapshot retains chunks and their manifest in the caller's transaction.
// A local savepoint also removes partial chunks when the caller handles a failure.
func SaveInputSnapshot(q Queries, project, id string, metadata, raw []byte) (err error) {
	var exists bool
	if err = q.QueryRow("SELECT EXISTS(SELECT 1 FROM pwnmesh_input_snapshots WHERE project_id=? AND id=?)", project, id).Scan(&exists); err != nil || exists {
		return err
	}
	if _, err = q.Exec("SAVEPOINT pwnmesh_input_snapshot"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, rollbackErr := q.Exec("ROLLBACK TO pwnmesh_input_snapshot")
			err = errors.Join(err, rollbackErr)
		}
		_, releaseErr := q.Exec("RELEASE pwnmesh_input_snapshot")
		err = errors.Join(err, releaseErr)
	}()
	stored, err := encodeInputSnapshot(q, project, raw)
	if err != nil {
		return err
	}
	_, err = q.Exec("INSERT OR IGNORE INTO pwnmesh_input_snapshots(project_id,id,metadata,state) VALUES(?,?,?,?)", project, id, metadata, stored)
	return err
}

func ReadInputSnapshotMetadata(q Queries, project, id string) ([]byte, error) {
	var raw []byte
	err := q.QueryRow("SELECT metadata FROM pwnmesh_input_snapshots WHERE project_id=? AND id=?", project, id).Scan(&raw)
	return raw, err
}

// ReadInputSnapshot verifies the retained bytes before they cross into Board's
// state decoder. Inline and chunked snapshots share the same content identity.
func ReadInputSnapshot(q Queries, project, id string) ([]byte, error) {
	var raw []byte
	err := q.QueryRow("SELECT state FROM pwnmesh_input_snapshots WHERE project_id=? AND id=?", project, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if raw, err = decodeInputSnapshot(q, project, raw); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != id {
		return nil, errors.New("input snapshot digest mismatch")
	}
	return raw, nil
}
