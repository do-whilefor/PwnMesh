import type { EvidenceDTO, ExecutionDTO, InputDTO, ProjectDTO, StateEventDTO, WorkspaceSnapshot } from '../../api/types';
export type { WorkspaceSnapshot } from '../../api/types';
export type ProjectAction = 'pause' | 'resume' | 'restart' | 'terminate' | 'delete';
export type ProjectStatus = 'running' | 'paused' | 'done' | 'terminated' | 'pending';
export type NodeType = 'goal' | 'step' | 'fact' | 'finding';
export interface NodeReference { type: NodeType; id: string }
export interface LogRecord {
  id: string; phase: string; level: string; worker: string; title: string; body: string; time: string;
  source: string; node?: NodeReference; revision?: number; status?: string; evidence?: NodeReference[];
  supportValid?: boolean | null; scope?: string; artifacts?: EvidenceDTO[]; kind?: string; statusLabel?: string;
  code?: string; runId?: string; truncated?: boolean;
}
export interface ProjectView {
  id: string; name: string; type: string; status: ProjectStatus; readOnly: boolean; generation: number;
  createdAt?: string; startedAt?: string | null; endedAt: string | null; target: string; goal: string;
  files: InputDTO[]; filesKnown?: boolean; history: ProjectView[]; state: WorkspaceSnapshot | null;
  logs: LogRecord[]; raw: ProjectDTO; runs?: ExecutionDTO[]; events?: StateEventDTO[];
  archived?: boolean; archivedAt?: string;
}
export interface CreateProjectValues { name: string; target: string; goal: string; type: string; files: File[] }
export interface HintValues { text: string; files: File[] }
export type CreateDraft = CreateProjectValues;
export type HintDraft = HintValues;
export interface ActivityEntry { id: string; title: string; body: string; evidence: string; phase: string; timestamp: string; time: string; key: string | null }
export interface FindingView { id: string; claim: string; status: string; statusLabel: string; supportValid: boolean; sources: NodeReference[] }
export interface ResultView { status: string; summary: string; notice: string; findings: FindingView[]; conclusions: LogRecord[]; truncated: boolean }
