//go:build linux

package dag

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Resource declarations coordinate effects; they do not constrain shell access.
// Keep the original strings in persisted definitions so old receipts stay bound.
func resourcePath(resource, workspace string) (string, bool) {
	explicit := strings.HasPrefix(resource, "path:")
	if explicit {
		resource = strings.TrimPrefix(resource, "path:")
	}
	if !explicit && (!strings.Contains(resource, "/") || strings.Contains(resource, "://")) {
		return resource, false
	}
	if !filepath.IsAbs(resource) && workspace != "" {
		resource = filepath.Join(workspace, resource)
	}
	return filepath.Clean(resource), true
}

func pathContains(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

func resourcesConflict(left, right []string, workspace string) bool {
	for _, a := range left {
		ap, aPath := resourcePath(a, workspace)
		for _, b := range right {
			bp, bPath := resourcePath(b, workspace)
			if aPath && bPath && (pathContains(ap, bp) || pathContains(bp, ap)) || !aPath && !bPath && a == b {
				return true
			}
		}
	}
	return false
}

func nodeAncestors(specs map[string]Node, id string) map[string]bool {
	seen := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		for _, dep := range specs[id].DependsOn {
			if !seen[dep.ID] {
				seen[dep.ID] = true
				visit(dep.ID)
			}
		}
	}
	visit(id)
	return seen
}

func validateSharedResources(scope Scope, spec Spec) error {
	byID := make(map[string]Node, len(spec.Nodes))
	for _, node := range spec.Nodes {
		byID[node.ID] = node
		if !scope.SharedWritesBound {
			continue
		}
		for _, resource := range node.Resources {
			path, isPath := resourcePath(resource, scope.Workspace)
			if !isPath || !pathContains(scope.Workspace, path) && !pathContains(path, scope.Workspace) || pathContains(scope.RunDir, path) {
				continue
			}
			allowed := false
			for _, writePath := range scope.WritePaths {
				if pathContains(filepath.Clean(writePath), path) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("node %s shared output resource %q is outside the Step write_paths; keep ordinary outputs private", node.ID, resource)
			}
		}
	}
	for i, a := range spec.Nodes {
		ancestors := nodeAncestors(byID, a.ID)
		for _, b := range spec.Nodes[i+1:] {
			if !ancestors[b.ID] && !nodeAncestors(byID, b.ID)[a.ID] && resourcesConflict(a.Resources, b.Resources, scope.Workspace) {
				return fmt.Errorf("shared resource paths require ordered nodes %s and %s", a.ID, b.ID)
			}
		}
	}
	return nil
}
