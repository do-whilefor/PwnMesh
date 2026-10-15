import type { APIClient, RequestOptions } from '../api/client';
import type { ArchiveChunkDTO, ArchiveEntryDTO, PageDTO } from '../api/types';

export async function readPages<T>(api: APIClient, path: string, options: RequestOptions = {}, cursor = -1): Promise<T[]> {
  const items: T[] = [];
  for (;;) {
    const page = await api.request<PageDTO<T>>(path + '?limit=100&cursor=' + cursor, options);
    if (!Array.isArray(page.items)) throw new Error('历史记录分页格式无效');
    items.push(...page.items);
    if (page.next_cursor == null) return items;
    if (!Number.isSafeInteger(page.next_cursor) || page.next_cursor <= cursor) throw new Error('历史记录分页边界无效');
    cursor = page.next_cursor;
  }
}

export async function readArchiveEntry<T>(api: APIClient, base: string, entry: ArchiveEntryDTO): Promise<T> {
  if (entry.bytes > 32 * 1024 * 1024) throw new Error('单条历史记录超过 32 MiB，无法在页面完整读取。');
  const bytes = new Uint8Array(entry.bytes);
  let offset = 0;
  for (;;) {
    const chunk = await api.request<ArchiveChunkDTO>(base + '/entries/' + entry.cursor + '?offset=' + offset + '&limit=32768');
    if (chunk.encoding !== 'base64' || chunk.offset !== offset || chunk.total_bytes !== entry.bytes) throw new Error('历史记录分块不一致');
    const raw = atob(chunk.data), end = offset + raw.length;
    const complete = chunk.next_offset == null;
    const invalidBoundary = complete ? end !== bytes.length : !raw.length || chunk.next_offset !== end || end >= bytes.length;
    if (invalidBoundary) throw new Error('历史记录分块边界无效');
    for (let i = 0; i < raw.length; i++) bytes[offset + i] = raw.charCodeAt(i);
    if (complete) break;
    offset = end;
  }
  // Join bytes before decoding: a chunk may split a UTF-8 character.
  return JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(bytes)) as T;
}
