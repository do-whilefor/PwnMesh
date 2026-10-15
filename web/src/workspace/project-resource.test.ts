import { expect, test } from 'vitest';
import { APIError, Client } from '../api/client';
import type { RequestOptions } from '../api/client';
import type { ExecutionChanges, RoundDTO, WorkspaceSnapshot } from '../api/types';
import { ProjectResource, projectView, snapshotView } from './project-resource';

const stamp = '2026-10-10T02:00:00Z';
function snapshot(id = 'a', generation = 0, revision = 1): WorkspaceSnapshot {
  return {graph: {project: {id, title: id, scenario: 'pentest', status: 'active', orchestration_version: 1, generation, created_at: stamp}, facts: [{id: 'origin', description: 'https://authorized.test'}, {id: 'goal', description: 'Find supported evidence'}], intents: [], hints: []}, goals: [], steps: [], fact_records: [], findings: [], revision};
}
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return {promise, resolve}; }
class FixtureAPI extends Client {
  calls: {path: string; options: RequestOptions}[] = [];
  executionCalls: {after: number; generation: number}[] = [];
  rounds: RoundDTO[] = [];
  handler = (path: string, _options: RequestOptions): unknown | Promise<unknown> => this.response(path);
  changes = async (_after: number, _generation: number): Promise<ExecutionChanges> => ({items: [], deleted: [], through: 0});
  constructor(readonly states: Record<string, WorkspaceSnapshot> = {a: snapshot()}) { super(); }
  response(path: string): unknown {
    if (path === '/projects') return Object.values(this.states).map(state => structuredClone(state.graph.project));
    const [, id, suffix] = path.match(/^\/projects\/([^/]+)(.*)$/) || [];
    const state = this.states[id || ''];
    if (!state) throw new Error(path);
    if (suffix === '/state') return structuredClone(state);
    if (suffix === '/identity') return {id, generation: state.graph.project.generation};
    if (suffix === '/inputs') return [];
    if (suffix?.startsWith('/rounds?')) return {items: this.rounds};
    if (suffix?.startsWith('/state/events?')) return [];
    throw new Error(path);
  }
  override async request<T = unknown>(path: string, options: RequestOptions = {}): Promise<T> { this.calls.push({path, options}); return await this.handler(path, options) as T; }
  override async executionChanges(_path: string, after: number, generation: number): Promise<ExecutionChanges> { this.executionCalls.push({after, generation}); return this.changes(after, generation); }
}

test('projections map lifecycle, real inputs, unspecified scenarios and legacy read-only state', () => {
  const state = snapshot(), p = snapshotView(state);
  expect(p).toMatchObject({name: 'a', target: 'https://authorized.test', goal: 'Find supported evidence', status: 'running', endedAt: null});
  for (const [status, expected] of [['stopped', 'paused'], ['completed', 'done'], ['terminated', 'terminated']]) expect(projectView({...state.graph.project, status: status!}).status).toBe(expected);
  for (const scenario of [undefined, '', 'ctf', 'audit', 'pentest']) expect(projectView({...state.graph.project, scenario}, {type: 'pentest'}).type).toBe(scenario || '');
  for (const orchestration_version of [undefined, 0]) expect(projectView({...state.graph.project, orchestration_version}).readOnly).toBe(true);
});

test('refresh loads materials, rounds and identity, preserving unknown scenario labels in history', async () => {
  const state = snapshot('a', 1); delete state.graph.project.scenario;
  const api = new FixtureAPI({a: state}); api.rounds = [{generation: 0, archived_at: stamp}];
  const resource = new ProjectResource(api), value = await resource.refresh('a');
  expect(value?.selected?.type).toBe(''); expect(value?.selected?.history[0]?.type).toBe('');
  expect(api.calls.map(call => call.path)).toEqual(expect.arrayContaining(['/projects/a/inputs', '/projects/a/rounds?limit=100&cursor=-1', '/projects/a/identity']));
});

test('late obsolete project responses cannot publish even if transport ignores cancellation', async () => {
  const api = new FixtureAPI({a: snapshot(), b: snapshot('b')}), gate = deferred<WorkspaceSnapshot>(), started = deferred<void>();
  api.handler = path => { if (path === '/projects/a/state') { started.resolve(); return gate.promise; } return api.response(path); };
  const resource = new ProjectResource(api), first = resource.refresh('a'); await started.promise;
  await resource.refresh('b'); gate.resolve(snapshot());
  expect(await first).toBeNull(); expect(resource.projects.find(project => project.id === 'b')?.state?.graph.project.id).toBe('b');
});

test('generation fences reject both sides of a restart and preserve the last accepted view', async () => {
  for (const mismatch of ['state', 'identity']) {
    const api = new FixtureAPI(), resource = new ProjectResource(api); await resource.refresh('a'); const accepted = resource.projects;
    api.handler = path => path === '/projects/a/' + mismatch ? mismatch === 'state' ? snapshot('a', 1) : {id: 'a', generation: 1} : api.response(path);
    await expect(resource.refresh('a')).rejects.toMatchObject({status: 409}); expect(resource.projects).toBe(accepted);
  }
  const legacy = snapshot(); delete legacy.graph.project.generation;
  expect((await new ProjectResource(new FixtureAPI({a: legacy})).refresh('a'))?.selected?.generation).toBe(0);
});

test('failed refresh preserves accepted state and terminal views still observe late executions', async () => {
  const state = snapshot(); state.graph.project.status = 'completed';
  const api = new FixtureAPI({a: state}), resource = new ProjectResource(api); await resource.refresh('a');
  api.calls = []; await resource.refresh('a'); expect(api.calls.map(call => call.path)).toEqual(['/projects', '/projects/a/identity']);
  const accepted = resource.projects; api.handler = () => { throw new APIError('offline'); };
  await expect(resource.refresh('a')).rejects.toThrow('offline'); expect(resource.projects).toBe(accepted);
  api.handler = path => api.response(path); api.states.a = {...state, revision: 2};
  api.changes = async () => ({items: [{id: 'late', generation: 0, status: 'cancelled'}], deleted: [], through: 2});
  const updated = (await resource.refresh('a'))?.selected;
  expect(updated?.state?.revision).toBe(2); expect(updated?.runs?.[0]?.status).toBe('cancelled');
});

test('first terminal read reloads state after parallel execution changes and still fences restarts', async () => {
  for (const status of ['completed', 'terminated']) {
    const state = snapshot('a', 0, 6); state.graph.project.status = status;
    const api = new FixtureAPI({a: state});
    api.changes = async () => { api.states.a = {...state, revision: 7}; return {items: [{id: 'late', generation: 0, status: 'cancelled'}], deleted: [], through: 2}; };
    expect((await new ProjectResource(api).refresh('a'))?.selected?.state?.revision).toBe(7);
    const conflictAPI = new FixtureAPI({a: state}); conflictAPI.changes = async () => { conflictAPI.states.a = snapshot('a', 1, 0); return {items: [{id: 'late', generation: 0}], deleted: [], through: 2}; };
    await expect(new ProjectResource(conflictAPI).refresh('a')).rejects.toMatchObject({status: 409});
  }
});

test('active refresh merges changed executions and deletions without reloading stable inputs or rounds', async () => {
  const api = new FixtureAPI(); let turn = 0;
  api.changes = async () => ++turn === 1 ? {items: [{id: 'running', generation: 0, status: 'running'}, {id: 'old', generation: 0}], deleted: [], through: 2}
    : {items: [{id: 'running', generation: 0, status: 'succeeded'}], deleted: ['old'], through: 4};
  const resource = new ProjectResource(api); await resource.refresh('a'); api.calls = [];
  expect((await resource.refresh('a'))?.selected?.runs).toEqual([{id: 'running', generation: 0, status: 'succeeded'}]);
  expect(api.executionCalls.map(call => call.after)).toEqual([0, 2]);
  expect(api.calls.some(call => /\/rounds\?|\/inputs$|\/state\/events\?/.test(call.path))).toBe(false);
});

test('restart and cursor reset discard the old execution cache', async () => {
  const api = new FixtureAPI(), resource = new ProjectResource(api); let reset = false;
  api.changes = async (after, generation) => { if (reset && after) throw new APIError('cursor reset', 409); return {items: [{id: 'run-' + generation, generation}], deleted: [], through: reset ? 1 : 7}; };
  await resource.refresh('a'); reset = true;
  await expect(resource.refresh('a')).rejects.toMatchObject({status: 409}); await resource.refresh('a');
  expect(api.executionCalls.map(call => call.after)).toEqual([0, 7, 0]);
  api.states.a = snapshot('a', 1); expect((await resource.refresh('a'))?.selected?.runs?.map(run => run.id)).toEqual(['run-1']);
  expect(api.executionCalls.at(-1)?.after).toBe(0);
});

test('event projection stops at state revision and old-round executions are excluded', async () => {
  const api = new FixtureAPI({a: snapshot('a', 2, 2)});
  api.changes = async () => ({items: [{id: 'old', generation: 1}, {id: 'current', generation: 2}], deleted: [], through: 2});
  api.handler = path => path.includes('/state/events?') ? [{revision: 2, op: 'event'}, {revision: 3, op: 'event'}] : api.response(path);
  const selected = (await new ProjectResource(api).refresh('a'))?.selected;
  expect(selected?.runs?.map(run => run.id)).toEqual(['current']); expect(selected?.events?.map(event => event.revision)).toEqual([2]);
});

test('terminal refresh resumes bounded event backlog without mutating accepted projections', async () => {
  const state = snapshot('a', 0, 5002); state.graph.project.status = 'completed';
  const api = new FixtureAPI({a: state}), cursors: number[] = [];
  const events = Array.from({length: 5002}, (_, index) => ({revision: index + 1, op: 'checkpoint'}));
  api.handler = path => { if (!path.includes('/state/events?')) return api.response(path); const after = Number(new URL(path, 'http://local').searchParams.get('after')); cursors.push(after); return events.slice(after, after + 1000); };
  const resource = new ProjectResource(api), first = (await resource.refresh('a'))?.selected;
  expect(first?.events).toHaveLength(5000);
  const second = (await resource.refresh('a'))?.selected; expect(second?.events).toHaveLength(5002); expect(first?.events).toHaveLength(5000);
  const third = (await resource.refresh('a'))?.selected; expect(third?.logs).toBe(second?.logs); expect(cursors).toEqual([0, 1000, 2000, 3000, 4000, 5000]);
});

test('failed event catch-up retries accepted cursor; revision rollback starts again at zero', async () => {
  const api = new FixtureAPI(), cursors: number[] = [], resource = new ProjectResource(api); let fail = false;
  const events = Array.from({length: 1002}, (_, index) => ({revision: index + 1, op: 'checkpoint'}));
  api.handler = (path, options) => { if (!path.includes('/state/events?')) return api.response(path); expect(options.signal).toBeInstanceOf(AbortSignal);
    const after = Number(new URL(path, 'http://local').searchParams.get('after')); cursors.push(after);
    if (fail && after === 1001) throw new APIError('event interrupted', 503);
    return events.slice(after, Math.min(after + 1000, api.states.a!.revision)); };
  await resource.refresh('a'); const accepted = resource.projects; api.states.a = snapshot('a', 0, 1002); fail = true;
  await expect(resource.refresh('a')).rejects.toThrow('event interrupted'); expect(resource.projects).toBe(accepted);
  fail = false; expect((await resource.refresh('a'))?.selected?.events).toHaveLength(1002); expect(cursors).toEqual([0, 1, 1001, 1, 1001]);
  api.states.a = snapshot('a', 0, 2); expect((await resource.refresh('a'))?.selected?.events).toHaveLength(2); expect(cursors.at(-1)).toBe(0);
});

test('archive hydration survives concurrent refresh, split UTF-8 and retained manifests', async () => {
  const state = snapshot('a', 0); state.graph.project.title = '中文项目'; state.graph.project.status = 'terminated'; state.graph.project.terminated_at = stamp;
  const bytes = Buffer.from(JSON.stringify(state)), split = bytes.indexOf(Buffer.from('中文')) + 1;
  const files = [{id: 'file', name: 'old.txt', size: 1}], manifest = Buffer.from(JSON.stringify(files));
  const api = new FixtureAPI({a: snapshot('a', 1)}); api.rounds = [{generation: 0, archived_at: '2026-10-10T03:00:00Z'}];
  const gate = deferred<void>(), started = deferred<void>(); let first = true;
  api.handler = async path => {
    if (path.includes('/rounds/0/entries?')) return {items: [{cursor: 1, kind: 'state', bytes: bytes.length}, {cursor: 2, kind: 'inputs', bytes: manifest.length}, {cursor: 3, kind: 'future', bytes: 1}]};
    if (path.includes('/rounds/0/entries/')) {
      if (first) { first = false; started.resolve(); await gate.promise; }
      const data = path.includes('/entries/1?') ? bytes : manifest, offset = Number(new URL(path, 'http://local').searchParams.get('offset'));
      const end = data === bytes && offset === 0 ? split : data.length;
      return {encoding: 'base64', offset, total_bytes: data.length, data: data.subarray(offset, end).toString('base64'), ...(end < data.length ? {next_offset: end} : {})};
    }
    return api.response(path);
  };
  const resource = new ProjectResource(api); await resource.refresh('a'); const loading = resource.archive('a', 0); await started.promise;
  await resource.refresh('a'); gate.resolve(); const archive = await loading;
  expect(archive).toMatchObject({name: '中文项目', archived: true, filesKnown: true, files, endedAt: new Date(stamp).toISOString(), status: 'terminated'});
  expect(resource.projects[0]?.history[0]).toBe(archive); expect(resource.projects[0]?.state?.graph.project.generation).toBe(1);
  await resource.refresh('a'); expect(resource.projects[0]?.history[0]?.files).toEqual(files); expect(resource.projects[0]?.history[0]?.endedAt).toBe(new Date(stamp).toISOString()); expect(resource.projects[0]?.history[0]?.status).toBe('terminated');
  api.calls = []; await resource.archive('a', 0); expect(api.calls).toHaveLength(0);
});

test('legacy archive distinguishes absent and empty manifests', async () => {
  for (const filesKnown of [false, true]) {
    const api = new FixtureAPI({a: snapshot('a', 1)}); api.rounds = [{generation: 0, archived_at: stamp}];
    const stateBytes = Buffer.from(JSON.stringify(snapshot())), empty = Buffer.from('[]');
    api.handler = path => {
      if (path.includes('/rounds/0/entries?')) return {items: [{cursor: 1, kind: 'state', bytes: stateBytes.length}, ...(filesKnown ? [{cursor: 2, kind: 'inputs', bytes: empty.length}] : [])]};
      if (path.includes('/rounds/0/entries/')) { const bytes = path.includes('/entries/1?') ? stateBytes : empty; return {encoding: 'base64', offset: 0, total_bytes: bytes.length, data: bytes.toString('base64')}; }
      return api.response(path);
    };
    const resource = new ProjectResource(api); await resource.refresh('a'); expect((await resource.archive('a', 0)).filesKnown).toBe(filesKnown);
  }
});

test.each(['a', 'b'])('archive hydration published during refresh of %s survives that refresh', async preferred => {
  const archivedState = snapshot('a'), bytes = Buffer.from(JSON.stringify(archivedState));
  const api = new FixtureAPI({a: snapshot('a', 1), b: snapshot('b', 1)}); api.rounds = [{generation: 0, archived_at: stamp}];
  const resource = new ProjectResource(api); await resource.refresh('a');
  const accepted = resource.projects, started = deferred<void>(), gate = deferred<void>();
  api.handler = async path => {
    if (path === '/projects/' + preferred + '/state') { started.resolve(); await gate.promise; }
    if (path.includes('/rounds/0/entries?')) return {items: [{cursor: 1, kind: 'state', bytes: bytes.length}]};
    if (path.includes('/rounds/0/entries/1?')) return {encoding: 'base64', offset: 0, total_bytes: bytes.length, data: bytes.toString('base64')};
    return api.response(path);
  };
  const refreshing = resource.refresh(preferred); await started.promise;
  const archive = await resource.archive('a', 0);
  expect(resource.projects.find(project => project.id === 'a')?.history[0]).toBe(archive);
  gate.resolve(); await refreshing;
  expect(resource.projects.find(project => project.id === 'a')?.history[0]?.state).toEqual(archivedState);
  expect(accepted.find(project => project.id === 'a')?.history[0]?.state).toBeNull();
});
