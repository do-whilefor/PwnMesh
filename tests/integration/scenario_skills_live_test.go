//go:build linux

package integration

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"pwnmesh/internal/agent"
	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

var scenarioPythonCommand = regexp.MustCompile(`(?m)(?:^|&&[ \t]*|;[ \t]*)python(?:3)?[ \t]+(?:-u[ \t]+)?["']?(/workspace/[A-Za-z0-9_./-]+\.py)["']?(?:[ \t]|$)`)

type scenarioSkillFixture struct {
	mu     sync.Mutex
	seen   map[string]int
	bodies map[string][]byte
	source []byte
	nonce  string
}

func newScenarioSkillFixture() (*scenarioSkillFixture, *httptest.Server) {
	f := &scenarioSkillFixture{seen: map[string]int{}, bodies: map[string][]byte{}, nonce: fmt.Sprintf("%x", randomSkillNonce())}
	f.source = []byte(fmt.Sprintf(`import hashlib
import json
from pathlib import Path

INVOICES = {1001: {"owner": "alice", "value": "synthetic-alice"},
            1002: {"owner": "bob", "value": "synthetic-bob"}}

def vulnerable_invoice(user, invoice_id):
    if user not in ("alice", "bob"):
        return 401, None
    return 200, INVOICES[invoice_id]

def fixed_invoice(user, invoice_id):
    status, invoice = vulnerable_invoice(user, invoice_id)
    if status != 200:
        return status, None
    if invoice["owner"] != user:
        return 403, None
    return status, invoice

if __name__ == "__main__":
    results = [fn("alice", object_id) for fn in (vulnerable_invoice, fixed_invoice)
               for object_id in (1001, 1002)]
    print(json.dumps({"nonce": %q,
        "source_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "statuses": [result[0] for result in results],
        "owners": [result[1]["owner"] if result[1] else None for result in results]}))
`, f.nonce))
	for _, variant := range []string{"vulnerable", "fixed"} {
		for _, id := range []int{1001, 1002} {
			status, owner := 200, "alice"
			if id == 1002 {
				owner = "bob"
				if variant == "fixed" {
					status, owner = 403, ""
				}
			}
			path := fmt.Sprintf("/%s/invoices/%d", variant, id)
			f.bodies[path], _ = json.Marshal(map[string]any{"nonce": f.nonce, "variant": variant, "invoice": id, "owner": owner, "status": status})
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/audit.py" {
			w.Header().Set("Content-Type", "text/plain")
			f.record(r.URL.Path, http.StatusOK)
			_, _ = w.Write(f.source)
			return
		}
		body, exists := f.bodies[r.URL.Path]
		if !exists {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Test-User") != "alice" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusOK
		if r.URL.Path == "/fixed/invoices/1002" {
			status = http.StatusForbidden
		}
		f.record(r.URL.Path, status)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	return f, server
}

func randomSkillNonce() []byte {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return nonce
}

func (f *scenarioSkillFixture) record(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[path] = status
}

// Run each scenario separately so its durable archive and database are isolated.
// The task never mentions the Skills: discovery must come from production prompts.
func TestLiveScenarioSkillsProject(t *testing.T) {
	if os.Getenv("PWNMESH_LIVE_SCENARIO_SKILLS_TEST") != "1" {
		t.Skip("opt in to real model and Docker scenario Skills acceptance")
	}
	for _, scenario := range []string{"audit", "pentest"} {
		t.Run(scenario, func(t *testing.T) {
			output := os.Getenv("PWNMESH_LIVE_OUTPUT")
			if output == "" {
				t.Fatal("PWNMESH_LIVE_OUTPUT is required")
			}
			t.Setenv("PWNMESH_LIVE_OUTPUT", filepath.Join(output, scenario))
			fixture, server := newScenarioSkillFixture()
			defer server.Close()
			origin := fmt.Sprintf("Use only synthetic local resources at %s and a Step-private directory under /workspace. Do not install packages, access credentials, or contact other targets. Authorize exactly one ordinary Step. Publish one evidence-backed fact and finish using that fact; no candidates, curation, extra report-writing Step or scoring are needed. ", server.URL)
			if scenario == "audit" {
				origin += "Review /audit.py for object ownership enforcement. Fetch and preserve the original source, inspect the vulnerable_invoice and fixed_invoice paths, then execute the unchanged file with Python 3 using its absolute path in the private directory. Its built-in smoke exercises alice against objects 1001 and 1002 in both variants. Save AND print the actual JSON stdout. Cite both original source and stdout as evidence, explain the code path causing the observed difference, and distinguish this synthetic execution from a deployed-system claim."
			} else {
				origin += "Test the HTTP API using the synthetic account alice via header X-Test-User: alice. Objects 1001 and 1002 belong to alice and bob respectively; both are expressly authorized test data. Compare GET /vulnerable/invoices/1001, /vulnerable/invoices/1002, /fixed/invoices/1001, and /fixed/invoices/1002. These four read-only requests are the whole target scope. Preserve and cite each original response BODY in a separate evidence file, record actual HTTP status, and explain whether foreign-owner access is possible and whether the fixed control enforces ownership."
			}
			bundle := scenarioSkillBundle(t, scenario)
			runObservedScenarioProject(t, "Synthetic "+scenario+" Skills smoke", origin, "Complete the bounded ownership check with original target and control evidence.", scenario, "scenario_skills_and_ownership_controls", func(state board.State, files map[string][]byte) []string {
				fixture.mu.Lock()
				err := saveLiveJSON(filepath.Join(output, scenario, "fixture-observations.json"), fixture.seen)
				fixture.mu.Unlock()
				if err != nil {
					t.Error(err)
				}
				return validateScenarioSkills(state, files, scenario, bundle, fixture)
			}, 1)
		})
	}
}

func scenarioSkillBundle(t *testing.T, scenario string) map[string][]byte {
	t.Helper()
	name := "web-vulnhunt"
	if scenario == "audit" {
		name = "flounder"
	}
	root := filepath.Join("..", "..", "internal", "worker", "skills", name)
	bundle := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		bundle[filepath.ToSlash(relative)], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func scenarioSkillReads(journal []byte, scenario string, bundle map[string][]byte) (entry, reference bool) {
	for _, call := range assetToolReceipts(journal) {
		var input struct{ Path string }
		if call.Name != "read_"+scenario+"_skill" || json.Unmarshal(call.Input, &input) != nil {
			continue
		}
		if input.Path == "" {
			input.Path = "SKILL.md"
		}
		expected := bundle[input.Path]
		if len(expected) == 0 || !bytes.Equal(call.Output, expected) {
			continue
		}
		entry = entry || input.Path == "SKILL.md"
		reference = reference || strings.HasPrefix(input.Path, "references/")
	}
	return
}

func scenarioAuditExecuted(journal, source, receipt []byte, files map[string][]byte) bool {
	for _, call := range assetToolReceipts(journal) {
		var input struct{ Command string }
		if call.Name != "bash" || json.Unmarshal(call.Input, &input) != nil || !bytes.Contains(call.Output, bytes.TrimSpace(receipt)) {
			continue
		}
		for _, match := range scenarioPythonCommand.FindAllStringSubmatch(strings.TrimSpace(input.Command), -1) {
			if bytes.Equal(files[match[1]], source) {
				return true
			}
		}
	}
	return false
}

func scenarioHTTPResponse(raw, body []byte, status int) bool {
	reader := bufio.NewReader(bytes.NewReader(raw))
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	actual, err := io.ReadAll(response.Body)
	return err == nil && response.StatusCode == status && bytes.Equal(actual, body) && reader.Buffered() == 0
}

func validateScenarioSkills(state board.State, files map[string][]byte, scenario string, bundle map[string][]byte, fixture *scenarioSkillFixture) []string {
	failures := []string{}
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, message)
		}
	}
	project := state.Graph.Project
	check(project.Status == "completed" && project.OrchestrationVersion == 1 && project.Scenario == scenario, "scenario project did not complete")
	entries, reference := map[string]bool{}, false
	for path, raw := range files {
		if !strings.HasSuffix(path, "/job.json") {
			continue
		}
		var job worker.Job
		if json.Unmarshal(raw, &job) != nil || job.Graph.Project.ID != project.ID || job.Graph.Project.Scenario != scenario || (job.Kind != "reason" && job.Kind != "explore") {
			continue
		}
		entry, ref := scenarioSkillReads(files[strings.TrimSuffix(path, "job.json")+"events.jsonl"], scenario, bundle)
		entries[job.Kind], reference = entries[job.Kind] || entry, reference || ref
	}
	check(entries["reason"] && entries["explore"], "reason and explore lack successful byte-exact entry reads")
	check(reference, "no successful byte-exact bundled reference read")
	var steps []board.Step
	for _, step := range state.Steps {
		if !staleRepairCompletionMarker(state, step) {
			steps = append(steps, step)
		}
	}
	check(len(steps) == 1, "expected one ordinary Step")
	facts := map[string]board.FactRecord{}
	for _, fact := range state.FactRecords {
		facts[fact.ID] = fact
	}
	if len(steps) != 1 || steps[0].Worker == nil || steps[0].Result == nil {
		return append(failures, "missing completed Step result")
	}
	step := steps[0]
	_, successful := orchestrationSuccessfulRun(project, step, *step.Worker, facts, files)
	check(successful, "Step lacks its successful production run and valid result fact")
	fact := facts[*step.Result]
	observed := map[string]bool{}
	auditReceipt := false
	for _, evidence := range fact.Evidence {
		valid := orchestrationEvidenceValid(evidence, fact.RunID, files)
		check(valid, "result evidence failed original bytes/hash/provenance")
		if !valid {
			continue
		}
		raw := files[evidence.Path]
		if bytes.Equal(raw, fixture.source) {
			observed["/audit.py"] = true
		}
		for path, body := range fixture.bodies {
			status := http.StatusOK
			if path == "/fixed/invoices/1002" {
				status = http.StatusForbidden
			}
			observed[path] = observed[path] || bytes.Equal(raw, body) || scenarioHTTPResponse(raw, body, status)
		}
		var receipt struct {
			Nonce    string
			SHA256   string `json:"source_sha256"`
			Statuses []int
			Owners   []*string
		}
		if json.Unmarshal(raw, &receipt) == nil && receipt.Nonce == fixture.nonce && receipt.SHA256 == fmt.Sprintf("%x", sha256.Sum256(fixture.source)) {
			statuses, _ := json.Marshal(receipt.Statuses)
			owners, _ := json.Marshal(receipt.Owners)
			journal := files["/workspace/.pwnmesh/runs/"+orchestrationRunID(fact.RunID)+"/events.jsonl"]
			auditReceipt = string(statuses) == "[200,200,200,403]" && string(owners) == `["alice","bob","alice",null]` && scenarioAuditExecuted(journal, fixture.source, raw, files)
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if scenario == "audit" {
		check(fixture.seen["/audit.py"] == http.StatusOK && observed["/audit.py"] && auditReceipt, "audit lacks original fetched source and actual target/control execution receipt")
	} else {
		for path := range fixture.bodies {
			status := http.StatusOK
			if path == "/fixed/invoices/1002" {
				status = http.StatusForbidden
			}
			check(fixture.seen[path] == status && observed[path], "pentest lacks actual request/status and original control/target body: "+path)
		}
	}
	return failures
}

func TestScenarioSkillReadsRejectMissingOrForgedReceipts(t *testing.T) {
	for _, scenario := range []string{"audit", "pentest"} {
		t.Run(scenario, func(t *testing.T) {
			bundle := scenarioSkillBundle(t, scenario)
			journal := func(name string, arguments map[string]string, output string, failed bool) []byte {
				input, _ := json.Marshal(arguments)
				content, _ := json.Marshal(output)
				var raw []byte
				for _, message := range []agent.Message{
					{Role: "assistant", Content: []agent.Block{{Type: "tool_use", ID: "read", Name: name, Input: input}}},
					{Role: "user", Content: []agent.Block{{Type: "tool_result", ToolUseID: "read", Content: content, IsError: failed}}},
				} {
					line, _ := json.Marshal(agent.Event{Type: "message_end", Message: &message})
					raw = append(raw, append(line, '\n')...)
				}
				return raw
			}
			tool := "read_" + scenario + "_skill"
			entry := journal(tool, map[string]string{}, string(bundle["SKILL.md"]), false)
			reference := journal(tool, map[string]string{"path": "references/verification.md"}, string(bundle["references/verification.md"]), false)
			if e, r := scenarioSkillReads(append(entry, reference...), scenario, bundle); !e || !r {
				t.Fatal("genuine entry/reference receipts were rejected")
			}
			for _, raw := range [][]byte{
				[]byte(`{"message":{"role":"assistant","content":[{"type":"text","text":"I read the Skills successfully"}]}}`),
				journal(tool, map[string]string{}, string(bundle["SKILL.md"]), true),
				journal(tool, map[string]string{}, "I read the correct guide", false),
				journal("read_other_skill", map[string]string{}, string(bundle["SKILL.md"]), false),
				journal(tool, map[string]string{"path": "references/verification.md"}, string(bundle["SKILL.md"]), false),
			} {
				if e, r := scenarioSkillReads(raw, scenario, bundle); e || r {
					t.Fatal("failed, forged or prose-only Skill read was accepted")
				}
			}
			fixture, server := newScenarioSkillFixture()
			defer server.Close()
			receipt := []byte(`{"statuses":[200,200,200,403]}`)
			validRun := journal("bash", map[string]string{"command": "python3 /workspace/source.py | tee /workspace/receipt.json"}, string(receipt), false)
			printed := journal("bash", map[string]string{"command": "printf '%s' '" + string(receipt) + "'"}, string(receipt), false)
			files := map[string][]byte{"/workspace/source.py": fixture.source}
			if !scenarioAuditExecuted(validRun, fixture.source, receipt, files) || scenarioAuditExecuted(printed, fixture.source, receipt, files) || scenarioAuditExecuted(validRun, []byte("changed source"), receipt, files) {
				t.Fatal("audit execution validation did not bind actual source invocation")
			}
			body := fixture.bodies["/fixed/invoices/1002"]
			response := []byte(fmt.Sprintf("HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
			if !scenarioHTTPResponse(response, body, 403) || scenarioHTTPResponse(body, body, 403) || scenarioHTTPResponse(response, body, 200) || scenarioHTTPResponse(response, []byte("forged body"), 403) {
				t.Fatal("HTTP control validation did not bind original status and body")
			}
			state := board.State{Graph: board.Graph{Project: board.Project{Status: "completed", Scenario: scenario, OrchestrationVersion: 1}}}
			if len(validateScenarioSkills(state, nil, scenario, bundle, fixture)) < 3 {
				t.Fatal("project completion alone was mistaken for live acceptance")
			}
		})
	}
}
