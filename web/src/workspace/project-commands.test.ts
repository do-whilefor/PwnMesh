import { expect, test } from 'vitest';
import { APIError, Client } from '../api/client';
import type { RequestOptions } from '../api/client';
import { ProjectCommands } from './project-commands';
import { ProjectResource } from './project-resource';
import type { CreateProjectValues, ProjectAction } from './model/types';

class CommandAPI extends Client {
  calls: {path: string; options: RequestOptions}[] = [];
  handler = async (path: string, _options: RequestOptions): Promise<unknown> => path.endsWith('/identity') ? {id: 'created', generation: 0} : {project: {id: 'created', generation: 0}};
  upload = async (_base: string, _file: File): Promise<unknown> => null;
  batch = async (_base: string, _files: File[], _content: string, _key: string): Promise<unknown> => null;
  override async request<T = unknown>(path: string, options: RequestOptions = {}): Promise<T> { this.calls.push({path, options}); return await this.handler(path, options) as T; }
  override uploadInput(base: string, file: File): Promise<unknown> { return this.upload(base, file); }
  override uploadInputs(base: string, files: File[], content = '', key = ''): Promise<unknown> { return this.batch(base, files, content, key); }
}
const values = (files: File[] = []): CreateProjectValues => ({name: 'Test', type: 'pentest', target: 'authorized', goal: 'evidence', files});

test('failed creation retries only unfinished uploads on the original paused project', async () => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api)), uploads: string[] = [];
  let fail = true;
  api.upload = async (_, file) => { uploads.push(file.name); if (file.name === 'b.txt' && fail) { fail = false; throw new APIError('upload interrupted', 503); } };
  const draft = values([new File(['a'], 'a.txt'), new File(['b'], 'b.txt')]);
  await expect(commands.create(draft)).rejects.toMatchObject({pendingCreate: true, uncertainCreate: false, status: 503});
  expect(api.calls[0]?.options.body).toMatchObject({start_paused: true}); expect(commands.pendingCreate?.uploaded).toBe(1);
  expect(await commands.create(draft)).toBe('created'); expect(uploads).toEqual(['a.txt', 'b.txt', 'b.txt']);
  expect(api.calls.filter(call => call.path === '/projects')).toHaveLength(1); expect(api.calls.at(-1)?.options.body).toEqual({status: 'active', expected_generation: 0});
  expect(commands.pendingCreate).toBeNull();
});

test('unknown creation outcomes cannot be retried blindly', async () => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api));
  api.handler = async () => { throw new APIError('lost connection', 0); };
  await expect(commands.create(values())).rejects.toMatchObject({uncertainCreate: true});
  await expect(commands.create(values())).rejects.toThrow('尚未确认'); expect(api.calls).toHaveLength(1); expect(commands.uncertainCreate).toBe(true);
});

test.each(['resume', 'restart', 'terminate', 'delete'] as const)('successful %s retires an incomplete creation without another upload or create', async action => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api)); let uploads = 0;
  api.upload = async () => { uploads++; throw new APIError('upload interrupted', 503); };
  await expect(commands.create(values([new File(['data'], 'input.txt')]))).rejects.toMatchObject({pendingCreate: true});
  await commands.change('other', action, 0); expect(commands.pendingCreate).not.toBeNull();
  await commands.change('created', action, 0); expect(commands.pendingCreate).toBeNull();
  expect(uploads).toBe(1); expect(api.calls.filter(call => call.path === '/projects')).toHaveLength(1);
});

test('failed lifecycle changes and pause preserve an incomplete creation', async () => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api));
  api.upload = async () => { throw new APIError('upload interrupted', 503); };
  await expect(commands.create(values([new File(['data'], 'input.txt')]))).rejects.toMatchObject({pendingCreate: true});
  const pending = commands.pendingCreate;
  api.handler = async () => { throw new APIError('restart rejected', 409); };
  await expect(commands.change('created', 'restart', 0)).rejects.toThrow('restart rejected'); expect(commands.pendingCreate).toBe(pending);
  api.handler = async () => ({}); await commands.change('created', 'pause', 0); expect(commands.pendingCreate).toBe(pending);
});

test('resuming creation detects an external restart before uploading into the next round', async () => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api)); let uploads = 0;
  const draft = values([new File(['data'], 'input.txt')]);
  api.upload = async () => { uploads++; if (uploads === 1) throw new APIError('upload interrupted', 503); };
  await expect(commands.create(draft)).rejects.toMatchObject({pendingCreate: true});
  api.handler = async path => path.endsWith('/identity') ? {id: 'created', generation: 1} : {};
  await expect(commands.create(draft)).rejects.toMatchObject({pendingCreate: false, status: 409});
  expect(commands.pendingCreate).toBeNull(); expect(uploads).toBe(1); expect(api.calls.filter(call => call.path === '/projects')).toHaveLength(1);
});

test.each([404, 409])('definitive project conflict %s retires creation during upload or activation', async status => {
  for (const stage of ['upload', 'activation']) {
    const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api));
    api.upload = async () => { if (stage === 'upload') throw new APIError('Project is no longer writable', status); };
    api.handler = async path => {
      if (path.endsWith('/status')) throw new APIError('Project is no longer writable', status);
      return {project: {id: 'created', generation: 0}};
    };
    await expect(commands.create(values([new File(['data'], 'input.txt')]))).rejects.toMatchObject({pendingCreate: false, status});
    expect(commands.pendingCreate).toBeNull(); expect(api.calls.filter(call => call.path === '/projects')).toHaveLength(1);
  }
});

test('lifecycle calls encode project IDs and fence writes with their expected generation', async () => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api));
  for (const action of ['pause', 'resume', 'restart', 'terminate', 'delete'] as ProjectAction[]) await commands.change('a/b', action, 7);
  expect(api.calls.map(call => [call.path, call.options.method])).toEqual([
    ['/projects/a%2Fb/status', 'PUT'], ['/projects/a%2Fb/status', 'PUT'], ['/projects/a%2Fb/restart', 'POST'], ['/projects/a%2Fb/terminate', 'POST'], ['/projects/a%2Fb', 'DELETE'],
  ]);
  expect(api.calls[0]?.options.body).toEqual({status: 'stopped', expected_generation: 7}); expect(api.calls[1]?.options.body).toEqual({status: 'active', expected_generation: 7}); expect(api.calls[2]?.options.body).toEqual({expected_generation: 7});
});

test('hint retries retain per-project keys and replace them only for changed payload identity', async () => {
  const api = new CommandAPI(), commands = new ProjectCommands(new ProjectResource(api));
  const calls: {base: string; files: File[]; text: string; key: string}[] = [], files = [new File(['apk'], 'client.apk'), new File(['http'], 'capture.http')];
  let fail = true;
  api.batch = async (base, list, text, key) => { calls.push({base, files: list, text, key}); if (fail) throw new APIError('response lost', 0); return ['saved']; };
  const submit = (id: string, text: string, list: File[]) => expect(commands.hint(id, {text, files: list})).rejects.toThrow('response lost');
  await submit('a', 'scope', files); await submit('a', 'scope', files.slice());
  expect(calls[1]?.key).toBe(calls[0]?.key); expect(commands.isHintRetry('a', {text: 'scope', files: files.slice()})).toBe(true);
  expect(commands.isHintRetry('a', {text: 'scope', files: [new File(['apk'], 'client.apk'), files[1]!]})).toBe(false);
  await submit('b', 'scope', files); expect(calls[2]?.key).not.toBe(calls[1]?.key);
  await submit('a', 'changed', files); expect(calls[3]?.key).not.toBe(calls[1]?.key);
  await submit('a', 'changed', files.slice().reverse()); expect(calls[4]?.key).not.toBe(calls[3]?.key);
  fail = false; expect(await commands.hint('b', {text: 'scope', files})).toEqual(['saved']); expect(calls[5]?.key).toBe(calls[2]?.key);
  expect(commands.isHintRetry('b', {text: 'scope', files})).toBe(false); expect(commands.isHintRetry('a', {text: 'changed', files: files.slice().reverse()})).toBe(true);
  await commands.change('b', 'delete', 0); expect(commands.isHintRetry('a', {text: 'changed', files: files.slice().reverse()})).toBe(true);
  await commands.change('a', 'delete', 0); expect(commands.isHintRetry('a', {text: 'changed', files: files.slice().reverse()})).toBe(false);
});

test('command exclusion prevents concurrent mutations and always clears busy after failure', async () => {
  let resolve!: (value: unknown) => void;
  const gate = new Promise<unknown>(done => { resolve = done; }), api = new CommandAPI(), resource = new ProjectResource(api), commands = new ProjectCommands(resource);
  const scope = resource.scope.begin(); api.handler = () => gate;
  const first = commands.change('a', 'pause', 0); expect(commands.busy).toBe(true); expect(scope.signal.aborted).toBe(true);
  await expect(commands.change('a', 'delete', 0)).rejects.toThrow('正在处理上一项操作'); resolve(null); await first; expect(commands.busy).toBe(false);
  api.handler = async () => { throw new APIError('denied', 422); }; await expect(commands.change('a', 'pause', 0)).rejects.toThrow('denied'); expect(commands.busy).toBe(false);
});
