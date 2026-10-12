package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
)

type pendingStop struct {
	project string
	hard    bool
}

func (c *Client) clearPendingStops(project string) {
	c.pendingStops.Range(func(dir, stop any) bool {
		if stop.(pendingStop).project == project {
			c.pendingStops.Delete(dir)
		}
		return true
	})
}

// An unavailable Docker control connection does not prove that commands stopped.
// Fence every new Run in this project until its old processes are confirmed gone.
// Startup cleanup provides the durable boundary after a dispatcher restart.
func (c *Client) confirmPendingStops(ctx context.Context, project string) error {
	dirs := map[string]pendingStop{}
	c.pendingStops.Range(func(dir, stop any) bool {
		if stop.(pendingStop).project == project {
			dirs[dir.(string)] = stop.(pendingStop)
		}
		return true
	})
	if len(dirs) == 0 {
		return nil
	}
	var info struct {
		ID     string `json:"Id"`
		Config struct{ Labels map[string]string }
		State  struct{ Running bool }
	}
	err := c.json(ctx, "GET", "/containers/"+url.PathEscape(c.name(project))+"/json", nil, &info)
	if status(err, 404) {
		c.clearPendingStops(project)
		return nil
	}
	if err != nil {
		return fmt.Errorf("project stop is unconfirmed: %w", err)
	}
	if !c.owns(info.Config.Labels, project) || info.ID == "" {
		return errors.New("cannot confirm stop of a container with different ownership")
	}
	if !info.State.Running {
		c.clearPendingStops(project)
		return nil
	}
	for dir, stop := range dirs {
		argv := []string{"/usr/local/bin/pwnmesh", "worker", "--interrupt", dir}
		if stop.hard {
			argv = []string{"/usr/local/bin/pwnmesh", "worker", "--cancel", dir, "--force"}
		}
		if err := c.exec(ctx, info.ID, argv, nil, io.Discard); err != nil {
			return fmt.Errorf("project stop is unconfirmed: %w", err)
		}
		c.pendingStops.Delete(dir)
	}
	return nil
}
