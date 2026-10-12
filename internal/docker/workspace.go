package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

const workspaceVolumeLabel = "pwnmesh.workspace-volume"
const workspaceResource = "workspace-v1"

type workspaceMount struct {
	Type, Name, Destination string
	RW                      bool
}

type workspaceVolume struct {
	Name, Driver    string
	Labels, Options map[string]string
}

// Hash every ownership component so separators and long project names cannot
// alias another workspace or exceed Docker's volume-name restrictions.
func (c *Client) workspaceName(project string) string {
	raw, _ := json.Marshal([]string{c.ownerID(), c.Config.Namespace, project})
	sum := sha256.Sum256(raw)
	return "pwnmesh-workspace-" + hex.EncodeToString(sum[:])
}

func (c *Client) ownsWorkspace(volume workspaceVolume, project string) bool {
	return volume.Name == c.workspaceName(project) && c.owns(volume.Labels, project) && volume.Labels["pwnmesh.resource"] == workspaceResource
}

func (c *Client) inspectWorkspace(ctx context.Context, project string) (workspaceVolume, error) {
	var volume workspaceVolume
	err := c.json(ctx, "GET", "/volumes/"+url.PathEscape(c.workspaceName(project)), nil, &volume)
	if err == nil && (!c.ownsWorkspace(volume, project) || volume.Driver != "local" || len(volume.Options) != 0) {
		err = errors.New("workspace volume has unexpected ownership or storage configuration; preserve it for manual migration")
	}
	return volume, err
}

// Retained workspaces keep the same runtime binding as retained containers.
// Resolve shared-network aliases now; the alias may name another container later.
func (c *Client) workspaceRuntime(ctx context.Context, image string) (map[string]string, error) {
	network := networkMode(c.Config.Network)
	if strings.HasPrefix(network, "container:") {
		target := strings.TrimPrefix(network, "container:")
		if raw, err := hex.DecodeString(target); err != nil || len(raw) != 32 {
			var info struct {
				ID string `json:"Id"`
			}
			if err := c.json(ctx, "GET", "/containers/"+url.PathEscape(target)+"/json", nil, &info); err != nil {
				return nil, fmt.Errorf("resolve workspace shared network: %w", err)
			}
			target = info.ID
		}
		if raw, err := hex.DecodeString(target); err != nil || len(raw) != 32 {
			return nil, errors.New("Docker returned an invalid workspace shared-network ID")
		}
		network = "container:" + target
	}
	caps, _ := json.Marshal(capabilities(c.Config.CapAdd))
	return map[string]string{"pwnmesh.workspace-image": image, "pwnmesh.workspace-network": network, "pwnmesh.workspace-capabilities": string(caps)}, nil
}

func workspaceMatchesRuntime(volume workspaceVolume, runtime map[string]string) error {
	for key, value := range runtime {
		if volume.Labels[key] != value {
			return errors.New("saved workspace uses a different image, network, or capabilities; preserve and migrate its workspace before changing the runtime")
		}
	}
	return nil
}

func (c *Client) ensureWorkspace(ctx context.Context, project string, runtime map[string]string) (string, error) {
	volume, err := c.inspectWorkspace(ctx, project)
	if status(err, 404) {
		labels := c.labels(project, "dispatcher-v1")
		labels["pwnmesh.resource"] = workspaceResource
		for key, value := range runtime {
			labels[key] = value
		}
		input := map[string]any{"Name": c.workspaceName(project), "Driver": "local", "Labels": labels}
		err = c.json(ctx, "POST", "/volumes/create", input, nil)
		if err != nil && !status(err, 409) {
			return "", err
		}
		// Create may return an already existing volume. Never infer ownership
		// from a successful create response, including concurrent creators.
		volume, err = c.inspectWorkspace(ctx, project)
	}
	if err != nil {
		return "", err
	}
	if err = workspaceMatchesRuntime(volume, runtime); err != nil {
		return "", err
	}
	return volume.Name, nil
}

func (c *Client) checkWorkspace(ctx context.Context, project string, labels map[string]string, mounts []workspaceMount, runtime map[string]string) (bool, error) {
	if c.ownerID() == "" {
		return false, nil
	}
	var workspace []workspaceMount
	for _, mount := range mounts {
		destination := path.Clean(mount.Destination)
		if destination == "/" || destination == "/workspace" || strings.HasPrefix(destination, "/workspace/") {
			workspace = append(workspace, mount)
		}
	}
	marker := labels[workspaceVolumeLabel]
	if marker == "" && len(workspace) == 0 {
		// Existing writable-layer workspace; never migrate implicitly.
		return false, nil
	}
	if marker != c.workspaceName(project) || len(workspace) != 1 || workspace[0].Type != "volume" || workspace[0].Name != marker || workspace[0].Destination != "/workspace" || !workspace[0].RW {
		return false, errors.New("container has unexpected workspace mounts; preserve and migrate its workspace before reuse or cleanup")
	}
	volume, err := c.inspectWorkspace(ctx, project)
	if err != nil {
		return false, err
	}
	if err = workspaceMatchesRuntime(volume, runtime); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) removeWorkspace(ctx context.Context, project string) error {
	if c.ownerID() == "" {
		return nil
	}
	volume, err := c.inspectWorkspace(ctx, project)
	if status(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	// Do not force removal: another container may still be using this volume.
	err = c.json(ctx, "DELETE", "/volumes/"+url.PathEscape(volume.Name), nil, nil)
	if status(err, 404) {
		return nil
	}
	return err
}

func (c *Client) workspaceProjects(ctx context.Context, labels []string) ([]string, error) {
	if c.ownerID() == "" {
		return nil, nil
	}
	labels = append(labels, "pwnmesh.resource="+workspaceResource)
	filters, _ := json.Marshal(map[string][]string{"label": labels})
	var listed struct{ Volumes []workspaceVolume }
	if err := c.json(ctx, "GET", "/volumes?filters="+url.QueryEscape(string(filters)), nil, &listed); err != nil {
		return nil, err
	}
	var projects []string
	for _, volume := range listed.Volumes {
		if project := volume.Labels["pwnmesh.project"]; project != "" && c.ownsWorkspace(volume, project) {
			projects = append(projects, project)
		}
	}
	return projects, nil
}
