//go:build linux

package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
)

type traceFixture struct {
	job Job
	run board.TraceRun
	dir string
	raw []byte
}

func newTraceFixture(t *testing.T, contents ...string) traceFixture {
	t.Helper()
	root := t.TempDir()
	j := Job{RunID: "producer", Kind: "explore", GraphRPC: true, Workspace: root, Graph: board.Graph{Project: board.Project{ID: "project", Generation: 2}}, Intent: &board.Intent{ID: "step"}}
	dir := filepath.Join(root, ".pwnmesh", "runs", j.RunID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := identityFor(j, dir)
	if err != nil {
		t.Fatal(err)
	}
	run := board.TraceRun{ProjectID: "project", Generation: 2, RunID: j.RunID, StepID: "step", Status: "running", Workspace: root, JobSHA256: identity.JobDigest}
	var raw []byte
	appendEvent := func(event agent.Event) {
		b, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, append(b, '\n')...)
	}
	prompt := agent.Text("system", "system-secret-must-not-leak")
	appendEvent(agent.Event{Type: "message_end", Message: &prompt})
	for i, text := range contents {
		id := string(rune('a' + i))
		appendEvent(agent.Event{Type: "tool_start", ToolID: id, ToolName: "bash"})
		content, _ := json.Marshal(text)
		message := agent.Message{Role: "user", Sequence: uint64(i + 1), Content: []agent.Block{{Type: "text", Text: "user-prompt-must-not-leak"}, {Type: "tool_result", ToolUseID: id, Content: content}}}
		appendEvent(agent.Event{Type: "message_end", Message: &message})
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, Log: journalCheckpoint{Offset: int64(len(raw)), SHA256: graphRecordVersion(raw)}, TaskPrompt: "job-secret-must-not-leak"}
	b, _ := json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(dir, "session.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	j.RunID = "consumer"
	return traceFixture{j, run, dir, raw}
}

func (f traceFixture) tool(t *testing.T) agent.Tool {
	t.Helper()
	return workerTraceTool(f.job, &Options{}, func(_ context.Context, r GraphRequest) (string, error) {
		if r.Op != "read_trace_runs" {
			t.Fatalf("unexpected trace authorization op: %+v", r)
		}
		r.RequestID = strings.Repeat("a", 32)
		if err := ValidateGraphRequest(f.job, r); err != nil {
			t.Fatal(err)
		}
		items := []board.TraceRun{f.run}
		if len(r.IDs) > 0 && r.IDs[0] != f.run.RunID {
			items = nil
		}
		b, _ := json.Marshal(traceRunPage{Items: items})
		return string(b), nil
	})
}

func traceCall(t *testing.T, tool agent.Tool, request any) string {
	t.Helper()
	input, _ := json.Marshal(request)
	raw, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWorkerTraceSearchAndDetailRetainUnpublishedObservations(t *testing.T) {
	original := "HTTP 200 unpublished 世界 response\n" + strings.Repeat("字", 900)
	f := newTraceFixture(t, original, "HTTP 403 different identity")
	tool := f.tool(t)
	listing := traceCall(t, tool, traceReadRequest{})
	for _, private := range []string{f.job.Workspace, f.run.JobSHA256, "system-secret", "job-secret"} {
		if strings.Contains(listing, private) {
			t.Fatalf("internal capability exposed: %s", listing)
		}
	}
	var page struct {
		Items        []traceObservation `json:"items"`
		TraceVersion string             `json:"trace_version"`
		NextOffset   *int               `json:"next_offset"`
	}
	text := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Query: "UNPUBLISHED", Limit: 1})
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Items[0].Truncated || page.TraceVersion == "" || strings.Contains(text, "must-not-leak") {
		t.Fatalf("wrong search projection: %s", text)
	}
	var recovered strings.Builder
	for offset := 0; ; {
		var detail struct {
			Content    string `json:"content"`
			Next       *int   `json:"next_byte_offset"`
			Projection bool   `json:"projection"`
		}
		text := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Record: page.Items[0].Record, TraceVersion: page.TraceVersion, ByteOffset: offset, ByteLimit: 13})
		if err := json.Unmarshal([]byte(text), &detail); err != nil {
			t.Fatal(err)
		}
		if !detail.Projection {
			t.Fatal("trace detail presented as original evidence")
		}
		recovered.WriteString(detail.Content)
		if detail.Next == nil {
			break
		}
		offset = *detail.Next
	}
	if recovered.String() != original {
		t.Fatal("UTF-8 detail pages failed to reconstruct the original safe text")
	}
}

func TestWorkerTraceRegisteredMapOrderingMatchesSessionIdentity(t *testing.T) {
	f := newTraceFixture(t, "registered observation")
	j := f.job
	j.RunID = f.run.RunID
	j.InputView = json.RawMessage(`{"z":"last","a":"first"}`)
	j.Workspace += "/."
	typed, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(typed, &canonical); err != nil {
		t.Fatal(err)
	}
	registered, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	// This is the production register/reload sequence: HTTP canonicalizes the
	// embedded Job; the dispatcher then unmarshals the exact registered Job.
	var executing Job
	if err := json.Unmarshal(registered, &executing); err != nil {
		t.Fatal(err)
	}
	executing.Workspace, err = filepath.Abs(executing.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identityFor(executing, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.run.Workspace, f.run.RegisteredJob = j.Workspace, registered
	f.run, err = BindTraceRun(f.run)
	if err != nil {
		t.Fatal(err)
	}
	if f.run.JobSHA256 != identity.JobDigest || f.run.JobSHA256 == graphRecordVersion(registered) || f.run.RegisteredJob != nil {
		t.Fatal("trace binding used raw JSON instead of runtime Job serialization")
	}
	saved := session{SchemaVersion: sessionSchemaVersion, Identity: identity, Log: journalCheckpoint{Offset: int64(len(f.raw)), SHA256: graphRecordVersion(f.raw)}}
	raw, _ := json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	page := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID})
	if !strings.Contains(page, "registered observation") {
		t.Fatalf("registered runtime journal unavailable: %s", page)
	}
}

func TestWorkerTraceCommittedPrefixIgnoresAppendAndPartialTail(t *testing.T) {
	f := newTraceFixture(t, "first", "second")
	tool := f.tool(t)
	var first struct {
		TraceVersion string `json:"trace_version"`
		Next         *int   `json:"next_offset"`
	}
	if err := json.Unmarshal([]byte(traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Limit: 1})), &first); err != nil {
		t.Fatal(err)
	}
	if first.Next == nil {
		t.Fatal("missing continuation")
	}
	appendFile, err := os.OpenFile(filepath.Join(f.dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFile.WriteString("{\"type\":\"message_end\",\"unfinished\":"); err != nil {
		t.Fatal(err)
	}
	appendFile.Close()
	page := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Offset: *first.Next, TraceVersion: first.TraceVersion, Limit: 1})
	if !strings.Contains(page, "second") || strings.Contains(page, "unfinished") {
		t.Fatalf("append drift: %s", page)
	}
	changed := append([]byte{}, f.raw...)
	changed[10] ^= 1
	if err := os.WriteFile(filepath.Join(f.dir, "events.jsonl"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(traceReadRequest{RunID: f.run.RunID, TraceVersion: first.TraceVersion})
	if _, err := tool.Execute(context.Background(), input); err == nil || !strings.Contains(err.Error(), "trace_changed") {
		t.Fatalf("modified prefix accepted: %v", err)
	}
}

func TestWorkerTraceContinuationKeepsPrefixAfterCheckpointAdvances(t *testing.T) {
	f := newTraceFixture(t, "first", "second")
	tool := f.tool(t)
	var first struct {
		TraceVersion string `json:"trace_version"`
	}
	if err := json.Unmarshal([]byte(traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Limit: 1})), &first); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal("newly committed")
	message := agent.Message{Role: "user", Sequence: 3, Content: []agent.Block{{Type: "tool_result", ToolUseID: "later", Content: content}}}
	for _, event := range []agent.Event{{Type: "tool_start", ToolID: "later", ToolName: "bash"}, {Type: "message_end", Message: &message}} {
		raw, _ := json.Marshal(event)
		f.raw = append(f.raw, append(raw, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "events.jsonl"), f.raw, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Log = journalCheckpoint{Offset: int64(len(f.raw)), SHA256: graphRecordVersion(f.raw)}
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	old := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Offset: 1, TraceVersion: first.TraceVersion})
	if strings.Contains(old, "newly committed") || !strings.Contains(old, `"total":2`) {
		t.Fatalf("continuation drifted to live log: %s", old)
	}
	live := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID})
	if !strings.Contains(live, "newly committed") || !strings.Contains(live, `"total":3`) {
		t.Fatalf("fresh search missed advanced checkpoint: %s", live)
	}
}

func TestWorkerTraceSearchContinuationKeepsQuery(t *testing.T) {
	f := newTraceFixture(t, "first needle", "unrelated observation", "second needle")
	tool := f.tool(t)
	var first struct {
		TraceVersion string `json:"trace_version"`
		Next         *int   `json:"next_offset"`
	}
	page := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Query: "needle", Limit: 1})
	if err := json.Unmarshal([]byte(page), &first); err != nil {
		t.Fatal(err)
	}
	if first.Next == nil {
		t.Fatal("matched search lacks continuation")
	}
	// Following the returned continuation alone keeps the original filter.
	next := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Offset: *first.Next, Limit: 1, TraceVersion: first.TraceVersion})
	if !strings.Contains(next, "second needle") || strings.Contains(next, "unrelated observation") || !strings.Contains(next, `"query":"needle"`) {
		t.Fatalf("search continuation lost query: %s", next)
	}
	input, _ := json.Marshal(traceReadRequest{RunID: f.run.RunID, Query: "unrelated", Offset: *first.Next, TraceVersion: first.TraceVersion})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Fatal("search silently changed filters during continuation")
	}
}

func TestWorkerTraceEscapedQueryContinuationRoundTrip(t *testing.T) {
	query := strings.Repeat("\x01", 256)
	f := newTraceFixture(t, query+" first", query+" second")
	tool := f.tool(t)
	var first struct {
		TraceVersion string `json:"trace_version"`
		Next         *int   `json:"next_offset"`
	}
	page := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Query: query, Limit: 1})
	if err := json.Unmarshal([]byte(page), &first); err != nil {
		t.Fatal(err)
	}
	if first.Next == nil || len(first.TraceVersion) <= 1024 || len(first.TraceVersion) > maxTraceVersionBytes {
		t.Fatalf("unexpected bound query encoding: token bytes %d", len(first.TraceVersion))
	}
	request := traceReadRequest{RunID: f.run.RunID, Offset: *first.Next, TraceVersion: first.TraceVersion}
	input, _ := json.Marshal(request)
	if err := agent.ValidateArguments(tool.Schema, input); err != nil {
		t.Fatalf("generated token rejected by tool schema: %v", err)
	}
	next := traceCall(t, tool, request)
	if !strings.Contains(next, "second") || strings.Contains(next, "first") {
		t.Fatalf("escaped query did not round-trip: %s", next)
	}
}

func TestWorkerTraceDetailBoundsEncodedJSONAndPreservesUTF8(t *testing.T) {
	original := strings.Repeat("<\x01世", 4000)
	f := newTraceFixture(t, original)
	tool := f.tool(t)
	var first struct {
		TraceVersion string             `json:"trace_version"`
		Items        []traceObservation `json:"items"`
	}
	if err := json.Unmarshal([]byte(traceCall(t, tool, traceReadRequest{RunID: f.run.RunID})), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 {
		t.Fatal("fixture trace missing")
	}
	var recovered strings.Builder
	for offset := 0; ; {
		page := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, TraceVersion: first.TraceVersion, Record: first.Items[0].Record, ByteOffset: offset, ByteLimit: maxTracePageBytes})
		if len(page) > maxTracePageBytes {
			t.Fatalf("escaped response exceeded byte budget: %d", len(page))
		}
		var detail struct {
			Content string `json:"content"`
			Next    *int   `json:"next_byte_offset"`
		}
		if err := json.Unmarshal([]byte(page), &detail); err != nil {
			t.Fatal(err)
		}
		recovered.WriteString(detail.Content)
		if detail.Next == nil {
			break
		}
		if *detail.Next <= offset || *detail.Next != offset+len(detail.Content) {
			t.Fatal("detail byte cursor failed to advance over exact UTF-8 text")
		}
		offset = *detail.Next
	}
	if recovered.String() != original {
		t.Fatal("response byte cap lost or changed projected content")
	}
}

func TestWorkerTraceCancellationAfterAuthorizationReturnsNoPage(t *testing.T) {
	f := newTraceFixture(t, "observation")
	for _, input := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"run_id":"producer"}`)} {
		ctx, cancel := context.WithCancel(context.Background())
		tool := workerTraceTool(f.job, &Options{}, func(context.Context, GraphRequest) (string, error) {
			raw, _ := json.Marshal(traceRunPage{Items: []board.TraceRun{f.run}})
			cancel()
			return string(raw), nil
		})
		page, err := tool.Execute(ctx, input)
		cancel()
		if !errors.Is(err, context.Canceled) || page != "" {
			t.Fatalf("cancelled request returned a page: %q %v", page, err)
		}
	}
}

func TestWorkerTraceRejectsFileAndIdentitySubstitution(t *testing.T) {
	for _, kind := range []string{"missing", "symlink-file", "symlink-run", "short", "identity", "partial-checkpoint", "wrong-generation", "wrong-workspace", "wrong-project"} {
		t.Run(kind, func(t *testing.T) {
			f := newTraceFixture(t, "observation")
			events := filepath.Join(f.dir, "events.jsonl")
			switch kind {
			case "missing":
				if err := os.Remove(events); err != nil {
					t.Fatal(err)
				}
			case "symlink-file":
				other := filepath.Join(t.TempDir(), "other")
				if err := os.WriteFile(other, f.raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(events); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, events); err != nil {
					t.Fatal(err)
				}
			case "symlink-run":
				other := filepath.Join(t.TempDir(), "other")
				if err := os.Rename(f.dir, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, f.dir); err != nil {
					t.Fatal(err)
				}
			case "short":
				if err := os.WriteFile(events, f.raw[:10], 0600); err != nil {
					t.Fatal(err)
				}
			case "identity":
				f.run.JobSHA256 = strings.Repeat("0", 64)
			case "partial-checkpoint":
				data, err := os.ReadFile(filepath.Join(f.dir, "session.json"))
				if err != nil {
					t.Fatal(err)
				}
				var s session
				if err = json.Unmarshal(data, &s); err != nil {
					t.Fatal(err)
				}
				s.Log.Offset--
				s.Log.SHA256 = graphRecordVersion(f.raw[:len(f.raw)-1])
				data, _ = json.Marshal(s)
				if err = os.WriteFile(filepath.Join(f.dir, "session.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-generation":
				f.run.Generation++
			case "wrong-workspace":
				f.run.Workspace = t.TempDir()
			case "wrong-project":
				f.run.ProjectID = "foreign"
			}
			input, _ := json.Marshal(traceReadRequest{RunID: f.run.RunID})
			if _, err := f.tool(t).Execute(context.Background(), input); err == nil {
				t.Fatal("unsafe trace accepted")
			}
		})
	}
}

func TestWorkerTraceRequestAndRoleBoundaries(t *testing.T) {
	f := newTraceFixture(t, "safe")
	for _, input := range []string{`{"run_id":"../outside"}`, `{"run_id":"consumer"}`, `{"run_id":"missing"}`, `{"run_id":"producer","offset":1}`, `{"run_id":"producer","record":"0:1"}`, `{"run_id":"producer","path":"/etc/passwd"}`, `{"query":"missing run"}`, `{"limit":21}`, `{} {}`, `null`} {
		if _, err := f.tool(t).Execute(context.Background(), json.RawMessage(input)); err == nil {
			t.Fatalf("unsafe request accepted: %s", input)
		}
	}
	for _, kind := range []string{"reason", "curate", "bootstrap", ""} {
		j := f.job
		j.Kind = kind
		tool := workerTraceTool(j, &Options{}, func(context.Context, GraphRequest) (string, error) {
			t.Fatal("unauthorized role reached bridge")
			return "", nil
		})
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Fatalf("unauthorized role %s", kind)
		}
	}
	j := f.job
	j.GraphRPC = false
	if _, err := workerTraceTool(j, &Options{}, nil).Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("offline worker acquired trace capability")
	}
	var wrong traceBinding
	raw, _, err := readTraceJournal(context.Background(), f.job.Workspace, f.run, "")
	if err != nil {
		t.Fatal(err)
	}
	wrong = traceBinding{ProjectID: "foreign", Generation: 2, RunID: f.run.RunID, Offset: int64(len(raw)), SHA256: graphRecordVersion(raw)}
	token, _ := json.Marshal(wrong)
	if _, _, err := readTraceJournal(context.Background(), f.job.Workspace, f.run, base64.RawURLEncoding.EncodeToString(token)); err == nil {
		t.Fatal("foreign continuation accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := readTraceJournal(ctx, f.job.Workspace, f.run, ""); err == nil {
		t.Fatal("cancelled read continued")
	}
}

func TestWorkerTraceRedactsCredentialsWithoutChangingJournal(t *testing.T) {
	text := "Authorization: Bearer token-value\nCookie: session=private-cookie\nANTHROPIC_AUTH_TOKEN=private-token\n{\"api_key\":\"private-key\",\"status\":200}\nhttps://user:private-password@example.test/path\nsk-abcdefghijklmnop\nHTTP 200 visible"
	f := newTraceFixture(t, text)
	page := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID})
	for _, secret := range []string{"token-value", "private-cookie", "private-token", "private-key", "private-password", "sk-abcdefghijklmnop"} {
		if strings.Contains(page, secret) {
			t.Fatalf("recognizable credential exposed: %s", page)
		}
	}
	if !strings.Contains(page, "HTTP 200 visible") || !strings.Contains(page, `"redacted":true`) {
		t.Fatalf("redaction missing visibility/projection flag: %s", page)
	}
	retained, err := os.ReadFile(filepath.Join(f.dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(f.raw) {
		t.Fatal("redaction mutated raw journal")
	}
}

func TestWorkerTraceRedactsEscapedCredentialValuesInSearchAndDetail(t *testing.T) {
	for _, text := range []string{
		`{"password":"prefix\"private-suffix","status":200}`,
		`{"api_key":"prefix\\\"private-suffix","status":200}`,
		`secret='prefix\'private-suffix'`,
	} {
		f := newTraceFixture(t, text)
		tool := f.tool(t)
		var page struct {
			TraceVersion string             `json:"trace_version"`
			Items        []traceObservation `json:"items"`
		}
		search := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID})
		if err := json.Unmarshal([]byte(search), &page); err != nil || len(page.Items) != 1 {
			t.Fatalf("invalid trace search: %s, %v", search, err)
		}
		detail := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Record: page.Items[0].Record, TraceVersion: page.TraceVersion})
		for _, response := range []string{search, detail} {
			if strings.Contains(response, "private-suffix") || !strings.Contains(response, `"redacted":true`) {
				t.Fatalf("escaped credential escaped redaction: %s", response)
			}
		}
		retained, err := os.ReadFile(filepath.Join(f.dir, "events.jsonl"))
		if err != nil || string(retained) != string(f.raw) {
			t.Fatalf("projection changed retained journal: %v", err)
		}
	}
}

func TestWorkerTraceDetailRetainsObservationProvenance(t *testing.T) {
	for _, failed := range []bool{false, true} {
		f := newTraceFixture(t, "a long unpublished tool observation")
		if failed {
			// Keep the checkpoint bound to the changed journal just as a source
			// Worker would when committing a failed tool result.
			f.raw = []byte(strings.Replace(string(f.raw), `"type":"tool_result"`, `"type":"tool_result","is_error":true`, 1))
			if err := os.WriteFile(filepath.Join(f.dir, "events.jsonl"), f.raw, 0600); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(f.dir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved session
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			saved.Log = journalCheckpoint{Offset: int64(len(f.raw)), SHA256: graphRecordVersion(f.raw)}
			raw, _ = json.Marshal(saved)
			if err := os.WriteFile(filepath.Join(f.dir, "session.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		tool := f.tool(t)
		var search struct {
			TraceVersion string             `json:"trace_version"`
			Items        []traceObservation `json:"items"`
		}
		if err := json.Unmarshal([]byte(traceCall(t, tool, traceReadRequest{RunID: f.run.RunID})), &search); err != nil || len(search.Items) != 1 {
			t.Fatalf("invalid trace search: %+v, %v", search, err)
		}
		for offset := 0; ; {
			var detail struct {
				RunID          string `json:"run_id"`
				StepID         string `json:"step_id"`
				Sequence       uint64 `json:"sequence"`
				Tool           string `json:"tool"`
				IsError        *bool  `json:"is_error"`
				Projection     bool   `json:"projection"`
				NextByteOffset *int   `json:"next_byte_offset"`
			}
			raw := traceCall(t, tool, traceReadRequest{RunID: f.run.RunID, Record: search.Items[0].Record, TraceVersion: search.TraceVersion, ByteOffset: offset, ByteLimit: 8})
			if err := json.Unmarshal([]byte(raw), &detail); err != nil {
				t.Fatal(err)
			}
			if detail.RunID != f.run.RunID || detail.StepID != f.run.StepID || detail.Tool != search.Items[0].Tool || detail.Sequence != search.Items[0].Sequence || detail.IsError == nil || *detail.IsError != failed || !detail.Projection {
				t.Fatalf("trace detail lost observation provenance: %s", raw)
			}
			if detail.NextByteOffset == nil {
				break
			}
			offset = *detail.NextByteOffset
		}
	}
}

func TestWorkerTraceSearchOutputHasByteBound(t *testing.T) {
	contents := make([]string, 50)
	for i := range contents {
		contents[i] = strings.Repeat("\x01", 512)
	}
	f := newTraceFixture(t, contents...)
	page := traceCall(t, f.tool(t), traceReadRequest{RunID: f.run.RunID, Limit: 20})
	if len(page) > maxTracePageBytes || !strings.Contains(page, "next_offset") {
		t.Fatalf("unbounded output: %d", len(page))
	}
}

func TestWorkerTraceCapabilityDoesNotReachChildAgents(t *testing.T) {
	j := graphWrapperJob(t)
	o := Options{RunDir: t.TempDir(), Tools: []agent.Tool{workerTraceTool(j, &Options{}, nil)}, graphProvider: func(string) (agent.Provider, error) {
		return scenarioProvider(func(_ context.Context, _ []agent.Message, defs []agent.Definition, _ agent.Emit) (agent.Message, error) {
			for _, def := range defs {
				if def.Name == "read_worker_trace" {
					t.Fatal("child inherited parent trace capability")
				}
			}
			return agent.Text("assistant", "local observation"), nil
		}), nil
	}}
	checkpoint, err := mixedGraphCall(t, context.Background(), j, o, commandGraphSpec{Key: "trace-isolation", Nodes: []commandGraphNode{{ID: "child", Kind: "agent", Task: "local inspection", Resources: []string{}}}})
	if err != nil || checkpoint.Status != "succeeded" {
		t.Fatalf("child trace isolation: %+v %v", checkpoint, err)
	}
}

func TestWorkerTraceOmitsInputProjectionsAndAssistantMessages(t *testing.T) {
	var raw []byte
	appendEvent := func(e agent.Event) { b, _ := json.Marshal(e); raw = append(raw, append(b, '\n')...) }
	for _, name := range []string{"read_graph", "read_snapshot", "read_evidence", "read_updates", "read_worker_trace", "bash"} {
		appendEvent(agent.Event{Type: "tool_start", ToolID: name, ToolName: name})
		content, _ := json.Marshal(name + " private input projection")
		m := agent.Message{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: name, Content: content}}}
		appendEvent(agent.Event{Type: "message_end", Message: &m})
	}
	m := agent.Message{Role: "assistant", Content: []agent.Block{{Type: "thinking", Thinking: "private reasoning"}, {Type: "text", Text: "assistant narrative"}}}
	appendEvent(agent.Event{Type: "message_end", Message: &m})
	items, err := traceObservations(context.Background(), raw)
	if err != nil || len(items) != 1 || items[0].Tool != "bash" {
		t.Fatalf("input/assistant projections entered trace: %+v %v", items, err)
	}
}
