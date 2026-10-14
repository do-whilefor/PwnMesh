package model

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

func DecodeAction(raw json.RawMessage, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return Err(422, "Invalid action payload: "+err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Err(422, "Action payload must contain one JSON object")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return Err(422, "Action payload must be an object")
	}
	return nil
}

func Required(value string, max int) bool { return strings.TrimSpace(value) != "" && len(value) <= max }

var decisionAlias = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func ValidDecisionAlias(value string) bool { return decisionAlias.MatchString(value) }
