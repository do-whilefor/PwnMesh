package protocol

import (
	"errors"
	"slices"
	"strings"

	"pwnmesh/internal/board/model"
)

// Parent selectors are read metadata, not observed catalog identities. Keep
// them outside Asset so canonical State bytes and frozen versions stay stable.
func graphAssetView(asset model.Asset) any {
	var selectors map[string]string
	if parents := model.AssetAncestors(asset); len(parents) > 0 {
		selectors = map[string]string{"host": parents[0]}
		if len(parents) > 1 {
			selectors["service"] = parents[1]
		}
	}
	return struct {
		model.Asset
		AncestorIDs map[string]string `json:"ancestor_ids,omitempty"`
	}{asset, selectors}
}

func validateGraphAssetFilter(r GraphRequest) error {
	if len(r.AssetIDs) == 0 {
		if r.AssetNeighborhood {
			return errors.New("asset_neighborhood requires asset_ids")
		}
		return nil
	}
	if (r.Op != "" && r.Op != "read_graph" && r.Op != "read_snapshot") || !slices.Contains([]string{"assets", "anchors", "facts", "steps", "findings", "candidates", "history"}, r.Section) {
		return errors.New("asset_ids requires an assets, anchors, facts, steps, findings, candidates or history read")
	}
	if len(r.AssetIDs) > 32 {
		return errors.New("at most 32 asset IDs are allowed")
	}
	seen := map[string]bool{}
	for _, id := range r.AssetIDs {
		if strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			return errors.New("asset IDs must be unique, nonempty and at most 256 bytes")
		}
		seen[id] = true
	}
	return nil
}

// Filter identities, never evidence validity. Both the catalogue and anchors
// come from the same State, including when that State is a frozen read view.
func graphAssetFilter(s model.State, r GraphRequest) (func(graphRecordIdentity) bool, error) {
	if len(r.AssetIDs) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	parents := map[string][]string{}
	virtual := map[string]bool{}
	for _, asset := range s.Assets {
		known[asset.ID] = true
		if r.AssetNeighborhood {
			parents[asset.ID] = model.AssetAncestors(asset)
			for _, id := range parents[asset.ID] {
				virtual[id] = true
			}
		}
	}
	selected := map[string]bool{}
	for _, id := range r.AssetIDs {
		if !known[id] && !virtual[id] {
			return nil, errors.New("asset is absent from this graph input")
		}
		selected[id] = true
	}
	if r.AssetNeighborhood {
		// Expand each requested identity to its ancestors and descendants only.
		// Newly selected parents do not broaden the query to sibling services or
		// endpoints. An unstored parent is a selector only when an existing
		// child proves its syntax-defined identity. Returned records and anchors
		// still come exclusively from this frozen State.
		requested := make(map[string]bool, len(selected))
		for id := range selected {
			requested[id] = true
		}
		for _, asset := range s.Assets {
			ancestors := parents[asset.ID]
			if requested[asset.ID] {
				for _, id := range ancestors {
					if known[id] {
						selected[id] = true
					}
				}
			}
			for _, id := range ancestors {
				if requested[id] {
					selected[asset.ID] = true
					// A virtual service selector has the same existing host
					// ancestor as a stored service, without sibling expansion.
					for _, parent := range ancestors {
						if known[parent] {
							selected[parent] = true
						}
					}
				}
			}
		}
	}
	attached := map[string]bool{}
	for _, anchor := range s.AssetAnchors {
		if selected[anchor.AssetID] {
			attached[anchor.NodeKind+":"+anchor.NodeID] = true
		}
	}
	kind := map[string]string{"facts": "fact", "steps": "step", "findings": "finding", "candidates": "candidate"}[r.Section]
	return func(node graphRecordIdentity) bool {
		switch r.Section {
		case "assets":
			return selected[node.id]
		case "anchors":
			return selected[node.assetID]
		case "history":
			for _, id := range node.assetIDs {
				if selected[id] {
					return true
				}
			}
			return false
		default:
			return attached[kind+":"+node.id]
		}
	}, nil
}

type graphRecordIdentity struct {
	id, source, target, assetID string
	assetIDs                    []string
}

func graphIdentity(record any) graphRecordIdentity {
	switch v := record.(type) {
	case model.FactRecord:
		return graphRecordIdentity{id: v.ID}
	case model.Goal:
		return graphRecordIdentity{id: v.ID}
	case model.Step:
		return graphRecordIdentity{id: v.ID}
	case model.Finding:
		return graphRecordIdentity{id: v.ID}
	case model.Candidate:
		return graphRecordIdentity{id: v.ID}
	case model.Dispute:
		return graphRecordIdentity{id: v.ID}
	case model.FactRelation:
		return graphRecordIdentity{source: v.Source, target: v.Target}
	case model.Hint:
		return graphRecordIdentity{id: v.ID}
	case model.Asset:
		return graphRecordIdentity{id: v.ID}
	case model.AssetAnchor:
		return graphRecordIdentity{id: v.NodeID, assetID: v.AssetID}
	case model.HistoryEntry:
		return graphRecordIdentity{id: v.ID, assetIDs: v.AssetIDs}
	default:
		panic("unsupported graph record type")
	}
}

func validateTraceRunRequest(j Job, r GraphRequest) error {
	if !j.GraphRPC || j.Kind != "explore" || j.Intent == nil || j.RunID == "" || j.Graph.Project.ID == "" {
		return errors.New("trace reads require a registered Execute bridge")
	}
	if r.Section != "" || r.ByteOffset != nil || r.ExpectedVersion != "" || r.RecordVersion != "" || r.Updates != nil || r.Batch != nil || r.Action.Op != "" || len(r.IDs) > 1 || r.Offset < 0 || r.Limit < 0 || r.Limit > 20 {
		return errors.New("trace reads accept only one run ID or an offset/limit page")
	}
	if len(r.IDs) == 1 && (strings.TrimSpace(r.IDs[0]) == "" || len(r.IDs[0]) > 256 || r.Offset != 0) {
		return errors.New("trace run must identify a registered execution in the current project")
	}
	return nil
}
