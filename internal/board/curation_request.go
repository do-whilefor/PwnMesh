package board

import (
	"encoding/json"
	"slices"
	"strings"

	"pwnmesh/internal/board/model"
)

type CurationRequest = model.CurationRequest
type CurationRequestReview = model.CurationRequestReview

func (t *Tx) requestCuration(s State, d *stateData, raw json.RawMessage) (string, any, bool, error) {
	var input struct {
		Sources []string `json:"sources"`
		Reason  string   `json:"reason"`
	}
	if err := decodeAction(raw, &input); err != nil {
		return "", nil, false, err
	}
	if len(input.Sources) == 0 || len(input.Sources) > 32 || !required(input.Reason, 8192) {
		return "", nil, false, Err(422, "curation_request requires 1 to 32 fact sources and a reason")
	}
	pending := s.PendingCurationRequest()
	if pending != nil && s.CurationRequestNeedsEvidence() {
		seen := map[string]bool{}
		var added []string
		for _, id := range input.Sources {
			if seen[id] {
				return "", nil, false, Err(422, "duplicate source fact")
			}
			seen[id] = true
			if !slices.Contains(pending.Sources, id) {
				added = append(added, id)
			}
		}
		for _, id := range pending.Sources {
			if !seen[id] {
				return "", nil, false, Err(409, "an unresolved curation request must retain every original source")
			}
		}
		// Original observations remain reviewable even if the previous review
		// invalidated their support. Only additional sources must be effective.
		if len(added) > 0 {
			if err := s.ValidateFactSources(added, true); err != nil {
				return "", nil, false, err
			}
		}
	} else if err := s.ValidateFactSources(input.Sources, true); err != nil {
		return "", nil, false, err
	}
	if pending != nil && !s.CurationRequestNeedsEvidence() {
		if pending.Reason == input.Reason && sameSupportSet(pending.Sources, input.Sources) {
			return "curation_request", *pending, false, nil
		}
		return "", nil, false, Err(409, "a distinct curation request is already pending; its evidence must be reviewed first")
	}
	request := CurationRequest{Sources: append([]string{}, input.Sources...), Reason: input.Reason, Revision: s.Revision + 1}
	d.Curation.Generation, d.Curation.Request = s.Graph.Project.Generation, &request
	d.Curation.RequestReview = nil
	return "curation_request", request, true, nil
}

func validateCurationRequestReview(s State, input *model.CurationRequestReview) (*model.CurationRequestReview, error) {
	request := s.PendingCurationRequest()
	if request == nil {
		if input != nil {
			return nil, Err(422, "request_review requires a pending curation request")
		}
		return nil, nil
	}
	needsEvidence := s.CurationRequestNeedsEvidence()
	if needsEvidence {
		if input == nil {
			input = s.Curation.RequestReview
		} else if input.Disposition == "resolved" || input.Disposition == "no_change" {
			return nil, Err(409, "an unresolved curation request requires an explicit planner re-request before resolution")
		}
	}
	if input == nil || !slices.Contains([]string{"resolved", "no_change", "needs_review"}, input.Disposition) || !required(input.Reason, 8192) {
		return nil, Err(422, "explicit curation request requires request_review with disposition,checked_sources and reason")
	}
	if input.Revision != 0 && input.Revision != request.Revision {
		return nil, Err(409, "request_review revision does not match the pending request")
	}
	seen := map[string]bool{}
	for _, id := range input.CheckedSources {
		if seen[id] || !slices.Contains(request.Sources, id) {
			return nil, Err(422, "checked_sources must contain each requested source exactly once")
		}
		seen[id] = true
	}
	if len(seen) != len(request.Sources) {
		return nil, Err(422, "checked_sources must contain each requested source exactly once")
	}
	out := *input
	if needsEvidence {
		// Other candidate groups can be reconciled while this request waits,
		// without replacing the original unresolved review or its blocker.
		out = *s.Curation.RequestReview
	}
	out.Revision, out.Reason = request.Revision, strings.TrimSpace(out.Reason)
	out.CheckedSources = append([]string{}, out.CheckedSources...)
	slices.Sort(out.CheckedSources)
	return &out, nil
}
