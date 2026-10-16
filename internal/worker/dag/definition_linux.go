//go:build linux

package dag

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"pwnmesh/internal/workergraph"
)

var commandGraphID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// v2 retained combined stdout/stderr. Its bytes cannot be safely reinterpreted
// as stdout, so recovery and reuse must reject it without replaying commands.
const commandGraphVersion = "mixed-dag-v3"

type Spec struct {
	Key         string `json:"key"`
	Parallelism int    `json:"parallelism,omitempty"`
	Nodes       []Node `json:"nodes"`
}

type Node struct {
	ID        string                   `json:"id"`
	Kind      string                   `json:"kind,omitempty"`
	Command   string                   `json:"command,omitempty"`
	Task      string                   `json:"task,omitempty"`
	Timeout   int                      `json:"timeout,omitempty"`
	DependsOn []workergraph.Dependency `json:"depends_on,omitempty"`
	Resources []string                 `json:"resources"`
	Artifacts []string                 `json:"artifacts,omitempty"`
	Inputs    []Input                  `json:"inputs,omitempty"`
	ReuseFrom *Reuse                   `json:"reuse_from,omitempty"`
	Optional  bool                     `json:"optional,omitempty"`
	When      *When                    `json:"when,omitempty"`
}

type When struct {
	Node     string `json:"node"`
	Status   string `json:"status,omitempty"`
	Contains string `json:"contains,omitempty"`
}

type Output struct {
	Stdout     string        `json:"stdout"`
	OutputPath string        `json:"output_path"`
	Stderr     string        `json:"stderr,omitempty"`
	StderrPath string        `json:"stderr_path,omitempty"`
	ExitCode   int           `json:"exit_code"`
	ReusedFrom *ReuseReceipt `json:"reused_from,omitempty"`
}

type Evidence struct {
	NodeID              string `json:"node_id"`
	Path                string `json:"path"`
	SHA256              string `json:"sha256"`
	PreviewComplete     bool   `json:"preview_complete"`
	StartLine           int    `json:"start_line,omitempty"`
	EndLine             int    `json:"end_line,omitempty"`
	VerifiedAt          string `json:"verified_at,omitempty"`
	ExecutionStartedAt  string `json:"execution_started_at,omitempty"`
	ExecutionFinishedAt string `json:"execution_finished_at,omitempty"`
}

func validateCommandGraph(spec *Spec) error {
	if !commandGraphID.MatchString(spec.Key) || len(spec.Nodes) < 1 || len(spec.Nodes) > 64 {
		return errors.New("run_graph requires a safe key and 1..64 cumulative nodes")
	}
	if spec.Parallelism == 0 {
		spec.Parallelism = min(len(spec.Nodes), 16)
	}
	if spec.Parallelism < 1 || spec.Parallelism > 16 {
		return errors.New("run_graph parallelism must be 1..16")
	}
	byID := make(map[string]Node, len(spec.Nodes))
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
			if strings.TrimSpace(resource) == "" || resource == "path:" || len(resource) > 128 {
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
			// A required edge blocks before When can observe failure or a skip.
			// A required node failure stops the entire graph even with an optional
			// edge. Reject these unreachable routes before any source side effect.
			if n.When.Status != "succeeded" && !slices.ContainsFunc(n.DependsOn, func(d workergraph.Dependency) bool { return d.ID == n.When.Node && d.Optional }) {
				return errors.New("when failed/skipped requires an optional dependency")
			}
			if n.When.Status == "failed" && !byID[n.When.Node].Optional {
				return errors.New("when failed requires an optional source node; required failure stops the graph")
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
			if resourcesConflict(a.Resources, b.Resources, "") {
				return fmt.Errorf("shared resource requires ordered nodes %s and %s", a.ID, b.ID)
			}
		}
	}
	slices.SortFunc(spec.Nodes, func(a, b Node) int { return strings.Compare(a.ID, b.ID) })
	return nil
}

func commandArtifactPath(path string) bool {
	return path != "" && len(path) <= 256 && path != "." && path != ".." && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, "../") && path != "dependencies.json" && path != "stdout.log" && path != "stderr.log" && path != "session.json" && path != "events.jsonl"
}
