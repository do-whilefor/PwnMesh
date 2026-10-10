'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');

test('v5 browser shell keeps canvas, materials, result and log readers usable', {
  timeout:60000, skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for v5 browser coverage',
}, async () => {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE), assets = new Map();
  for (const name of await fs.readdir(path.join(__dirname,'../static'))) assets.set('/static/' + name,await fs.readFile(path.join(__dirname,'../static',name)));
  const server = http.createServer((req,res) => { const pathname = new URL(req.url,'http://local').pathname, asset = pathname === '/' ? '/static/index.html' : pathname;
    if (!assets.has(asset)) { res.writeHead(404); res.end(); return; }
    res.writeHead(200,{'Content-Type':({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.png':'image/png','.woff2':'font/woff2'})[path.extname(asset)] || 'application/octet-stream'}); res.end(assets.get(asset));
  });
  await new Promise(resolve => server.listen(0,'127.0.0.1',resolve)); let browser;
  try {
    browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
    const page = await browser.newPage({viewport:{width:1440,height:960}}); const errors=[]; page.on('pageerror', error => errors.push(error.message));
    const project = {id:'v5-test',title:'授权环境 · 认证与订单接口评估',scenario:'pentest',status:'active',generation:0,created_at:'2026-10-10T02:00:00Z'};
    const facts = [{id:'origin',description:'https://authorized.example.test',status:'input',created_at:project.created_at},{id:'goal',description:'核对授权边界，并记录可复现的支持证据。',status:'input',created_at:project.created_at},{id:'proof',description:'日志开头。'.repeat(70) + '隐藏全文检索标记 <script>literal-only</script>',status:'valid',created_at:project.created_at}];
    const state = {graph:{project,facts,intents:[],hints:[]},revision:1,goals:[{id:'goal',condition:'核对授权边界，并记录可复现的支持证据。',status:'open'}],steps:[{id:'inspect',description:'核对订单授权',status:'running',goal_id:'goal',from:['origin']}],fact_records:facts,findings:[],fact_relations:[]};
    const text = '<script>window.previewExecuted=true</script>\nGET /orders HTTP/1.1';
    const inputs = [{id:'text',name:'request.http',size:Buffer.byteLength(text),path:'/workspace/inputs/request.http',sha256:'a'.repeat(64),created_at:project.created_at},{id:'binary',name:'client.apk',size:12,path:'/workspace/inputs/client.apk',sha256:'b'.repeat(64),created_at:project.created_at}];
    const calls=[];
    await page.route('**/projects**', route => {
      const pathname = new URL(route.request().url()).pathname; calls.push(pathname);
      const reply = body => route.fulfill({contentType:'application/json',body:JSON.stringify(body)});
      if (pathname === '/projects') return reply([project]);
      if (pathname.endsWith('/state/events')) return reply([]);
      if (pathname.endsWith('/state')) return reply(state);
      if (pathname.endsWith('/identity')) return reply({id:project.id,generation:0});
      if (pathname.endsWith('/executions')) return reply({items:[],through:0});
      if (pathname.endsWith('/inputs')) return reply(inputs);
      if (pathname.endsWith('/inputs/text')) return route.fulfill({contentType:'application/octet-stream',body:text});
      if (pathname.endsWith('/inputs/binary')) return route.fulfill({contentType:'application/octet-stream',body:Buffer.from([0,255,0])});
      return route.fulfill({status:404,body:'{}'});
    });
    await page.goto('http://127.0.0.1:' + server.address().port,{waitUntil:'networkidle'});
    await page.waitForFunction(() => document.querySelectorAll('.graph-node').length === 5);
    const screenshots=path.resolve(__dirname,'../../tmp/v5-workbench-integration'); await fs.mkdir(screenshots,{recursive:true});
    for (const width of [1440,1920]) {
      await page.setViewportSize({width,height:960});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),'shell must not overflow viewport');
      const boxes=await page.evaluate(() => Object.fromEntries(['.project-heading','.main-pane','.activity-pane'].map(selector => {const r=document.querySelector(selector).getBoundingClientRect();return [selector,{left:r.left,right:r.right,top:r.top,bottom:r.bottom}];})));
      assert.ok(boxes['.project-heading'].bottom <= boxes['.main-pane'].top);
      assert.ok(boxes['.main-pane'].right < boxes['.activity-pane'].left);
      assert.ok(boxes['.activity-pane'].bottom <= 960);
      await page.screenshot({path:path.join(screenshots,'workbench-' + width + '.png')});
    }
    await page.locator('#log-search').fill('隐藏全文检索标记'); await page.waitForFunction(() => document.querySelectorAll('#activity-content .timeline-entry').length === 1);
    await page.locator('#activity-content .read-log').click(); assert.match(await page.locator('#reader-content').textContent(),/<script>literal-only<\/script>/);
    await page.locator('[data-close="reader-dialog"]').click();
    await page.locator('#view-materials').click(); await page.waitForFunction(() => document.querySelectorAll('.material-card').length === 2);
    const textCard=page.locator('.material-card').filter({hasText:'request.http'}); assert.match(await textCard.locator('a').getAttribute('href'),/\/inputs\/text$/);
    await textCard.getByRole('button',{name:'查看详情'}).click(); await page.waitForFunction(() => document.querySelector('.material-preview')?.textContent.includes('GET /orders'));
    assert.equal(await page.evaluate(() => window.previewExecuted),undefined); assert.equal(await page.locator('.material-preview script').count(),0);
    await page.screenshot({path:path.join(screenshots,'material-reader.png')}); await page.locator('[data-close="reader-dialog"]').click();
    await page.locator('.material-card').filter({hasText:'client.apk'}).getByRole('button',{name:'查看详情'}).click(); assert.match(await page.locator('#reader-content').textContent(),/二进制文件/);
    assert.equal(calls.includes('/projects/v5-test/inputs/binary'),false,'binary previews must not eagerly fetch the file');
    await page.locator('[data-close="reader-dialog"]').click(); await page.locator('#view-results').click(); assert.match(await page.locator('#results-view').textContent(),/答案正在探索中/);
    await page.locator('#view-canvas').click(); await page.locator('#project-info').click(); assert.match(await page.locator('#reader-content').textContent(),/authorized\.example\.test/);
    assert.deepEqual(errors,[]);
  } finally { if (browser) await browser.close(); await new Promise(resolve => server.close(resolve)); }
});
