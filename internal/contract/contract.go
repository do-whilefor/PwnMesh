// Package contract validates structured results from the current execution protocol.
package contract

import (
	"encoding/json"
	"errors"
	"strings"
)

type Result struct {
	Kind        string
	Outcome     string
	Reason      string
	FactID      string
	FactPayload json.RawMessage
}

func Extract(text string) (map[string]json.RawMessage, error) {
	text = strings.TrimSpace(text)
	var whole map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &whole) == nil && whole != nil {
		return whole, nil
	}
	for index, ch := range text {
		if ch != '{' {
			continue
		}
		var candidate map[string]json.RawMessage
		if json.NewDecoder(strings.NewReader(text[index:])).Decode(&candidate) == nil && candidate != nil {
			return candidate, nil
		}
	}
	return nil, errors.New("no JSON object found in output")
}
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	err := json.Unmarshal(raw, &m)
	if err == nil && m == nil {
		err = errors.New("expected object")
	}
	return m, err
}
func text(raw json.RawMessage) (string, error) {
	var s string
	err := json.Unmarshal(raw, &s)
	s = strings.TrimSpace(s)
	if err == nil && s == "" {
		err = errors.New("description is required")
	}
	return s, err
}
