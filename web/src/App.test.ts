// @vitest-environment happy-dom
import { afterEach, expect, test, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { defineComponent, h } from 'vue';
import { createMemoryHistory, createRouter } from 'vue-router';
import { Client } from './api/client';
import type { RequestOptions } from './api/client';
import type { ProjectDTO, WorkspaceSnapshot } from './api/types';
import App from './App.vue';
import ProjectForms from './workspace/dialogs/ProjectForms.vue';

afterEach(() => vi.restoreAllMocks());

test('successive creations keep navigation on each newly created project', async () => {
  const projects: ProjectDTO[] = [];
  const state = (project: ProjectDTO): WorkspaceSnapshot => ({graph: {project}, revision: 1, goals: [], steps: [], fact_records: [], findings: []});
  vi.spyOn(Client.prototype, 'request').mockImplementation(async <T,>(path: string, options: RequestOptions = {}): Promise<T> => {
    let result: unknown;
    if (path === '/projects' && options.method === 'POST') {
      const project: ProjectDTO = {id: 'project-' + (projects.length + 1), title: 'Created ' + (projects.length + 1), status: 'active', generation: 0, orchestration_version: 1};
      projects.push(project); result = {project};
    } else if (path === '/projects') result = projects.map(project => ({...project}));
    else {
      const project = projects.find(value => path.startsWith('/projects/' + value.id + '/'))!;
      result = path.endsWith('/state') ? state(project) : path.endsWith('/identity') ? {id: project.id, generation: 0} : path.includes('/rounds?') ? {items: []} : [];
    }
    return result as T;
  });
  vi.spyOn(Client.prototype, 'executionChanges').mockResolvedValue({items: [], deleted: [], through: 0});
  const page = defineComponent({name: 'WorkspacePage', props: ['project'], setup(props) { return () => h('h1', props.project?.name); }});
  const router = createRouter({history: createMemoryHistory(), routes: [
    {path: '/projects', name: 'projects', component: page},
    {path: '/project/:id', name: 'project', component: page},
  ]});
  await router.push('/projects'); await router.isReady();
  const sidebar = defineComponent({setup(_, {expose}) { expose({clear() {}}); return () => h('aside'); }});
  const wrapper = mount(App, {global: {plugins: [router], stubs: {ProjectSidebar: sidebar, ProjectForms: true, ProjectManagement: true, ActionDialog: true}}});
  try {
    await flushPromises();
    for (let index = 1; index <= 3; index++) {
      await wrapper.findComponent(ProjectForms).props('create')({name: 'Created ' + index, target: 'Authorized fixture', goal: 'Verify navigation', type: 'ctf', files: []});
      await flushPromises();
      expect(router.currentRoute.value.path).toBe('/project/project-' + index);
      expect(wrapper.get('h1').text()).toBe('Created ' + index);
    }
  } finally { wrapper.unmount(); }
});
