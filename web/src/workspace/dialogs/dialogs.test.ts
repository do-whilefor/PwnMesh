// @vitest-environment happy-dom
import {flushPromises, mount} from '@vue/test-utils';
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest';
import {APIError} from '@/api/client';
import type {CreateDraft, ProjectView} from '../model/types';
import ProjectForms from './ProjectForms.vue';
import ActionDialog from './ActionDialog.vue';
import ProjectManagement from './ProjectManagement.vue';

const project = (id = 'project'): ProjectView => ({id, name: id, type: 'pentest', status: 'running', readOnly: false, generation: 2, target: 'scope', goal: 'goal', endedAt: null, files: [], history: [], state: null, logs: [], raw: {id, title: id, status: 'active'}});
beforeEach(() => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
  vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (this: HTMLDialogElement) {this.open = true;});
  vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (this: HTMLDialogElement) {this.open = false; this.dispatchEvent(new Event('close'));});
});
afterEach(() => {vi.useRealTimers(); document.body.replaceChildren();});
const finish = async () => {await flushPromises(); await vi.advanceTimersByTimeAsync(301); await flushPromises();};

describe('project forms', () => {
  it('retains per-project drafts, validates keyboard submission and preserves file identities', async () => {
    const create = vi.fn(async (_draft: CreateDraft) => undefined), hint = vi.fn(async () => undefined);
    const wrapper = mount(ProjectForms, {attachTo: document.body, props: {getProject: id => project(id), create, hint}});
    await wrapper.vm.openCreate();
    await wrapper.get('#create-form').trigger('keydown', {key: 'Enter', ctrlKey: true});
    expect(create).not.toHaveBeenCalled(); expect(document.activeElement?.id).toBe('project-name');
    await wrapper.get('#project-name').setValue(' Draft '); await wrapper.get('#project-target').setValue(' scope '); await wrapper.get('#project-goal').setValue(' goal ');
    const file = new File(['data'], 'evidence.txt'), input = wrapper.get<HTMLInputElement>('#project-files');
    Object.defineProperty(input.element, 'files', {value: [file], configurable: true}); await input.trigger('change');
    await wrapper.vm.openHint('one'); await wrapper.get('#hint-text').setValue('first draft');
    await wrapper.vm.openHint('two'); await wrapper.get('#hint-text').setValue('second draft');
    await wrapper.vm.openHint('one'); expect(wrapper.get<HTMLTextAreaElement>('#hint-text').element.value).toBe('first draft');
    await wrapper.vm.openCreate(); expect(wrapper.get<HTMLInputElement>('#project-name').element.value).toBe(' Draft ');
    await wrapper.get('#create-form').trigger('keydown', {key: 'Enter', metaKey: true}); await finish();
    expect(create).toHaveBeenCalledOnce(); expect(create.mock.calls[0]?.[0]).toEqual({name: 'Draft', target: 'scope', goal: 'goal', type: 'pentest', files: [file]});
    expect(wrapper.get<HTMLDialogElement>('#create-dialog').element.open).toBe(false); wrapper.unmount();
  });
  it('keeps an uncertain create blocked across closing and locks an upload continuation draft', async () => {
    const failed = Object.assign(new Error('saved but upload failed'), {pendingCreate: true});
    const create = vi.fn(async () => {throw failed;});
    const wrapper = mount(ProjectForms, {props: {getProject: () => project(), create, hint: async () => undefined}});
    await wrapper.vm.openCreate();
    await wrapper.get('#project-name').setValue('name'); await wrapper.get('#project-target').setValue('target'); await wrapper.get('#project-goal').setValue('goal');
    await wrapper.get('#create-form').trigger('submit'); await finish();
    expect(wrapper.get<HTMLInputElement>('#project-name').element.disabled).toBe(true);
    expect(wrapper.get<HTMLButtonElement>('#create-form [type="submit"]').element.disabled).toBe(false);
    create.mockImplementation(async () => {throw Object.assign(new Error('unknown outcome'), {uncertainCreate: true});});
    await wrapper.get('#create-form').trigger('submit'); await finish();
    wrapper.vm.close(); await wrapper.vm.openCreate();
    expect(wrapper.get<HTMLButtonElement>('#create-form [type="submit"]').element.disabled).toBe(true);
    await wrapper.get('#create-form').trigger('keydown', {key: 'Enter', ctrlKey: true}); expect(create).toHaveBeenCalledTimes(2);
    wrapper.vm.resetCreate(); await flushPromises(); expect(wrapper.get<HTMLInputElement>('#project-name').element.value).toBe(''); wrapper.unmount();
  });
  it('does not count a retried batch twice and blocks Escape during a pending submission', async () => {
    const value = project(), file = new File(['data'], 'evidence.txt');
    let resolve: (() => void) | undefined;
    const hint = vi.fn(() => new Promise<void>(done => {resolve = done;}));
    const wrapper = mount(ProjectForms, {props: {getProject: () => value, create: async () => undefined, hint, isHintRetry: () => true}});
    await wrapper.vm.openHint(value.id);
    const input = wrapper.get<HTMLInputElement>('#hint-files'); Object.defineProperty(input.element, 'files', {value: [file]}); await input.trigger('change');
    value.files = Array.from({length: 32}, (_, index) => ({id: String(index), name: `${index}.txt`, size: 1}));
    await wrapper.get('#hint-form').trigger('submit');
    expect(hint).toHaveBeenCalledOnce();
    const cancel = new Event('cancel', {cancelable: true}); wrapper.get('#hint-dialog').element.dispatchEvent(cancel); expect(cancel.defaultPrevented).toBe(true);
    resolve?.(); await finish(); expect(wrapper.get<HTMLDialogElement>('#hint-dialog').element.open).toBe(false); wrapper.unmount();
  });
});
it('captures generation and status when confirming and requires a fresh confirmation after conflict', async () => {
  const perform = vi.fn(async () => {throw new APIError('changed', 409);}), value = project();
  const wrapper = mount(ActionDialog, {props: {perform}});
  wrapper.vm.open(value, 'restart'); value.generation = 3;
  await wrapper.get('#action-form').trigger('submit'); await flushPromises();
  expect(perform).toHaveBeenCalledWith({id: 'project', action: 'restart', generation: 2, status: 'running'});
  expect(wrapper.get('#action-error').text()).toContain('刷新后重新确认');
  await wrapper.get('#action-form').trigger('submit'); expect(perform).toHaveBeenCalledOnce(); wrapper.unmount();
});
it('management uses current props and keeps historical rounds read-only', async () => {
  const value = project(), wrapper = mount(ProjectManagement, {props: {project: value}});
  await wrapper.vm.open(); await wrapper.get('[data-action="pause"]').trigger('click');
  expect(wrapper.emitted('action')?.[0]).toEqual([{id: value.id, action: 'pause', generation: 2, status: 'running'}]);
  await wrapper.setProps({project: {...value, archived: true, filesKnown: false}});
  expect(wrapper.findAll('[data-action]')).toHaveLength(0); expect(wrapper.get('.project-info-meta').text()).toContain('未知'); wrapper.unmount();
});

it('lets a disconnected action dialog close after its pending submission settles', async () => {
  let reject: (error: Error) => void = () => {};
  const perform = vi.fn(() => new Promise<void>((_resolve, fail) => { reject = fail; }));
  const wrapper = mount(ActionDialog, {props: {perform}});
  try {
    wrapper.vm.open(project(), 'restart');
    await wrapper.get('#action-form').trigger('submit');
    await wrapper.setProps({disabled: true});
    await wrapper.get('#action-form').trigger('submit');
    expect(perform).toHaveBeenCalledOnce();
    const busyCancel = new Event('cancel', {cancelable: true});
    wrapper.get('dialog').element.dispatchEvent(busyCancel);
    expect(busyCancel.defaultPrevented).toBe(true);
    expect(wrapper.get<HTMLButtonElement>('[data-close]').element.disabled).toBe(true);
    wrapper.vm.close(); expect(wrapper.get<HTMLDialogElement>('dialog').element.open).toBe(true);
    reject(new APIError('Disconnected', 0)); await flushPromises();
    expect(wrapper.get<HTMLButtonElement>('#confirm-action').element.disabled).toBe(true);
    expect(wrapper.get<HTMLButtonElement>('[data-close]').element.disabled).toBe(false);
    const cancel = new Event('cancel', {cancelable: true});
    wrapper.get('dialog').element.dispatchEvent(cancel); expect(cancel.defaultPrevented).toBe(false);
    await wrapper.get('[data-close]').trigger('click');
    expect(wrapper.get<HTMLDialogElement>('dialog').element.open).toBe(false);
  } finally { wrapper.unmount(); }
});

it('keeps management dismissal available while disconnected', async () => {
  const wrapper = mount(ProjectManagement, {props: {project: project()}});
  try {
    await wrapper.vm.open(); await wrapper.setProps({disabled: true});
    expect(wrapper.get<HTMLButtonElement>('[data-action]').element.disabled).toBe(true);
    expect(wrapper.get<HTMLButtonElement>('[data-close]').element.disabled).toBe(false);
    const cancel = new Event('cancel', {cancelable: true});
    wrapper.get('dialog').element.dispatchEvent(cancel); expect(cancel.defaultPrevented).toBe(false);
    await wrapper.get('[data-close]').trigger('click');
    expect(wrapper.get<HTMLDialogElement>('dialog').element.open).toBe(false);
  } finally { wrapper.unmount(); }
});
