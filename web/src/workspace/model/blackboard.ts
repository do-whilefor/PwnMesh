import type { AssetAnchorDTO, AssetDTO, CandidateDTO, EntityDTO, WorkspaceSnapshot } from '../../api/types';

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

export interface AssociatedRecord { kind: AssetAnchorDTO['node_kind']; record: EntityDTO; associations: AssetAssociation[] }
// These values describe retained identities, never evidence validity or authority.
export function blackboardView(state: WorkspaceSnapshot, assetID = '') {
  const records: AssociatedRecord[] = [];
  const associationsByNode = associationIndex(state);
  const collections: [AssociatedRecord['kind'], EntityDTO[]][] = [
    ['step', state.steps], ['fact', state.fact_records], ['finding', state.findings], ['candidate', state.candidates || []],
  ];
  for (const [kind, items] of collections) for (const record of items) {
    const associations = (associationsByNode.get(kind + ':' + record.id) || []).filter(item => !assetID || item.asset.id === assetID);
    if (associations.length) records.push({kind, record, associations});
  }
  const attached = new Set(records.map(item => item.kind + ':' + item.record.id));
  const candidates: CandidateDTO[] = (state.candidates || []).filter(candidate => !assetID || attached.has('candidate:' + candidate.id));
  const disputes = (state.disputes || []).filter(dispute => !assetID || attached.has('finding:' + dispute.finding_id) || dispute.candidate_ids.some(id => attached.has('candidate:' + id)));
  return {records, candidates, disputes};
}
