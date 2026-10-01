package board

import (
	"encoding/json"
	"pwnmesh/internal/artifactcheck"
	"slices"
	"strings"
)

// MatchingStep finds an already planned direction in the current board round.
// Sources form a set; priority and worker ownership are not new task inputs.
// Terminal steps also match: adding the same plan must not bypass the explicit
// execution retry authorization. New evidence or a changed task remains new.
func (s State) MatchingStep(goal string, from []string, description string, dependencies ...[]string) (Step, bool) {
	var ids []string
	if len(dependencies) != 0 {
		ids = dependencies[0]
	}
	return s.matchingRepairStep(goal, from, description, ids, nil, nil)
}

func (s State) matchingRepairStep(goal string, from []string, description string, dependencies []string, repair *artifactcheck.Spec, writePaths []string) (Step, bool) {
	if goal == "" {
		goal = "goal"
	}
	sources := append([]string(nil), from...)
	slices.Sort(sources)
	sources = slices.Compact(sources)
	var dependsOn []string
	if len(dependencies) != 0 {
		dependsOn = append([]string(nil), dependencies...)
		slices.Sort(dependsOn)
		dependsOn = slices.Compact(dependsOn)
	}
	writePaths, err := normalizeStepWritePaths(writePaths, repair)
	if err != nil {
		return Step{}, false
	}
	for _, step := range s.Steps {
		existingWrites, err := normalizeStepWritePaths(step.WritePaths, step.Repair)
		if err != nil || !slices.Equal(writePaths, existingWrites) {
			continue
		}
		wanted, _ := json.Marshal(repair)
		existingRepair, _ := json.Marshal(step.Repair)
		if string(wanted) != string(existingRepair) {
			continue
		}
		if step.GoalID != goal || strings.TrimSpace(step.Description) != strings.TrimSpace(description) {
			continue
		}
		existing := append([]string(nil), step.From...)
		slices.Sort(existing)
		existingDependencies := append([]string(nil), step.DependsOn...)
		slices.Sort(existingDependencies)
		if slices.Equal(sources, slices.Compact(existing)) && slices.Equal(dependsOn, slices.Compact(existingDependencies)) {
			return step, true
		}
	}
	return Step{}, false
}
