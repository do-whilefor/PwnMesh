package board

import (
	"slices"
	"strings"

	"pwnmesh/internal/board/model"
)

const assetSchema = `
CREATE TABLE IF NOT EXISTS xloom_assets(
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 id TEXT NOT NULL,kind TEXT NOT NULL,value TEXT NOT NULL,method TEXT NOT NULL,
 PRIMARY KEY(project_id,id),UNIQUE(project_id,kind,value,method));
CREATE TABLE IF NOT EXISTS xloom_asset_anchors(
 project_id TEXT NOT NULL,generation INTEGER NOT NULL,node_kind TEXT NOT NULL,node_id TEXT NOT NULL,asset_id TEXT NOT NULL,
 PRIMARY KEY(project_id,generation,node_kind,node_id,asset_id),
 FOREIGN KEY(project_id,asset_id) REFERENCES xloom_assets(project_id,id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS xloom_asset_anchors_asset ON xloom_asset_anchors(project_id,generation,asset_id);`

const MaxActionAssets = model.MaxActionAssets

type AssetSpec = model.AssetSpec
type Asset = model.Asset
type AssetAnchor = model.AssetAnchor

func NormalizeAsset(spec AssetSpec) (Asset, error) { return model.NormalizeAsset(spec) }

func normalizeActionAssets(specs []AssetSpec) ([]Asset, error) {
	if len(specs) > MaxActionAssets {
		return nil, Err(422, "an action may reference at most 32 assets")
	}
	var out []Asset
	for _, spec := range specs {
		a, err := NormalizeAsset(spec)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Asset) int { return strings.Compare(a.ID, b.ID) })
	return slices.CompactFunc(out, func(a, b Asset) bool { return a.ID == b.ID }), nil
}

func assetIDs(assets []Asset) []string {
	var out []string
	for _, asset := range assets {
		out = append(out, asset.ID)
	}
	return out
}

// AssetIDs reads the immutable snapshot's projected anchors, never live SQL.
func (s State) AssetIDs(nodeKind, nodeID string) []string {
	var ids []string
	for _, anchor := range s.AssetAnchors {
		if anchor.NodeKind == nodeKind && anchor.NodeID == nodeID {
			ids = append(ids, anchor.AssetID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (t *Tx) addAssetAnchors(project string, generation int64, kind, id string, assets []Asset) error {
	for _, a := range assets {
		if _, err := t.Exec(`INSERT OR IGNORE INTO xloom_assets(project_id,id,kind,value,method) VALUES(?,?,?,?,?)`, project, a.ID, a.Kind, a.Value, a.Method); err != nil {
			return err
		}
		if _, err := t.Exec(`INSERT OR IGNORE INTO xloom_asset_anchors(project_id,generation,node_kind,node_id,asset_id) VALUES(?,?,?,?,?)`, project, generation, kind, id, a.ID); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tx) projectAssets(s *State) error {
	rows, err := t.Query(`SELECT a.id,a.kind,a.value,a.method,n.node_kind,n.node_id FROM xloom_asset_anchors n JOIN xloom_assets a ON a.project_id=n.project_id AND a.id=n.asset_id WHERE n.project_id=? AND n.generation=? ORDER BY a.id,n.node_kind,n.node_id`, s.Graph.Project.ID, s.Graph.Project.Generation)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]bool{}
	type nodeKey struct{ kind, id string }
	byNode := map[nodeKey][]string{}
	for rows.Next() {
		var a Asset
		var anchor AssetAnchor
		if err := rows.Scan(&a.ID, &a.Kind, &a.Value, &a.Method, &anchor.NodeKind, &anchor.NodeID); err != nil {
			return err
		}
		anchor.AssetID = a.ID
		if !seen[a.ID] {
			s.Assets = append(s.Assets, a)
			seen[a.ID] = true
		}
		s.AssetAnchors = append(s.AssetAnchors, anchor)
		key := nodeKey{anchor.NodeKind, anchor.NodeID}
		byNode[key] = append(byNode[key], anchor.AssetID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// A finding's association follows its present support references. It is a
	// read projection, not a new claim or evidence-strength decision.
	active := map[string]Candidate{}
	for _, candidate := range s.ActiveCandidates() {
		active[candidate.ID] = candidate
	}
	for _, finding := range s.Findings {
		ids := []string{}
		for _, source := range finding.Sources {
			ids = append(ids, byNode[nodeKey{"fact", source}]...)
		}
		for _, candidateID := range finding.CandidateIDs {
			candidate, ok := active[candidateID]
			if !ok || candidate.Revision > finding.CuratedRevision || !candidateAssetsSupportFinding(*s, candidate, finding) {
				continue
			}
			ids = append(ids, byNode[nodeKey{"candidate", candidateID}]...)
		}
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			s.AssetAnchors = append(s.AssetAnchors, AssetAnchor{AssetID: id, NodeKind: "finding", NodeID: finding.ID})
		}
	}
	return nil
}

func candidateAssetsSupportFinding(s State, candidate Candidate, finding Finding) bool {
	if len(candidate.Sources) > 0 && s.ValidateFactSources(candidate.Sources, true) != nil {
		return false
	}
	for _, source := range candidate.Sources {
		if !slices.Contains(finding.Sources, source) {
			return false
		}
	}
	for _, evidence := range candidate.Evidence {
		if !slices.Contains(finding.Evidence, evidence) {
			return false
		}
	}
	return len(candidate.Sources)+len(candidate.Evidence) > 0 || finding.Status == "candidate"
}
