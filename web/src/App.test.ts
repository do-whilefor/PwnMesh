// @vitest-environment happy-dom
import { afterEach, expect, test, vi } from 'vitest';
import { flushPromises, mount } from '@vue/test-utils';
import { defineComponent, h } from 'vue';
import { createMemoryHistory, createRouter } from 'vue-router';
import { APIError, Client } from './api/client';
import type { RequestOptions } from './api/client';
import type { ProjectDTO, WorkspaceSnapshot } from './api/types';
import App from './App.vue';
import ProjectForms from './workspace/dialogs/ProjectForms.vue';
import ActionDialog from './workspace/dialogs/ActionDialog.vue';

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

test.each(['resume', 'restart', 'terminate', 'delete', 'external restart', 'external termination'] as const)('%s resets a retired creation draft without submitting it again', async action => {
  const projects: ProjectDTO[] = [], reset = vi.fn();
  const request = vi.spyOn(Client.prototype, 'request').mockImplementation(async <T,>(path: string, options: RequestOptions = {}): Promise<T> => {
    let result: unknown;
    if (path === '/projects' && options.method === 'POST') {
      const project: ProjectDTO = {id: 'created', title: 'Saved draft', status: 'stopped', generation: 0, orchestration_version: 1};
      projects.push(project); result = {project: {...project}};
    } else if (path === '/projects') result = projects.map(project => ({...project}));
    else {
      const project = projects[0]!;
      if (options.method === 'DELETE') projects.splice(0);
      else if (options.method === 'POST' && path.endsWith('/restart')) { project.generation = 1; project.status = 'active'; }
      else if (options.method === 'POST' && path.endsWith('/terminate')) project.status = 'terminated';
      else if (options.method === 'PUT') project.status = 'active';
      result = path.endsWith('/state') ? {graph: {project}, revision: 1, goals: [], steps: [], fact_records: [], findings: []}
        : path.endsWith('/identity') ? {id: project.id, generation: project.generation} : path.includes('/rounds?') ? {items: []} : [];
    }
    return result as T;
  });
  const upload = vi.spyOn(Client.prototype, 'uploadInput').mockRejectedValue(new APIError('upload interrupted', 503));
  vi.spyOn(Client.prototype, 'executionChanges').mockResolvedValue({items: [], deleted: [], through: 0});
  const page = defineComponent({name: 'WorkspacePage', setup() { return () => h('div'); }});
  const router = createRouter({history: createMemoryHistory(), routes: [{path: '/projects', name: 'projects', component: page}, {path: '/project/:id', name: 'project', component: page}]});
  await router.push('/projects'); await router.isReady();
  const forms = defineComponent({props: ['create'], setup(_, {expose}) { expose({resetCreate: reset}); return () => h('div'); }});
  const wrapper = mount(App, {global: {plugins: [router], stubs: {ProjectSidebar: true, ProjectForms: forms, ProjectManagement: true, ActionDialog: true}}});
  try {
    await flushPromises();
    const create = wrapper.findComponent(forms).props('create'), values = {name: 'Saved draft', target: 'Authorized fixture', goal: 'Verify retry', type: 'ctf', files: [new File(['data'], 'input.txt')]};
    await expect(create(values)).rejects.toMatchObject({pendingCreate: true}); expect(reset).not.toHaveBeenCalled();
    if (action === 'external restart') {
      projects[0]!.generation = 1;
      await expect(create(values)).rejects.toMatchObject({pendingCreate: false, status: 409});
    } else if (action === 'external termination') {
      projects[0]!.status = 'terminated'; upload.mockRejectedValue(new APIError('Project is no longer writable', 409));
      await expect(create(values)).rejects.toMatchObject({pendingCreate: false, status: 409});
    } else await wrapper.findComponent(ActionDialog).props('perform')({id: 'created', generation: 0, status: 'paused', action});
    expect(reset).toHaveBeenCalledOnce(); expect(upload).toHaveBeenCalledTimes(action === 'external termination' ? 2 : 1);
    expect(request.mock.calls.filter(([path, options]) => path === '/projects' && options?.method === 'POST')).toHaveLength(1);
  } finally { wrapper.unmount(); }
});
