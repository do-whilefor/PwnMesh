//go:build linux

package worker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"

	"pwnmesh/internal/agent"
	eventlog "pwnmesh/internal/worker/internal/journal"
)

const recallPrefix = "Retained work index (historical tool receipts, not current file checks or verified conclusions). Search read_worker_trace with this run_id for omitted observations.\n"
const maxRecallBytes = 4096
const maxRecallEntries = 6

type recallEntry struct {
	Sequence  uint64 `json:"sequence"`
	Tool      string `json:"tool"`
	IsError   bool   `json:"is_error"`
	Path      string `json:"path,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Preview   string `json:"preview,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
}

// Files and commands have separate small budgets so command churn cannot evict
// every retained script. This index never inventories the live filesystem or
// infers test success from text; a result's original failure flag is retained.
type workerRecall struct {
	RunID           string        `json:"run_id"`
	Files           []recallEntry `json:"files"`
	Commands        []recallEntry `json:"commands"`
	OmittedFiles    int           `json:"omitted_files,omitempty"`
	OmittedCommands int           `json:"omitted_commands,omitempty"`
	Unavailable     string        `json:"unavailable,omitempty"`
	projection      traceProjector
}

func restoreWorkerRecall(ctx context.Context, j Job, runDir string, checkpoint eventlog.Checkpoint) *workerRecall {
	index := &workerRecall{RunID: j.RunID, projection: traceProjector{Own: true}}
	if checkpoint.SHA256 == "" {
		return index
	}
	err := eventlog.VisitCommittedEvents(ctx, filepath.Join(runDir, "events.jsonl"), checkpoint, func(event agent.Event) error {
		return index.observe(event)
	})
	if err != nil {
		// No projected observations survive a failed prefix verification. The
		// durable session remains authoritative and execution is not replayed.
		index = &workerRecall{RunID: j.RunID, projection: traceProjector{Own: true}, Unavailable: "The retained index could not verify its committed journal; use the trace tool to inspect availability."}
	}
	return index
}

func (r *workerRecall) observe(event agent.Event) error {
	return r.projection.observe(0, event, func(item traceObservation) {
		entry := recallEntry{Sequence: item.Sequence, Tool: item.Tool, IsError: item.IsError, Redacted: item.Redacted}
		switch item.Tool {
		case "write", "edit":
			var receipt struct{ Path, SHA256 string }
			if !item.IsError {
				_ = json.Unmarshal([]byte(item.Content), &receipt)
			}
			entry.Path, entry.SHA256 = receipt.Path, receipt.SHA256
			if entry.Path == "" {
				entry.Path = item.Subject
			}
			if entry.Path != "" && !strings.HasSuffix(entry.Path, " [truncated]") && len(entry.Path) <= 512 {
				r.retainFile(entry)
			} else {
				r.OmittedFiles++
			}
		case "bash", "run_graph":
			entry.Subject, entry.Truncated = traceExcerpt(item.Subject, 256)
			var clipped bool
			entry.Preview, clipped = traceExcerpt(item.Content, 160)
			entry.Truncated = entry.Truncated || clipped
			if len(r.Commands) == maxRecallEntries {
				r.Commands = r.Commands[1:]
				r.OmittedCommands++
			}
			r.Commands = append(r.Commands, entry)
			if item.Tool == "run_graph" {
				var graph struct {
					Nodes []struct {
						Output struct {
							Artifacts []struct{ Path, SHA256 string }
						}
					}
				}
				// A failed graph keeps its JSON diagnostics before the tool error.
				if json.NewDecoder(strings.NewReader(item.Content)).Decode(&graph) == nil {
					for _, node := range graph.Nodes {
						for _, file := range node.Output.Artifacts {
							if name := filepath.Base(file.Path); name == "stdout.log" || name == "stderr.log" {
								continue
							}
							if file.Path == "" || len(file.Path) > 512 {
								r.OmittedFiles++
								continue
							}
							r.retainFile(recallEntry{Sequence: item.Sequence, Tool: item.Tool, IsError: item.IsError, Path: file.Path, SHA256: file.SHA256, Redacted: item.Redacted})
						}
					}
				}
			}
		}
	})
}

func (r *workerRecall) retainFile(entry recallEntry) {
	if decoded, err := hex.DecodeString(entry.SHA256); err != nil || len(decoded) != 32 {
		entry.SHA256 = ""
	}
	for n, old := range r.Files {
		if old.Path == entry.Path {
			r.Files = append(r.Files[:n], r.Files[n+1:]...)
			break
		}
	}
	if len(r.Files) == maxRecallEntries {
		r.Files = r.Files[1:]
		r.OmittedFiles++
	}
	r.Files = append(r.Files, entry)
}

func (r *workerRecall) contextData() string {
	if r == nil || len(r.Files)+len(r.Commands) == 0 && r.Unavailable == "" {
		return ""
	}
	copy := *r
	for {
		raw, _ := json.Marshal(copy)
		if len(recallPrefix)+len(raw) <= maxRecallBytes {
			return recallPrefix + string(raw)
		}
		if len(copy.Commands) > 0 {
			copy.Commands = copy.Commands[1:]
			copy.OmittedCommands++
		} else if len(copy.Files) > 0 {
			copy.Files = copy.Files[1:]
			copy.OmittedFiles++
		} else {
			return ""
		}
	}
}

func (r *workerRecall) pin(loop *agent.Loop) {
	data := make([]string, 0, len(loop.ContextData)+1)
	for _, item := range loop.ContextData {
		if !strings.HasPrefix(item, recallPrefix) {
			data = append(data, item)
		}
	}
	if index := r.contextData(); index != "" {
		data = append(data, index)
	}
	loop.ContextData = data
}
