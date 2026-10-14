package board

import (
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"pwnmesh/internal/artifactcheck"
	"pwnmesh/internal/board/internal/sqlstore"
)

// These declarations coordinate shared outputs, not filesystem permissions.
// The Server cannot resolve container symlinks or stop undeclared shell writes.
func normalizeStepWritePaths(paths []string, repair *artifactcheck.Spec) ([]string, error) {
	if len(paths) > 16 {
		return nil, Err(422, "write_paths accepts at most 16 paths")
	}
	paths = append([]string(nil), paths...)
	if repair != nil {
		paths = append(paths, repair.Path)
	}
	for n, value := range paths {
		if len(value) > 4096 || !utf8.ValidString(value) || !path.IsAbs(value) || strings.Contains(value, "\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, Err(422, "write_paths requires absolute Linux paths inside /workspace")
		}
		value = path.Clean(value)
		if !strings.HasPrefix(value, "/workspace/") || value == "/workspace/.pwnmesh" || strings.HasPrefix(value, "/workspace/.pwnmesh/") {
			return nil, Err(422, "write_paths must stay inside /workspace and outside its runtime directory")
		}
		paths[n] = value
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) > 16 {
		return nil, Err(422, "write_paths including the repair target accepts at most 16 paths")
	}
	return paths, nil
}

func stepWritePaths(step Step) []string {
	paths, _ := normalizeStepWritePaths(step.WritePaths, step.Repair)
	return paths
}

func stepWriteConflict(step Step, others []Step) string {
	paths := stepWritePaths(step)
	if len(paths) == 0 {
		return ""
	}
	for _, other := range others {
		// Compare live reservations projected from leases and pending Runs;
		// unclaimed plans and historical completed Steps do not own outputs.
		if other.ID == step.ID || other.Worker == nil || other.Result != nil || other.Status == "abandoned" {
			continue
		}
		otherPaths := stepWritePaths(other)
		for _, a := range paths {
			for _, b := range otherPaths {
				if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
					return other.ID
				}
			}
		}
	}
	return ""
}

// Read all current owners, not just the scheduler's current page. Revocation
// can clear an Intent lease before its Worker returns, so pending executions
// retain their reservation through that handoff. Existing terminal statuses
// release it; they are not an independent proof of operating-system cleanup.
func (t *Tx) activeStepWriteOwners(project string, metadata []stepMetadata) ([]Step, error) {
	byID := make(map[string]stepMetadata, len(metadata))
	for _, step := range metadata {
		byID[step.ID] = step
	}
	var owners []Step
	err := sqlstore.VisitStepWriteOwners(t, project, func(id, worker string) error {
		step := byID[id]
		// An abandoned or already concluded Step can still have a pending Run.
		// Do not project its business status into the reservation's lifetime.
		owners = append(owners, Step{ID: id, Worker: Ptr(worker), Status: "running", WritePaths: step.WritePaths, Repair: step.Repair})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return owners, nil
}

func (t *Tx) stepWriteBlocked(project, id string) (bool, error) {
	data, _, _, err := t.stateData(project)
	if err != nil {
		return false, err
	}
	for _, metadata := range data.Steps {
		if metadata.ID != id || len(metadata.WritePaths) == 0 && metadata.Repair == nil {
			continue
		}
		owners, err := t.activeStepWriteOwners(project, data.Steps)
		return stepWriteConflict(Step{ID: id, WritePaths: metadata.WritePaths, Repair: metadata.Repair}, owners) != "", err
	}
	return false, nil
}
