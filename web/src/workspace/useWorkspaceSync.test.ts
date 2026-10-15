// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { defineComponent, h, nextTick, ref } from 'vue';
import { APIError, Client } from '../api/client';
import type { RequestOptions } from '../api/client';
import type { WorkspaceSnapshot } from '../api/types';
import { ProjectResource } from './project-resource';
import { useWorkspaceSync } from './useWorkspaceSync';

function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done; }); return {promise, resolve}; }
function snapshot(id: string): WorkspaceSnapshot { return {graph: {project: {id, title: id, status: 'active', orchestration_version: 1, generation: 0}}, goals: [], steps: [], fact_records: [], findings: [], revision: 1}; }
class API extends Client {
  lists = 0;
  fail = false;
  state = async (id: string, _options: RequestOptions): Promise<WorkspaceSnapshot> => snapshot(id);
  override async request<T = unknown>(path: string, options: RequestOptions = {}): Promise<T> {
    let value: unknown;
    if (path === '/projects') { this.lists++; if (this.fail) throw new APIError('offline'); value = ['a', 'b'].map(id => snapshot(id).graph.project); }
    else {
      const [, id, suffix] = path.match(/^\/projects\/([^/]+)(.*)$/) || [];
      if (suffix === '/state') value = await this.state(id!, options);
      else if (suffix === '/identity') value = {id, generation: 0};
      else if (suffix?.startsWith('/rounds?')) value = {items: []};
      else value = [];
    }
    return value as T;
  }
  override async executionChanges() { return {items: [], deleted: [], through: 0}; }
}
const cleanup: (() => void)[] = [];
function mountSync(api = new API()) {
  const id = ref('a'), toast = vi.fn(), resource = new ProjectResource(api);
  let sync!: ReturnType<typeof useWorkspaceSync>;
  const wrapper = mount(defineComponent({setup() { sync = useWorkspaceSync(id, toast, resource); return () => h('div'); }}));
  cleanup.push(() => wrapper.unmount());
  return {api, resource, id, toast, sync, wrapper};
}
beforeEach(() => { vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout']}); vi.spyOn(document, 'hidden', 'get').mockReturnValue(false); });
afterEach(() => { for (const dispose of cleanup.splice(0)) dispose(); vi.useRealTimers(); vi.restoreAllMocks(); });

test('mutation waits for reconciliation of the newest route without waiting for an obsolete read', async () => {
  const timeouts = vi.spyOn(globalThis, 'setTimeout');
  const api = new API(), gate = deferred<WorkspaceSnapshot>(), started = deferred<void>();
  api.state = async id => { if (id === 'a') { started.resolve(); return gate.promise; } return snapshot(id); };
  const {sync, id} = mountSync(api); await started.promise;
  try {
    await sync.mutate(async () => { id.value = 'b'; await nextTick(); });
    expect(sync.current.value?.id).toBe('b'); expect(sync.current.value?.state?.graph.project.id).toBe('b'); expect(sync.connected.value).toBe(true);
    gate.resolve(snapshot('a')); await flushPromises();
    expect(sync.current.value?.state?.graph.project.id).toBe('b'); expect(timeouts.mock.calls.filter(call => call[1] === 2500)).toHaveLength(1);
  } finally { gate.resolve(snapshot('a')); }
});

test('route changes supersede ignored aborts and repeated same-route requests share one refresh', async () => {
  const api = new API(), gate = deferred<WorkspaceSnapshot>(), started = deferred<void>();
  api.state = async id => { if (id === 'a') { started.resolve(); return gate.promise; } return snapshot(id); };
  const {sync, id, toast} = mountSync(api); await started.promise;
  try {
    id.value = 'b'; await nextTick(); const first = sync.refresh(), second = sync.refresh(); await Promise.all([first, second]);
    expect(sync.current.value?.state?.graph.project.id).toBe('b'); expect(api.lists).toBe(2);
    gate.resolve(snapshot('a')); await flushPromises(); expect(toast).not.toHaveBeenCalled(); expect(vi.getTimerCount()).toBe(1);
    await vi.advanceTimersByTimeAsync(2500); await flushPromises(); expect(api.lists).toBe(3); expect(vi.getTimerCount()).toBe(1);
  } finally { gate.resolve(snapshot('a')); }
});

test('failed mutation still refreshes accepted data, keeps exclusion and schedules one loop', async () => {
  const {sync, api} = mountSync(); await flushPromises(); const gate = deferred<void>();
  const pending = sync.mutate(async () => { await gate.promise; throw new Error('command rejected'); });
  await expect(sync.mutate(async () => undefined)).rejects.toThrow('正在处理上一项操作'); expect(sync.mutating.value).toBe(true);
  const assertion = expect(pending).rejects.toThrow('command rejected'); gate.resolve(); await assertion;
  expect(sync.mutating.value).toBe(false); expect(api.lists).toBe(2); expect(vi.getTimerCount()).toBe(1);
});

test('mutation reconciliation follows a route that changes while its refresh is pending', async () => {
  const {sync, api, id} = mountSync(); await flushPromises();
  const old = deferred<WorkspaceSnapshot>(), latest = deferred<WorkspaceSnapshot>(), started = deferred<void>();
  api.state = async key => { if (key === 'a') { started.resolve(); return old.promise; } return latest.promise; };
  let finished = false;
  const command = sync.mutate(async () => undefined).then(() => { finished = true; }); await started.promise;
  try {
    id.value = 'b'; await nextTick(); old.resolve(snapshot('a')); await flushPromises();
    expect(finished).toBe(false);
    latest.resolve(snapshot('b')); await command; expect(sync.current.value?.state?.graph.project.id).toBe('b');
  } finally { old.resolve(snapshot('a')); latest.resolve(snapshot('b')); await command; }
});

test('failure retains accepted data and visibility transitions keep only one refresh timer', async () => {
  const {sync, api, toast} = mountSync(); await flushPromises(); const accepted = sync.projects.value;
  api.fail = true; await sync.refresh(); expect(sync.projects.value).toBe(accepted); expect(sync.connected.value).toBe(false); expect(toast).toHaveBeenCalledWith('offline');
  api.fail = false; vi.spyOn(document, 'hidden', 'get').mockReturnValue(true); await vi.advanceTimersByTimeAsync(2500); await flushPromises();
  expect(sync.connected.value).toBe(true); const count = api.lists; await vi.advanceTimersByTimeAsync(9999); expect(api.lists).toBe(count);
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false); document.dispatchEvent(new Event('visibilitychange')); await flushPromises();
  expect(api.lists).toBe(count + 1); expect(vi.getTimerCount()).toBe(1);
});

test('unmount and pagehide suppress late publication and restore from bfcache without duplicate loops', async () => {
  const {sync, api, wrapper} = mountSync(); await flushPromises(); const count = api.lists;
  window.dispatchEvent(new PageTransitionEvent('pagehide')); expect(vi.getTimerCount()).toBe(0);
  await vi.advanceTimersByTimeAsync(10000); expect(api.lists).toBe(count);
  const show = new Event('pageshow'); Object.defineProperty(show, 'persisted', {value: true});
  window.dispatchEvent(show); await flushPromises(); expect(api.lists).toBe(count + 1); expect(vi.getTimerCount()).toBe(1);
  const gate = deferred<WorkspaceSnapshot>(); api.state = () => gate.promise; const pending = sync.refresh(); await flushPromises(); const accepted = sync.projects.value;
  wrapper.unmount(); gate.resolve(snapshot('a')); await pending;
  expect(sync.projects.value).toBe(accepted); expect(vi.getTimerCount()).toBe(0);
});
