package docker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
)

// BindOwner is called before touching containers. Refuse a database switch in
// a running dispatcher instead of reinterpreting its existing resources.
func (c *Client) BindOwner(id string) error {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != id {
		return errors.New("server returned an invalid installation identity")
	}
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	if c.owner != "" && c.owner != id {
		return errors.New("server installation changed; restart the dispatcher before using another database")
	}
	c.owner = id
	return nil
}

func (c *Client) ownerID() string {
	c.ownerMu.RLock()
	defer c.ownerMu.RUnlock()
	return c.owner
}

func (c *Client) requireOwner() error {
	if c.requiresOwner && c.ownerID() == "" {
		return errors.New("container runner requires a server installation identity")
	}
	return nil
}

// ContainerName is also used by administration and acceptance tooling.
func (c *Client) ContainerName(project string) string {
	prefix := c.Config.Namespace
	if owner := c.ownerID(); owner != "" {
		prefix += "-" + owner
	}
	return prefix + "-dispatch-" + project
}

func (c *Client) owns(labels map[string]string, project string) bool {
	return labels["pwnmesh.namespace"] == c.Config.Namespace &&
		labels["pwnmesh.project"] == project && labels["pwnmesh.installation"] == c.ownerID()
}

func (c *Client) labels(project, boundary string) map[string]string {
	labels := map[string]string{"pwnmesh.namespace": c.Config.Namespace, "pwnmesh.project": project, "pwnmesh.model-boundary": boundary}
	if owner := c.ownerID(); owner != "" {
		labels["pwnmesh.installation"] = owner
	}
	return labels
}

func (c *Client) checkLegacyContainer(ctx context.Context, project string) error {
	if c.ownerID() == "" {
		return nil
	}
	name := c.Config.Namespace + "-dispatch-" + project
	var info struct {
		Config struct{ Labels map[string]string }
	}
	err := c.json(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, &info)
	if status(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("legacy container %s has no verified installation ownership; preserve and migrate its workspace before starting a replacement", name)
}
