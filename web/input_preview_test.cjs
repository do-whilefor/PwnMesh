'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const preview = require('./static/input-preview.js');
test('input preview only offers bounded text formats and encodes download paths', () => {
  assert.equal(preview.textFile({name:'request.har',size:1000}),true);
  assert.equal(preview.textFile({name:'app.apk',size:1000}),false);
  assert.equal(preview.textFile({name:'large.log',size:preview.MAX_BYTES+1}),false);
  assert.equal(preview.downloadURL('a/b','x/y'),'/projects/a%2Fb/inputs/x%2Fy');
});
test('input preview preserves literal markup and rejects binary or oversized bodies', async () => {
  const url = '/projects/a/inputs/b'; let options;
  assert.equal(await preview.read(url,{fetcher:async (_,value) => { options=value; return new Response('<script>literal</script>'); }}),'<script>literal</script>');
  assert.equal(options.credentials,'same-origin');
  await assert.rejects(preview.read(url,{fetcher:async () => new Response('a\u0000b')}),/二进制/);
  await assert.rejects(preview.read(url,{fetcher:async () => new Response(new Uint8Array([255]))}),/UTF-8/);
  await assert.rejects(preview.read(url,{fetcher:async () => new Response('x'.repeat(preview.MAX_BYTES+1))}),/256 KiB/);
  await assert.rejects(preview.read(url,{fetcher:async () => new Response('error',{status:404})}),/HTTP 404/);
  await assert.rejects(preview.read('https://external.test/material'),/地址无效/);
});
