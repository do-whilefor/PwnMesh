package model

import (
	"bytes"
	"encoding/json"
	"strings"
)

const MaxFactDerivationSources = 32

// FactDependency is a prerequisite of an observation's effective support.
// Planning inputs may include origin; explicit derivations and accepted
// upstream results must be evidence-backed observations.
type FactDependency struct {
	ID          string
	Observation bool
	StepID      string
}

// FactDependencies is shared by support projection, correction admission and
// execution updates. Only an accepted result inherits its Step prerequisites;
// unrelated raw observations never inherit a producer's other work.
func (s State) FactDependencies() map[string][]FactDependency {
	dependencies := make(map[string][]FactDependency, len(s.FactRecords))
	for _, fact := range s.FactRecords {
		for _, source := range fact.DerivedFrom {
			dependencies[fact.ID] = append(dependencies[fact.ID], FactDependency{ID: source, Observation: true})
		}
	}
	if s.Graph.Project.OrchestrationVersion != 1 {
		return dependencies
	}
	steps := make(map[string]Step, len(s.Steps))
	for _, step := range s.Steps {
		steps[step.ID] = step
	}
	for _, step := range s.Steps {
		if step.Result == nil || *step.Result == "goal" || s.ExternalFeedbackStep(step) {
			continue
		}
		for _, source := range step.From {
			dependencies[*step.Result] = append(dependencies[*step.Result], FactDependency{ID: source})
		}
		for _, upstream := range step.DependsOn {
			// Keep missing results as unsatisfied dependencies; never silently
			// turn an incomplete or malformed prerequisite into valid support.
			dependencies[*step.Result] = append(dependencies[*step.Result], FactDependency{ID: Value(steps[upstream].Result), Observation: true, StepID: upstream})
		}
	}
	return dependencies
}

// ParseFactDerivation preserves an absent field's historical encoding. Sources
// are immutable observation identities, never prose or forward references.
func ParseFactDerivation(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var ids []string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &ids) != nil || len(ids) > MaxFactDerivationSources {
		return nil, Err(422, "derived_from requires an array of at most 32 observation Fact IDs")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !Required(id, 256) || strings.TrimSpace(id) != id || seen[id] {
			return nil, Err(422, "derived_from requires unique nonempty observation Fact IDs")
		}
		seen[id] = true
	}
	return ids, nil
}

// factSupportClosure keeps explicit derivations and corrective observations
// discoverable together. It never infers dependence from a common producer.
func factSupportClosure(facts []FactRecord, relations []FactRelation, selected map[string]bool) {
	for {
		count := len(selected)
		for _, fact := range facts {
			if selected[fact.ID] {
				for _, source := range fact.DerivedFrom {
					selected[source] = true
				}
			}
		}
		decisionRelationClosure(relations, selected)
		if len(selected) == count {
			return
		}
	}
}
