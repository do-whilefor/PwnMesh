import { expect, test } from 'vitest';
import { APIError, Client, RequestScope } from './client';

test('execution pages keep the frozen revision, collect deletions and forward cancellation', async () => {
  const paths: string[] = [], signal = new AbortController().signal;
  const client = new Client(async (path, options) => {
    paths.push(String(path)); expect(options?.signal?.aborted).toBe(false);
    return Response.json(paths.length === 1 ? {items: [{id: 'mutable', status: 'succeeded'}], deleted: [], through: 9, generation: 2, next_cursor: 8}
      : {items: [], deleted: ['removed'], through: 9, generation: 2});
  });
  expect(await client.executionChanges('/projects/p', 7, 2, {signal})).toEqual({items: [{id: 'mutable', status: 'succeeded'}], deleted: ['removed'], through: 9});
  expect(paths).toEqual(['/projects/p/executions/changes?generation=2&after=7&cursor=7&through=0&limit=20', '/projects/p/executions/changes?generation=2&after=7&cursor=8&through=9&limit=20']);
});

test('broken continuations reject instead of exposing partial execution history', async () => {
  for (const tail of [{next_cursor: 1, through: 2}, {through: 3}, {through: 2, next_cursor: 3}, {through: 2, next_cursor: 1.5}, {through: 2, generation: 1}, {through: 2, deleted: null}]) {
    let calls = 0;
    const client = new Client(async () => Response.json(++calls === 1 ? {items: [{id: 'first'}], deleted: [], generation: 0, next_cursor: 1, through: 2} : {items: [], deleted: [], generation: 0, ...tail}));
    await expect(client.executionChanges('/projects/p', 0, 0)).rejects.toThrow('分页边界无效');
  }
});

test('204 deletion keeps same-origin credentials and rejects external paths before fetch', async () => {
  let calls = 0;
  const client = new Client(async (_, options) => {
    calls++; expect(options?.credentials).toBe('same-origin'); expect(options?.body).toBeUndefined();
    return new Response(null, {status: 204});
  });
  expect(await client.request('/projects/p', {method: 'DELETE'})).toBeNull();
  for (const path of ['https://example.com', '//example.com', '/\\example.com']) await expect(client.request(path)).rejects.toBeInstanceOf(APIError);
  expect(calls).toBe(1);
});

test('writes surface generation conflicts once without replay', async () => {
  let calls = 0;
  const client = new Client(async () => { calls++; return Response.json({detail: 'generation changed'}, {status: 409}); });
  await expect(client.request('/projects/p/restart', {method: 'POST', body: {expected_generation: 2}})).rejects.toMatchObject({status: 409, message: 'generation changed'});
  expect(calls).toBe(1);
});

test('connection and unreadable write responses preserve unknown outcome semantics', async () => {
  const client = new Client(async () => { throw new TypeError('Failed to fetch'); });
  await expect(client.request('/projects')).rejects.toMatchObject({status: 0, message: '无法连接 PwnMesh，请检查服务状态'});
  for (const status of [201, 422]) {
    let calls = 0;
    const broken = new Client(async () => { calls++; return new Response('<html>truncated response', {status}); });
    await expect(broken.request('/projects', {method: 'POST', body: {title: 'audit'}})).rejects.toMatchObject({status: status === 201 ? 0 : status});
    expect(calls).toBe(1);
  }
});

test('selection version rejects obsolete responses even if transport ignores abort', () => {
  const scope = new RequestScope(), first = scope.begin(), second = scope.begin();
  expect(first.signal.aborted).toBe(true); expect(scope.current(first.version)).toBe(false); expect(scope.current(second.version)).toBe(true);
  scope.cancel(); expect(second.signal.aborted).toBe(true); expect(scope.current(second.version)).toBe(false);
});

test('timeouts abort once and caller cancellation preserves the abort reason', async () => {
  let calls = 0;
  const client = new Client((_, options) => new Promise<Response>((_, reject) => {
    calls++; options?.signal?.addEventListener('abort', () => reject(options.signal?.reason), {once: true});
  }), 5);
  await expect(client.request('/projects', {method: 'POST', body: {title: 'draft'}})).rejects.toThrow('超时');
  expect(calls).toBe(1);
  const controller = new AbortController(), failure = new Error('selection changed');
  const pending = client.request('/projects', {signal: controller.signal}); controller.abort(failure);
  await expect(pending).rejects.toBe(failure);
});

test('single and batch uploads preserve binary FormData without fixing a multipart boundary', async () => {
  const bytes = new Uint8Array([0, 255, 13, 10]), files = [new File([bytes], '客户端.apk'), new File(['GET / HTTP/1.1\r\n'], 'capture.http')];
  const client = new Client(async (path, options) => {
    expect(new Headers(options?.headers).has('Content-Type')).toBe(false);
    expect(options?.method).toBe('POST');
    const body = options?.body; expect(body).toBeInstanceOf(FormData);
    if (!(body instanceof FormData)) throw new Error('Expected FormData');
    const first = body.getAll('file')[0]; if (!(first instanceof File)) throw new Error('Expected a binary file');
    expect(new Uint8Array(await first.arrayBuffer())).toEqual(bytes);
    if (String(path).endsWith('/batch')) {
      expect(body.getAll('file').map(file => (file as File).name)).toEqual(files.map(file => file.name));
      expect(body.get('content')).toBe('仅静态分析，不重放请求'); expect(body.get('idempotency_key')).toBe('retry-1');
    }
    await new Promise(resolve => setTimeout(resolve, 10)); expect(options?.signal?.aborted).toBe(false);
    return Response.json({id: 'input'});
  }, 1);
  expect(await client.uploadInput('/projects/p', files[0]!)).toEqual({id: 'input'});
  await client.uploadInputs('/projects/p', files, '仅静态分析，不重放请求', 'retry-1');
});
