// @vitest-environment happy-dom
import { expect, test, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { defineComponent, h } from 'vue';
import WorkspacePage from './WorkspacePage.vue';
import MaterialsView from './materials/MaterialsView.vue';
import { snapshotView } from './project-resource';
import type { ProjectView } from './model/types';
import * as preview from './model/input-preview';

const graphStub = defineComponent({name: 'GraphCanvas', setup(_, {expose}) { expose({clearSelection() {}, setStatusFilter() {}}); return () => h('div', {class: 'graph-stub'}); }});
const readerStub = defineComponent({name: 'RecordReader', setup(_, {expose}) { expose({close() {}, open() {}}); return () => h('div'); }});
function project(id = 'project', generation = 1): ProjectView {
  const view = snapshotView({graph: {project: {id, title: 'Current', status: 'active', generation, orchestration_version: 1}}, revision: 1, goals: [], steps: [], fact_records: [], findings: []});
  view.history = [{...snapshotView({graph: {project: {id, title: 'Historical round', status: 'completed', generation: 0, orchestration_version: 1}}, revision: 2, goals: [], steps: [], fact_records: [], findings: []}), archived: true, archivedAt: '2026-10-10T00:00:00Z'}];
  return view;
}

test('same project and generation refresh preserves the selected tab and historical round', async () => {
  const current = project(), loadRound = vi.fn(async () => current.history[0]);
  const wrapper = mount(WorkspacePage, {props: {project: current, disabled: false, modelSummary: '', loadRound}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: true, ResultsView: true}}});
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

test('changing generation or project resets local workspace navigation', async () => {
  const wrapper = mount(WorkspacePage, {props: {project: project(), disabled: false, modelSummary: '', loadRound: async () => undefined}, global: {stubs: {GraphCanvas: graphStub, RecordReader: readerStub, ActivityPanel: true, MaterialsView: true, ResultsView: true}}});
  for (const next of [project('project', 2), project('other', 2)]) {
    await wrapper.get('#view-tab-result').trigger('click'); await wrapper.get('#run-select').setValue('0'); await flushPromises();
    await wrapper.setProps({project: next});
    expect(wrapper.get('#view-tab-graph').attributes('aria-selected')).toBe('true'); expect(wrapper.get<HTMLSelectElement>('#run-select').element.value).toBe('current');
  }
  wrapper.unmount();
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
