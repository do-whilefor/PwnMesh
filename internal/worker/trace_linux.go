//go:build linux

package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
	board "pwnmesh/internal/board/model"
	eventlog "pwnmesh/internal/worker/internal/journal"
)

const maxTraceSessionBytes = 16 << 20
const maxTracePageBytes = 16 << 10
const maxTraceVersionBytes = 8 << 10

type traceReadRequest struct {
	RunID        string `json:"run_id,omitempty"`
	Query        string `json:"query,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	Record       string `json:"record,omitempty"`
	TraceVersion string `json:"trace_version,omitempty"`
	ByteOffset   int    `json:"byte_offset,omitempty"`
	ByteLimit    int    `json:"byte_limit,omitempty"`
}

type traceRunPage struct {
	Items      []board.TraceRun `json:"items"`
	NextOffset *int             `json:"next_offset,omitempty"`
}

type traceBinding struct {
	ProjectID  string `json:"project_id"`
	Generation int64  `json:"generation"`
	RunID      string `json:"run_id"`
	Offset     int64  `json:"offset"`
	SHA256     string `json:"sha256"`
	Query      string `json:"query,omitempty"`
	checkpoint eventlog.Checkpoint
}

type traceObservation struct {
	Record    string `json:"record"`
	Sequence  uint64 `json:"sequence"`
	Tool      string `json:"tool,omitempty"`
	Subject   string `json:"subject,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Content   string `json:"content"`
	Redacted  bool   `json:"redacted,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// workerTraceTool deliberately returns a projection of tool results, never the
// session, system/user messages or internal run capability. Own-run reads also
// expose bounded, redacted command/path labels for locating retained work.
func workerTraceTool(j Job, o *Options, request func(context.Context, GraphRequest) (string, error)) agent.Tool {
	return agent.Tool{Definition: agent.Definition{
		Name:        "read_worker_trace",
		Description: "Find retained tool observations from registered Explore runs in this project and round, including the current run. With no run_id, list runs using offset/limit. With run_id, search its committed journal using optional case-insensitive query; follow next_offset with trace_version. Read a returned record using run_id, record, trace_version and byte_offset/byte_limit. Own-run subjects identify commands and paths. Text is a credential-redacted projection, not original evidence or a verified Fact; is_error preserves failure status. Nested agents and control roles are excluded. Missing journals or oversized individual records are explicitly unavailable.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string","maxLength":256},"query":{"type":"string","maxLength":256},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":20},"record":{"type":"string","maxLength":64},"trace_version":{"type":"string","maxLength":8192},"byte_offset":{"type":"integer","minimum":0},"byte_limit":{"type":"integer","minimum":4,"maximum":16384}},"additionalProperties":false}`),
	}, Execute: func(parent context.Context, input json.RawMessage) (string, error) {
		if !j.GraphRPC || j.Kind != "explore" || j.Intent == nil || j.RunID == "" || request == nil {
			return "", errors.New("trace reads require a registered Explore bridge")
		}
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		var r traceReadRequest
		if trimmed := bytes.TrimSpace(input); len(trimmed) == 0 || trimmed[0] != '{' {
			return "", errors.New("read_worker_trace requires one JSON object")
		}
		decoder := json.NewDecoder(bytes.NewReader(input))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&r); err != nil {
			return "", err
		}
		if decoder.Decode(new(any)) != io.EOF {
			return "", errors.New("read_worker_trace requires one JSON object")
		}
		if err := validateTraceRequest(r); err != nil {
			return "", err
		}
		if r.Limit == 0 {
			r.Limit = 20
		}
		if r.ByteLimit == 0 {
			r.ByteLimit = maxTracePageBytes
		}
		lookup := GraphRequest{Op: "read_trace_runs", Offset: r.Offset, Limit: r.Limit}
		if r.RunID != "" {
			lookup.IDs, lookup.Offset, lookup.Limit = []string{r.RunID}, 0, 1
		}
		raw, err := request(ctx, lookup)
		if err != nil {
			return "", err
		}
		var authorized traceRunPage
		if err := json.Unmarshal([]byte(raw), &authorized); err != nil {
			return "", err
		}
		if len(authorized.Items) > lookup.Limit {
			return "", errors.New("invalid trace authorization page")
		}
		for _, run := range authorized.Items {
			if run.ProjectID != j.Graph.Project.ID || run.Generation != j.Graph.Project.Generation || run.Workspace != j.Workspace || !validTraceRunID(run.RunID) {
				return "", errors.New("trace authorization identity mismatch")
			}
		}
		if r.RunID == "" {
			type summary struct {
				RunID  string `json:"run_id"`
				StepID string `json:"step_id"`
				Status string `json:"status"`
			}
			items := []summary{}
			for _, run := range authorized.Items {
				items = append(items, summary{run.RunID, run.StepID, run.Status})
			}
			out, err := marshalTrace(ctx, struct {
				Items      []summary `json:"items"`
				NextOffset *int      `json:"next_offset,omitempty"`
			}{items, authorized.NextOffset})
			if len(out) > maxTracePageBytes {
				return "", errors.New("trace unavailable: run metadata exceeds page budget; use a smaller limit")
			}
			return string(out), err
		}
		if len(authorized.Items) != 1 || authorized.Items[0].RunID != r.RunID {
			return "", errors.New("trace unavailable: run is absent from this project's current Explore executions")
		}
		return readWorkerTrace(ctx, j, authorized.Items[0], r)
	}}
}

func validateTraceRequest(r traceReadRequest) error {
	if r.Offset < 0 || r.Limit < 0 || r.Limit > 20 || len(r.Query) > 256 || len(r.TraceVersion) > maxTraceVersionBytes || len(r.Record) > 64 || r.ByteOffset < 0 || r.ByteLimit != 0 && (r.ByteLimit < 4 || r.ByteLimit > maxTracePageBytes) {
		return errors.New("invalid trace page bounds")
	}
	if r.RunID == "" {
		if r.Query != "" || r.Record != "" || r.TraceVersion != "" || r.ByteOffset != 0 || r.ByteLimit != 0 {
			return errors.New("select a listed run_id before searching or reading observations")
		}
		return nil
	}
	if !validTraceRunID(r.RunID) {
		return errors.New("trace requires a registered run_id, never a path")
	}
	if r.Record != "" {
		if r.TraceVersion == "" || r.Query != "" || r.Offset != 0 || r.Limit != 0 {
			return errors.New("trace record reads require trace_version and byte pagination only")
		}
	} else if r.ByteOffset != 0 || r.ByteLimit != 0 || r.Offset > 0 && r.TraceVersion == "" {
		return errors.New("trace search continuation requires trace_version; byte pagination requires record")
	}
	return nil
}

func validTraceRunID(id string) bool {
	return id != "" && len(id) <= 256 && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

func readWorkerTrace(ctx context.Context, j Job, run board.TraceRun, r traceReadRequest) (string, error) {
	journal, binding, err := openTraceJournal(ctx, j.Workspace, run, r.TraceVersion)
	if err != nil {
		return "", err
	}
	defer journal.Close()
	if r.TraceVersion == "" {
		binding.Query = r.Query
	} else {
		if r.Query != "" && strings.ToLower(r.Query) != strings.ToLower(binding.Query) {
			return "", errors.New("trace_changed: query differs from the bound search; restart without trace_version")
		}
		r.Query = binding.Query
	}
	encoded, _ := json.Marshal(binding)
	version := base64.RawURLEncoding.EncodeToString(encoded)
	if len(version) > maxTraceVersionBytes {
		return "", errors.New("trace unavailable: bound identity exceeds continuation budget")
	}
	page := []traceObservation{}
	var detail *traceObservation
	total := 0
	query := strings.ToLower(r.Query)
	projection := traceProjector{Own: run.RunID == j.RunID}
	err = eventlog.VisitCommittedFile(ctx, journal, binding.checkpoint, func(offset int64, event agent.Event) error {
		return projection.observe(offset, event, func(item traceObservation) {
			if r.Record != "" {
				if item.Record == r.Record {
					copy := item
					detail = &copy
				}
				return
			}
			if query != "" && !strings.Contains(strings.ToLower(item.Content), query) && !strings.Contains(strings.ToLower(item.Subject), query) {
				return
			}
			if total >= r.Offset && len(page) < r.Limit {
				item.Content, item.Truncated = traceExcerpt(item.Content, 512)
				page = append(page, item)
			}
			total++
		})
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("trace_changed: committed journal verification failed: %w", err)
	}
	if r.Record != "" {
		if detail != nil {
			item := *detail
			content := []byte(item.Content)
			if r.ByteOffset > len(content) || r.ByteOffset < len(content) && !utf8.RuneStart(content[r.ByteOffset]) {
				return "", errors.New("trace byte_offset must be at a UTF-8 boundary")
			}
			end := min(len(content), r.ByteOffset+r.ByteLimit)
			for end < len(content) && !utf8.RuneStart(content[end]) {
				end--
			}
			marshalDetail := func() ([]byte, error) {
				var next *int
				if end < len(content) {
					next = &end
				}
				return marshalTrace(ctx, struct {
					RunID          string `json:"run_id"`
					StepID         string `json:"step_id"`
					TraceVersion   string `json:"trace_version"`
					Record         string `json:"record"`
					Sequence       uint64 `json:"sequence"`
					Tool           string `json:"tool"`
					Subject        string `json:"subject,omitempty"`
					IsError        bool   `json:"is_error"`
					ByteOffset     int    `json:"byte_offset"`
					TotalBytes     int    `json:"total_bytes"`
					NextByteOffset *int   `json:"next_byte_offset,omitempty"`
					Content        string `json:"content"`
					Redacted       bool   `json:"redacted"`
					Projection     bool   `json:"projection"`
				}{run.RunID, run.StepID, version, item.Record, item.Sequence, item.Tool, item.Subject, item.IsError, r.ByteOffset, len(content), next, string(content[r.ByteOffset:end]), item.Redacted, true})
			}
			out, err := marshalDetail()
			// The content byte allowance is an upper bound. JSON escaping and
			// the bound cursor must also fit the response's encoded byte budget.
			for err == nil && len(out) > maxTracePageBytes {
				end = r.ByteOffset + (end-r.ByteOffset)/2
				for end < len(content) && !utf8.RuneStart(content[end]) {
					end--
				}
				if end <= r.ByteOffset {
					return "", errors.New("trace unavailable: record metadata exceeds page budget")
				}
				out, err = marshalDetail()
			}
			return string(out), err
		}
		return "", errors.New("trace record is absent from the bound committed prefix")
	}
	if r.Offset > total {
		return "", errors.New("trace offset exceeds matched observations")
	}
	end := r.Offset + len(page)
	var next *int
	if end < total {
		next = &end
	}
	marshalPage := func() ([]byte, error) {
		return marshalTrace(ctx, struct {
			RunID          string             `json:"run_id"`
			StepID         string             `json:"step_id"`
			TraceVersion   string             `json:"trace_version"`
			CommittedBytes int64              `json:"committed_bytes"`
			Projection     bool               `json:"projection"`
			Query          string             `json:"query,omitempty"`
			Items          []traceObservation `json:"items"`
			Offset         int                `json:"offset"`
			NextOffset     *int               `json:"next_offset,omitempty"`
			Total          int                `json:"total"`
		}{run.RunID, run.StepID, version, binding.Offset, true, r.Query, page, r.Offset, next, total})
	}
	out, err := marshalPage()
	for err == nil && len(out) > maxTracePageBytes && len(page) > 1 {
		page = page[:len(page)-1]
		end = r.Offset + len(page)
		next = &end
		out, err = marshalPage()
	}
	if len(out) > maxTracePageBytes {
		return "", errors.New("trace unavailable: search metadata exceeds page budget")
	}
	return string(out), err
}

func marshalTrace(ctx context.Context, value any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

func openTraceJournal(ctx context.Context, workspace string, run board.TraceRun, version string) (*os.File, traceBinding, error) {
	var bound traceBinding
	if err := ctx.Err(); err != nil {
		return nil, bound, err
	}
	root, err := filepath.Abs(workspace)
	if err != nil || workspace == "" || workspace != run.Workspace || !validTraceRunID(run.RunID) {
		return nil, bound, errors.New("trace unavailable: invalid registered workspace or run")
	}
	// Open from / one component at a time. O_NOFOLLOW on only the workspace
	// itself would still permit a substituted ancestor directory.
	dir, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, bound, errors.New("trace unavailable: cannot open retained root")
	}
	defer func() { unix.Close(dir) }()
	parts := append(strings.Split(strings.TrimPrefix(root, "/"), "/"), ".pwnmesh", "runs", run.RunID)
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, err := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, bound, errors.New("trace unavailable: retained directory is missing or unsafe")
		}
		unix.Close(dir)
		dir = next
	}
	open := func(name string) (*os.File, error) {
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, errors.New("trace unavailable: retained journal or checkpoint is missing or unsafe")
		}
		return os.NewFile(uintptr(fd), name), nil
	}
	f, err := open("session.json")
	if err != nil {
		return nil, bound, err
	}
	raw, err := readEvidenceHandle(ctx, f, maxTraceSessionBytes)
	f.Close()
	if err != nil {
		if err := ctx.Err(); err != nil {
			return nil, bound, err
		}
		return nil, bound, errors.New("trace unavailable: checkpoint is unreadable or exceeds the 16 MiB bound")
	}
	var saved struct {
		SchemaVersion int                 `json:"schema_version"`
		Identity      executionIdentity   `json:"identity"`
		Log           eventlog.Checkpoint `json:"log_checkpoint"`
	}
	if json.Unmarshal(raw, &saved) != nil || !supportedSessionSchema(saved.SchemaVersion, saved.Log) || saved.Identity.ProjectID != run.ProjectID || saved.Identity.RunID != run.RunID || saved.Identity.StepID != run.StepID || saved.Identity.Kind != "explore" || saved.Identity.Workspace != root || saved.Identity.WorkspaceTarget != root || saved.Identity.RunDir != filepath.Join(root, ".pwnmesh", "runs", run.RunID) || saved.Identity.RunDirTarget != saved.Identity.RunDir || saved.Identity.JobDigest != run.JobSHA256 {
		return nil, bound, errors.New("trace unavailable: retained checkpoint does not match the registered execution")
	}
	bound = traceBinding{ProjectID: run.ProjectID, Generation: run.Generation, RunID: run.RunID, Offset: saved.Log.Offset, SHA256: saved.Log.SHA256}
	if version != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(version)
		if err != nil || json.Unmarshal(decoded, &bound) != nil || bound.ProjectID != run.ProjectID || bound.Generation != run.Generation || bound.RunID != run.RunID || bound.Offset > saved.Log.Offset || len(bound.Query) > 256 {
			return nil, bound, errors.New("trace_changed: invalid prefix identity; restart the search")
		}
	}
	// Continuation tokens select only the byte prefix; they cannot remove the
	// saved exclusions and turn discarded crash records into observations.
	bound.checkpoint, err = saved.Log.Prefix(bound.Offset, bound.SHA256)
	if err != nil {
		return nil, bound, err
	}
	digest, err := hex.DecodeString(bound.SHA256)
	if err != nil || len(digest) != 32 || bound.Offset < 0 {
		return nil, bound, errors.New("trace unavailable: invalid committed checkpoint")
	}
	f, err = open("events.jsonl")
	if err != nil {
		return nil, bound, err
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < bound.Offset {
		f.Close()
		return nil, bound, errors.New("trace unavailable: journal is not a regular file or is shorter than its checkpoint")
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, bound, err
	}
	return f, bound, nil
}

type traceCallLabel struct {
	Name, Subject string
	Redacted      bool
}

// Pending labels are bounded to one tool batch. Tool-result text remains the
// only cross-run projection; own-run command/path labels help find old work.
type traceProjector struct {
	Own     bool
	pending map[string]traceCallLabel
}

func (p *traceProjector) observe(offset int64, event agent.Event, consume func(traceObservation)) error {
	if p.pending == nil {
		p.pending = map[string]traceCallLabel{}
	}
	if event.Type == "message_end" && event.Message != nil && event.Message.Role == "assistant" {
		p.pending = map[string]traceCallLabel{}
		for _, block := range event.Message.Content {
			if block.Type != "tool_use" || len(block.ID) > 256 || len(block.Name) > 128 {
				continue
			}
			label := traceCallLabel{Name: block.Name}
			if p.Own {
				label.Subject, label.Redacted = traceSubject(block)
			}
			p.pending[block.ID] = label
			if len(p.pending) > 1024 {
				return errors.New("trace unavailable: tool batch exceeds 1024 labels")
			}
		}
	}
	if event.Type == "tool_start" && len(event.ToolID) <= 256 && len(event.ToolName) <= 128 {
		label := p.pending[event.ToolID]
		label.Name = event.ToolName
		p.pending[event.ToolID] = label
		if len(p.pending) > 1024 {
			return errors.New("trace unavailable: tool batch exceeds 1024 labels")
		}
	}
	if event.Type != "message_end" || event.Message == nil || event.Message.Role != "user" {
		return nil
	}
	for index, block := range event.Message.Content {
		if block.Type != "tool_result" {
			continue
		}
		label := p.pending[block.ToolUseID]
		delete(p.pending, block.ToolUseID)
		// Do not recursively import graph/session/input projections.
		switch label.Name {
		case "", "read_worker_trace", "read_graph", "read_snapshot", "read_evidence", "read_updates":
			continue
		}
		text, ok := traceResultText(block.Content)
		if !ok {
			continue
		}
		filtered := redactTraceCredentials(text)
		consume(traceObservation{Record: strconv.FormatInt(offset, 10) + ":" + strconv.Itoa(index), Sequence: event.Message.Sequence, Tool: label.Name, Subject: label.Subject, IsError: block.IsError, Content: filtered, Redacted: label.Redacted || filtered != text})
	}
	return nil
}

func traceSubject(block agent.Block) (string, bool) {
	var fields struct{ Path, Command, Key string }
	if json.Unmarshal(block.Input, &fields) != nil {
		return "", false
	}
	var subject string
	switch block.Name {
	case "bash":
		subject = fields.Command
	case "write", "edit", "read":
		subject = fields.Path
	case "run_graph":
		subject = fields.Key
	}
	filtered := redactTraceCredentials(subject)
	bounded, omitted := traceExcerpt(filtered, 512)
	if omitted {
		bounded += " [truncated]"
	}
	return bounded, filtered != subject
}

func traceExcerpt(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	// A short retained page/index must not keep a large source string alive.
	return strings.Clone(text[:limit]), true
}

func traceResultText(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, utf8.ValidString(text)
	}
	var blocks []agent.Block
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var lines []string
	for _, b := range blocks {
		if b.Type == "text" {
			lines = append(lines, b.Text)
		}
	}
	return strings.Join(lines, "\n"), len(lines) > 0
}

var traceCredentialFields = regexp.MustCompile(`(?im)(["']?(?:authorization|proxy-authorization|cookie|set-cookie|[a-z0-9_]*(?:password|passwd|api[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|secret)[a-z0-9_]*)["']?\s*[:=]\s*)(?:"(?:\\[^\r\n]|[^"\\\r\n])*"|'(?:\\[^\r\n]|[^'\\\r\n])*'|[^\r\n,}]+)`)
var traceBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
var traceAPIKey = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`)
var traceURLCredentials = regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`)

// This removes recognizable credential fields and token formats. The source
// journal is untouched; the response explicitly marks this as a projection.
func redactTraceCredentials(text string) string {
	text = traceCredentialFields.ReplaceAllString(text, "${1}[redacted]")
	text = traceBearer.ReplaceAllString(text, "Bearer [redacted]")
	text = traceAPIKey.ReplaceAllString(text, "[redacted]")
	return traceURLCredentials.ReplaceAllString(text, "${1}[redacted]@")
}
