import { expect, test } from 'vitest';
import { downloadURL, MAX_BYTES, read, textFile } from './input-preview';

test('preview permits only bounded text formats and encodes download paths', () => {
  expect(textFile({name: 'request.har', size: 1000})).toBe(true);
  expect(textFile({name: 'app.apk', size: 1000})).toBe(false);
  expect(textFile({name: 'large.log', size: MAX_BYTES + 1})).toBe(false);
  expect(downloadURL('a/b', 'x/y')).toBe('/projects/a%2Fb/inputs/x%2Fy');
});
test('preview preserves literal markup and rejects binary, non-UTF8, oversized and external content', async () => {
  const url = '/projects/a/inputs/b';
  expect(await read(url, {fetcher: async (_, options) => { expect(options?.credentials).toBe('same-origin'); return new Response('<script>literal</script>'); }})).toBe('<script>literal</script>');
  for (const [body, message] of [['a\u0000b', '二进制'], [new Uint8Array([255]), 'UTF-8'], ['x'.repeat(MAX_BYTES + 1), '256 KiB']] as const)
    await expect(read(url, {fetcher: async () => new Response(body)})).rejects.toThrow(message);
  await expect(read(url, {fetcher: async () => new Response('error', {status: 404})})).rejects.toThrow('HTTP 404');
  await expect(read('https://external.test/material')).rejects.toThrow('地址无效');
});
