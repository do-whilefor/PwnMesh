//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"pwnmesh/internal/agent"
	"pwnmesh/internal/process"
	"pwnmesh/internal/workergraph"
)

var commandGraphID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type commandGraphSpec struct {
	Key         string             `json:"key"`
	Parallelism int                `json:"parallelism,omitempty"`
	Nodes       []commandGraphNode `json:"nodes"`
}

type commandGraphNode struct {
	ID        string                   `json:"id"`
	Kind      string                   `json:"kind,omitempty"`
	Command   string                   `json:"command,omitempty"`
	Task      string                   `json:"task,omitempty"`
	Timeout   int                      `json:"timeout,omitempty"`
	DependsOn []workergraph.Dependency `json:"depends_on,omitempty"`
	Resources []string                 `json:"resources"`
	Artifacts []string                 `json:"artifacts,omitempty"`
	Inputs    []commandGraphInput      `json:"inputs,omitempty"`
	ReuseFrom *commandGraphReuse       `json:"reuse_from,omitempty"`
	Optional  bool                     `json:"optional,omitempty"`
	When      *commandGraphWhen        `json:"when,omitempty"`
}

type commandGraphWhen struct {
	Node     string `json:"node"`
	Status   string `json:"status,omitempty"`
	Contains string `json:"contains,omitempty"`
}

type commandGraphOutput struct {
	Stdout     string               `json:"stdout"`
	OutputPath string               `json:"output_path"`
	ExitCode   int                  `json:"exit_code"`
	ReusedFrom *commandReuseReceipt `json:"reused_from,omitempty"`
}

type commandGraphEvidence struct {
	NodeID          string `json:"node_id"`
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	PreviewComplete bool   `json:"preview_complete"`
	StartLine       int    `json:"start_line,omitempty"`
	EndLine         int    `json:"end_line,omitempty"`
}

// Each call runs the currently known graph. The parent Loop can observe its
// results and append tasks on a later call; every child owns a separate Loop.
func commandGraphTool(j Job, o Options) agent.Tool {
	tool := agent.Tool{Definition: agent.Definition{
		Name:        "run_graph",
		Description: "Run a command/agent DAG. To extend, reuse the key with the full cumulative nodes list; retain every prior node unchanged. Completed nodes are reverified, not rerun. succeeded covers submitted nodes, not Step completion. kind=command (default) runs bash; kind=agent runs an independent Agent Loop with task text and file/bash tools. Children cannot delegate, publish blackboard records or finish the Step. Supply their context and synthesize results; same-Run Agents are not independent review. Nodes share a container but have private directories, not security sandboxes. PWNMESH_WORKSPACE and PWNMESH_NODE_DIR are absolute directories. Read dependencies with json.load(open(os.environ[\"PWNMESH_DEPENDENCIES\"])); each dependency has id, status and output.files[name]={path,sha256}. Use files by name: declare a separate JSON artifact instead of parsing combined logs. Declare artifacts as paths relative to PWNMESH_NODE_DIR; Agents must create them there. Consumer inputs:[{node,artifact}] requires a declared file from a direct required dependency; missing declarations fail before any node runs, changed bytes fail before consumption. output.value.stdout previews 8000 bytes of stdout/stderr; output.value.output_path locates the full stdout.log. Agent logs are accounts, not raw evidence. Required failure stops new nodes while active independent nodes finish; the key remains terminal. After a resolved failure, use a NEW key with kind=reuse,reuse_from:{key,node},resources:[] to import a successful node from this parent Run without executing it again. Do not override its task, command, dependencies or artifacts. All source definitions and files are verified before new work; missing definitions, changed files, unresolved operations and reuse chains are rejected, never silently rerun. Declare shared mutable resources and order nodes sharing them; resources:[] asserts independent effects. Required dependencies must succeed; optional dependencies allow failed/skipped results. when checks dependency status and optional full verified stdout contains text; failed/skipped routes need optional dependencies. Bounds: 1..64 nodes, parallelism 1..16 (default min(node count,16)), command timeout <=120s, Agent <=600s, files <=64 MiB, within parent deadline. verified_evidence contains only verified command logs; preview_complete=true supplies exact UTF-8 content and line bounds for finish_step. Assess meaning directly; reread only changed, additional or truncated evidence. observed_at is verification time, not acceptance.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"},"parallelism":{"type":"integer","minimum":1,"maximum":16},"nodes":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"id":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"},"kind":{"type":"string","enum":["command","agent","reuse"]},"command":{"type":"string","minLength":1,"maxLength":32768},"task":{"type":"string","minLength":1,"maxLength":32768},"timeout":{"type":"integer","minimum":1,"maximum":600},"depends_on":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"id":{"type":"string"},"optional":{"type":"boolean"}},"required":["id"],"additionalProperties":false}},"resources":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":128}},"artifacts":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256},"description":"Declared output files relative to PWNMESH_NODE_DIR; [] means logs only."},"optional":{"type":"boolean"},"when":{"type":"object","properties":{"node":{"type":"string"},"status":{"type":"string","enum":["succeeded","failed","skipped"]},"contains":{"type":"string","minLength":1,"maxLength":256}},"required":["node"],"additionalProperties":false},"inputs":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"node":{"type":"string"},"artifact":{"type":"string"}},"required":["node","artifact"],"additionalProperties":false},"description":"Required named files from direct required dependencies; validated before graph execution and before consumption."},"reuse_from":{"type":"object","properties":{"key":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"},"node":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"}},"required":["key","node"],"additionalProperties":false}},"required":["id","resources"],"additionalProperties":false}}},"required":["key","nodes"],"additionalProperties":false}`),
	}, Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
		return runCommandGraph(ctx, j, o, raw)
	}}
	return tool
}

func validateCommandGraph(spec *commandGraphSpec) error {
	if !commandGraphID.MatchString(spec.Key) || len(spec.Nodes) < 1 || len(spec.Nodes) > 64 {
		return errors.New("run_graph requires a safe key and 1..64 cumulative nodes")
	}
	if spec.Parallelism == 0 {
		spec.Parallelism = min(len(spec.Nodes), 16)
	}
	if spec.Parallelism < 1 || spec.Parallelism > 16 {
		return errors.New("run_graph parallelism must be 1..16")
	}
	byID := make(map[string]commandGraphNode, len(spec.Nodes))
	for i := range spec.Nodes {
		n := &spec.Nodes[i]
		if n.Kind == "" {
			n.Kind = "command"
		}
		if (n.Kind != "command" && n.Kind != "agent" && n.Kind != "reuse") || (n.Kind == "command" && (strings.TrimSpace(n.Command) == "" || n.Task != "")) || (n.Kind == "agent" && (strings.TrimSpace(n.Task) == "" || n.Command != "")) {
			return errors.New("node requires command, agent task, or reuse_from matching its kind")
		}
		if !commandGraphID.MatchString(n.ID) || len(n.Command) > 32768 || len(n.Task) > 32768 || len(n.DependsOn) > 64 || n.Resources == nil || len(n.Resources) > 16 || len(n.Artifacts) > 16 {
			return errors.New("invalid command graph node")
		}
		if _, exists := byID[n.ID]; exists {
			return errors.New("duplicate command graph node")
		}
		if n.Kind == "reuse" {
			if n.ReuseFrom == nil || !commandGraphID.MatchString(n.ReuseFrom.Key) || !commandGraphID.MatchString(n.ReuseFrom.Node) || n.ReuseFrom.Key == spec.Key || n.Command != "" || n.Task != "" || n.Timeout != 0 || len(n.DependsOn) != 0 || len(n.Resources) != 0 || len(n.Artifacts) != 0 || len(n.Inputs) != 0 || n.When != nil {
				return errors.New("reuse requires a different graph key and node; command, task, dependencies, resources, artifacts, inputs, timeout and when cannot be overridden")
			}
			byID[n.ID] = *n
			continue
		}
		if n.ReuseFrom != nil {
			return errors.New("reuse_from requires kind=reuse")
		}
		limit := 120
		if n.Kind == "agent" {
			limit = 600
		}
		if n.Timeout == 0 {
			n.Timeout = limit
		}
		if n.Timeout < 1 || n.Timeout > limit {
			return fmt.Errorf("%s node timeout must be 1..%d seconds", n.Kind, limit)
		}
		for _, resource := range n.Resources {
			if strings.TrimSpace(resource) == "" || len(resource) > 128 {
				return errors.New("invalid shared resource key")
			}
		}
		for _, path := range n.Artifacts {
			if !commandArtifactPath(path) {
				return errors.New("artifacts must be clean relative paths inside their node directory")
			}
		}
		slices.Sort(n.Resources)
		n.Resources = slices.Compact(n.Resources)
		slices.Sort(n.Artifacts)
		n.Artifacts = slices.Compact(n.Artifacts)
		slices.SortFunc(n.DependsOn, func(a, b workergraph.Dependency) int { return strings.Compare(a.ID, b.ID) })
		byID[n.ID] = *n
	}
	// Validate all edges before any node can execute. Reachability also makes
	// shared resource ordering explicit instead of blocking a goroutine on locks.
	ancestors := map[string]map[string]bool{}
	visiting := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("command graph dependency cycle")
		}
		if ancestors[id] != nil {
			return nil
		}
		n, ok := byID[id]
		if !ok {
			return errors.New("unknown command graph dependency")
		}
		visiting[id] = true
		a := map[string]bool{}
		for i, dep := range n.DependsOn {
			if i > 0 && dep.ID == n.DependsOn[i-1].ID {
				return errors.New("duplicate command graph dependency")
			}
			if err := visit(dep.ID); err != nil {
				return err
			}
			a[dep.ID] = true
			for parent := range ancestors[dep.ID] {
				a[parent] = true
			}
		}
		if n.When != nil {
			if n.When.Status == "" {
				n.When.Status = "succeeded"
			}
			if !slices.Contains([]string{"succeeded", "failed", "skipped"}, n.When.Status) || len(n.When.Contains) > 256 || !slices.ContainsFunc(n.DependsOn, func(d workergraph.Dependency) bool { return d.ID == n.When.Node }) || (n.When.Status != "succeeded" && n.When.Contains != "") {
				return errors.New("when requires a direct dependency, a terminal status and optional successful-output contains text")
			}
		}
		visiting[id], ancestors[id] = false, a
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	for i, a := range spec.Nodes {
		for _, b := range spec.Nodes[i+1:] {
			if ancestors[a.ID][b.ID] || ancestors[b.ID][a.ID] {
				continue
			}
			for _, resource := range a.Resources {
				if slices.Contains(b.Resources, resource) {
					return fmt.Errorf("shared resource %q requires ordered nodes %s and %s", resource, a.ID, b.ID)
				}
			}
		}
	}
	slices.SortFunc(spec.Nodes, func(a, b commandGraphNode) int { return strings.Compare(a.ID, b.ID) })
	return nil
}

func commandArtifactPath(path string) bool {
	return path != "" && len(path) <= 256 && path != "." && path != ".." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, "../") && path != "dependencies.json" && path != "stdout.log" && path != "session.json" && path != "events.jsonl"
}

// The run root is already identity-bound by runSession. Reject symlinked child
// directories so a reused key cannot redirect checkpoint or artifact writes.
func commandGraphDirectory(root string, elements ...string) (string, error) {
	dir := root
	for _, element := range elements {
		dir = filepath.Join(dir, element)
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("command graph directory must be a real directory")
		}
	}
	return dir, nil
}

func runCommandGraph(ctx context.Context, j Job, o Options, raw json.RawMessage) (string, error) {
	if !o.graphDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, o.graphDeadline)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var spec commandGraphSpec
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return "", err
	}
	if err := validateCommandGraph(&spec); err != nil {
		return "", err
	}
	identity, err := identityFor(j, o.RunDir)
	if err != nil {
		return "", err
	}
	reuses, releaseSources, err := prepareCommandReuses(ctx, j, o, spec)
	if err != nil {
		return "", err
	}
	defer releaseSources()
	inputSpec := spec
	inputSpec.Nodes = slices.Clone(spec.Nodes)
	for i := range inputSpec.Nodes {
		if reused := reuses[inputSpec.Nodes[i].ID]; reused != nil {
			inputSpec.Nodes[i].Artifacts = reused.SourceSpec.Artifacts
		}
	}
	if err := validateCommandInputs(&inputSpec); err != nil {
		return "", err
	}
	dir, err := commandGraphDirectory(o.RunDir, "graph-tools", spec.Key)
	if err != nil {
		return "", err
	}
	unlock, err := process.Lock(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	input, err := json.Marshal(struct {
		Identity executionIdentity `json:"identity"`
		Key      string            `json:"key"`
	}{identity, spec.Key})
	if err != nil {
		return "", err
	}
	definition := workergraph.Definition{Version: "mixed-dag-v2"}
	for _, specNode := range spec.Nodes {
		nodeSpec := specNode
		nodeDir := filepath.Join(dir, "nodes", nodeSpec.ID)
		node := commandNodeDefinition(nodeSpec)
		if reused := reuses[nodeSpec.ID]; reused != nil {
			node.Kind = reused.SourceNode.Kind
			node.Run = func(ctx context.Context, _ workergraph.Input) (workergraph.Output, error) {
				if err := ctx.Err(); err != nil {
					return workergraph.Output{}, err
				}
				return reused.Output(), nil
			}
			node.Verify = func(ctx context.Context, _ workergraph.Input, out workergraph.Output) error {
				return reused.Verify(ctx, out)
			}
			definition.Nodes = append(definition.Nodes, node)
			continue
		}
		node.Run = func(ctx context.Context, in workergraph.Input) (workergraph.Output, error) {
			if err := process.CheckLaunch(o.RunDir); err != nil {
				return workergraph.Output{}, err
			}
			if process.Cancelled(o.RunDir) {
				return workergraph.Output{}, context.Canceled
			}
			if err := validateCommandInputFiles(ctx, nodeSpec, in.Dependencies); err != nil {
				return workergraph.Output{}, err
			}
			if _, err := commandGraphDirectory(dir, "nodes", nodeSpec.ID); err != nil {
				return workergraph.Output{}, err
			}
			if nodeSpec.Kind == "agent" {
				return executeAgentNode(ctx, j, o, spec.Key, nodeDir, nodeSpec, in.Dependencies)
			}
			return executeCommandNode(ctx, o.RunDir, j.Workspace, nodeDir, nodeSpec, in.Dependencies)
		}
		node.Verify = func(ctx context.Context, _ workergraph.Input, out workergraph.Output) error {
			return verifyCommandNodeOutput(ctx, nodeDir, nodeSpec, out)
		}
		if nodeSpec.When != nil {
			node.When = func(ctx context.Context, in workergraph.Input) (bool, string, error) {
				if err := ctx.Err(); err != nil {
					return false, "", err
				}
				for _, dep := range in.Dependencies {
					if dep.ID == nodeSpec.When.Node {
						matches := dep.Status == nodeSpec.When.Status
						if matches && nodeSpec.When.Contains != "" {
							var err error
							depDir := filepath.Join(dir, "nodes", dep.ID)
							if reused := reuses[dep.ID]; reused != nil {
								depDir = reused.SourceDir
							}
							matches, err = commandOutputContains(ctx, depDir, dep.Output, nodeSpec.When.Contains)
							if err != nil {
								return false, "", err
							}
						}
						if matches {
							return true, "", nil
						}
						return false, "dependency status or output did not match condition", nil
					}
				}
				return false, "", errors.New("condition dependency missing")
			}
		}
		// No Reconcile callback: a crash after shell side effects but before the
		// durable receipt requires inspection, never an automatic command replay.
		definition.Nodes = append(definition.Nodes, node)
	}
	if err := storeCommandDefinitions(dir, spec, definition); err != nil {
		return "", err
	}
	started := time.Now()
	checkpoint, runErr := workergraph.Run(ctx, definition, workergraph.Options{RunID: j.RunID, Input: input, Dir: dir, Parallelism: spec.Parallelism, Extend: true, DrainOnFailure: true})
	response := struct {
		Key            string                 `json:"key"`
		Status         string                 `json:"status"`
		ElapsedMS      float64                `json:"elapsed_ms"`
		Nodes          []commandGraphNodeView `json:"nodes"`
		ObservedAt     string                 `json:"observed_at"`
		Verification   string                 `json:"verification"`
		Evidence       []commandGraphEvidence `json:"verified_evidence"`
		EvidenceErrors map[string]string      `json:"evidence_errors,omitempty"`
		Error          string                 `json:"error,omitempty"`
	}{Key: spec.Key, Status: checkpoint.Status, ElapsedMS: float64(time.Since(started)) / float64(time.Millisecond), Nodes: commandGraphViews(checkpoint.Nodes),
		ObservedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Verification: "Only verified_evidence entries passed final regular-file, no-symlink, full-file SHA-256 and stdout-preview checks after successful node execution. Node statuses describe execution history; evidence_errors identify outputs that changed or became unreadable before handoff. This verifies retained bytes, not task meaning or Step acceptance.",
		Evidence:     []commandGraphEvidence{}, EvidenceErrors: map[string]string{}}
	for _, node := range checkpoint.Nodes {
		if runErr != nil {
			break // Recovery errors may leave old succeeded statuses unverified.
		}
		// An Agent's final account is an interpretation, not a raw observation.
		// Keep it in node output for the parent to assess, but never offer it as
		// the direct evidence shortcut accepted by finish_step.
		if node.Kind != "function" || node.Status != "succeeded" || len(node.Output.Artifacts) == 0 {
			continue
		}
		artifact := node.Output.Artifacts[0]
		var value commandGraphOutput
		if json.Unmarshal(node.Output.Value, &value) != nil {
			continue
		}
		outputDir := filepath.Join(dir, "nodes", node.ID)
		if reused := reuses[node.ID]; reused != nil {
			outputDir = reused.SourceDir
		}
		current, preview, err := inspectCommandFile(ctx, outputDir, "stdout.log", 8000)
		if err != nil {
			response.EvidenceErrors[node.ID] = err.Error()
			continue
		}
		if current != artifact || preview.Text != value.Stdout {
			response.EvidenceErrors[node.ID] = "stdout changed after node verification"
			continue
		}
		ref := commandGraphEvidence{NodeID: node.ID, Path: artifact.Path, SHA256: artifact.SHA256}
		// Completeness and hashing come from the same streaming read. A large
		// unchanged file remains verified, but needs an explicit excerpt.
		if preview.Complete {
			ref.PreviewComplete = true
			ref.StartLine = 1
			ref.EndLine = strings.Count(preview.Text, "\n")
			if !strings.HasSuffix(preview.Text, "\n") {
				ref.EndLine++
			}
		}
		response.Evidence = append(response.Evidence, ref)
	}
	if runErr != nil {
		response.Error = runErr.Error()
	}
	encoded, err := json.Marshal(response)
	return string(encoded), errors.Join(runErr, err)
}

func verifyCommandNodeOutput(ctx context.Context, dir string, spec commandGraphNode, out workergraph.Output) error {
	var value commandGraphOutput
	if json.Unmarshal(out.Value, &value) != nil || value.ExitCode != 0 || value.ReusedFrom != nil || value.OutputPath != filepath.Join(dir, "stdout.log") || len(out.Artifacts) != len(spec.Artifacts)+1 {
		return errors.New("invalid command node output")
	}
	paths := append([]string{"stdout.log"}, spec.Artifacts...)
	for i, path := range paths {
		previewLimit := 0
		if i == 0 {
			previewLimit = 8000
		}
		artifact, preview, err := commandFile(ctx, dir, path, previewLimit)
		if err != nil {
			return err
		}
		if out.Artifacts[i] != artifact {
			return errors.New("command node artifact SHA-256 changed")
		}
		if i == 0 && value.Stdout != preview {
			return errors.New("command node stdout preview differs from its artifact")
		}
	}
	return nil
}

func executeCommandNode(ctx context.Context, runDir, workspace, dir string, spec commandGraphNode, dependencies []workergraph.NodeState) (workergraph.Output, error) {
	if dependencies == nil {
		dependencies = []workergraph.NodeState{}
	}
	deps, err := json.Marshal(commandGraphViews(dependencies))
	if err != nil {
		return workergraph.Output{}, err
	}
	depsPath := filepath.Join(dir, "dependencies.json")
	if err := commandNewFile(depsPath, deps); err != nil {
		return workergraph.Output{}, err
	}
	path := filepath.Join(dir, "stdout.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return workergraph.Output{}, err
	}
	child, cancel := context.WithTimeout(ctx, time.Duration(spec.Timeout)*time.Second)
	defer cancel()
	// Bash sets both soft and hard limits when neither -S nor -H is supplied.
	// Its file-size unit is 1024 bytes; children inherit this per-file ceiling.
	// Pass model commands as an argument, never interpolate them in the wrapper.
	args := append(graphNodeEnvironment(workspace, dir), "bash", "-c", `ulimit -f 65536 || exit; exec bash -c "$1"`, "pwnmesh-run-graph", spec.Command)
	commandErr := process.Run(child, dir, runDir, f, "env", args...)
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return workergraph.Output{}, errors.Join(commandErr, err)
	}
	if ctx.Err() != nil {
		return workergraph.Output{}, ctx.Err()
	}
	exitCode := 0
	if commandErr != nil {
		var exit *exec.ExitError
		if errors.As(commandErr, &exit) {
			exitCode = exit.ExitCode()
		} else {
			return workergraph.Output{}, fmt.Errorf("command failed; inspect %s: %w", path, commandErr)
		}
	}
	artifact, preview, err := commandFile(ctx, dir, "stdout.log", 8000)
	if err != nil {
		return workergraph.Output{}, errors.Join(commandErr, err)
	}
	value, _ := json.Marshal(commandGraphOutput{Stdout: preview, OutputPath: path, ExitCode: exitCode})
	out := workergraph.Output{Value: value, Artifacts: []workergraph.Artifact{artifact}}
	if commandErr != nil {
		return out, fmt.Errorf("command exited %d; inspect %s: %w", exitCode, path, commandErr)
	}
	for _, name := range spec.Artifacts {
		artifact, _, err := commandFile(ctx, dir, name, 0)
		if err != nil {
			return out, err
		}
		out.Artifacts = append(out.Artifacts, artifact)
	}
	return out, nil
}

func graphNodeEnvironment(workspace, dir string) []string {
	return []string{"PWNMESH_WORKSPACE=" + workspace, "PWNMESH_NODE_DIR=" + dir, "PWNMESH_DEPENDENCIES=" + filepath.Join(dir, "dependencies.json")}
}

func commandNewFile(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

// Stream full artifacts into the digest while retaining only a bounded preview.
func commandFile(ctx context.Context, dir, name string, previewLimit int) (workergraph.Artifact, string, error) {
	artifact, preview, err := inspectCommandFile(ctx, dir, name, previewLimit)
	return artifact, preview.Text, err
}

type commandPreview struct {
	Text     string
	Complete bool
}

// Conditions consume the same retained bytes that are verified, not the lossy
// preview. Keep only a chunk and its overlapping suffix, and finish hashing even
// after a match so a changed file cannot authorize a downstream side effect.
func commandOutputContains(ctx context.Context, dir string, out workergraph.Output, contains string) (bool, error) {
	var value commandGraphOutput
	if json.Unmarshal(out.Value, &value) != nil || value.ExitCode != 0 || value.OutputPath != filepath.Join(dir, "stdout.log") || len(out.Artifacts) == 0 {
		return false, errors.New("invalid condition dependency output")
	}
	needle := []byte(contains)
	matched := len(needle) == 0
	window := make([]byte, 0, (64<<10)+len(needle))
	artifact, preview, err := inspectCommandFileChunks(ctx, dir, "stdout.log", 8000, func(chunk []byte) {
		if matched {
			return
		}
		window = append(window, chunk...)
		matched = bytes.Contains(window, needle)
		keep := min(len(window), len(needle)-1)
		copy(window, window[len(window)-keep:])
		window = window[:keep]
	})
	if err != nil {
		return false, err
	}
	if artifact != out.Artifacts[0] || preview.Text != value.Stdout {
		return false, errors.New("condition dependency stdout SHA-256 or preview changed")
	}
	return matched, nil
}

func inspectCommandFile(ctx context.Context, dir, name string, previewLimit int) (workergraph.Artifact, commandPreview, error) {
	return inspectCommandFileChunks(ctx, dir, name, previewLimit, nil)
}

func inspectCommandFileChunks(ctx context.Context, dir, name string, previewLimit int, consume func([]byte)) (workergraph.Artifact, commandPreview, error) {
	path := filepath.Join(dir, name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return workergraph.Artifact{}, commandPreview{}, fmt.Errorf("command artifact %q must exist and not use symlinks", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return workergraph.Artifact{}, commandPreview{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return workergraph.Artifact{}, commandPreview{}, errors.New("command artifact must be a regular file no larger than 64 MiB")
	}
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	preview := make([]byte, 0, previewLimit)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return workergraph.Artifact{}, commandPreview{}, err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			size += int64(n)
			if size > 64<<20 {
				return workergraph.Artifact{}, commandPreview{}, errors.New("command artifact grew beyond 64 MiB")
			}
			digest.Write(buffer[:n])
			preview = append(preview, buffer[:min(n, previewLimit-len(preview))]...)
			if consume != nil {
				consume(buffer[:n])
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return workergraph.Artifact{}, commandPreview{}, readErr
		}
	}
	after, err := f.Stat()
	if err != nil || info.Size() != size || after.Size() != size || !info.ModTime().Equal(after.ModTime()) {
		return workergraph.Artifact{}, commandPreview{}, errors.New("command artifact changed while reading")
	}
	text := strings.ToValidUTF8(string(preview), "�")
	if previewLimit > 0 && info.Size() > int64(previewLimit) {
		text += "\n[Preview truncated; read output_path for full output.]"
	}
	return workergraph.Artifact{Path: path, SHA256: hex.EncodeToString(digest.Sum(nil))}, commandPreview{Text: text, Complete: size == int64(len(preview)) && utf8.Valid(preview) && strings.TrimSpace(text) != ""}, nil
}
