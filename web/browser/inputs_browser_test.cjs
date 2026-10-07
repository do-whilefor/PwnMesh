'use strict';

// Uses the real workbench assets and browser FormData against isolated API fixtures.
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');
const {createHash} = require('node:crypto');

test('browser uploads test materials and resumes the same paused project after an interrupted batch', {
  timeout:60000,
  skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE to run the isolated upload browser test',
}, async () => {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
  const files = new Map();
  for (const name of await fs.readdir(path.join(__dirname,'../static'))) files.set('/static/' + name, await fs.readFile(path.join(__dirname,'../static',name)));
  const server = http.createServer((request,response) => {
    const pathname = new URL(request.url,'http://localhost').pathname, asset = pathname === '/' ? '/static/index.html' : pathname;
    if (!files.has(asset)) { response.writeHead(404); response.end(); return; }
    const mime = {'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'}[path.extname(asset)];
    response.writeHead(200,{'Content-Type':mime || 'application/octet-stream'}); response.end(files.get(asset));
  });
  await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
  let browser;
  try {
    browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
    const page = await browser.newPage({viewport:{width:1280,height:900}}); page.setDefaultTimeout(10000);
    const errors = [], uploads = [], inputs = [], writes = []; let project = null, creates = 0, failAPK = true;
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/projects**', async route => {
      const request = route.request(), pathname = new URL(request.url()).pathname, method = request.method();
      const reply = (body,status = 200) => route.fulfill({status,contentType:'application/json',body:JSON.stringify(body)});
      if (method !== 'GET') writes.push(method + ' ' + pathname);
      if (pathname === '/projects' && method === 'POST') {
        creates++; const payload = request.postDataJSON(); assert.equal(payload.start_paused,true);
        project = {id:'uploaded',title:payload.title,status:'stopped',scenario:payload.scenario,generation:0,created_at:'2026-10-07T00:00:00Z'};
        return reply({project},201);
      }
      if (pathname === '/projects') return reply(project ? [project] : []);
      if (pathname === '/projects/uploaded/inputs') {
        if (method === 'GET') return reply(inputs);
        assert.match(request.headers()['content-type'],/^multipart\/form-data; boundary=/);
        const body = request.postDataBuffer(), name = body.toString().match(/filename="([^"]+)"/)[1]; uploads.push(name);
        assert.ok(body.includes(Buffer.from(name === 'client.apk' ? [0,255,13,10] : 'fixture')),'browser preserves input bytes');
        if (name === 'client.apk' && failAPK) { failAPK = false; return reply({detail:'Controlled APK upload failure'},503); }
        const input = {id:String(inputs.length + 1),name,size:7,path:'/workspace/.pwnmesh/inputs/' + (inputs.length + 1) + '/' + name,sha256:'a'.repeat(64),created_at:project.created_at};
        inputs.push(input); return reply(input,201);
      }
      if (pathname === '/projects/uploaded/status') { assert.equal(inputs.length,2); project.status = request.postDataJSON().status; return reply(project); }
      if (pathname === '/projects/uploaded/state') return reply({graph:{project,facts:[],intents:[],hints:[]},goals:[],steps:[],fact_records:[],findings:[],revision:1});
      if (pathname === '/projects/uploaded/state/events') return reply([]);
      if (pathname === '/projects/uploaded/executions') return reply({items:[],through:0});
      if (pathname === '/projects/uploaded/identity') return reply({id:project.id,generation:0});
      errors.push('Unexpected API call ' + method + ' ' + pathname); return reply({detail:'Unknown fixture'},404);
    });
    await page.goto('http://127.0.0.1:' + server.address().port,{waitUntil:'networkidle'});
    await page.locator('#new-project').click();
    await page.locator('#create-name').fill('客户端测试材料'); await page.locator('#create-origin').fill('授权测试的 APK 和接口流量'); await page.locator('#create-goal').fill('分析客户端配置和接口认证');
    await page.locator('#create-files').setInputFiles([{name:'capture.har',mimeType:'application/json',buffer:Buffer.from('fixture')},{name:'client.apk',mimeType:'application/vnd.android.package-archive',buffer:Buffer.from([0,255,13,10])}]);
    await page.locator('#submit-create').click(); await page.waitForFunction(() => document.getElementById('create-error').textContent.includes('Controlled APK upload failure'));
    assert.equal(creates,1); assert.equal(project.status,'stopped'); assert.equal(await page.locator('#create-name').isDisabled(),true);
    assert.deepEqual(uploads,['capture.har','client.apk']);
    await page.locator('#submit-create').click(); await page.locator('#create-dialog').waitFor({state:'hidden'});
    assert.equal(creates,1); assert.equal(project.status,'active'); assert.deepEqual(uploads,['capture.har','client.apk','client.apk']);
    await page.locator('#add-hint').click(); await page.locator('.imported-inputs summary').click();
    await page.waitForFunction(() => document.getElementById('imported-inputs').textContent.includes('SHA-256:'));
    assert.match(await page.locator('#imported-inputs').textContent(),/capture.har/);
    await page.locator('#hint-files').setInputFiles({name:'source.zip',mimeType:'application/zip',buffer:Buffer.from('fixture')});
    assert.equal(await page.locator('#hint-input').inputValue(),''); await page.locator('#send-hint').click(); await page.locator('#hint-dialog').waitFor({state:'hidden'});
    assert.equal(inputs.length,3); assert.equal(writes.some(write => write.endsWith('/hints')),false);
    await page.locator('#add-hint').click(); assert.equal(await page.locator('#hint-file-list li').count(),0);
    const widths = await page.evaluate(() => ({width:innerWidth,scroll:document.documentElement.scrollWidth})); assert.ok(widths.scroll <= widths.width);
    assert.deepEqual(errors,[]);
  } finally { if (browser) await browser.close(); await new Promise(resolve => server.close(resolve)); }
});

// Requires a dedicated empty server with no dispatcher. Only the project
// created by this test is removed; no pre-existing project is ever modified.
test('real service preserves browser-uploaded client inputs and file-only supplements', {
  timeout:90000,
  skip:process.env.PWNMESH_WEB_URL ? false : 'Set PWNMESH_WEB_URL to a dedicated empty service without a dispatcher',
}, async () => {
  const base = new URL(process.env.PWNMESH_WEB_URL);
  assert.ok(['http:','https:'].includes(base.protocol));
  const request = async (endpoint, options = {}) => {
    const response = await fetch(new URL(endpoint,base), {signal:AbortSignal.timeout(10000), ...options});
    assert.ok(response.ok, `${options.method || 'GET'} ${endpoint}: HTTP ${response.status} ${response.ok ? '' : await response.text()}`);
    return response;
  };
  assert.deepEqual(await (await request('/projects')).json(), [], 'use an isolated empty server; existing projects are never modified');
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
  const browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
  const page = await browser.newPage({viewport:{width:1440,height:960}}); page.setDefaultTimeout(15000);
  let createdProject;
  const errors = [], uploads = [], explicitHints = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => {
    if (request.method() !== 'POST') return;
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith('/inputs')) uploads.push(pathname);
    if (pathname.endsWith('/hints')) explicitHints.push(pathname);
  });
  const fixtures = [
    {name:'客户端 fixture.apk',mimeType:'application/vnd.android.package-archive',buffer:Buffer.from([80,75,3,4,0,255,13,10,128])},
    {name:'client-config.json',mimeType:'application/json',buffer:Buffer.from('{"base_url":"https://api.example.invalid","debug":false}\n')},
    {name:'source.go',mimeType:'text/plain',buffer:Buffer.from('package fixture\n\nconst API = "https://api.example.invalid"\n')},
    {name:'capture.har',mimeType:'application/json',buffer:Buffer.from('{"log":{"version":"1.2","creator":{"name":"test","version":"1"},"entries":[]}}\n')},
  ];
  const supplement = {name:'request.http',mimeType:'text/plain',buffer:Buffer.from('GET /api/profile HTTP/1.1\r\nHost: api.example.invalid\r\n\r\n')};
  const verifyInputs = async expected => {
    const inputs = await (await request('/projects/' + createdProject.id + '/inputs')).json();
    assert.equal(inputs.length, expected.length);
    for (const fixture of expected) {
      const input = inputs.find(input => input.name === fixture.name); assert.ok(input, 'missing metadata for ' + fixture.name);
      const hash = createHash('sha256').update(fixture.buffer).digest('hex');
      assert.equal(input.size, fixture.buffer.length); assert.equal(input.sha256, hash);
      assert.equal(input.path, '/workspace/.pwnmesh/inputs/' + input.id + '/' + input.name);
      const download = await request('/projects/' + createdProject.id + '/inputs/' + input.id);
      assert.equal(download.headers.get('content-type'), 'application/octet-stream');
      assert.equal(download.headers.get('x-content-sha256'), hash);
      assert.deepEqual(Buffer.from(await download.arrayBuffer()), fixture.buffer, 'server changed uploaded bytes for ' + fixture.name);
    }
    return inputs;
  };
  try {
    await page.goto(base.href,{waitUntil:'networkidle'}); await page.locator('#new-project').click();
    const title = 'Browser client input integration ' + process.pid;
    await page.locator('#create-name').fill(title); await page.locator('#create-origin').fill('Synthetic client artifacts and traffic for upload acceptance.');
    await page.locator('#create-goal').fill('Verify input transfer without starting a dispatcher or model.'); await page.locator('#create-files').setInputFiles(fixtures);
    const createResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/projects' && response.request().method() === 'POST');
    await page.locator('#submit-create').click(); const response = await createResponse;
    assert.equal(response.status(),201); const graph = await response.json(); createdProject = graph.project;
    assert.equal(response.request().postDataJSON().start_paused,true); assert.equal(createdProject.status,'stopped');
    await page.locator('#create-dialog').waitFor({state:'hidden'});
    await page.waitForFunction(title => document.getElementById('project-title').textContent === title,title);
    const after = await (await request('/projects/' + createdProject.id)).json(); assert.equal(after.project.status,'active');
    assert.equal(uploads.length,fixtures.length); const initialInputs = await verifyInputs(fixtures);
    assert.equal(after.hints.length,fixtures.length); assert.ok(initialInputs.every(input => after.hints.some(hint => hint.content.includes(input.path))));
    await page.reload({waitUntil:'networkidle'}); await page.locator('#add-hint').click(); await page.locator('.imported-inputs summary').click();
    await page.waitForFunction(count => document.querySelectorAll('#imported-inputs li').length === count,fixtures.length);
    const metadata = await page.locator('#imported-inputs').textContent();
    for (const input of initialInputs) { assert.ok(metadata.includes(input.name)); assert.ok(metadata.includes(input.path)); assert.ok(metadata.includes(input.sha256)); }
    await page.locator('#hint-files').setInputFiles(supplement); assert.equal(await page.locator('#hint-input').inputValue(),'');
    await page.locator('#send-hint').click(); await page.locator('#hint-dialog').waitFor({state:'hidden'});
    await verifyInputs([...fixtures,supplement]); assert.equal(uploads.length,fixtures.length + 1); assert.deepEqual(explicitHints,[]);
    const supplemented = await (await request('/projects/' + createdProject.id)).json(); assert.equal(supplemented.project.status,'active'); assert.equal(supplemented.hints.length,fixtures.length + 1);
    await page.locator('#add-hint').click(); assert.equal(await page.locator('#hint-file-list li').count(),0);
    await page.waitForFunction(count => document.querySelectorAll('#imported-inputs li').length === count,fixtures.length + 1);
    assert.deepEqual(errors,[]);
  } finally {
    await browser.close();
    if (createdProject) await request('/projects/' + createdProject.id,{method:'DELETE'});
  }
});
