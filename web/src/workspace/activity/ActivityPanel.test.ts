// @vitest-environment happy-dom
import { expect, test } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import ActivityPanel from './ActivityPanel.vue';
import LogEntry from './LogEntry.vue';
import { snapshotView } from '../project-resource';

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
