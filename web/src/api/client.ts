import type { ExecutionChangePageDTO, ExecutionChanges } from './types';

export type Fetcher = typeof fetch;
export interface RequestOptions { method?: string; body?: unknown; signal?: AbortSignal; timeout?: number }
export interface APIClient {
  request<T = unknown>(path: string, options?: RequestOptions): Promise<T>;
  executionChanges(projectPath: string, after: number, generation: number, options?: RequestOptions): Promise<ExecutionChanges>;
  uploadInput(projectPath: string, file: File): Promise<unknown>;
  uploadInputs(projectPath: string, files: File[], content?: string, idempotencyKey?: string): Promise<unknown>;
}
export class APIError extends Error {
  constructor(message: string, public readonly status = 0) { super(message); this.name = 'APIError'; }
}
function errorMessage(detail: unknown, status: number): string {
  if (typeof detail === 'string') return detail;
  if (Array.isArray(detail)) return detail.map((value: unknown) => {
    const item = value && typeof value === 'object' ? value as Record<string, unknown> : {};
    return [Array.isArray(item.loc) ? item.loc.at(-1) : undefined, item.msg].filter(Boolean).join('：');
  }).join('；');
  return '请求失败（HTTP ' + status + '）';
}
export class Client implements APIClient {
  constructor(private readonly fetcher: Fetcher = (...args) => fetch(...args), private readonly timeout = 15000) {}
  async executionChanges(projectPath: string, after: number, generation: number, options: RequestOptions = {}): Promise<ExecutionChanges> {
    const items: ExecutionChanges['items'] = [], deleted: string[] = [];
    let cursor = after, through = 0;
    do {
      const page = await this.request<ExecutionChangePageDTO>(projectPath + '/executions/changes?generation=' + generation + '&after=' + after + '&cursor=' + cursor + '&through=' + through + '&limit=20', options);
      if (!Array.isArray(page.items) || !Array.isArray(page.deleted) || page.generation !== generation ||
          !Number.isSafeInteger(page.through) || page.through < after ||
          (page.next_cursor !== undefined && (!Number.isSafeInteger(page.next_cursor) || page.next_cursor <= cursor || page.next_cursor > page.through)) ||
          (through !== 0 && page.through !== through)) throw new APIError('执行记录分页边界无效');
      items.push(...page.items); deleted.push(...page.deleted);
      through = page.through; cursor = page.next_cursor || 0;
    } while (cursor);
    return {items, deleted, through};
  }
  async uploadInput(projectPath: string, file: File): Promise<unknown> {
    const body = new FormData(); body.append('file', file, file.name);
    return this.request(projectPath + '/inputs', {method: 'POST', body, timeout: 300000});
  }
  async uploadInputs(projectPath: string, files: File[], content = '', idempotencyKey = ''): Promise<unknown> {
    const body = new FormData();
    for (const file of files) body.append('file', file, file.name);
    if (content) body.append('content', content);
    if (idempotencyKey) body.append('idempotency_key', idempotencyKey);
    return this.request(projectPath + '/inputs/batch', {method: 'POST', body, timeout: 300000});
  }
  async request<T = unknown>(path: string, {method = 'GET', body, signal, timeout = this.timeout}: RequestOptions = {}): Promise<T> {
    if (!path.startsWith('/') || path.startsWith('//') || path.includes('\\')) throw new APIError('仅支持本站接口');
    const controller = new AbortController();
    const abort = () => controller.abort(signal?.reason);
    if (signal?.aborted) abort(); else signal?.addEventListener('abort', abort, {once: true});
    const timer = setTimeout(() => controller.abort(new Error('请求超时')), timeout);
    const multipart = typeof FormData !== 'undefined' && body instanceof FormData;
    try {
      const response = await this.fetcher(path, {method, credentials: 'same-origin', cache: 'no-store',
        headers: body === undefined || multipart ? {Accept: 'application/json'} : {Accept: 'application/json', 'Content-Type': 'application/json'},
        body: body === undefined || multipart ? body : JSON.stringify(body), signal: controller.signal});
      if (response.status === 204) return null as T;
      const text = await response.text();
      let result: unknown;
      try { result = JSON.parse(text); }
      catch { throw new APIError(response.ok ? '服务返回了无法读取的数据' : '请求失败（HTTP ' + response.status + '）', response.ok ? 0 : response.status); }
      if (!response.ok) throw new APIError(errorMessage(result && typeof result === 'object' && 'detail' in result ? result.detail : undefined, response.status), response.status);
      return result as T;
    } catch (error) {
      if (signal?.aborted) throw error;
      if (controller.signal.aborted) throw new APIError('连接超时，请稍后重试');
      if (error instanceof APIError) throw error;
      throw new APIError('无法连接 PwnMesh，请检查服务状态');
    } finally { clearTimeout(timer); signal?.removeEventListener('abort', abort); }
  }
}
/** Selection and refresh share a scope so obsolete responses cannot publish. */
export class RequestScope {
  version = 0;
  controller: AbortController | null = null;
  begin(): {version: number; signal: AbortSignal} {
    this.controller?.abort(); this.controller = new AbortController();
    return {version: ++this.version, signal: this.controller.signal};
  }
  current(version: number): boolean { return version === this.version && !this.controller?.signal.aborted; }
  cancel(): void { this.controller?.abort(); this.version++; }
}
