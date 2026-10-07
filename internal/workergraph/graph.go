//go:build linux

// Package workergraph runs small code-defined graphs around existing work.
// Agent callbacks own their existing Loop; this package never calls a model.
package workergraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

const maxNodes = 64
const maxValueBytes = 1 << 20

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ErrInterrupted leaves a started callback unresolved for explicit same-run
// recovery. It is distinct from a terminal business or deterministic failure.
var ErrInterrupted = errors.New("graph node interrupted; reconciliation required")

type Dependency struct {
	ID       string `json:"id"`
	Optional bool   `json:"optional,omitempty"`
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Output struct {
	Value     json.RawMessage `json:"value,omitempty"`
	Artifacts []Artifact      `json:"artifacts,omitempty"`
}

// Input and its dependency results belong to one callback invocation. They do
// not share mutable JSON bytes or artifact slices with concurrent callbacks.
type Input struct {
	NodeID       string
	Attempt      int
	Initial      json.RawMessage
	Value        json.RawMessage
	Dependencies []NodeState
}

type Node struct {
	ID        string
	Kind      string // function or agent; both use the same callback interface.
	Input     json.RawMessage
	DependsOn []Dependency
	Optional  bool // A failed optional node need not fail the whole graph.
	Run       func(context.Context, Input) (Output, error)
	// When must be a pure check. False requires a nonempty durable skip reason.
	// It shares the node's parallel slot with Run and Verify, honors context,
	// and may be evaluated again after interruption before Run starts.
	When func(context.Context, Input) (bool, string, error)
	// Verify checks a result before acceptance and is required when reusing a
	// successful node after restart, including its external artifacts. Live
	// verification occupies the node's parallel slot and may run concurrently
	// with independent nodes, so it must honor context cancellation.
	Verify func(context.Context, Input, Output) error
	// Reconcile owns recovery after a crash during a callback. It must inspect
	// external state or resume an existing session, never blindly replay an
	// uncertain side effect. A nil callback makes this boundary fail closed.
	Reconcile func(context.Context, Input, NodeState) (Output, error)
}

type Definition struct {
	Version string
	Nodes   []Node
}

type Options struct {
	RunID       string
	Input       json.RawMessage
	Dir         string
	Parallelism int
	// DrainOnFailure lets already-started independent Run callbacks finish
	// after an ordinary required Run failure. No further Run callbacks start.
	// Conditions are cancelled; interruption, required verification/condition
	// failure, parent cancellation and checkpoint errors still stop active work.
	DrainOnFailure bool
	// Extend allows adding nodes to the same run after validating every saved
	// node. Existing definitions, results and failure boundaries stay intact.
	Extend bool
}

type NodeState struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	Status           string    `json:"status"`
	Attempt          int       `json:"attempt"`
	DefinitionSHA256 string    `json:"definition_sha256,omitempty"`
	InputSHA256      string    `json:"input_sha256,omitempty"`
	Output           Output    `json:"output"`
	Error            string    `json:"error,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
	ReadyAt          time.Time `json:"ready_at,omitempty"`
	// Durations cover callback work, not checkpoint I/O. Pure condition retries
	// accumulate; recovery never overwrites the original execution timings.
	ConditionDurationMS      float64 `json:"condition_duration_ms,omitempty"`
	RunDurationMS            float64 `json:"run_duration_ms,omitempty"`
	VerifyDurationMS         float64 `json:"verify_duration_ms,omitempty"`
	ReconcileDurationMS      float64 `json:"reconcile_duration_ms,omitempty"`
	RecoveryVerifyDurationMS float64 `json:"recovery_verify_duration_ms,omitempty"`
}

type Checkpoint struct {
	SchemaVersion    int         `json:"schema_version"`
	RunID            string      `json:"run_id"`
	Version          string      `json:"version"`
	DefinitionSHA256 string      `json:"definition_sha256"`
	InputSHA256      string      `json:"input_sha256"`
	Status           string      `json:"status"`
	Nodes            []NodeState `json:"nodes"`
}

type nodeDefinition struct {
	ID, Kind              string
	Input                 json.RawMessage
	DependsOn             []Dependency
	Optional, Conditional bool
}

func describe(node Node) nodeDefinition {
	return nodeDefinition{node.ID, node.Kind, node.Input, node.DependsOn, node.Optional, node.When != nil}
}

func hash(v any) string {
	raw, _ := json.Marshal(v)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validValue(raw json.RawMessage) bool {
	return len(raw) == 0 || len(raw) <= maxValueBytes && json.Valid(raw)
}

func prepare(d Definition, o Options) ([]Node, string, error) {
	if d.Version == "" || len(d.Version) > 128 || !validID.MatchString(o.RunID) || o.Dir == "" || !validValue(o.Input) || len(d.Nodes) == 0 || len(d.Nodes) > maxNodes || o.Parallelism < 1 || o.Parallelism > 16 {
		return nil, "", errors.New("graph requires a version, run identity, checkpoint directory, bounded JSON input, 1..64 nodes and parallelism 1..16")
	}
	nodes := append([]Node(nil), d.Nodes...)
	slices.SortFunc(nodes, func(a, b Node) int { return compareID(a.ID, b.ID) })
	byID := map[string]Node{}
	var definitions []nodeDefinition
	for n := range nodes {
		node := &nodes[n]
		if !validID.MatchString(node.ID) || (node.Kind != "function" && node.Kind != "agent") || node.Run == nil || !validValue(node.Input) {
			return nil, "", errors.New("invalid graph node")
		}
		if _, exists := byID[node.ID]; exists {
			return nil, "", errors.New("duplicate graph node")
		}
		node.Input = append(json.RawMessage(nil), node.Input...)
		node.DependsOn = append([]Dependency(nil), node.DependsOn...)
		slices.SortFunc(node.DependsOn, func(a, b Dependency) int { return compareID(a.ID, b.ID) })
		for i, dependency := range node.DependsOn {
			if dependency.ID == node.ID || i > 0 && dependency.ID == node.DependsOn[i-1].ID {
				return nil, "", errors.New("self or duplicate graph dependency")
			}
		}
		byID[node.ID] = *node
		definitions = append(definitions, describe(*node))
	}
	visited, visiting := map[string]bool{}, map[string]bool{}
	var ordered []Node
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("graph dependency cycle")
		}
		if visited[id] {
			return nil
		}
		node, exists := byID[id]
		if !exists {
			return errors.New("unknown graph dependency")
		}
		visiting[id] = true
		for _, dep := range node.DependsOn {
			if err := visit(dep.ID); err != nil {
				return err
			}
		}
		visiting[id], visited[id] = false, true
		ordered = append(ordered, node)
		return nil
	}
	for _, node := range nodes {
		if err := visit(node.ID); err != nil {
			return nil, "", err
		}
	}
	return ordered, hash(definitions), nil
}

func compareID(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// Prefer ready work that unlocks the longest remaining dependency chain. IDs
// only break ties, so an unrelated leaf cannot consume every parallel slot
// merely because its name sorts first. This is a bounded unit-cost heuristic,
// not an estimate of callback duration. Keep nodes in topological order for
// recovery and definition binding; scheduling gets its own ordered copy.
func schedulingOrder(nodes []Node, states map[string]*NodeState) []Node {
	depth := make(map[string]int, len(nodes))
	for i := len(nodes) - 1; i >= 0; i-- {
		node := nodes[i]
		if status := states[node.ID].Status; status != "pending" && status != "running" {
			continue
		}
		depth[node.ID] = max(depth[node.ID], 1)
		for _, dep := range node.DependsOn {
			if status := states[dep.ID].Status; status == "pending" || status == "running" {
				depth[dep.ID] = max(depth[dep.ID], depth[node.ID]+1)
			}
		}
	}
	ordered := slices.Clone(nodes)
	slices.SortFunc(ordered, func(a, b Node) int {
		if depth[a.ID] != depth[b.ID] {
			return depth[b.ID] - depth[a.ID]
		}
		return compareID(a.ID, b.ID)
	})
	return ordered
}

// Callback boundaries need owned slices, not JSON encoding. Copy raw values
// exactly so isolation neither rewrites input bytes nor hides malformed output.
func cloneOutput(output Output) Output {
	output.Value = slices.Clone(output.Value)
	output.Artifacts = slices.Clone(output.Artifacts)
	return output
}

func cloneNodeState(state NodeState) NodeState {
	state.Output = cloneOutput(state.Output)
	return state
}

func cloneInput(input Input) Input {
	input.Initial = slices.Clone(input.Initial)
	input.Value = slices.Clone(input.Value)
	input.Dependencies = slices.Clone(input.Dependencies)
	for i := range input.Dependencies {
		input.Dependencies[i] = cloneNodeState(input.Dependencies[i])
	}
	return input
}

// A condition runs before the node's side effects and must be pure. Other
// callback panics leave an already-started operation requiring reconciliation.
func invoke[T any](phase string, uncertain bool, callback func() (T, error)) (value T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("node %s callback panic: %v", phase, recovered)
			if uncertain {
				err = errors.Join(ErrInterrupted, err)
			}
		}
	}()
	return callback()
}

func nodeInput(node Node, state NodeState, initial json.RawMessage, states map[string]*NodeState) Input {
	// The scheduler owns these immutable bytes. Keep a read-only view here;
	// every callback gets its own cloneInput at the invocation boundary.
	// Copy dependency metadata so later recovery timing updates cannot alter it.
	input := Input{NodeID: node.ID, Attempt: state.Attempt, Initial: initial, Value: node.Input}
	// Preserve the existing hash representation for empty RawMessage slices.
	if len(input.Initial) == 0 {
		input.Initial = nil
	}
	if len(input.Value) == 0 {
		input.Value = nil
	}
	if len(node.DependsOn) > 0 {
		input.Dependencies = make([]NodeState, 0, len(node.DependsOn))
	}
	for _, dep := range node.DependsOn {
		input.Dependencies = append(input.Dependencies, *states[dep.ID])
	}
	return input
}

func inputHash(input Input) string {
	// Attempts and timing are observations, not semantic inputs. Stable IDs,
	// statuses and outputs still bind conditional and optional dependencies.
	type dependency struct {
		ID, Status, Reason, Error string
		Output                    Output
	}
	deps := make([]dependency, 0, len(input.Dependencies))
	for _, state := range input.Dependencies {
		deps = append(deps, dependency{state.ID, state.Status, state.Reason, state.Error, state.Output})
	}
	return hash(struct {
		ID             string
		Initial, Value json.RawMessage
		Dependencies   []dependency
	}{input.NodeID, input.Initial, input.Value, deps})
}

func validateOutput(output Output) error {
	if !validValue(output.Value) || len(output.Artifacts) > 64 {
		return errors.New("invalid or oversized graph node output")
	}
	for _, artifact := range output.Artifacts {
		digest, err := hex.DecodeString(artifact.SHA256)
		if !filepath.IsAbs(artifact.Path) || len(artifact.Path) > 4096 || err != nil || len(digest) != sha256.Size {
			return errors.New("artifact requires an absolute path and SHA-256")
		}
	}
	return nil
}

func save(dir string, checkpoint Checkpoint) error {
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".graph-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(dir, "graph.json"))
	}
	if err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Run requires its caller to exclusively own Dir for the run (normally the
// existing Worker process lock). Independent graph nodes need independent Loop
// instances, session directories and artifact ownership in their callbacks.
// Callback failures do not authorize business retries. An interrupted callback
// stays running until an explicit Reconcile resolves the uncertain boundary.
func Run(ctx context.Context, definition Definition, options Options) (Checkpoint, error) {
	// Bind all node inputs to one owned snapshot, including later dependants.
	// A callback or caller retaining the supplied bytes cannot change the run
	// after its input digest has been persisted.
	options.Input = slices.Clone(options.Input)
	nodes, signature, err := prepare(definition, options)
	if err != nil {
		return Checkpoint{}, err
	}
	if err = ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	if err = os.MkdirAll(options.Dir, 0700); err != nil {
		return Checkpoint{}, err
	}
	checkpoint := Checkpoint{SchemaVersion: 1, RunID: options.RunID, Version: definition.Version, DefinitionSHA256: signature, InputSHA256: hash(options.Input), Status: "running"}
	byID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
		checkpoint.Nodes = append(checkpoint.Nodes, NodeState{ID: node.ID, Kind: node.Kind, DefinitionSHA256: hash(describe(node)), Status: "pending"})
	}
	slices.SortFunc(checkpoint.Nodes, func(a, b NodeState) int { return compareID(a.ID, b.ID) })
	resumed := false
	if raw, readErr := os.ReadFile(filepath.Join(options.Dir, "graph.json")); readErr == nil {
		var saved Checkpoint
		if json.Unmarshal(raw, &saved) != nil || saved.SchemaVersion != 1 || saved.RunID != checkpoint.RunID || saved.Version != checkpoint.Version || saved.InputSHA256 != checkpoint.InputSHA256 || len(saved.Nodes) == 0 || len(saved.Nodes) > len(checkpoint.Nodes) || !slices.Contains([]string{"running", "succeeded", "failed", "interrupted"}, saved.Status) {
			return checkpoint, errors.New("graph checkpoint identity, definition or input mismatch")
		}
		extending := len(saved.Nodes) < len(checkpoint.Nodes)
		if !options.Extend && (extending || saved.DefinitionSHA256 != signature) {
			return checkpoint, errors.New("graph checkpoint definition mismatch")
		}
		var savedDefinitions []nodeDefinition
		for i, state := range saved.Nodes {
			node, exists := byID[state.ID]
			if !exists || i > 0 && saved.Nodes[i-1].ID >= state.ID || state.Kind != node.Kind || state.Attempt < 0 || state.Attempt > 1 || !slices.Contains([]string{"pending", "running", "succeeded", "skipped", "failed", "blocked", "cancelled"}, state.Status) || validateOutput(state.Output) != nil {
				return checkpoint, errors.New("invalid graph node checkpoint")
			}
			if state.DefinitionSHA256 == "" && extending {
				return checkpoint, errors.New("legacy graph checkpoint requires original definition recovery before extension")
			}
			if state.DefinitionSHA256 != "" && state.DefinitionSHA256 != hash(describe(node)) {
				return checkpoint, fmt.Errorf("node %s definition changed", node.ID)
			}
			savedDefinitions = append(savedDefinitions, describe(node))
			if (state.Status == "running" || state.Status == "succeeded") && (state.Attempt != 1 || state.StartedAt.IsZero()) || state.Status == "succeeded" && state.FinishedAt.IsZero() || (state.Status == "pending" || state.Status == "skipped" || state.Status == "blocked") && state.Attempt != 0 {
				return checkpoint, errors.New("invalid graph node attempt or timing")
			}
		}
		if hash(savedDefinitions) != saved.DefinitionSHA256 {
			return checkpoint, errors.New("graph checkpoint definition mismatch")
		}
		checkpoint, resumed = saved, true
	} else if !os.IsNotExist(readErr) {
		return checkpoint, readErr
	}
	states := map[string]*NodeState{}
	for i := range checkpoint.Nodes {
		states[checkpoint.Nodes[i].ID] = &checkpoint.Nodes[i]
	}
	persist := func() error { return save(options.Dir, checkpoint) }
	accept := func(ctx context.Context, node Node, input Input, output Output) error {
		if err := validateOutput(output); err != nil {
			return err
		}
		if node.Verify != nil {
			_, err := invoke("verification", true, func() (struct{}, error) {
				return struct{}{}, node.Verify(ctx, cloneInput(input), cloneOutput(output))
			})
			return err
		}
		return nil
	}
	if resumed {
		for _, node := range nodes {
			state := states[node.ID]
			if state == nil {
				continue // New nodes do not exist durably until recovery succeeds.
			}
			if state.Status == "pending" || state.Status == "blocked" || state.Status == "cancelled" || state.Status == "failed" {
				continue
			}
			input := nodeInput(node, *state, options.Input, states)
			if state.InputSHA256 != inputHash(input) {
				return checkpoint, fmt.Errorf("node %s input binding changed", node.ID)
			}
			switch state.Status {
			case "succeeded":
				if node.Verify == nil {
					return checkpoint, fmt.Errorf("node %s requires verification before reuse", node.ID)
				}
				started := time.Now()
				verifyErr := accept(ctx, node, input, state.Output)
				state.RecoveryVerifyDurationMS += float64(time.Since(started)) / float64(time.Millisecond)
				if verifyErr != nil {
					return checkpoint, errors.Join(fmt.Errorf("node %s result verification: %w", node.ID, verifyErr), persist())
				}
			case "skipped":
				if state.Reason == "" {
					return checkpoint, errors.New("skipped node requires a reason")
				}
			case "running":
				if node.Reconcile == nil {
					return checkpoint, fmt.Errorf("node %s has an uncertain side effect; reconciliation required", node.ID)
				}
				started := time.Now()
				output, reconcileErr := invoke("reconciliation", true, func() (Output, error) {
					return node.Reconcile(ctx, cloneInput(input), cloneNodeState(*state))
				})
				state.ReconcileDurationMS += float64(time.Since(started)) / float64(time.Millisecond)
				if reconcileErr == nil {
					started = time.Now()
					reconcileErr = accept(ctx, node, input, output)
					state.RecoveryVerifyDurationMS += float64(time.Since(started)) / float64(time.Millisecond)
				}
				if reconcileErr != nil {
					state.Error = reconcileErr.Error()
					if err = persist(); err != nil {
						return checkpoint, errors.Join(reconcileErr, err)
					}
					return checkpoint, fmt.Errorf("node %s reconciliation: %w", node.ID, reconcileErr)
				}
				state.Output, state.Status, state.Error, state.FinishedAt = cloneOutput(output), "succeeded", "", time.Now().UTC()
				if err = persist(); err != nil {
					return checkpoint, err
				}
			}
		}
		// Keep recovery writes bound to the old definition. Only after all old
		// results and uncertain operations are validated may the larger graph
		// replace it atomically, before any new callback can start.
		for i := range checkpoint.Nodes {
			state := &checkpoint.Nodes[i]
			state.DefinitionSHA256 = hash(describe(byID[state.ID]))
		}
		if len(checkpoint.Nodes) < len(nodes) && checkpoint.Status != "failed" {
			checkpoint.Status = "running"
		}
		for _, node := range nodes {
			if states[node.ID] == nil {
				checkpoint.Nodes = append(checkpoint.Nodes, NodeState{ID: node.ID, Kind: node.Kind, DefinitionSHA256: hash(describe(node)), Status: "pending"})
			}
		}
		checkpoint.DefinitionSHA256 = signature
		slices.SortFunc(checkpoint.Nodes, func(a, b NodeState) int { return compareID(a.ID, b.ID) })
		for i := range checkpoint.Nodes {
			states[checkpoint.Nodes[i].ID] = &checkpoint.Nodes[i]
		}
	}
	if err = persist(); err != nil {
		return checkpoint, err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	conditions, cancelConditions := context.WithCancel(child)
	defer cancelConditions()
	type completion struct {
		node       Node
		input      Input
		condition  bool
		run        bool
		reason     string
		output     Output
		err        error
		runFailed  bool
		durationMS float64
		verifyMS   float64
	}
	done := make(chan completion, options.Parallelism)
	// Pure conditions have no durable side effects, so their checkpoint stays
	// pending until evaluation succeeds. This set reserves their parallel slots
	// without introducing an uncertain operation that requires reconciliation.
	conditioning := map[string]bool{}
	startRun := func(node Node, input Input) {
		go func() {
			result := completion{node: node}
			started := time.Now()
			output, callErr := invoke("run", true, func() (Output, error) {
				return node.Run(child, cloneInput(input))
			})
			result.durationMS = float64(time.Since(started)) / float64(time.Millisecond)
			// Malformed JSON must reach validation, not become empty success.
			result.output = cloneOutput(output)
			result.err = callErr
			result.runFailed = callErr != nil
			if result.err == nil && ctx.Err() == nil {
				started = time.Now()
				result.err = accept(child, node, input, result.output)
				result.verifyMS = float64(time.Since(started)) / float64(time.Millisecond)
			}
			done <- result
		}()
	}
	running := 0
	aborted, interrupted, terminalFailure := false, false, false
	var firstError error
	for _, node := range nodes {
		status := states[node.ID].Status
		if !node.Optional && (status == "failed" || status == "blocked" || status == "cancelled") && !terminalFailure {
			aborted, terminalFailure, firstError = true, true, fmt.Errorf("required node %s previously %s: %s", node.ID, status, states[node.ID].Error)
		}
	}
	for {
		if !aborted && ctx.Err() != nil {
			aborted, interrupted, firstError = true, true, ctx.Err()
			cancel()
		}
		progress := false
		if !aborted {
			for _, node := range schedulingOrder(nodes, states) {
				state := states[node.ID]
				if state.Status != "pending" || conditioning[node.ID] {
					continue
				}
				ready, blocked := true, ""
				for _, dep := range node.DependsOn {
					status := states[dep.ID].Status
					if status == "pending" || status == "running" {
						ready = false
						break
					}
					if !dep.Optional && status != "succeeded" {
						blocked = "required dependency " + dep.ID + " is " + status
					}
				}
				if !ready {
					continue
				}
				if state.ReadyAt.IsZero() {
					state.ReadyAt = time.Now().UTC()
				}
				if blocked != "" {
					state.Status, state.Error, state.FinishedAt = "blocked", blocked, time.Now().UTC()
					progress = true
					if !node.Optional {
						aborted, terminalFailure, firstError = true, true, errors.New(blocked)
						cancel()
					}
					if err = persist(); err != nil {
						aborted, terminalFailure, firstError = true, true, err
						cancel()
					}
					if aborted {
						break
					}
					continue
				}
				if running >= options.Parallelism {
					continue
				}
				input := nodeInput(node, *state, options.Input, states)
				state.InputSHA256 = inputHash(input)
				if node.When != nil {
					if err = persist(); err != nil {
						aborted, terminalFailure, firstError = true, true, err
						cancel()
						break
					}
					conditioning[node.ID], progress = true, true
					running++
					go func(node Node, input Input) {
						started := time.Now()
						decision, conditionErr := invoke("condition", false, func() (completion, error) {
							run, reason, err := node.When(conditions, cloneInput(input))
							return completion{run: run, reason: reason}, err
						})
						decision.node, decision.input, decision.condition = node, input, true
						decision.err = conditionErr
						decision.durationMS = float64(time.Since(started)) / float64(time.Millisecond)
						if !decision.run && decision.err == nil && decision.reason == "" {
							decision.err = errors.New("condition skip requires a reason")
						}
						done <- decision
					}(node, input)
					continue
				}
				state.Status, state.Attempt, state.StartedAt = "running", 1, time.Now().UTC()
				input.Attempt = state.Attempt
				if err = persist(); err != nil {
					aborted, terminalFailure, firstError = true, true, err
					cancel()
					break
				}
				running++
				progress = true
				startRun(node, input)
			}
		}
		if running == 0 {
			if !progress || aborted {
				break
			}
			continue
		}
		var result completion
		if aborted {
			result = <-done
		} else {
			select {
			case result = <-done:
			case <-ctx.Done():
				aborted, interrupted, firstError = true, true, ctx.Err()
				cancel()
				continue
			}
		}
		running--
		state := states[result.node.ID]
		if result.condition {
			delete(conditioning, result.node.ID)
			state.ConditionDurationMS += result.durationMS
			cancelledCondition := conditions.Err() != nil && (errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded))
			if result.err != nil && !cancelledCondition {
				// A definite required failure remains terminal even if a sibling
				// interruption was delivered first. Cancellation is not a failure.
				state.Status, state.Error, state.FinishedAt = "failed", result.err.Error(), time.Now().UTC()
				if !result.node.Optional {
					if !terminalFailure {
						aborted, terminalFailure, firstError = true, true, result.err
					}
					cancel()
				}
			} else if aborted || child.Err() != nil {
				// No Run callback has started, so a pure condition can be safely
				// retried on the next same-run resume without Reconcile.
				if !aborted {
					aborted, interrupted, firstError = true, true, child.Err()
					cancel()
				}
			} else if !result.run {
				state.FinishedAt = time.Now().UTC()
				state.Status, state.Reason = "skipped", result.reason
			} else {
				state.Status, state.Attempt, state.StartedAt = "running", 1, time.Now().UTC()
				result.input.Attempt = state.Attempt
			}
			if err = persist(); err != nil {
				aborted, terminalFailure, firstError = true, true, err
				cancel()
			}
			if !aborted && state.Status == "running" {
				running++
				startRun(result.node, result.input)
			}
			continue
		}
		state.RunDurationMS, state.VerifyDurationMS = result.durationMS, result.verifyMS
		uncertain := errors.Is(result.err, ErrInterrupted) || ctx.Err() != nil || child.Err() != nil && (errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded))
		if uncertain {
			// Keep the persisted intent unresolved. The callback may have completed
			// external work before observing cancellation; reconciliation decides.
			aborted, interrupted = true, true
			if firstError == nil {
				firstError = result.err
				if ctx.Err() != nil {
					firstError = ctx.Err()
				}
			}
			state.Error = "interrupted; external outcome requires reconciliation"
			if result.err != nil {
				state.Error += ": " + result.err.Error()
			}
			cancel()
		} else {
			state.FinishedAt = time.Now().UTC()
			if result.err == nil {
				state.Status, state.Output = "succeeded", result.output
			} else {
				state.Status, state.Error = "failed", result.err.Error()
				if !result.node.Optional {
					if !terminalFailure {
						aborted, terminalFailure, firstError = true, true, result.err
					}
					if options.DrainOnFailure && result.runFailed {
						cancelConditions()
					} else {
						cancel()
					}
				}
			}
		}
		if err = persist(); err != nil {
			aborted, terminalFailure, firstError = true, true, err
			cancel()
		}
	}
	checkpoint.Status = "succeeded"
	if terminalFailure {
		checkpoint.Status = "failed"
		for i := range checkpoint.Nodes {
			if checkpoint.Nodes[i].Status == "pending" {
				checkpoint.Nodes[i].Status, checkpoint.Nodes[i].Error = "blocked", "graph stopped after required node failure"
			}
		}
	} else if interrupted {
		checkpoint.Status = "interrupted"
	}
	if err = persist(); err != nil {
		return checkpoint, err
	}
	return checkpoint, firstError
}
