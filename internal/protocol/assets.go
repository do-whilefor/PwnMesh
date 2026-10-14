package protocol

import (
	"errors"
	"slices"
	"strings"

	"pwnmesh/internal/board"
)

func validateGraphAssetFilter(r GraphRequest) error {
	if len(r.AssetIDs) == 0 {
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
func graphAssetFilter(s board.State, r GraphRequest) (func(graphRecordIdentity) bool, error) {
	if len(r.AssetIDs) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	for _, asset := range s.Assets {
		known[asset.ID] = true
	}
	selected := map[string]bool{}
	for _, id := range r.AssetIDs {
		if !known[id] {
			return nil, errors.New("asset is absent from this graph input")
		}
		selected[id] = true
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
	case board.FactRecord:
		return graphRecordIdentity{id: v.ID}
	case board.Goal:
		return graphRecordIdentity{id: v.ID}
	case board.Step:
		return graphRecordIdentity{id: v.ID}
	case board.Finding:
		return graphRecordIdentity{id: v.ID}
	case board.Candidate:
		return graphRecordIdentity{id: v.ID}
	case board.Dispute:
		return graphRecordIdentity{id: v.ID}
	case board.FactRelation:
		return graphRecordIdentity{source: v.Source, target: v.Target}
	case board.Hint:
		return graphRecordIdentity{id: v.ID}
	case board.Asset:
		return graphRecordIdentity{id: v.ID}
	case board.AssetAnchor:
		return graphRecordIdentity{id: v.NodeID, assetID: v.AssetID}
	case board.HistoryEntry:
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
	if len(r.IDs) == 1 && (strings.TrimSpace(r.IDs[0]) == "" || len(r.IDs[0]) > 256 || r.IDs[0] == j.RunID || r.Offset != 0) {
		return errors.New("trace run must identify another execution in the current project")
	}
	return nil
}
