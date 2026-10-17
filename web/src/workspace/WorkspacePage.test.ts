// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { defineComponent, h } from 'vue';
import WorkspacePage from './WorkspacePage.vue';
import GraphCanvas from './graph/GraphCanvas.vue';
import MaterialsView from './materials/MaterialsView.vue';
import ResultsView from './results/ResultsView.vue';
import ActivityPanel from './activity/ActivityPanel.vue';
import { projectView, snapshotView } from './project-resource';
import type { ProjectView } from './model/types';
import * as preview from './model/input-preview';

// Assert navigation semantics without animation delays; browser tests sample real frames.
beforeEach(() => {
  const preference = window.matchMedia('(prefers-reduced-motion: reduce)');
  Object.defineProperty(preference, 'matches', {value: true});
  vi.spyOn(window, 'matchMedia').mockReturnValue(preference);
});
afterEach(() => vi.restoreAllMocks());

const graphStub = defineComponent({name: 'GraphCanvas', props: ['state'], setup(_, {expose}) { expose({clearSelection() {}, setStatusFilter() {}}); return () => h('div', {class: 'graph-stub'}); }});
const readerStub = defineComponent({name: 'RecordReader', setup(_, {expose}) { expose({close() {}, open() {}}); return () => h('div'); }});
const materialsStub = defineComponent({name: 'MaterialsView', setup(_, {expose}) { expose({cancel() {}}); return () => h('div'); }});
function project(id = 'project', generation = 1): ProjectView {
  const view = snapshotView({graph: {project: {id, title: 'Current', status: 'active', generation, orchestration_version: 1}}, revision: 1, goals: [], steps: [], fact_records: [], findings: []});
  view.history = [{...snapshotView({graph: {project: {id, title: 'Historical round', status: 'completed', generation: 0, orchestration_version: 1}}, revision: 2, goals: [], steps: [], fact_records: [], findings: []}), archived: true, archivedAt: '2026-10-10T00:00:00Z'}];
  return view;
}

test('server review data keeps three workspace views and keyboard navigation cycles through them', async () => {
  const current = project();
  Object.assign(current.state!, {
    candidates: [{id: 'note', claim: 'Retained judgment', status: 'verified', sources: ['fact']}],
    disputes: [{id: 'review', finding_id: 'finding', candidate_ids: ['note'], status: 'open', question: 'Check evidence', reason: 'Conflicting observations', review_step_ids: [], review_fact_ids: []}],
    curation: {generation: current.generation, through_revision: 1, request: {sources: ['fact'], reason: 'Review retained evidence', revision: 1}},
    findings: [{id: 'finding', claim: 'Retained finding', candidate_ids: ['note'], dispute_id: 'review', curated_revision: 1}],
  });
  const wrapper = mount(WorkspacePage, {attachTo: document.body, props: {project: current, disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    expect(wrapper.findAll('[role="tab"]').map(tab => tab.attributes('data-view'))).toEqual(['graph', 'materials', 'result']);
    for (const [id, component] of [['materials', MaterialsView], ['result', ResultsView], ['graph', GraphCanvas]] as const) {
      await wrapper.get('#view-tab-' + id).trigger('click');
      expect(wrapper.findComponent(component).exists()).toBe(true);
      expect(wrapper.get('#project-view').attributes('aria-labelledby')).toBe('view-tab-' + id);
    }
    for (const [key, id] of [['ArrowRight', 'materials'], ['ArrowRight', 'result'], ['ArrowRight', 'graph'], ['ArrowLeft', 'result'], ['Home', 'graph'], ['End', 'result']]) {
      await wrapper.get('[role="tab"][aria-selected="true"]').trigger('keydown', {key});
      await flushPromises();
      const tab = wrapper.get('#view-tab-' + id);
      expect(tab.attributes('aria-selected')).toBe('true');
      expect(document.activeElement).toBe(tab.element);
    }
  } finally { wrapper.unmount(); }
});

test('canvas Fact details retain derivation and discovery provenance without manufacturing evidence', async () => {
  const current = project(), state = current.state!;
  const fact = {id: 'derived', description: 'Computed observation', status: 'valid', derived_from: ['raw'], support_invalid: true};
  state.fact_records = [fact];
  state.assets = [{id: 'asset', kind: 'endpoint', value: 'https://example.test/a', method: 'GET'}];
  state.asset_anchors = [{asset_id: 'asset', node_kind: 'fact', node_id: 'derived', discovery_step_id: 'work'}];
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    wrapper.findComponent(GraphCanvas).vm.$emit('select-node', {id: fact.id, key: 'fact:derived', type: 'fact', label: fact.description, status: fact.status, raw: fact});
    await flushPromises();
    const detail = wrapper.findComponent(ActivityPanel).props('selection')!;
    expect(detail.meta).toContainEqual({label: 'Derived from', value: 'raw'});
    expect(detail.meta).toContainEqual({label: 'Asset associations', value: 'GET https://example.test/a (Discovery via Step work)'});
    expect(detail.evidence).toEqual([]);
  } finally { wrapper.unmount(); }
});

test('same project and generation refresh preserves the selected tab and historical round', async () => {
  const current = project(), loadRound = vi.fn(async () => current.history[0]);
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  await wrapper.get('#view-tab-materials').trigger('click');
  await wrapper.get('#run-select').setValue('0'); await flushPromises();
  expect(wrapper.get('#view-tab-materials').attributes('aria-selected')).toBe('true');
  expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('0');
  expect(wrapper.get('h1').text()).toBe('Historical round');
  await wrapper.setProps({project: {...structuredClone(current), name: 'Refreshed current title'}});
  expect(wrapper.get('#view-tab-materials').attributes('aria-selected')).toBe('true');
  expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('0');
  expect(wrapper.get('h1').text()).toBe('Historical round'); expect(loadRound).toHaveBeenCalledOnce();
  wrapper.unmount();
});

test.each(['round', 'reset'])('a %s change cancels node location pending behind a view transition', async change => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout']});
  vi.spyOn(window, 'matchMedia').mockReturnValue({matches: false, addEventListener() {}, removeEventListener() {}} as unknown as MediaQueryList);
  const selectNode = vi.fn(), focusNode = vi.fn();
  const graph = defineComponent({setup(_, {expose}) {expose({selectNode, focusNode, clearSelection() {}, setStatusFilter() {}}); return () => h('div');}});
  const current = project(), wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => current.history[0]}, global: {stubs: {GraphCanvas: graph, RecordReader: readerStub, ActivityPanel: true, ResultsView: true}}});
  try {
    await wrapper.get('#view-tab-result').trigger('click');
    await vi.advanceTimersByTimeAsync(600); await flushPromises();
    wrapper.findComponent(ResultsView).vm.$emit('locate', 'finding:obsolete');
    await wrapper.vm.$nextTick();
    expect(wrapper.get<HTMLButtonElement>('[data-filter="done"]').element.disabled).toBe(true);
    if (change === 'round') await wrapper.get('#run-select').setValue('0');
    else wrapper.vm.reset();
    await vi.advanceTimersByTimeAsync(600); await flushPromises();
    expect(wrapper.get('#view-tab-graph').attributes('aria-selected')).toBe('true');
    expect(selectNode).not.toHaveBeenCalled(); expect(focusNode).not.toHaveBeenCalled();
    expect(wrapper.get<HTMLElement>('#project-view').element.inert).toBe(false);
    expect(wrapper.get<HTMLButtonElement>('[data-filter="done"]').element.disabled).toBe(false);
  } finally {wrapper.unmount(); vi.useRealTimers();}
});

test('project transitions retain outgoing state, block stale actions, and accept only the latest selection', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout']});
  vi.spyOn(window, 'matchMedia').mockReturnValue({matches: false, addEventListener() {}, removeEventListener() {}} as unknown as MediaQueryList);
  const first = {...project('first'), name: 'First project'}, second = {...project('second'), name: 'Second project'}, latest = {...project('latest'), name: 'Latest project'};
  const wrapper = mount(WorkspacePage, {props: {project: first, disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    const pane = wrapper.get<HTMLElement>('.project-content').element, graph = wrapper.getComponent(GraphCanvas).element;
    await wrapper.setProps({project: second});
    expect(pane.dataset.viewMotion).toBe('leave'); expect(pane.inert).toBe(false);
    expect(wrapper.get<HTMLElement>('#project-view').element.inert).toBe(true);
    expect(wrapper.get<HTMLElement>('.canvas-footer').element.inert).toBe(true);
    expect(wrapper.getComponent(ActivityPanel).props('blocked')).toBe(true);
    expect(wrapper.get<HTMLButtonElement>('#project-manage').element.disabled).toBe(true);
    expect(wrapper.get('h1').text()).toBe('First project');
    expect(wrapper.getComponent(GraphCanvas).props('state')).toEqual(first.state);
    await wrapper.get('#project-manage').trigger('click'); await wrapper.get('#add-hint').trigger('click');
    expect(wrapper.emitted('manage')).toBeUndefined(); expect(wrapper.emitted('hint')).toBeUndefined();
    await vi.advanceTimersByTimeAsync(30); await wrapper.setProps({project: latest});
    expect(wrapper.get('h1').text()).toBe('First project');
    await vi.advanceTimersByTimeAsync(600); await flushPromises();
    expect(wrapper.get('h1').text()).toBe('Latest project'); expect(wrapper.getComponent(GraphCanvas).element).toBe(graph);
    expect(pane.inert).toBe(false); expect(pane.dataset.viewMotion).toBeUndefined();
    expect(wrapper.get<HTMLElement>('#project-view').element.inert).toBe(false);
    expect(wrapper.getComponent(ActivityPanel).props('blocked')).toBe(false);
    await wrapper.get('#add-hint').trigger('click'); expect(wrapper.emitted('hint')).toEqual([['latest']]);
    await wrapper.setProps({project: {...latest, name: 'Latest refreshed'}});
    expect(wrapper.get('h1').text()).toBe('Latest refreshed'); expect(pane.dataset.viewMotion).toBeUndefined();
  } finally {wrapper.unmount(); vi.useRealTimers();}
});

test('round transitions retain outgoing graph while the selector follows accepted requests and can reverse rapidly', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout']});
  vi.spyOn(window, 'matchMedia').mockReturnValue({matches: false, addEventListener() {}, removeEventListener() {}} as unknown as MediaQueryList);
  const current = project(), wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => current.history[0]}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    const pane = wrapper.get<HTMLElement>('.project-content').element;
    await wrapper.get('#run-select').setValue('0'); await flushPromises();
    expect(wrapper.get('h1').text()).toBe('Current'); expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('0');
    expect(pane.dataset.viewMotion).toBe('leave');
    expect(wrapper.get<HTMLElement>('#project-view').element.inert).toBe(true);
    expect(wrapper.get('#run-select').element.closest('[inert]')).toBeNull();
    await vi.advanceTimersByTimeAsync(30); await wrapper.get('#run-select').setValue('current'); await flushPromises();
    await vi.advanceTimersByTimeAsync(600); await flushPromises();
    expect(wrapper.get('h1').text()).toBe('Current'); expect(pane.inert).toBe(false);
    await wrapper.get('#run-select').setValue('0'); await flushPromises(); await vi.advanceTimersByTimeAsync(600); await flushPromises();
    expect(wrapper.get('h1').text()).toBe('Historical round'); expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('0');
    expect(pane.dataset.viewMotion).toBeUndefined();
  } finally {wrapper.unmount(); vi.useRealTimers();}
});

test('changing generation or project resets local workspace navigation', async () => {
  const wrapper = mount(WorkspacePage, {props: {project: project(), disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  for (const next of [project('project', 2), project('other', 2)]) {
    await wrapper.get('#view-tab-result').trigger('click'); await wrapper.get('#run-select').setValue('0'); await flushPromises();
    await wrapper.setProps({project: next});
    expect(wrapper.get('#view-tab-graph').attributes('aria-selected')).toBe('true'); expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('current');
  }
  wrapper.unmount();
});

test('round selection keeps the displayed round while loading and after rejection', async () => {
  let reject: (error: Error) => void = () => {};
  const loadRound = () => new Promise<unknown>((_resolve, fail) => { reject = fail; });
  const wrapper = mount(WorkspacePage, {props: {project: project(), disabled: false, modelSummary: '', loadRound}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    await wrapper.get('#run-select').setValue('0');
    expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('current');
    expect(wrapper.get('h1').text()).toBe('Current');
    reject(new Error('Archive unavailable')); await flushPromises();
    expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('current');
    expect(wrapper.get('h1').text()).toBe('Current');
    expect(wrapper.emitted('toast')).toEqual([['Archive unavailable']]);
  } finally { wrapper.unmount(); }
});

test('superseded archive loads cannot replace the latest accepted round', async () => {
  const current = project('project', 2);
  current.history.push({...current.history[0]!, generation: 1, name: 'Latest archive'});
  const pending = new Map<number, () => void>();
  const loadRound = (_project: ProjectView, generation: number) => new Promise<void>(resolve => pending.set(generation, resolve));
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    await wrapper.get('#run-select').setValue('0');
    await wrapper.get('#run-select').setValue('1');
    expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('current');
    pending.get(1)?.(); await flushPromises();
    expect(wrapper.get('h1').text()).toBe('Latest archive');
    pending.get(0)?.(); await flushPromises();
    expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('1');
    expect(wrapper.get('h1').text()).toBe('Latest archive');
    await wrapper.get('#return-current').trigger('click');
    expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('current');
    expect(wrapper.get('h1').text()).toBe('Current');
  } finally { wrapper.unmount(); }
});

test('round changes keep status buttons and the canvas filter in agreement', async () => {
  const current = project(), wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => current.history[0]}, global: {stubs: {RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    for (const next of ['0', 'current']) {
      await wrapper.get('[data-filter="done"]').trigger('click');
      expect(wrapper.findComponent(GraphCanvas).vm.getStatusFilter()).toBe('done');
      await wrapper.get('#run-select').setValue(next); await flushPromises();
      expect(wrapper.findComponent(GraphCanvas).vm.getStatusFilter()).toBe('all');
      expect(wrapper.get('[data-filter="done"]').attributes('aria-pressed')).toBe('false');
    }
  } finally { wrapper.unmount(); }
});

test('summary-only historical projects explain unavailable details in every tab', async () => {
  const current = projectView({id: 'historical', title: 'Historical project', status: 'completed', orchestration_version: 0});
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    for (const tab of ['graph', 'materials', 'result']) {
      await wrapper.get('#view-tab-' + tab).trigger('click');
      expect(wrapper.get('#project-view').text()).toContain('此历史项目仅提供基本信息，详情暂不可查看。请新建项目继续。');
      expect(wrapper.findComponent(GraphCanvas).exists()).toBe(false);
      expect(wrapper.findComponent(MaterialsView).exists()).toBe(false);
      expect(wrapper.findComponent(ResultsView).exists()).toBe(false);
      expect(wrapper.find('[data-filter]').exists()).toBe(false);
      expect(wrapper.find('.task-progress').exists()).toBe(false);
    }
  } finally { wrapper.unmount(); }
});

test('a summary-only refresh clears stale selection and closes its record reader', async () => {
  const current = project(), close = vi.fn(), open = vi.fn();
  const reader = defineComponent({setup(_, {expose}) { expose({close, open}); return () => h('div'); }});
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: reader, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    await wrapper.get('#run-select').setValue('0'); await flushPromises();
    wrapper.findComponent(GraphCanvas).vm.$emit('select-node', {key: 'fact:record', type: 'fact', label: 'Saved detail', status: 'valid', raw: {}});
    await flushPromises();
    const activity = wrapper.findComponent(ActivityPanel);
    const selected = activity.props('selection');
    expect(selected).toMatchObject({title: 'Saved detail'});
    activity.vm.$emit('read', selected); await flushPromises(); expect(open).toHaveBeenCalledWith(selected);
    close.mockClear();
    await wrapper.setProps({project: projectView({...current.raw, orchestration_version: 0})});
    expect(activity.props('selection')).toBeNull();
    expect(close).toHaveBeenCalledOnce();
    expect(wrapper.find('#return-current').exists()).toBe(false);
  } finally { wrapper.unmount(); }
});

test('removing a project prunes all its round layouts while retaining other cameras', async () => {
  const graph = defineComponent({props: ['projectLayouts'], setup(_, {expose}) { expose({clearSelection() {}}); return () => h('div'); }});
  const wrapper = mount(WorkspacePage, {props: {project: project('b'), projectIds: ['a', 'b'], disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graph, RecordReader: readerStub, ActivityPanel: true, MaterialsView: materialsStub, ResultsView: true}}});
  try {
    const layouts = wrapper.findComponent(graph).props('projectLayouts');
    const retained = {projectId: 'b', generation: 0, positions: new Map(), camera: {scale: 1.5, tx: 120, ty: -40, width: 800, height: 600}};
    layouts.set('a:0', {...retained, projectId: 'a'}); layouts.set('a:1', {...retained, projectId: 'a', generation: 1}); layouts.set('b:0', retained);
    await wrapper.setProps({projectIds: ['b']});
    expect([...layouts.keys()]).toEqual(['b:0']); expect(layouts.get('b:0')).toBe(retained);
  } finally { wrapper.unmount(); }
});

test('ordinary refresh does not cancel an in-flight material preview, while identity changes do', async () => {
  const current = project(); current.files = [{id: 'material', name: 'request.http', size: 10}];
  const reads: {signal?: AbortSignal; resolve: (body: string) => void}[] = [];
  vi.spyOn(preview, 'read').mockImplementation((_url, options) => new Promise<string>(resolve => reads.push({signal: options?.signal, resolve})));
  const wrapper = mount(MaterialsView, {props: {project: current, active: true}, global: {stubs: {ContentCard: true}}});
  await wrapper.get('.material-row').trigger('click'); await wrapper.setProps({project: {...current, name: 'Refreshed'}});
  expect(reads[0]?.signal?.aborted).toBe(false); reads[0]?.resolve('retained response'); await flushPromises();
  expect(wrapper.emitted('read')?.[0]?.[0]).toMatchObject({body: 'retained response'});
  await wrapper.get('.material-row').trigger('click'); await wrapper.setProps({project: {...current, generation: 2}});
  expect(reads[1]?.signal?.aborted).toBe(true); reads[1]?.resolve('obsolete response'); await flushPromises();
  expect(wrapper.emitted('read')).toHaveLength(1); wrapper.unmount();
});

test.each(['input', 'activity'])('opening %s supersedes an in-flight material preview', async source => {
  const current = project(); current.target = 'Newer input'; current.files = [{id: 'material', name: 'request.http', size: 10}];
  const opened = vi.fn(), reads: {signal?: AbortSignal; resolve: (body: string) => void}[] = [];
  vi.spyOn(preview, 'read').mockImplementation((_url, options) => new Promise<string>(resolve => reads.push({signal: options?.signal, resolve})));
  const reader = defineComponent({setup(_, {expose}) { expose({close() {}, open: opened}); return () => h('div'); }});
  const activity = defineComponent({emits: ['read'], setup(_, {emit}) { return () => h('button', {class: 'read-activity', onClick: () => emit('read', {kind: 'Activity', title: 'Newer activity', body: 'Newer activity', meta: []})}); }});
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: reader, ActivityPanel: activity, ResultsView: true}}});
  await wrapper.get('#view-tab-materials').trigger('click');
  await wrapper.get('.material-row').trigger('click');
  await wrapper.get(source === 'input' ? '[data-content-read="input:target"]' : '.read-activity').trigger('click');
  expect(opened).toHaveBeenCalledOnce(); expect(opened).toHaveBeenLastCalledWith(expect.objectContaining({body: source === 'input' ? 'Newer input' : 'Newer activity'}));
  expect(reads[0]?.signal?.aborted).toBe(true);
  reads[0]?.resolve('Obsolete material'); await flushPromises();
  expect(opened).toHaveBeenCalledOnce(); wrapper.unmount();
});
