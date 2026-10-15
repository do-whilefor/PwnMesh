import { APIError, Client, RequestScope } from '../api/client';
import type { APIClient, RequestOptions } from '../api/client';
import type { ArchiveEntryDTO, ExecutionDTO, InputDTO, ProjectDTO, RoundDTO, StateEventDTO, WorkspaceSnapshot } from '../api/types';
import { buildLogs, projectTiming } from './model/data';
import type { ProjectStatus, ProjectView } from './model/types';
import { readArchiveEntry, readPages } from './history';

export const pathFor = (id: string): string => '/projects/' + encodeURIComponent(id);
const statuses: Record<string, ProjectStatus> = {active: 'running', stopped: 'paused', completed: 'done', terminated: 'terminated'};
export function projectView(raw: ProjectDTO, previous: Partial<ProjectView> = {}): ProjectView {
  return {...previous, id: raw.id, name: raw.title, type: raw.scenario || '', status: statuses[raw.status] || 'pending', readOnly: raw.orchestration_version !== 1,
    generation: raw.generation || 0, createdAt: raw.created_at, endedAt: raw.terminated_at || previous.endedAt || null, target: previous.target || '', goal: previous.goal || '',
    files: previous.files || [], history: previous.history || [], state: previous.state || null, logs: previous.logs || [], raw};
}
export function snapshotView(state: WorkspaceSnapshot, events: StateEventDTO[] = [], runs: ExecutionDTO[] = [], files: InputDTO[] = [], previous: Partial<ProjectView> = {}): ProjectView {
  const project = projectView(state.graph.project, previous), facts = state.graph.facts || [];
  const timing = projectTiming(state, runs);
  return {...project, state, logs: buildLogs(state, events, runs), runs, events, files, target: facts.find(f => f.id === 'origin')?.description || '',
    goal: facts.find(f => f.id === 'goal')?.description || state.goals?.find(g => g.id === 'goal')?.condition || '', startedAt: timing.startedAt, endedAt: timing.endedAt || null};
}
interface EventCache { generation: number; revision: number; after: number; caughtUp: boolean; items: StateEventDTO[] }
interface ReadView { generation: number; summary: string; state: WorkspaceSnapshot; runs: ExecutionDTO[]; files: InputDTO[]; rounds: RoundDTO[]; executionRevision: number }
export interface RefreshResult { projects: readonly ProjectView[]; selected: ProjectView | undefined }
async function readEventCache(api: APIClient, base: string, state: WorkspaceSnapshot, previous: EventCache | undefined, options: RequestOptions): Promise<EventCache> {
  const generation = state.graph.project.generation || 0, revision = state.revision;
  const cache: EventCache = !previous || previous.generation !== generation || revision < previous.revision
    ? {generation, revision: 0, after: 0, items: [], caughtUp: false} : previous;
  if (cache.caughtUp && cache.revision === revision) return cache;
  const batch: StateEventDTO[] = [];
  let after = cache.after, caughtUp = false;
  // A bounded refresh continues an unfinished backlog even when state is unchanged.
  for (let page = 0; page < 5; page++) {
    const items = await api.request<StateEventDTO[]>(base + '/state/events?after=' + after, options);
    if (!items.length) { caughtUp = true; break; }
    const next = Math.max(...items.map(event => event.revision));
    if (next <= after) { caughtUp = true; break; }
    batch.push(...items); after = next;
    if (items.length < 1000) { caughtUp = true; break; }
  }
  return {generation, revision, after, caughtUp, items: batch.length ? cache.items.concat(batch) : cache.items};
}

/** Sole owner of accepted workspace reads; Vue owns only the refresh schedule. */
export class ProjectResource {
  readonly scope = new RequestScope();
  private accepted: ProjectView[] = [];
  private readonly events = new Map<string, EventCache>();
  private readonly views = new Map<string, ReadView>();
  constructor(readonly api: APIClient = new Client()) {}
  get projects(): readonly ProjectView[] { return this.accepted; }
  cancel(): void { this.scope.cancel(); }
  invalidate(id: string): void { this.events.delete(id); this.views.delete(id); }
  async refresh(preferred = ''): Promise<RefreshResult | null> {
    const request = this.scope.begin(), options = {signal: request.signal};
    const list = await this.api.request<ProjectDTO[]>('/projects', options);
    if (!this.scope.current(request.version)) return null;
    const projects = list.map(raw => projectView(raw, this.accepted.find(p => p.id === raw.id))).sort((a, b) => String(b.createdAt).localeCompare(String(a.createdAt)) || b.id.localeCompare(a.id));
    const selected = projects.find(p => p.id === preferred) || projects[0];
    if (selected) {
      const base = pathFor(selected.id), previous = this.views.get(selected.id), summary = JSON.stringify(selected.raw);
      const sameRound = previous?.generation === selected.generation;
      // Late execution cleanup can add data after a project reaches its terminal status.
      const terminal = ['done', 'terminated'].includes(selected.status), stableTerminal = sameRound && terminal && previous.summary === summary;
      const [loadedState, changes, rounds] = await Promise.all([
        stableTerminal ? previous.state : this.api.request<WorkspaceSnapshot>(base + '/state', options),
        this.api.executionChanges(base, sameRound ? previous.executionRevision : 0, selected.generation, options).catch((error: unknown) => {
          if (error instanceof APIError && error.status === 409 && this.scope.current(request.version)) this.invalidate(selected.id);
          throw error;
        }),
        sameRound ? previous.rounds : readPages<RoundDTO>(this.api, base + '/rounds', options),
      ]);
      if (!this.scope.current(request.version)) return null;
      const state = terminal && (changes.items.length || changes.deleted.length) ? await this.api.request<WorkspaceSnapshot>(base + '/state', options) : loadedState;
      if (!this.scope.current(request.version)) return null;
      const generation = state.graph.project.generation || 0;
      const files = sameRound && state.revision === previous.state.revision ? previous.files : await this.api.request<InputDTO[]>(base + '/inputs', options);
      if (!this.scope.current(request.version)) return null;
      const runsByID = new Map((sameRound ? previous.runs : []).map(run => [run.id, run]));
      for (const id of changes.deleted) runsByID.delete(id);
      for (const run of changes.items) if ((run.generation || 0) === generation) runsByID.set(run.id, run);
      const runs = [...runsByID.values()], previousEvents = this.events.get(selected.id);
      const cache = await readEventCache(this.api, base, state, previousEvents, options);
      const identity = await this.api.request<{id: string; generation?: number}>(base + '/identity', options);
      if (!this.scope.current(request.version)) return null;
      // State/event revisions, execution cursors and project generations are different axes.
      if (selected.generation !== generation || (identity.generation || 0) !== generation) { this.invalidate(selected.id); throw new APIError('项目已进入新轮次，正在重新同步。', 409); }
      this.events.set(selected.id, cache);
      if (!stableTerminal || changes.items.length || changes.deleted.length || cache.items !== previousEvents?.items) Object.assign(selected, snapshotView(state, cache.items.filter(e => e.revision <= state.revision), runs, files, selected));
      const history = this.accepted.find(project => project.id === selected.id)?.history || selected.history;
      selected.history = rounds.map(round => {
        const prior = history.find(p => p.generation === round.generation);
        return {...projectView(selected.raw), ...prior, id: selected.id, name: selected.name, type: selected.type, generation: round.generation,
          status: prior?.status || 'paused', archived: true, archivedAt: round.archived_at, endedAt: prior?.endedAt || round.archived_at, files: prior?.files || [], filesKnown: prior?.filesKnown === true, history: []};
      });
      this.views.set(selected.id, {generation, summary, state, runs, files, rounds, executionRevision: changes.through});
    }
    if (!this.scope.current(request.version)) return null;
    // Archives may finish after the list was copied, including for an unselected project.
    for (const project of projects) if (project !== selected) project.history = this.accepted.find(value => value.id === project.id)?.history || project.history;
    for (const id of this.views.keys()) if (!projects.some(project => project.id === id)) this.invalidate(id);
    this.accepted = projects;
    return {projects: this.accepted, selected};
  }
  async archive(projectOrId: ProjectView | string, generation: number): Promise<ProjectView> {
    const id = typeof projectOrId === 'string' ? projectOrId : projectOrId.id;
    const summary = this.accepted.find(p => p.id === id)?.history.find(p => p.generation === generation);
    if (!summary) throw new Error('此历史轮次不存在。');
    if (summary.state) return summary;
    const base = pathFor(id) + '/rounds/' + generation;
    const entries = await readPages<ArchiveEntryDTO>(this.api, base + '/entries', {}, 0);
    const records: {state: WorkspaceSnapshot[]; event: StateEventDTO[]; execution: ExecutionDTO[]; inputs: InputDTO[][]} = {state: [], event: [], execution: [], inputs: []};
    for (const entry of entries) {
      if (entry.kind === 'state') records.state.push(await readArchiveEntry<WorkspaceSnapshot>(this.api, base, entry));
      else if (entry.kind === 'event') records.event.push(await readArchiveEntry<StateEventDTO>(this.api, base, entry));
      else if (entry.kind === 'execution') records.execution.push(await readArchiveEntry<ExecutionDTO>(this.api, base, entry));
      else if (entry.kind === 'inputs') records.inputs.push(await readArchiveEntry<InputDTO[]>(this.api, base, entry));
    }
    if (!records.state.length) throw new Error('历史轮次缺少状态快照。');
    if (records.inputs.length > 1 || (records.inputs.length === 1 && !Array.isArray(records.inputs[0]))) throw new Error('历史材料清单格式无效。');
    const filesKnown = records.inputs.length === 1;
    const snapshot = snapshotView(records.state[0]!, records.event, records.execution, filesKnown ? records.inputs[0]! : [], summary);
    const value: ProjectView = {...snapshot, archived: true, filesKnown, endedAt: snapshot.endedAt || summary.archivedAt || null, history: []};
    // Publish through the resource only, never mutate a caller's detached project object.
    this.accepted = this.accepted.map(project => project.id !== id ? project : {...project, history: project.history.map(round => round.generation === generation ? value : round)});
    return value;
  }
}
