package board

import (
	"encoding/json"
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
func (t *Tx) activeStepWriteOwners(project string) ([]Step, error) {
	var owners []Step
	var ids []string
	err := sqlstore.VisitStepWriteOwners(t.transactionQueries, project, func(id, worker string) error {
		// An abandoned or already concluded Step can still have a pending Run.
		// Do not project its business status into the reservation's lifetime.
		owners = append(owners, Step{ID: id, Worker: Ptr(worker), Status: "running"})
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	metadata, err := t.stepMetadataByID(project, ids...)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]stepMetadata, len(metadata))
	for _, step := range metadata {
		byID[step.ID] = step
	}
	for n := range owners {
		step := byID[owners[n].ID]
		owners[n].WritePaths, owners[n].Repair = step.WritePaths, step.Repair
	}
	return owners, nil
}

func (t *Tx) stepMetadataByID(project string, ids ...string) ([]stepMetadata, error) {
	rows, err := sqlstore.StateStepMetadataByID(t.transactionQueries, project, ids)
	if err != nil {
		return nil, err
	}
	metadata := make([]stepMetadata, 0, len(rows))
	for _, raw := range rows {
		var step stepMetadata
		if err := json.Unmarshal(raw, &step); err != nil {
			return nil, err
		}
		metadata = append(metadata, step)
	}
	return metadata, nil
}

func (t *Tx) stepWriteBlocked(project, id string) (bool, error) {
	steps, err := t.stepMetadataByID(project, id)
	if err != nil {
		return false, err
	}
	for _, metadata := range steps {
		if metadata.ID != id || len(metadata.WritePaths) == 0 && metadata.Repair == nil {
			continue
		}
		owners, err := t.activeStepWriteOwners(project)
		return stepWriteConflict(Step{ID: id, WritePaths: metadata.WritePaths, Repair: metadata.Repair}, owners) != "", err
	}
	return false, nil
}
