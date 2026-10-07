package dispatcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func (s *Scheduler) configureInputReader() {
	setter, ok := s.Runner.(interface {
		SetInputReader(func(context.Context, worker.Job, board.InputFile) (io.ReadCloser, error))
	})
	if !ok {
		return
	}
	setter.SetInputReader(s.Client.OpenInput)
}

func (c *Client) OpenInput(ctx context.Context, j worker.Job, f board.InputFile) (io.ReadCloser, error) {
	if !f.Valid() {
		return nil, fmt.Errorf("invalid input file metadata")
	}
	bound := false
	for _, input := range j.InputFiles {
		if input == f {
			bound = true
			break
		}
	}
	if !bound {
		return nil, fmt.Errorf("input file is not bound to the job")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.Base, "/")+projectPath(j.Graph.Project.ID)+"/inputs/"+url.PathEscape(f.ID), nil)
	if err != nil {
		return nil, err
	}
	h := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c.HTTP != nil {
		*h = *c.HTTP
		h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	res, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 || res.ContentLength != f.Size || res.Header.Get("X-Content-SHA256") != f.SHA256 {
		res.Body.Close()
		return nil, fmt.Errorf("input download metadata mismatch (HTTP %d)", res.StatusCode)
	}
	return res.Body, nil
}
