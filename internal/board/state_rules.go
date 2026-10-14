package board

import (
	"bytes"
	"encoding/json"
	"io"
	"path"
	"slices"
	"strings"
)

func decodeAction(raw json.RawMessage, target any) error {
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

func required(value string, max int) bool { return strings.TrimSpace(value) != "" && len(value) <= max }
func runMatches(identity, id string) bool {
	return identity == id || (id != "" && strings.HasSuffix(identity, "@"+id))
}
func validateEvidence(refs []EvidenceRef, run string, allowed []EvidenceRef, requiredRefs bool) error {
	if requiredRefs && len(refs) == 0 {
		return Err(422, "evidence must include an execution, path and necessary excerpt")
	}
	if len(refs) > 32 {
		return Err(422, "at most 32 evidence references are allowed")
	}
	for _, ref := range refs {
		if !required(ref.RunID, 256) || !required(ref.Path, 4096) || !required(ref.Excerpt, 8192) || strings.ContainsAny(ref.Path, "\x00\r\n") || ref.StartLine < 0 || ref.EndLine < ref.StartLine || (ref.StartLine == 0 && ref.EndLine != 0) {
			return Err(422, "invalid evidence reference")
		}
		if path.Clean(ref.Path) == "." || strings.Contains(ref.Path, "://") {
			return Err(422, "evidence path must locate a retained execution artifact")
		}
		if !runMatches(run, ref.RunID) && !slices.Contains(allowed, ref) {
			return Err(403, "evidence must belong to this execution or a cited source fact")
		}
	}
	return nil
}

func sameSupportSet[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for _, item := range a {
		if !slices.Contains(b, item) {
			return false
		}
	}
	return true
}
