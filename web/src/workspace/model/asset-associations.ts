import type { AssetAnchorDTO, AssetDTO, WorkspaceSnapshot } from '../../api/types';

export interface AssetAssociation {
  asset: AssetDTO;
  anchor: AssetAnchorDTO;
  provenance: string;
}
function associationIndex(state: WorkspaceSnapshot): Map<string, AssetAssociation[]> {
  const assets = new Map((state.assets || []).map(asset => [asset.id, asset]));
  const index = new Map<string, AssetAssociation[]>();
  for (const anchor of state.asset_anchors || []) {
    const asset = assets.get(anchor.asset_id);
    if (!asset) continue;
    const kind = anchor.node_kind, key = kind + ':' + anchor.node_id;
    const provenance = anchor.discovery_step_id ? 'Discovery via Step ' + anchor.discovery_step_id
      : kind === 'finding' ? 'Curated support association' : kind === 'step' ? 'Declared target' : 'Explicit association';
    const entries = index.get(key) || [];
    entries.push({asset, anchor, provenance});
    index.set(key, entries);
  }
  return index;
}
export function assetAssociations(state: WorkspaceSnapshot, kind: string, id: string): AssetAssociation[] {
  return associationIndex(state).get(kind + ':' + id) || [];
}
