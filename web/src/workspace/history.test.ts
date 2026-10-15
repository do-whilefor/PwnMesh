import { expect, test } from 'vitest';
import { Client } from '../api/client';
import { readArchiveEntry, readPages } from './history';

test('history reads every page and rejects non-progressing or malformed continuations', async () => {
  const paths: string[] = [], api = new Client(async path => { paths.push(String(path)); return Response.json(paths.length === 1 ? {items: [1], next_cursor: 3} : {items: [2]}); });
  expect(await readPages<number>(api, '/rounds')).toEqual([1, 2]); expect(paths).toEqual(['/rounds?limit=100&cursor=-1', '/rounds?limit=100&cursor=3']);
  for (const page of [{items: null}, {items: [], next_cursor: -1}, {items: [], next_cursor: 1.5}]) await expect(readPages(new Client(async () => Response.json(page)), '/rounds')).rejects.toThrow('分页');
});

test('archive rejects oversized records before fetch and malformed chunk boundaries', async () => {
  let called = false;
  const api = new Client(async () => { called = true; return Response.json({}); });
  await expect(readArchiveEntry(api, '/rounds/0', {cursor: 1, kind: 'state', bytes: 32 * 1024 * 1024 + 1})).rejects.toThrow('32 MiB'); expect(called).toBe(false);
  const good = {encoding: 'base64', offset: 0, total_bytes: 4, data: Buffer.from('null').toString('base64')};
  for (const changed of [{encoding: 'utf8'}, {offset: 1}, {total_bytes: 3}, {data: Buffer.from('nul').toString('base64')}, {next_offset: 0}, {next_offset: 4}, {next_offset: 1}]) {
    const broken = new Client(async () => Response.json({...good, ...changed}));
    await expect(readArchiveEntry(broken, '/rounds/0', {cursor: 1, kind: 'state', bytes: 4})).rejects.toThrow(/分块/);
  }
});
