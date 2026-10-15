import type { GraphNode, GraphProject, GraphEdgeDetails, Point, Rect, VisualStatus, EdgeSelection } from './types';
type PresentableNode = Pick<GraphNode, 'type' | 'status'> & Partial<Pick<GraphNode, 'key' | 'sources' | 'supportValid' | 'raw'>>;
const STATES: Record<string, string> = {open: '待执行', pending: '待执行', blocked: '等待依赖', running: '运行中', completed: '已完成', cancelled: '已取消', failed: '失败', abandoned: '已放弃', achieved: '已达成', withdrawn: '已撤回', valid: '有效', input: '输入', superseded: '被取代', refuted: '被反驳', narrowed: '已收窄', candidate: '待验证', verified: '已验证', paused: '已暂停', needs_review: '待复核', unknown: '未标明状态'};
const KINDS: Record<string, string> = {start: '起点', task: '任务', fact: '事实', goal: '终点', subgoal: '子目标', finding: '发现'};
export const edgeKey = (edge: Exclude<EdgeSelection, null | undefined>): string => typeof edge === 'string' ? edge : edge.id || 'edge:' + JSON.stringify([edge.kind, edge.source, edge.target]);
function supportProblem(node: PresentableNode) {
  if (node.raw?.invalid_sources?.length || node.raw?.support_invalid === true) return '支持失效';
  if (node.type === 'step' && node.status === 'completed' && node.supportValid === false) return '完成结果支持失效';
  if (node.type === 'goal' && node.status === 'achieved') {
    if (!node.sources?.length) return '缺少有效证据';
    if (node.supportValid !== true) return node.supportValid === false ? '支持失效' : '证据待核对';
  }
  if (node.type === 'finding') {
    if (node.status === 'verified') {
      if (!node.sources?.length) return '缺少有效证据';
      if (node.supportValid !== true) return node.supportValid === false ? '支持失效' : '证据待核对';
    }
    if (node.sources?.length && node.supportValid === false) return '支持失效';
  }
  return '';
}
export function visualStatus(node: PresentableNode): VisualStatus {
  if (supportProblem(node) || ['failed', 'cancelled', 'refuted', 'superseded', 'narrowed', 'withdrawn', 'abandoned', 'needs_review'].includes(node.status)) return 'invalid';
  if (['completed', 'achieved', 'valid', 'input', 'verified'].includes(node.status)) return 'done';
  if (node.status === 'running') return 'running';
  if (node.status === 'paused') return 'paused';
  return 'pending';
}
export function nodePresentation(node: PresentableNode, endKey: string | null | undefined) {
  const kind = node.key === 'fact:origin' ? 'start' : node.key === endKey ? 'goal' : node.type === 'step' ? 'task' : node.type === 'goal' ? 'subgoal' : node.type;
  const invalid = supportProblem(node);
  const statusLabel = (STATES[node.status] || node.status) + (invalid ? ' · ' + invalid : '');
  return {kind, label: node.key === 'fact:goal' ? '目标输入' : KINDS[kind] || node.type, status: visualStatus(node), statusLabel};
}
export function projectGeometry(nodes: readonly Pick<GraphNode, 'key'>[], positions: ReadonlyMap<string, Point>, width = 148, height = 78): Map<string, Rect> {
  return new Map(nodes.filter(node => positions.has(node.key)).map(node => [node.key, {...positions.get(node.key)!, width, height}]));
}
export function describeEdge(project: GraphProject | null, edge: EdgeSelection): GraphEdgeDetails | null {
  if (!project || !edge) return null;
  const record = project.edgeIndex.get(edgeKey(edge));
  if (!record) return null;
  const sourceNode = project.nodeIndex.get(record.source), targetNode = project.nodeIndex.get(record.target);
  if (!sourceNode || !targetNode) return null;
  if (record.kind === 'step_dependency') {
    // Completion alone is not readiness: the server validates the retained
    // result and its transitive supports before setting support_valid.
    const blocked = targetNode.raw?.blocked_by?.includes(sourceNode.id);
    let status: VisualStatus = 'pending', statusLabel = '等待前置步骤';
    if (sourceNode.supportValid === true && !blocked) { status = 'done'; statusLabel = '依赖已满足'; }
    else if (sourceNode.status === 'completed' && blocked) { status = 'invalid'; statusLabel = '前置结果需复核'; }
    else if (visualStatus(sourceNode) === 'invalid') { status = 'invalid'; statusLabel = '前置步骤需处理'; }
    else if (sourceNode.status === 'running') { status = 'running'; statusLabel = '前置步骤运行中'; }
    else if (sourceNode.status === 'paused') { status = 'paused'; statusLabel = '前置步骤已暂停'; }
    else if (sourceNode.status === 'completed') statusLabel = '等待有效完成结果';
    return {...record, sourceNode, targetNode, status, statusLabel, description: `${targetNode.label || targetNode.id} 依赖 ${sourceNode.label || sourceNode.id} 的有效完成结果。${statusLabel}。`};
  }
  const correction = ['refutes', 'supersedes', 'narrows'].includes(record.kind);
  const invalid = record.supportValid === false || (['goal_support', 'finding_support', 'step_input'].includes(record.kind) && (sourceNode.raw?.support_invalid === true || ['refuted', 'superseded', 'narrowed'].includes(sourceNode.status))) || (record.kind === 'step_result' && visualStatus(targetNode) === 'invalid');
  const step = record.kind === 'step_input' ? targetNode : record.kind === 'step_result' ? sourceNode : null;
  const supported = ['goal_support', 'finding_support'].includes(record.kind) ? targetNode : null;
  const status = invalid || correction ? 'invalid' : step ? visualStatus(step) : supported ? visualStatus(supported) : 'recorded';
  const statusLabel = invalid ? '支持已失效' : correction ? record.label : step ? nodePresentation(step, null).statusLabel : supported ? nodePresentation(supported, null).statusLabel : '已记录关系';
  return {...record, sourceNode, targetNode, status, statusLabel, description: record.raw?.description || record.raw?.reason || `${record.label}：${sourceNode.label} → ${targetNode.label}`};
}
export function collectSelection(project: GraphProject | null, nodeId: string | null, selectedEdge: EdgeSelection, direction: 'upstream' | 'downstream' = 'upstream') {
  const nodeIds = new Set<string>(), edgeKeys = new Set<string>();
  if (!project) return {active: false, nodeIds, edgeKeys};
  if (selectedEdge) {
    const edge = project.edgeIndex.get(edgeKey(selectedEdge));
    if (edge) { nodeIds.add(edge.source); nodeIds.add(edge.target); edgeKeys.add(edgeKey(edge)); }
  } else if (nodeId && project.nodeIndex.has(nodeId)) {
    const adjacency = direction === 'downstream' ? project.outgoing : project.incoming;
    const pending = [nodeId];
    while (pending.length) {
      const id = pending.pop()!;
      if (nodeIds.has(id)) continue;
      nodeIds.add(id);
      for (const edge of adjacency.get(id) || []) { edgeKeys.add(edgeKey(edge)); pending.push(direction === 'downstream' ? edge.target : edge.source); }
    }
  }
  return {active: nodeIds.size > 0, nodeIds, edgeKeys};
}
export function placeEdgeLabel(points: readonly Point[], label: string) {
  if (!points?.length) return null;
  const middle = Math.floor(points.length / 2), point = points[middle];
  const before = points[Math.max(0, middle - 1)], after = points[Math.min(points.length - 1, middle + 1)];
  const angle = Math.atan2(after.y - before.y, after.x - before.x);
  const rotation = angle > Math.PI / 2 ? angle - Math.PI : angle < -Math.PI / 2 ? angle + Math.PI : angle;
  return {text: label, x: point.x + Math.sin(rotation) * 12, y: point.y - Math.cos(rotation) * 12, angle: rotation * 180 / Math.PI};
}
