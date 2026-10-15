// @vitest-environment happy-dom
import { test, expect, vi } from 'vitest';
import { mount } from '@vue/test-utils';
import { defineComponent, h, KeepAlive, nextTick, ref } from 'vue';
import GraphCanvas from './GraphCanvas.vue';
import { GraphController } from './controller';
import type { WorkspaceSnapshot } from '../../api/types';

test('Vue keeps the graph controller mounted across snapshots and releases it on unmount', async () => {
  const frames = new Map<number, FrameRequestCallback>();
  let serial = 0;
  vi.spyOn(window, 'requestAnimationFrame').mockImplementation(fn => { frames.set(++serial, fn); return serial; });
  vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(id => { frames.delete(id); });
  const destroy = vi.spyOn(GraphController.prototype, 'destroy');
  const snapshot: WorkspaceSnapshot = {
    graph: {project: {id:'project', title:'Project', status:'active', generation:1}},
    goals: [], steps: [], fact_records: [{id:'origin', description:'Original', status:'input'}], findings: [], revision:1,
  };
  const wrapper = mount(GraphCanvas, {props:{state:snapshot}});
  const viewport = wrapper.find('.graph-viewport').element;
  const card = wrapper.find('.graph-node').element;
  expect(wrapper.classes()).toContain('graph-host');
  await wrapper.setProps({state:{...snapshot, fact_records:[{id:'origin', description:'Updated', status:'input'}], revision:2}});
  expect(wrapper.find('.graph-viewport').element).toBe(viewport);
  expect(wrapper.find('.graph-node').element).toBe(card);
  expect(card.textContent).toContain('Updated');
  await wrapper.find('.graph-node').trigger('click');
  expect(wrapper.emitted('select-node')?.at(-1)?.[0]).toMatchObject({key:'fact:origin', label:'Updated'});
  wrapper.vm.zoomBy(.8);
  expect(wrapper.emitted('zoom')?.at(-1)).toEqual([.8]);
  wrapper.vm.setStatusFilter('running');
  expect(wrapper.vm.getStatusFilter()).toBe('running');
  expect(wrapper.emitted('select-node')?.at(-1)).toEqual([null]);
  wrapper.unmount();
  expect(destroy).toHaveBeenCalledOnce();
  expect(frames.size).toBe(0);
});

test('KeepAlive releases inactive work and restores the current round camera, filter and selection', async () => {
  const frames = new Map<number, FrameRequestCallback>();
  let serial = 0;
  vi.spyOn(window, 'requestAnimationFrame').mockImplementation(fn => { frames.set(++serial, fn); return serial; });
  vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(id => { frames.delete(id); });
  const destroy = vi.spyOn(GraphController.prototype, 'destroy');
  const visible = ref(true);
  const state = ref<WorkspaceSnapshot>({
    graph: {project: {id:'project', title:'Project', status:'active', generation:1}},
    goals: [], steps: [], fact_records: [{id:'origin', description:'Original', status:'input'}], findings: [], revision:1,
  });
  const OtherPage = defineComponent({render: () => h('div', 'Settings')});
  const Parent = defineComponent({setup: () => () => h(KeepAlive, {}, {
    default: () => visible.value ? h(GraphCanvas, {state:state.value}) : h(OtherPage),
  })});
  const wrapper = mount(Parent);
  const graph = wrapper.findComponent(GraphCanvas);
  graph.vm.zoomBy(.8);
  graph.vm.setStatusFilter('done');
  graph.vm.selectNode('fact:origin');
  visible.value = false;
  await nextTick();
  expect(destroy).toHaveBeenCalledOnce();
  expect(frames.size).toBe(0);
  visible.value = true;
  await nextTick();
  expect(wrapper.findAll('.graph-viewport')).toHaveLength(1);
  expect(graph.vm.getStatusFilter()).toBe('done');
  expect(graph.emitted('zoom')?.at(-1)).toEqual([.8]);
  expect(graph.emitted('select-node')?.at(-1)?.[0]).toMatchObject({key:'fact:origin'});
  visible.value = false;
  await nextTick();
  state.value = {...state.value, graph:{project:{...state.value.graph.project, generation:2}}};
  visible.value = true;
  await nextTick();
  expect(graph.vm.getStatusFilter()).toBe('all');
  expect(graph.emitted('select-node')?.at(-1)).toEqual([null]);
  wrapper.unmount();
  expect(frames.size).toBe(0);
});
