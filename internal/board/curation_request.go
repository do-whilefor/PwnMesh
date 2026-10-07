package board

import "encoding/json"

// CurationRequest lets the planner ask for evidence reconciliation without
// manufacturing matching producer claims or classifying ordinary exploration.
type CurationRequest struct {
	Sources  []string `json:"sources"`
	Reason   string   `json:"reason"`
	Revision int64    `json:"revision"`
}

func (s State) PendingCurationRequest() *CurationRequest {
	request := s.Curation.Request
	if request == nil || s.Curation.Generation != s.Graph.Project.Generation || request.Revision <= s.Curation.ThroughRevision || request.Revision > s.Revision {
		return nil
	}
	return request
}

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
	if err := s.ValidateFactSources(input.Sources, true); err != nil {
		return "", nil, false, err
	}
	if pending := s.PendingCurationRequest(); pending != nil {
		if pending.Reason == input.Reason && sameSupportSet(pending.Sources, input.Sources) {
			return "curation_request", *pending, false, nil
		}
		return "", nil, false, Err(409, "a distinct curation request is already pending; its evidence must be reviewed first")
	}
	request := CurationRequest{Sources: append([]string{}, input.Sources...), Reason: input.Reason, Revision: s.Revision + 1}
	d.Curation.Generation, d.Curation.Request = s.Graph.Project.Generation, &request
	return "curation_request", request, true, nil
}
