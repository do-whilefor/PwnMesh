package board

import (
	"slices"
	"strings"

	"pwnmesh/internal/board/model"
)

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

func (t *Tx) addAssetAnchors(project string, generation int64, kind, id string, assets []Asset) error {
	for _, a := range assets {
		if _, err := t.Exec(`INSERT OR IGNORE INTO pwnmesh_assets(project_id,id,kind,value,method) VALUES(?,?,?,?,?)`, project, a.ID, a.Kind, a.Value, a.Method); err != nil {
			return err
		}
		if _, err := t.Exec(`INSERT OR IGNORE INTO pwnmesh_asset_anchors(project_id,generation,node_kind,node_id,asset_id) VALUES(?,?,?,?,?)`, project, generation, kind, id, a.ID); err != nil {
			return err
		}
	}
	return nil
}
