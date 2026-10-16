package model

import (
	"bytes"
	"encoding/json"
	"strings"
)

const MaxFactDerivationSources = 32

// ParseFactDerivation preserves an absent field's historical encoding. Sources
// are immutable observation identities, never prose or forward references.
func ParseFactDerivation(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var ids []string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &ids) != nil || len(ids) > MaxFactDerivationSources {
		return nil, Err(422, "derived_from requires an array of at most 32 observation Fact IDs")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !Required(id, 256) || strings.TrimSpace(id) != id || seen[id] {
			return nil, Err(422, "derived_from requires unique nonempty observation Fact IDs")
		}
		seen[id] = true
	}
	return ids, nil
}

// factSupportClosure keeps explicit derivations and corrective observations
// discoverable together. It never infers dependence from a common producer.
func factSupportClosure(facts []FactRecord, relations []FactRelation, selected map[string]bool) {
	for {
		count := len(selected)
		for _, fact := range facts {
			if selected[fact.ID] {
				for _, source := range fact.DerivedFrom {
					selected[source] = true
				}
			}
		}
		decisionRelationClosure(relations, selected)
		if len(selected) == count {
			return
		}
	}
}
