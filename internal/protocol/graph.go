package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"pwnmesh/internal/board/model"
)

const MaxGraphRPCBytes = 128 << 10

// Leave room for the request ID and the GraphResponse envelope.
const MaxGraphPageBytes = MaxGraphRPCBytes - 1024

type GraphRequest struct {
	RequestID       string                     `json:"request_id"`
	Op              string                     `json:"op"`
	Section         string                     `json:"section,omitempty"`
	Offset          int                        `json:"offset,omitempty"`
	ByteOffset      *int                       `json:"byte_offset,omitempty"`
	RecordVersion   string                     `json:"record_version,omitempty"`
	Limit           int                        `json:"limit,omitempty"`
	ExpectedVersion string                     `json:"expected_version,omitempty"`
	IDs             []string                   `json:"ids,omitempty"`
	AssetIDs        []string                   `json:"asset_ids,omitempty"`
	Action          model.StateAction          `json:"action,omitempty"`
	Batch           *model.DecisionBatch       `json:"batch,omitempty"`
	Updates         *model.ExecuteUpdateCursor `json:"updates,omitempty"`
}

type GraphRequestEvent struct {
	Type    string       `json:"type"`
	Request GraphRequest `json:"request"`
}

type GraphResponse struct {
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func ValidGraphDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}

func ValidGraphRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && id == strings.ToLower(id)
}

func ValidateGraphRequest(j Job, r GraphRequest) error {
	if err := validateGraphAssetFilter(r); err != nil {
		return err
	}
	if !ValidGraphRequestID(r.RequestID) {
		return errors.New("invalid graph request_id")
	}
	if j.Graph.Project.OrchestrationVersion < 0 || j.Graph.Project.OrchestrationVersion > 1 {
		return errors.New("unsupported orchestration protocol")
	}
	if j.Kind == "curate" && !OrchestrationJob(j) {
		return errors.New("curate requires orchestration protocol 1")
	}
	if r.Op == "read_trace_runs" {
		return validateTraceRunRequest(j, r)
	}
	if r.Op == "curate_receipt" {
		if j.Kind != "curate" || !j.GraphRPC || j.Intent != nil || r.Action.Op != "" || r.Batch != nil || r.Updates != nil || r.Section != "" || len(r.IDs) != 0 || r.Offset != 0 || r.ByteOffset != nil || r.Limit != 0 || r.ExpectedVersion != "" || r.RecordVersion != "" {
			return errors.New("curation receipt requires the registered curator identity only")
		}
		return nil
	}
	if r.Op == "read_updates" {
		if !j.GraphRPC || ControlJob(j) || j.Intent == nil || j.RunID == "" || j.Graph.Project.ID == "" {
			return errors.New("execution updates require a registered Execute bridge")
		}
		if r.Section != "" || r.Offset != 0 || r.ByteOffset != nil || r.Limit != 0 || r.ExpectedVersion != "" || r.RecordVersion != "" || len(r.IDs) != 0 || r.Batch != nil || r.Action.Op != "" {
			return errors.New("execution updates use the registered dependencies only")
		}
		if c := r.Updates; c != nil && (c.ProjectID != j.Graph.Project.ID || c.Generation != j.Graph.Project.Generation || c.StepID != j.Intent.ID || c.RunID != j.RunID || c.Revision < 0) {
			return errors.New("execution update cursor identity mismatch")
		}
		return nil
	}
	if r.Updates != nil {
		return errors.New("update cursor requires read_updates")
	}
	if r.Op == "decision_preview" || r.Op == "decision_commit" || r.Op == "decision_receipt" {
		if j.Kind != "reason" || j.Decision == nil || j.Decision.Version != 2 || !j.GraphRPC {
			return errors.New("decision batches require a registered version 2 Decide bridge")
		}
		if r.Op != "decision_receipt" && (r.Batch == nil || len(r.Batch.Actions) > 64 || r.Batch.ExpectedVersion == "") {
			return errors.New("decision batch requires a bound version and at most 64 actions")
		}
		return nil
	}
	if r.Op == "read_graph" || r.Op == "read_snapshot" {
		if err := validateGraphIDs(r); err != nil {
			return err
		}
		if err := validateGraphOffsets(r); err != nil {
			return err
		}
		if !slices.Contains([]string{"", "overview", "facts", "goals", "steps", "findings", "relations", "hints", "evidence", "sources", "candidates", "disputes", "assets", "anchors", "history"}, r.Section) {
			return errors.New("unknown graph section")
		}
		return nil
	}
	if r.Op != "graph_action" {
		return errors.New("unknown graph operation")
	}
	if j.Kind == "reason" && j.Decision != nil && j.Decision.Version == 2 {
		return errors.New("version 2 Decide writes require a batch")
	}
	allowed := GraphActions(j)
	if !slices.Contains(allowed, r.Action.Op) {
		return errors.New("graph action is not allowed in this task mode")
	}
	if j.Kind == "curate" && (ValidateCuratorInput(j) != nil || r.Action.ExpectedVersion != j.CurationVersion()) {
		return errors.New("curation mutation requires its immutable input version")
	}
	if len(r.Action.IdempotencyKey) == 0 || len(r.Action.IdempotencyKey) > 256 || !json.Valid(r.Action.Payload) {
		return errors.New("invalid graph action key or payload")
	}
	return nil
}

// GraphPage retains complete objects when they fit. Oversized evidence/support
// arrays have explicit omission markers and their own lossless detail pages.
// The server obtains State under the current execution fence before calling.
func GraphPage(s model.State, r GraphRequest) (any, error) {
	if err := validateGraphAssetFilter(r); err != nil {
		return nil, err
	}
	if err := validateGraphIDs(r); err != nil {
		return nil, err
	}
	version := model.DecisionStateVersion(s)
	if r.ExpectedVersion != "" && r.ExpectedVersion != version {
		return nil, errors.New("state_changed: graph changed since the previous view; read overview and re-read affected evidence before deciding")
	}
	if err := validateGraphOffsets(r); err != nil {
		return nil, err
	}
	if r.Section == "" {
		r.Section = "overview"
	}
	if r.Limit == 0 {
		r.Limit = 20
	}
	var data any
	if r.Section == "overview" {
		inputs, _ := s.UserInputFacts()
		data = struct {
			Project          model.Project  `json:"project"`
			Inputs           []model.Fact   `json:"user_inputs"`
			Revision         int64          `json:"revision"`
			DecisionRevision int64          `json:"decision_revision"`
			StateVersion     string         `json:"state_version"`
			Counts           map[string]int `json:"counts"`
			Curation         any            `json:"curation,omitempty"`
		}{s.Graph.Project, inputs, s.Revision, s.DecisionRevision, version, map[string]int{
			"facts": len(s.FactRecords), "goals": len(s.Goals), "steps": len(s.Steps),
			"findings": len(s.Findings), "relations": len(s.FactRelations), "hints": len(s.Graph.Hints),
			"candidates": len(s.Candidates), "disputes": len(s.Disputes),
			"assets": len(s.Assets), "anchors": len(s.AssetAnchors), "history": len(s.History()),
		}, s.Curation}
	} else {
		switch r.Section {
		case "evidence", "sources":
			return graphDetails(s, r, version)
		case "facts":
			return graphRecordsPage(s, r, version, s.FactRecords)
		case "goals":
			return graphRecordsPage(s, r, version, s.Goals)
		case "steps":
			return graphRecordsPage(s, r, version, s.Steps)
		case "findings":
			return graphRecordsPage(s, r, version, s.Findings)
		case "candidates":
			return graphRecordsPage(s, r, version, s.Candidates)
		case "disputes":
			return graphRecordsPage(s, r, version, s.Disputes)
		case "relations":
			return graphRecordsPage(s, r, version, s.FactRelations)
		case "hints":
			return graphRecordsPage(s, r, version, s.Graph.Hints)
		case "assets":
			return graphRecordsPage(s, r, version, s.Assets)
		case "anchors":
			return graphRecordsPage(s, r, version, s.AssetAnchors)
		case "history":
			return graphRecordsPage(s, r, version, s.History())
		default:
			return nil, errors.New("unknown graph section")
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if r.ByteOffset != nil {
		if r.Offset != 0 {
			return nil, errors.New("overview record offset must be zero")
		}
		return GraphRecordContent(r, version, raw)
	}
	if len(raw) > MaxGraphPageBytes {
		return GraphRecordReference{true, 0, len(raw), version, GraphRecordVersion(raw), GraphRecordReadMore}, nil
	}
	return data, nil
}

// byte_offset addresses UTF-8 bytes in the JSON encoding of one complete record,
// before support compaction. The state version binds every fragment to that input.
func validateGraphOffsets(r GraphRequest) error {
	if r.Offset < 0 || r.Limit < 0 || r.Limit > 50 {
		return errors.New("graph page requires offset >= 0 and limit <= 50")
	}
	if r.ByteOffset != nil && (*r.ByteOffset < 0 || r.ExpectedVersion == "") {
		return errors.New("byte pages require byte_offset >= 0 and expected_version from the original page")
	}
	if r.ByteOffset != nil && *r.ByteOffset > 0 && r.RecordVersion == "" {
		return errors.New("byte continuation requires record_version from the original page")
	}
	return nil
}

// Select identities before encoding payloads. A byte continuation serializes
// only its bound record; an ordinary page serializes only its requested window.
func graphRecordsPage[T any](s model.State, r GraphRequest, version string, records []T) (any, error) {
	if len(r.IDs) == 0 && len(r.AssetIDs) == 0 {
		return graphItemsPage(s, r, version, records, nil)
	}
	assetFilter, err := graphAssetFilter(s, r)
	if err != nil {
		return nil, err
	}
	selected := make([]T, 0, len(records))
	matched := map[string]bool{}
	for _, record := range records {
		identity := graphIdentity(record)
		if assetFilter != nil && !assetFilter(identity) {
			continue
		}
		include := len(r.IDs) == 0
		for _, id := range r.IDs {
			if id == identity.id || id == identity.source || id == identity.target {
				include, matched[id] = true, true
			}
		}
		if include {
			selected = append(selected, record)
		}
	}
	missing := []string{}
	for _, id := range r.IDs {
		if !matched[id] {
			missing = append(missing, id)
		}
	}
	return graphItemsPage(s, r, version, selected, missing)
}

func graphItemsPage[T any](s model.State, r GraphRequest, version string, records []T, missing []string) (any, error) {
	start := min(r.Offset, len(records))
	end := start + min(r.Limit, len(records)-start)
	if r.ByteOffset != nil {
		if start == len(records) {
			return nil, errors.New("graph record offset is outside the selected records")
		}
		end = start + 1
	}
	items := make([]json.RawMessage, end-start)
	for index := start; index < end; index++ {
		var record any = records[index]
		if candidate, ok := record.(model.Candidate); ok {
			record = s.CandidateView(candidate)
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		items[index-start] = raw
	}
	if r.ByteOffset != nil {
		return GraphRecordContent(r, version, items[0])
	}
	return boundedGraphPage(s, r, version, len(records), items, missing)
}

const GraphRecordReadMore = "Read the same section and ids with offset:record_offset, byte_offset:0, expected_version:state_version and record_version. Concatenate content fragments, following next_byte_offset until absent, then parse the complete JSON record."

type GraphRecordReference struct {
	RecordOmitted bool   `json:"record_omitted"`
	RecordOffset  int    `json:"record_offset"`
	RecordBytes   int    `json:"record_bytes"`
	StateVersion  string `json:"state_version"`
	RecordVersion string `json:"record_version"`
	ReadMore      string `json:"read_more"`
}

type GraphContentPage struct {
	StateVersion   string `json:"state_version"`
	RecordVersion  string `json:"record_version"`
	Section        string `json:"section"`
	Offset         int    `json:"offset"`
	ByteOffset     int    `json:"byte_offset"`
	TotalBytes     int    `json:"total_bytes"`
	NextByteOffset *int   `json:"next_byte_offset,omitempty"`
	Content        string `json:"content"`
}

func GraphRecordContent(r GraphRequest, version string, raw []byte) (GraphContentPage, error) {
	recordVersion := GraphRecordVersion(raw)
	if r.RecordVersion != "" && r.RecordVersion != recordVersion {
		return GraphContentPage{}, errors.New("state_changed: record changed since the previous fragment; re-read this record from byte_offset 0")
	}
	start := *r.ByteOffset
	if start > len(raw) || (start < len(raw) && !utf8.RuneStart(raw[start])) {
		return GraphContentPage{}, errors.New("byte_offset must be within the record at a UTF-8 boundary")
	}
	// JSON-encoded JSON text can expand through quoting and escaping. Keep each
	// fragment small enough even when every byte needs escaping in the envelope.
	end := start + min(len(raw)-start, (MaxGraphPageBytes-1024)/6)
	for end < len(raw) && !utf8.RuneStart(raw[end]) {
		end--
	}
	page := GraphContentPage{StateVersion: version, RecordVersion: recordVersion, Section: r.Section, Offset: r.Offset, ByteOffset: start, TotalBytes: len(raw), Content: string(raw[start:end])}
	if end < len(raw) {
		page.NextByteOffset = &end
	}
	return page, nil
}

func GraphRecordVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func graphDetails(s model.State, r GraphRequest, version string) (any, error) {
	if r.Section == "evidence" {
		for _, fact := range s.FactRecords {
			if fact.ID == r.IDs[0] {
				return graphItemsPage(s, r, version, fact.Evidence, nil)
			}
		}
	}
	for _, finding := range s.Findings {
		if finding.ID == r.IDs[0] {
			if r.Section == "sources" {
				return graphItemsPage(s, r, version, finding.Sources, nil)
			}
			return graphItemsPage(s, r, version, finding.Evidence, nil)
		}
	}
	for _, candidate := range s.Candidates {
		if candidate.ID == r.IDs[0] {
			if r.Section == "sources" {
				return graphItemsPage(s, r, version, candidate.Sources, nil)
			}
			return graphItemsPage(s, r, version, candidate.Evidence, nil)
		}
	}
	return graphItemsPage[json.RawMessage](s, r, version, nil, r.IDs)
}

func boundedGraphPage(s model.State, r GraphRequest, version string, total int, items []json.RawMessage, missing []string) (GraphItemsPage, error) {
	start := min(r.Offset, total)
	end := start + len(items)
	value := GraphItemsPage{Section: r.Section, Offset: start, Total: total, Revision: s.Revision, Generation: s.Graph.Project.Generation, StateVersion: version, RequestedIDs: r.IDs, MissingIDs: missing}
	// Echoing the filter is optional; missing IDs are authoritative. Repeating
	// maximally escaped IDs in both fields can consume the entire response frame.
	if raw, err := json.Marshal(value); err != nil {
		return GraphItemsPage{}, err
	} else if len(raw) > MaxGraphPageBytes/2 {
		value.RequestedIDs = nil
	}
	for {
		value.Items = items[:end-start]
		if value.Items == nil {
			value.Items = []json.RawMessage{}
		}
		value.NextOffset = nil
		if end < total {
			next := end
			value.NextOffset = &next
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return GraphItemsPage{}, err
		}
		if len(raw) <= MaxGraphPageBytes {
			return value, nil
		}
		if end-start > 1 {
			end--
			continue
		}
		if end > start {
			compact, err := compactGraphRecord(r.Section, items[0])
			if err != nil {
				return GraphItemsPage{}, err
			}
			value.Items = []json.RawMessage{compact}
			raw, err = json.Marshal(value)
			if err != nil {
				return GraphItemsPage{}, err
			}
			if len(raw) <= MaxGraphPageBytes {
				return value, nil
			}
			reference, err := json.Marshal(GraphRecordReference{true, start, len(items[0]), version, GraphRecordVersion(items[0]), GraphRecordReadMore})
			if err != nil {
				return GraphItemsPage{}, err
			}
			value.Items = []json.RawMessage{reference}
			return value, nil
		}
		return GraphItemsPage{}, fmt.Errorf("graph %s record at offset %d has scalar content exceeding the response budget; reducing limit cannot retrieve it", r.Section, start)
	}
}

func compactGraphRecord(section string, raw json.RawMessage) (json.RawMessage, error) {
	if section != "facts" && section != "findings" && section != "candidates" {
		return raw, nil
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	omitted := false
	for _, field := range []string{"evidence", "sources"} {
		var entries []json.RawMessage
		if len(record[field]) == 0 {
			continue
		}
		if err := json.Unmarshal(record[field], &entries); err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		delete(record, field)
		record[field+"_omitted"] = json.RawMessage("true")
		record[field+"_count"], _ = json.Marshal(len(entries))
		omitted = true
	}
	if omitted {
		// The runtime can retain this complete metadata projection when restoring
		// all support would exceed its logical read budget. Bind smaller automatic
		// restorations to the original record, just like scalar continuations.
		record["record_bytes"], _ = json.Marshal(len(raw))
		record["record_version"], _ = json.Marshal(GraphRecordVersion(raw))
		record["read_more"], _ = json.Marshal("Omitted support is not absent. Use the same read tool with section evidence or sources, ids:[this record's id], offset:0; follow next_offset until absent. Sources pages apply to Findings and Candidates.")
	}
	return json.Marshal(record)
}

// CompactGraphActionResult keeps the successful write acknowledgement within
// the bridge frame. The saved entity remains available through graph pages.
type CompactFactPublication struct {
	model.FactRecord
	EvidenceExcerptsOmitted bool     `json:"evidence_excerpts_omitted,omitempty"`
	EvidenceExcerptSHA256   []string `json:"evidence_excerpt_sha256,omitempty"`
}

func CompactGraphActionResult(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) <= MaxGraphPageBytes {
		return raw, nil
	}
	var result model.StateActionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Op == "fact" {
		// Keep the publication's identity and exact evidence selection usable by
		// finish_step. Excerpts already exist in retained files and the original
		// tool call; their hashes bind reconstruction without another graph read.
		var fact model.FactRecord
		if err := json.Unmarshal(result.Result, &fact); err != nil {
			return nil, err
		}
		publication := CompactFactPublication{FactRecord: fact, EvidenceExcerptsOmitted: true}
		for i := range publication.Evidence {
			publication.EvidenceExcerptSHA256 = append(publication.EvidenceExcerptSHA256, GraphRecordVersion([]byte(publication.Evidence[i].Excerpt)))
			publication.Evidence[i].Excerpt = ""
		}
		projected, err := json.Marshal(publication)
		if err != nil {
			return nil, err
		}
		result.Result = projected
		compact, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if len(compact) <= MaxGraphPageBytes {
			return compact, nil
		}
	}
	return json.Marshal(struct {
		Op            string   `json:"op"`
		ID            string   `json:"id"`
		Revision      int64    `json:"revision"`
		StateVersion  string   `json:"state_version,omitempty"`
		Unchanged     bool     `json:"unchanged,omitempty"`
		Committed     bool     `json:"committed,omitempty"`
		ResultOmitted bool     `json:"result_omitted"`
		ReadMore      string   `json:"read_more"`
		AssetIDs      []string `json:"asset_ids,omitempty"`
	}{result.Op, result.ID, result.Revision, result.StateVersion, result.Unchanged, result.Committed, true, "Write succeeded. Read this entity by id in its graph section; follow any evidence_omitted or sources_omitted detail pages. Do not resubmit the write because its entity payload is omitted.", result.AssetIDs})
}

type GraphItemsPage struct {
	RequestedIDs []string          `json:"requested_ids,omitempty"`
	MissingIDs   []string          `json:"missing_ids,omitempty"`
	Revision     int64             `json:"revision"`
	Generation   int64             `json:"generation"`
	StateVersion string            `json:"state_version"`
	Section      string            `json:"section"`
	Offset       int               `json:"offset"`
	Total        int               `json:"total"`
	NextOffset   *int              `json:"next_offset,omitempty"`
	Items        []json.RawMessage `json:"items"`
}

func validateGraphIDs(r GraphRequest) error {
	if (r.Section == "evidence" || r.Section == "sources") && len(r.IDs) != 1 {
		return errors.New("evidence and sources pages require exactly one record ID; evidence accepts Fact or Finding IDs, sources accepts a Finding ID")
	}
	if len(r.IDs) > 50 {
		return errors.New("at most 50 graph IDs are allowed")
	}
	if len(r.IDs) > 0 && (r.Section == "" || r.Section == "overview") {
		return errors.New("graph IDs require a specific section")
	}
	seen := map[string]bool{}
	for _, id := range r.IDs {
		if strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			return errors.New("graph IDs must be unique, nonempty and at most 256 bytes")
		}
		seen[id] = true
	}
	return nil
}
