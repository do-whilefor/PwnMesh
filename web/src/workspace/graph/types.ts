export type NodeType = 'goal' | 'step' | 'fact' | 'finding';
export type VisualStatus = 'invalid' | 'done' | 'running' | 'paused' | 'pending';
export type StatusFilter = 'all' | 'done' | 'running' | 'pending';
export interface Point { x: number; y: number; width?: number; height?: number }
export interface Rect extends Point { width: number; height: number }
export interface Camera { scale: number; tx: number; ty: number }
export interface GraphRecord {
  [field: string]: unknown;
  id?: string;
  status?: string;
  description?: string;
  condition?: string;
  claim?: string;
  parent_id?: string;
  goal_id?: string;
  result?: string;
  sources?: string[];
  derived_from?: string[];
  from?: string[];
  depends_on?: string[];
  blocked_by?: string[];
  invalid_sources?: string[];
  support_valid?: boolean | null;
  support_invalid?: boolean;
  evidence?: unknown[];
  legacy?: boolean;
  source_step_id?: string;
  source?: string;
  target?: string;
  kind?: string;
  reason?: string;
}
export interface GraphNode {
  key: string;
  id: string;
  type: NodeType;
  label: string;
  title: string;
  description: string;
  status: string;
  evidence: unknown[];
  sources: string[];
  supportValid: boolean | null;
  raw: GraphRecord;
}
export interface GraphEdge {
  id: string;
  kind: string;
  source: string;
  target: string;
  label: string;
  supportValid: boolean | null;
  raw: GraphRecord | null;
}
export interface GraphEdgeDetails extends GraphEdge {
  sourceNode: GraphNode;
  targetNode: GraphNode;
  status: VisualStatus | 'recorded';
  statusLabel: string;
  description: string;
}
export type GraphDiagnostic =
  | {kind: 'invalid_node'; type: NodeType}
  | {kind: 'duplicate_node'; key: string}
  | {kind: 'missing_reference'; relation: string; source: string; target: string; missing: string[]}
  | {kind: 'invalid_relation'};
export interface GraphProject {
  projectId: string;
  generation: number;
  nodes: GraphNode[];
  edges: GraphEdge[];
  diagnostics: GraphDiagnostic[];
  nodeIndex: Map<string, GraphNode>;
  edgeIndex: Map<string, GraphEdge>;
  incoming: Map<string, GraphEdge[]>;
  outgoing: Map<string, GraphEdge[]>;
}
export type NodeSelection = string | {type: string; id: string} | null | undefined;
export type EdgeSelection = string | GraphEdge | null | undefined;
export interface LayoutNode { key?: string; id: string; type?: string; raw?: {parent_id?: string} }
export interface LayoutOptions { gapX?: number; gapY?: number; padding?: number; seed?: string }
export interface GraphLayout {
  projectId: string;
  generation: number;
  positions: Map<string, Point>;
  width?: number;
  height?: number;
  startKey?: string | null;
  endKey?: string | null;
  camera?: Camera & {width: number; height: number};
}
export interface RoutedEdge { id?: string; kind?: string; source: string; target: string }
export interface EdgeRoute {
  d: string;
  points: Point[];
  sourcePort: string;
  targetPort: string;
  start: Point;
  end: Point;
  control1: Point;
  control2: Point;
}
