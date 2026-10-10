(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api; else root.PwnMeshInputPreview = api;
}(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  const MAX_BYTES = 256 * 1024;
  const textFile = input => input && input.size <= MAX_BYTES && /\.(txt|md|json|jsonl|har|yaml|yml|xml|html?|css|[cm]?js|ts|tsx|jsx|py|go|rs|java|c|h|cpp|hpp|sh|sql|csv|log|conf|ini|toml|http|req|resp)$/i.test(input.name || '');
  const downloadURL = (project, input) => '/projects/' + encodeURIComponent(project) + '/inputs/' + encodeURIComponent(input);
  async function read(url, {fetcher = fetch, signal} = {}) {
    if (!/^\/projects\/[^/]+\/inputs\/[^/]+$/.test(url)) throw new Error('材料地址无效。');
    const response = await fetcher(url, {credentials:'same-origin',cache:'no-store',signal});
    if (!response.ok) throw new Error('材料读取失败（HTTP ' + response.status + '）');
    if (Number(response.headers.get('content-length')) > MAX_BYTES) { await response.body?.cancel(); throw new Error('文件超过 256 KiB，请下载查看。'); }
    const reader = response.body?.getReader(); if (!reader) throw new Error('当前浏览器不支持材料预览，请下载查看。');
    const chunks = []; let size = 0;
    try {
      for (;;) { const item = await reader.read(); if (item.done) break; size += item.value.byteLength; if (size > MAX_BYTES) { await reader.cancel(); throw new Error('文件超过 256 KiB，请下载查看。'); } chunks.push(item.value); }
    } finally { reader.releaseLock(); }
    const bytes = new Uint8Array(size); let offset = 0; for (const chunk of chunks) { bytes.set(chunk,offset); offset += chunk.byteLength; }
    let text; try { text = new TextDecoder('utf-8', {fatal:true}).decode(bytes); } catch { throw new Error('文件不是 UTF-8 文本，请下载查看。'); }
    if (text.includes('\u0000')) throw new Error('文件包含二进制内容，请下载查看。');
    return text;
  }
  return {MAX_BYTES,textFile,downloadURL,read};
}));
