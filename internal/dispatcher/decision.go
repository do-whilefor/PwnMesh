package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"pwnmesh/internal/board"
)

func (s *Scheduler) schedulePage(ctx context.Context, id string, query url.Values) (board.SchedulePage, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("protocol_version", strconv.Itoa(board.ScheduleProtocolVersion))
	var page board.SchedulePage
	if err := s.Client.Do(ctx, "GET", projectPath(id)+"/scheduling?"+query.Encode(), nil, &page, nil); err != nil {
		return board.SchedulePage{}, err
	}
	if page.ProtocolVersion != board.ScheduleProtocolVersion {
		return board.SchedulePage{}, fmt.Errorf("scheduling protocol mismatch: Server returned version %d, Dispatcher requires %d; upgrade Server and Dispatcher together", page.ProtocolVersion, board.ScheduleProtocolVersion)
	}
	return page, nil
}

func (s *Scheduler) scheduleInput(ctx context.Context, id string) (board.SchedulePage, error) {
	var input board.SchedulePage
	for offset := 0; ; {
		query := url.Values{"offset": {strconv.Itoa(offset)}, "namespace": {s.namespace()}}
		if offset > 0 {
			query.Set("expected_version", input.StateVersion)
		}
		page, err := s.schedulePage(ctx, id, query)
		if err != nil {
			return input, err
		}
		if offset == 0 {
			input = page
		} else {
			if page.StateVersion != input.StateVersion || page.Project.Generation != input.Project.Generation {
				return input, errors.New("state_changed: scheduling pages do not match")
			}
			input.Steps = append(input.Steps, page.Steps...)
			for key, check := range page.ExecutionChecks {
				if input.ExecutionChecks == nil {
					input.ExecutionChecks = map[string]board.ExecutionCheck{}
				}
				input.ExecutionChecks[key] = check
			}
		}
		if page.NextOffset == 0 {
			return input, nil
		}
		if page.NextOffset <= offset {
			return input, errors.New("scheduling cursor did not advance")
		}
		offset = page.NextOffset
	}
}
