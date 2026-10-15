import { stepSupportValid } from '../model/evidence';
import type { GraphNode, GraphEdge, GraphRecord, GraphProject, GraphDiagnostic, NodeType, NodeSelection } from './types';
const TYPES = ['goal', 'step', 'fact', 'finding'];
const RELATION_LABELS: Record<string, string> = {supersedes: '取代', refutes: '反驳', narrows: '收窄'};
const array = (value: unknown): unknown[] => Array.isArray(value) ? value : [];
const strings = (value: unknown): string[] => array(value).filter((item): item is string => typeof item === 'string' && item.length > 0);
const clone = <T>(value: T): T => value === undefined ? value : JSON.parse(JSON.stringify(value));
const text = (value: unknown): string => typeof value === 'string' ? value : '';
const keyOf = (type: string, id: unknown) => type + ':' + id;
// State is the server's complete /projects/{id}/state response. No inferred facts
// or evidence are added here: a missing endpoint is reported and its edge omitted.
export function mapState(input: unknown): GraphProject {
  const state = input && typeof input === 'object' ? input as Record<string, unknown> : {};
  const graph = state.graph && typeof state.graph === 'object' ? state.graph as Record<string, unknown> : {};
  const project = graph.project && typeof graph.project === 'object' ? graph.project as Record<string, unknown> : {};
  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  const diagnostics: GraphDiagnostic[] = [];
  const nodeIndex = new Map<string, GraphNode>(), edgeIndex = new Map<string, GraphEdge>();
  const incoming = new Map<string, GraphEdge[]>(), outgoing = new Map<string, GraphEdge[]>();
  const addNode = (type: NodeType, value: unknown, labelField: keyof GraphRecord) => {
    const raw = value && typeof value === 'object' ? value as GraphRecord : null;
    if (!raw || typeof raw !== 'object' || typeof raw.id !== 'string' || !raw.id) {
      diagnostics.push({kind: 'invalid_node', type});
      return;
    }
    const key = keyOf(type, raw.id);
    if (nodeIndex.has(key)) {
      diagnostics.push({kind: 'duplicate_node', key});
      return;
    }
    const label = text(raw[labelField]);
    const node: GraphNode = {
      key, id: raw.id, type, label, title: label, description: label, status: text(raw.status) || 'unknown',
      evidence: clone(array(raw.evidence)), sources: [...strings(raw.sources)],
      supportValid: typeof raw.support_valid === 'boolean' ? raw.support_valid : null,
      raw: clone(raw)
    };
    nodes.push(node);
    nodeIndex.set(key, node);
    incoming.set(key, []); outgoing.set(key, []);
  };
  array(state.goals).forEach(raw => addNode('goal', raw, 'condition'));
  array(state.steps).forEach(raw => addNode('step', raw, 'description'));
  array(state.fact_records).forEach(raw => addNode('fact', raw, 'description'));
  array(state.findings).forEach(raw => addNode('finding', raw, 'claim'));
  const addEdge = (kind: string, source: string, target: string, label: string, raw: GraphRecord | null, supportValid: boolean | null = null) => {
    const missing = [source, target].filter(key => !nodeIndex.has(key));
    if (missing.length) {
      diagnostics.push({kind: 'missing_reference', relation: kind, source, target, missing});
      return;
    }
    const id = 'edge:' + JSON.stringify([kind, source, target]);
    if (edgeIndex.has(id)) return;
    const edge = {id, kind, source, target, label, supportValid, raw: clone(raw)};
    edgeIndex.set(id, edge); edges.push(edge);
  };
  nodes.forEach(node => {
    const raw = node.raw;
    if (node.type === 'goal') {
      if (text(raw.parent_id)) addEdge('parent', keyOf('goal', raw.parent_id), node.key, '子目标', null);
      if (node.status === 'achieved') node.sources.forEach(id => addEdge('goal_support', keyOf('fact', id), node.key, '支持目标', null, node.supportValid));
    } else if (node.type === 'step') {
      const result = nodeIndex.get(keyOf('fact', raw.result));
      node.supportValid = stepSupportValid(raw, result?.raw, typeof project.orchestration_version === 'number' ? project.orchestration_version : undefined);
      if (text(raw.goal_id)) addEdge('goal_step', keyOf('goal', raw.goal_id), node.key, '任务归属', null);
      strings(raw.depends_on).forEach(id => addEdge('step_dependency', keyOf('step', id), node.key, '执行依赖', null));
      strings(raw.from).forEach(id => addEdge('step_input', keyOf('fact', id), node.key, '依据', null, strings(raw.invalid_sources).includes(id) ? false : null));
      if (text(raw.result)) addEdge('step_result', node.key, keyOf('fact', raw.result), '产生', null);
    } else if (node.type === 'finding') {
      node.sources.forEach(id => addEdge('finding_support', keyOf('fact', id), node.key, '支持发现', null, node.supportValid));
    }
  });
  array(state.fact_relations).forEach(value => {
    const relation = value && typeof value === 'object' ? value as GraphRecord : null;
    if (!relation || !text(relation.source) || !text(relation.target) || !text(relation.kind)) {
      diagnostics.push({kind: 'invalid_relation'});
      return;
    }
    // A correcting source refutes/narrows/supersedes its old target.
    addEdge(text(relation.kind), keyOf('fact', relation.source), keyOf('fact', relation.target), RELATION_LABELS[text(relation.kind)] || text(relation.kind), relation);
  });
  nodes.sort((a, b) => a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
  edges.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  for (const edge of edges) { incoming.get(edge.target)!.push(edge); outgoing.get(edge.source)!.push(edge); }
  const projectId = text(project.id);
  const generation = Number(project.generation) || 0;
  return {projectId, generation, nodes, edges, diagnostics, nodeIndex, edgeIndex, incoming, outgoing};
}

export function resolveNodeKey(nodes: readonly Pick<GraphNode, 'key' | 'id'>[], selection: NodeSelection): string | null {
  if (selection === null || selection === undefined) return null;
  if (typeof selection === 'object') {
    if (!TYPES.includes(selection.type) || typeof selection.id !== 'string') return null;
    const key = keyOf(selection.type, selection.id);
    return nodes.some(node => node.key === key) ? key : null;
  }
  if (typeof selection !== 'string') return null;
  if (nodes.some(node => node.key === selection)) return selection;
  const matches = nodes.filter(node => node.id === selection);
  return matches.length === 1 ? matches[0].key : null;
}
