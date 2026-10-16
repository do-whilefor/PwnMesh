/** Wire contracts used by the workspace. Optional fields also support archived legacy rounds. */
export interface EvidenceDTO { run_id?: string; path?: string; excerpt?: string; start_line?: number; end_line?: number }
export interface EntityDTO {
  id: string; status?: string; description?: string; condition?: string; claim?: string;
  reason?: string; scope?: string; created_at?: string; updated_at?: string; observed_at?: string;
  sources?: string[]; evidence?: EvidenceDTO[]; support_valid?: boolean | null; support_invalid?: boolean;
  parent_id?: string; goal_id?: string; from?: string[]; result?: string | null; worker?: string | null;
  invalid_sources?: string[]; depends_on?: string[]; blocked_by?: string[]; legacy?: boolean; source_step_id?: string; run_id?: string;
  /** Legacy archived detail retained by the material/result reader. */
  detail?: {body?: string; text?: string; output?: string; scope?: string; evidence?: EvidenceDTO[]};
}
export interface GoalDTO extends EntityDTO { condition?: string }
export interface StepDTO extends EntityDTO { description?: string }
export interface FactDTO extends EntityDTO { description?: string; derived_from?: string[] }
export interface FindingDTO extends EntityDTO { claim?: string }
export interface AssetDTO { id: string; kind: 'host' | 'service' | 'endpoint'; value: string; method?: string }
export interface AssetAnchorDTO { asset_id: string; node_kind: 'step' | 'fact' | 'candidate' | 'finding'; node_id: string; discovery_step_id?: string }
export interface FactRelationDTO { kind: string; source: string; target: string; reason?: string; run_id?: string; created_at?: string; description?: string }
export interface ReasonDTO { worker?: string; trigger?: string; started_at?: string }
export interface ProjectDTO {
  id: string; title: string; status: string; scenario?: string; orchestration_version?: number;
  generation?: number; created_at?: string; restarted_at?: string; terminated_at?: string; reason?: ReasonDTO | null;
}
export interface IntentDTO { id: string; from?: string[]; to?: string | null; description?: string; creator?: string; worker?: string | null; created_at?: string; started_at?: string; concluded_at?: string | null }
export interface HintDTO { id: string; content?: string; creator?: string; created_at?: string }
export interface WorkspaceSnapshot {
  graph: { project: ProjectDTO; facts?: FactDTO[]; intents?: IntentDTO[]; hints?: HintDTO[] };
  goals: GoalDTO[]; steps: StepDTO[]; fact_records: FactDTO[]; findings: FindingDTO[]; fact_relations?: FactRelationDTO[];
  revision: number; decision_revision?: number;
  assets?: AssetDTO[]; asset_anchors?: AssetAnchorDTO[];
}
export interface StateEventDTO { revision: number; op: string; id?: string; run_id?: string; created_at?: string; payload?: unknown; result?: unknown }
export interface ExecutionDTO { id: string; project_id?: string; generation?: number; kind?: string; backend?: string; intent?: string; status?: string; resumes?: number; created_at?: string; updated_at?: string; started_at?: string; result?: { text?: string; error?: string; truncated?: boolean } | null }
export interface InputDTO { id: string; name: string; size: number; sha256?: string; path?: string; created_at?: string }
export interface RoundDTO { generation: number; archived_at: string }
export interface ArchiveEntryDTO { cursor: number; kind: string; bytes: number }
export interface ArchiveChunkDTO { encoding: string; offset: number; total_bytes: number; data: string; next_offset?: number | null }
export interface PageDTO<T> { items: T[]; next_cursor?: number | null }
export interface ExecutionChangePageDTO { items: ExecutionDTO[]; deleted: string[]; generation: number; through: number; next_cursor?: number }
export interface ExecutionChanges { items: ExecutionDTO[]; deleted: string[]; through: number }
export interface CreateProjectDTO { title: string; origin: string; goal: string; scenario: string; orchestration_version: 1; start_paused?: boolean }
export interface CreatedProjectDTO { project: ProjectDTO }
