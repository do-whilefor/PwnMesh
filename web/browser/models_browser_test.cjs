'use strict';
// This test writes model settings: only run with a dedicated QA database/config.
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');

test('real model settings persist, probe Anthropic tools, and report failures; materials survive reload', {
  timeout:120000,
  skip:process.env.PWNMESH_WEB_QA === '1' && process.env.PWNMESH_WEB_URL ? false : 'Requires an isolated PWNMESH_WEB_QA service',
}, async t => {
  const base = new URL(process.env.PWNMESH_WEB_URL), requests = [], owned = [];
  let rejectModel = false;
  const upstream = http.createServer(async (req,res) => {
    let raw = ''; for await (const chunk of req) raw += chunk;
    const input = JSON.parse(raw);
    requests.push({url:req.url,version:req.headers['anthropic-version'],authorized:req.headers['x-api-key'] === 'qa-secret-only' || req.headers.authorization === 'Bearer qa-secret-only',input});
    res.setHeader('Content-Type','application/json');
    if (rejectModel) { res.writeHead(401); res.end(JSON.stringify({error:{message:'controlled authentication failure'}})); return; }
    const content = input.messages?.at(-1)?.content;
    const second = Array.isArray(content) && content.some(block => block.type === 'tool_result');
    res.end(JSON.stringify({id:'qa-message',type:'message',role:'assistant',model:input.model,stop_reason:second ? 'end_turn' : 'tool_use',
      content:second ? [{type:'text',text:'PWNMESH_OK'}] : [{type:'tool_use',id:'check-1',name:'connection_check',input:{ready:true}}],usage:{input_tokens:12,output_tokens:8}}));
  });
  await new Promise(resolve => upstream.listen(0,'0.0.0.0',resolve));
  const endpoint = `http://${process.env.PWNMESH_QA_UPSTREAM_HOST || 'host.docker.internal'}:${upstream.address().port}/anthropic`;
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
  const browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
  const page = await browser.newPage({viewport:{width:1440,height:900}}), errors = [];
  page.setDefaultTimeout(15000); page.on('pageerror', error => errors.push(error.message));
  const api = async (route,method = 'GET',body) => {
    const response = await fetch(new URL(route,base),{method,headers:{'Content-Type':'application/json'},body:body === undefined ? undefined : JSON.stringify(body)});
    assert.ok(response.ok,`${method} ${route}: ${response.status}`);
    const text = await response.text(); return text ? JSON.parse(text) : null;
  };
  const responseFor = (pathname,method) => page.waitForResponse(response => new URL(response.url()).pathname === pathname && (!method || response.request().method() === method));
  try {
    assert.deepEqual(await api('/projects'),[],'use a dedicated empty test database');
    await page.goto(base.href,{waitUntil:'networkidle'});
    await t.test('configuration survives reload and never returns the token',async () => {
      await page.locator('#model-entry').click(); await page.waitForFunction(() => !document.querySelector('.llm-test')?.disabled); await page.locator('#llm-url').fill(endpoint);
      await page.locator('#llm-model').fill('controlled-anthropic'); await page.locator('#llm-key').fill('qa-secret-only');
      await page.locator('#llm-connection-mode').selectOption('direct');
      const probe = responseFor('/model-settings/test'); await page.locator('.llm-test').click(); assert.equal((await (await probe).json()).ok,true);
      const saved = responseFor('/model-settings','PUT'); await page.locator('.llm-save').click(); assert.ok((await saved).ok());
      const settings = await api('/model-settings'); assert.equal(settings.model,'controlled-anthropic'); assert.equal(settings.has_token,true);
      assert.ok(!JSON.stringify(settings).includes('qa-secret-only'));
      await page.reload({waitUntil:'networkidle'});
      await page.waitForFunction(value => document.getElementById('llm-url')?.value === value,endpoint);
      assert.equal(await page.locator('#llm-key').inputValue(),'');
    });
    await t.test('simulation performs both tool rounds and reports upstream rejection',async () => {
      requests.length = 0;
      const checked = responseFor('/model-settings/test'); await page.locator('.llm-test').click();
      const result = await (await checked).json(); assert.equal(result.ok,true,result.message); assert.equal(requests.length,2);
      assert.ok(requests.every(req => req.url === '/anthropic/v1/messages' && req.authorized && req.version === '2023-06-01'));
      assert.equal(requests[0].input.model,'controlled-anthropic');
      assert.ok(requests[1].input.messages.at(-1).content.some(block => block.type === 'tool_result' && block.tool_use_id === 'check-1'));
      rejectModel = true; const failed = responseFor('/model-settings/test'); await page.locator('.llm-test').click();
      assert.equal((await (await failed).json()).ok,false);
      await page.waitForFunction(() => /失败|错误|401/.test(document.querySelector('.llm-status')?.textContent || ''));
      assert.ok(!(await page.locator('.llm-status').innerText()).includes('qa-secret-only'));
      rejectModel = false; await page.locator('.llm-back').click();
    });
    await t.test('material upload persists original bytes and unfinished results stay unfinished',async () => {
      const graph = await api('/projects','POST',{title:'V5 真实材料验收',origin:'Controlled local input only',goal:'Verify material persistence',scenario:'audit',start_paused:true});
      owned.push(graph.project.id); const projectPath = '/projects/' + encodeURIComponent(graph.project.id);
      await page.goto(new URL('#project/' + graph.project.id,base).href,{waitUntil:'networkidle'});
      await page.locator('#add-hint').click(); const payload = Buffer.from('Controlled local evidence.\nNo external target.\n');
      await page.locator('#hint-files').setInputFiles({name:'evidence.txt',mimeType:'text/plain',buffer:payload});
      const uploaded = responseFor(projectPath + '/inputs/batch','POST'); await page.locator('#hint-form button[type="submit"]').click(); assert.ok((await uploaded).ok());
      await page.locator('#hint-dialog').waitFor({state:'hidden'}); await page.reload({waitUntil:'networkidle'});
      await page.locator('#view-tab-materials').click(); await page.getByText('evidence.txt',{exact:true}).first().waitFor();
      const inputs = await api(projectPath + '/inputs'); assert.equal(inputs.length,1);
      const downloaded = await fetch(new URL(projectPath + '/inputs/' + encodeURIComponent(inputs[0].id),base));
      assert.deepEqual(Buffer.from(await downloaded.arrayBuffer()),payload);
      await page.locator('#view-tab-result').click(); assert.ok(!/已完成|已验证漏洞/.test(await page.locator('#project-view').innerText()));
    });
    assert.deepEqual(errors,[]);
  } finally {
    for (const id of owned) await api('/projects/' + encodeURIComponent(id),'DELETE');
    await browser.close(); await new Promise(resolve => upstream.close(resolve));
  }
});
