package board

import "encoding/json"

// Curation is the final field in the current canonical encoding. Keep that
// order: frozen inputs and decision state versions hash these exact JSON bytes.
func (s State) MarshalJSON() ([]byte, error) {
	type stateAlias State
	view := struct {
		*stateAlias
		Curation *CurationProgress `json:"curation"`
	}{stateAlias: (*stateAlias)(&s), Curation: &s.Curation}
	return json.Marshal(view)
}
