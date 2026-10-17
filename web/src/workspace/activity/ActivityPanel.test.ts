// @vitest-environment happy-dom
import { expect, test, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import ActivityPanel from './ActivityPanel.vue';
import LogEntry from './LogEntry.vue';
import { snapshotView } from '../project-resource';
import SelectionInspector from '../details/SelectionInspector.vue';
import type { DetailRecord } from '../details/detail';

function project(id = 'first', generation = 0, text = 'original') {
  const value = snapshotView({graph: {project: {id, title: id, status: 'active', generation, orchestration_version: 1}}, revision: 1, goals: [], steps: [], fact_records: [], findings: []});
  value.logs = [{id: 'entry', phase: 'WORKSPACE', level: 'info', worker: '', title: `${id} round ${generation} log`, body: text, time: '2026-10-15T00:00:00Z', source: 'state'}];
  return value;
}

test.each([project('second'), project('first', 1)])('project and round switches restore independent log reading state: $id:$generation', async next => {
  const original = project();
  const wrapper = mount(ActivityPanel, {props: {project: original, projectIds: ['first', 'second']}, global: {stubs: {LogEntry: true}}});
  await wrapper.get('#activity-search').setValue('log');
  wrapper.getComponent(LogEntry).vm.$emit('expand', 'entry', true);
  const host = wrapper.get<HTMLElement>('#activity-content'); host.element.scrollTop = 350; await host.trigger('scroll');
  await wrapper.setProps({project: next}); await flushPromises();
  expect(wrapper.get<HTMLInputElement>('#activity-search').element.value).toBe('');
  expect(wrapper.getComponent(LogEntry).props('expanded')).toBe(false);
  expect(host.element.scrollTop).toBe(0);
  await wrapper.setProps({project: original}); await flushPromises();
  expect(wrapper.get<HTMLInputElement>('#activity-search').element.value).toBe('log');
  expect(wrapper.getComponent(LogEntry).props('expanded')).toBe(true);
  expect(host.element.scrollTop).toBe(350);
  wrapper.unmount();
});

test('unchanged refresh retains position while new log content returns to the latest entry', async () => {
  const wrapper = mount(ActivityPanel, {props: {project: project()}, global: {stubs: {LogEntry: true}}});
  const host = wrapper.get<HTMLElement>('#activity-content'); host.element.scrollTop = 250; await host.trigger('scroll');
  await wrapper.setProps({project: project()}); await flushPromises();
  expect(host.element.scrollTop).toBe(250);
  await wrapper.setProps({project: project('first', 0, 'updated')}); await flushPromises();
  expect(host.element.scrollTop).toBe(0);
  wrapper.unmount();
});

test('returning to changed logs keeps search and expansion but resets the saved position', async () => {
  const wrapper = mount(ActivityPanel, {props: {project: project()}, global: {stubs: {LogEntry: true}}});
  await wrapper.get('#activity-search').setValue('log');
  wrapper.getComponent(LogEntry).vm.$emit('expand', 'entry', true);
  const host = wrapper.get<HTMLElement>('#activity-content'); host.element.scrollTop = 350; await host.trigger('scroll');
  await wrapper.setProps({project: project('second')}); await flushPromises();
  await wrapper.setProps({project: project('first', 0, 'updated while away')}); await flushPromises();
  expect(wrapper.get<HTMLInputElement>('#activity-search').element.value).toBe('log');
  expect(wrapper.getComponent(LogEntry).props('expanded')).toBe(true);
  expect(host.element.scrollTop).toBe(0);
  wrapper.unmount();
});

const selection = (key: string, body = 'Selected content'): DetailRecord => ({nodeKey: key, title: key, kind: 'Node', body, meta: []});
const settleMotion = async () => {await vi.advanceTimersByTimeAsync(600); await flushPromises();};

test('activity retains outgoing content until the fade boundary and restores the log position', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout']});
  const wrapper = mount(ActivityPanel, {props: {project: project()}, global: {stubs: {LogEntry: true}}});
  try {
    const pane = wrapper.get<HTMLElement>('.activity-pane').element, host = wrapper.get<HTMLElement>('#activity-content').element;
    host.scrollTop = 320; await wrapper.get('#activity-content').trigger('scroll');
    await wrapper.setProps({selection: selection('fact:first')});
    expect(pane.dataset.viewMotion).toBe('leave'); expect(pane.inert).toBe(true);
    expect(wrapper.find('.log-stream').exists()).toBe(true); expect(wrapper.findComponent(SelectionInspector).exists()).toBe(false);
    await settleMotion();
    expect(wrapper.getComponent(SelectionInspector).props('record').nodeKey).toBe('fact:first');
    expect(host.scrollTop).toBe(0); expect(pane.inert).toBe(false);
    await wrapper.setProps({selection: null});
    expect(wrapper.findComponent(SelectionInspector).exists()).toBe(true);
    await settleMotion();
    expect(wrapper.find('.log-stream').exists()).toBe(true); expect(host.scrollTop).toBe(320);
    expect(pane.dataset.viewMotion).toBeUndefined(); expect(pane.inert).toBe(false);
  } finally {wrapper.unmount(); vi.useRealTimers();}
});

test('rapid activity selections settle on the latest record and polling does not restart motion', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout']});
  const wrapper = mount(ActivityPanel, {props: {project: project()}, global: {stubs: {LogEntry: true}}});
  try {
    const pane = wrapper.get<HTMLElement>('.activity-pane').element, host = wrapper.get<HTMLElement>('#activity-content').element;
    await wrapper.setProps({selection: selection('fact:first')}); await vi.advanceTimersByTimeAsync(30);
    await wrapper.setProps({selection: null}); await settleMotion();
    expect(wrapper.find('.log-stream').exists()).toBe(true); expect(pane.inert).toBe(false);
    await wrapper.setProps({selection: selection('fact:first')}); await settleMotion();
    await wrapper.setProps({selection: selection('fact:second')}); await vi.advanceTimersByTimeAsync(30);
    await wrapper.setProps({selection: selection('fact:first')}); await vi.advanceTimersByTimeAsync(30);
    await wrapper.setProps({selection: selection('fact:latest')}); await settleMotion();
    expect(wrapper.getComponent(SelectionInspector).props('record').nodeKey).toBe('fact:latest');
    expect(pane.dataset.viewMotion).toBeUndefined(); expect(pane.inert).toBe(false);
    host.scrollTop = 180;
    await wrapper.setProps({selection: selection('fact:latest', 'Updated live content')}); await flushPromises();
    expect(wrapper.getComponent(SelectionInspector).props('record').body).toBe('Updated live content');
    expect(pane.dataset.viewMotion).toBeUndefined(); expect(host.scrollTop).toBe(180);
  } finally {wrapper.unmount(); vi.useRealTimers();}
});

test('stable edge identity updates in place and reduced motion switches activity immediately', async () => {
  vi.spyOn(window, 'matchMedia').mockReturnValue({matches: true, addEventListener() {}, removeEventListener() {}} as unknown as MediaQueryList);
  const edge = {...selection(''), nodeKey: undefined, kind: 'Relation'};
  const wrapper = mount(ActivityPanel, {props: {project: project(), selection: edge, selectionKey: 'edge:first'}, global: {stubs: {LogEntry: true}}});
  try {
    const host = wrapper.get<HTMLElement>('#activity-content').element; host.scrollTop = 90;
    await wrapper.setProps({selection: {...edge, title: 'Updated edge label'}});
    expect(host.scrollTop).toBe(90); expect(wrapper.getComponent(SelectionInspector).props('record').title).toBe('Updated edge label');
    await wrapper.setProps({selection: selection('fact:next'), selectionKey: 'fact:next'});
    expect(wrapper.getComponent(SelectionInspector).props('record').nodeKey).toBe('fact:next');
    expect(wrapper.get<HTMLElement>('.activity-pane').element.dataset.viewMotion).toBeUndefined();
    await wrapper.setProps({selection: null, selectionKey: undefined});
    expect(wrapper.find('.log-stream').exists()).toBe(true);
  } finally {wrapper.unmount(); vi.restoreAllMocks();}
});
