// This test writes model settings: only run with a dedicated QA database/config.
import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import type {IncomingHttpHeaders, Server} from 'node:http';
import type {Response} from 'playwright';
import type {CreatedProjectDTO, InputDTO} from '../src/api/types.js';
import type {PublicSettings, SettingsPayload} from '../src/model-settings/rules.js';
import {launch, staticServer} from './visual_fixture.js';
const proxyProtocols = {http:'HTTP',https:'HTTPS',socks5:'SOCKS5',socks5h:'SOCKS5h'};

interface ToolResult {type: 'tool_result'; tool_use_id: string; content: string}
type AnthropicContent = ToolResult | {type: 'text'; text: string} | {type: 'tool_use'; id: string; name: string; input: Record<string, unknown>};
interface AnthropicMessage {role: string; content: string | AnthropicContent[]}
interface AnthropicInput {model: string; messages: AnthropicMessage[]}
interface CapturedRequest {url: string | undefined; version: IncomingHttpHeaders[string]; authorized: boolean; input: AnthropicInput}
interface SettingsCall {path: string; body: SettingsPayload | null}
interface ProbeResult {ok: boolean; message?: string}

function listeningPort(server: Server): number {
  const address = server.address();
  assert.ok(address && typeof address !== 'string', 'the fixture must listen on a TCP port');
  return address.port;
}

function lastSettingsBody(calls: SettingsCall[]): SettingsPayload {
  const body = calls.at(-1)?.body;
  assert.ok(body, 'the latest model settings request must include a payload');
  return body;
}

async function probeResult(response: Promise<Response>): Promise<ProbeResult> {
  return await (await response).json() as ProbeResult;
}

test('real model settings persist, probe Anthropic tools, and report failures; materials survive reload', {
  timeout:120000,
  skip:process.env.PWNMESH_WEB_QA === '1' && process.env.PWNMESH_WEB_URL ? false : 'Requires an isolated PWNMESH_WEB_QA service',
}, async t => {
  assert.ok(process.env.PWNMESH_WEB_URL);
  const base = new URL(process.env.PWNMESH_WEB_URL), requests: CapturedRequest[] = [], owned: string[] = [];
  let rejectModel = false, acceptedKey = 'qa-secret-only';
  const upstream = http.createServer(async (req,res) => {
    let raw = ''; for await (const chunk of req) raw += chunk;
    const input = JSON.parse(raw) as AnthropicInput;
    requests.push({url:req.url,version:req.headers['anthropic-version'],authorized:req.headers['x-api-key'] === acceptedKey || req.headers.authorization === 'Bearer '+acceptedKey,input});
    res.setHeader('Content-Type','application/json');
    if (rejectModel) { res.writeHead(401); res.end(JSON.stringify({error:{message:'controlled authentication failure'}})); return; }
    const content = input.messages?.at(-1)?.content;
    const toolResult = Array.isArray(content) ? content.find(block => block.type === 'tool_result') : undefined;
    const second = Boolean(toolResult);
    res.end(JSON.stringify({id:'qa-message',type:'message',role:'assistant',model:input.model,stop_reason:second ? 'end_turn' : 'tool_use',
      content:toolResult ? [{type:'text',text:toolResult.content}] : [{type:'tool_use',id:'check-1',name:'connection_check',input:{ready:true}}],usage:{input_tokens:12,output_tokens:8}}));
  });
  await new Promise<void>(resolve => upstream.listen(0,'0.0.0.0',resolve));
  const proxied: string[] = [];
  const proxy = http.createServer((req,res) => {
    assert.ok(req.url);
    const destination = new URL(req.url);proxied.push(destination.href);
    const forwarded = http.request({hostname:'127.0.0.1',port:listeningPort(upstream),path:destination.pathname,method:req.method,headers:req.headers},response => {
      res.writeHead(response.statusCode ?? 502,response.headers);response.pipe(res);
    });
    forwarded.on('error',() => {res.writeHead(502);res.end();});req.pipe(forwarded);
  });
  await new Promise<void>(resolve => proxy.listen(0,'0.0.0.0',resolve));
  const endpoint = `http://${process.env.PWNMESH_QA_UPSTREAM_HOST || 'host.docker.internal'}:${listeningPort(upstream)}/anthropic`;
  const browser = await launch();
  const page = await browser.newPage({viewport:{width:1440,height:900}}), errors: string[] = [];
  page.setDefaultTimeout(15000); page.on('pageerror', error => errors.push(error.message));
  const api = async <T = unknown>(route: string,method = 'GET',body?: unknown): Promise<T> => {
    const response = await fetch(new URL(route,base),{method,headers:{'Content-Type':'application/json'},body:body === undefined ? undefined : JSON.stringify(body)});
    assert.ok(response.ok,`${method} ${route}: ${response.status}`);
    const text = await response.text(); return (text ? JSON.parse(text) : null) as T;
  };
  const responseFor = (pathname: string,method?: string) => page.waitForResponse(response => new URL(response.url()).pathname === pathname && (!method || response.request().method() === method));
  try {
    assert.deepEqual(await api('/projects'),[],'use a dedicated empty test database');
    await page.goto(base.href,{waitUntil:'networkidle'});
    await t.test('configuration survives reload and never returns the token',async () => {
      await page.locator('#model-entry').click(); await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false); await page.locator('#llm-url').fill(endpoint);
      await page.locator('#llm-model').fill('controlled-anthropic'); await page.locator('#llm-key').fill('qa-secret-only');
      await page.locator('#llm-connection-mode').selectOption('direct');
      const probe = responseFor('/model-settings/test'); await page.locator('.llm-test').click(); assert.equal((await probeResult(probe)).ok,true);
      const saved = responseFor('/model-settings','PUT'); await page.locator('.llm-save').click(); assert.ok((await saved).ok());
      const settings = await api<PublicSettings>('/model-settings'); assert.equal(settings.model,'controlled-anthropic'); assert.equal(settings.has_token,true);
      assert.ok(!JSON.stringify(settings).includes('qa-secret-only'));
      await page.reload({waitUntil:'networkidle'});
      await page.waitForFunction(value => document.querySelector<HTMLInputElement>('#llm-url')?.value === value,endpoint);
      assert.equal(await page.locator('#llm-key').inputValue(),'');
    });
    await t.test('simulation performs both tool rounds and reports upstream rejection',async () => {
      requests.length = 0;
      const checked = responseFor('/model-settings/test'); await page.locator('.llm-test').click();
      const result = await probeResult(checked); assert.equal(result.ok,true,result.message); assert.equal(requests.length,2);
      assert.ok(requests.every(req => req.url === '/anthropic/v1/messages' && req.authorized && req.version === '2023-06-01'));
      assert.equal(requests[0].input.model,'controlled-anthropic');
      const content = requests[1].input.messages.at(-1)?.content;
      assert.ok(Array.isArray(content) && content.some(block => block.type === 'tool_result' && block.tool_use_id === 'check-1'));
      rejectModel = true; const failed = responseFor('/model-settings/test'); await page.locator('.llm-test').click();
      assert.equal((await probeResult(failed)).ok,false);
      await page.waitForFunction(() => /失败|错误|401/.test(document.querySelector('.llm-status')?.textContent || ''));
      assert.ok(!(await page.locator('.llm-status').innerText()).includes('qa-secret-only'));
      rejectModel = false; await page.locator('.llm-back').click();
    });
    await t.test('updated endpoint and key persist through a real HTTP proxy and direct mode bypasses it',async () => {
      await page.locator('#model-entry').click();await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
      acceptedKey = 'qa-rotated-key';requests.length = 0;
      await page.locator('#llm-url').fill(endpoint+'/updated');await page.locator('#llm-key').fill(acceptedKey);
      await page.locator('#llm-connection-mode').selectOption('proxy');await page.locator('#llm-proxy-protocol').selectOption('http');
      await page.locator('#llm-proxy-host').fill(process.env.PWNMESH_QA_UPSTREAM_HOST || 'host.docker.internal');
      await page.locator('#llm-proxy-port').fill(String(listeningPort(proxy)));
      const checked = responseFor('/model-settings/test');await page.locator('.llm-test').click();assert.equal((await probeResult(checked)).ok,true);
      assert.equal(proxied.length,2);assert.ok(requests.every(req => req.url === '/anthropic/updated/v1/messages' && req.authorized));
      const saved = responseFor('/model-settings','PUT');await page.locator('.llm-save').click();assert.ok((await saved).ok());
      const settings = await api<PublicSettings>('/model-settings');assert.equal(settings.connection_mode,'proxy');assert.equal(settings.base_url,endpoint+'/updated');
      assert.ok(!JSON.stringify(settings).includes(acceptedKey));
      await page.reload({waitUntil:'networkidle'});await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
      assert.equal(await page.locator('#llm-proxy-port').inputValue(),String(listeningPort(proxy)));assert.equal(await page.locator('#llm-key').inputValue(),'');
      await page.locator('#llm-connection-mode').selectOption('direct');
      const direct = responseFor('/model-settings/test');await page.locator('.llm-test').click();assert.equal((await probeResult(direct)).ok,true);
      assert.equal(proxied.length,2,'direct requests must bypass the saved proxy');assert.equal(requests.length,4);
      assert.ok(requests.every(req => req.authorized),'the rotated server-side key is reused');
      const directSaved = responseFor('/model-settings','PUT');await page.locator('.llm-save').click();assert.ok((await directSaved).ok());
      await page.locator('.llm-back').click();
    });
    await t.test('material upload persists original bytes and unfinished results stay unfinished',async () => {
      const graph = await api<CreatedProjectDTO>('/projects','POST',{title:'V5 真实材料验收',origin:'Controlled local input only',goal:'Verify material persistence',scenario:'audit',start_paused:true});
      owned.push(graph.project.id); const projectPath = '/projects/' + encodeURIComponent(graph.project.id);
      await page.goto(new URL('#project/' + graph.project.id,base).href,{waitUntil:'networkidle'});
      await page.locator('#add-hint').click(); const payload = Buffer.from('Controlled local evidence.\nNo external target.\n');
      await page.locator('#hint-files').setInputFiles({name:'evidence.txt',mimeType:'text/plain',buffer:payload});
      const uploaded = responseFor(projectPath + '/inputs/batch','POST'); await page.locator('#hint-form button[type="submit"]').click(); assert.ok((await uploaded).ok());
      await page.locator('#hint-dialog').waitFor({state:'hidden'}); await page.reload({waitUntil:'networkidle'});
      await page.locator('#view-tab-materials').click(); await page.getByText('evidence.txt',{exact:true}).first().waitFor();
      const inputs = await api<InputDTO[]>(projectPath + '/inputs'); assert.equal(inputs.length,1);
      const downloaded = await fetch(new URL(projectPath + '/inputs/' + encodeURIComponent(inputs[0].id),base));
      assert.deepEqual(Buffer.from(await downloaded.arrayBuffer()),payload);
      await page.locator('#view-tab-result').click(); assert.ok(!/已完成|已验证漏洞/.test(await page.locator('#project-view').innerText()));
    });
    assert.deepEqual(errors,[]);
  } finally {
    for (const id of owned) await api('/projects/' + encodeURIComponent(id),'DELETE');
    await browser.close();await new Promise<void>(resolve => proxy.close(() => resolve()));await new Promise<void>(resolve => upstream.close(() => resolve()));
  }
});

test('model protocol selection persists and requires a new probe after switching', {
  timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE to check model protocol selection',
}, async () => {
  const server = await staticServer(), browser = await launch(), calls: SettingsCall[] = [];
  let state: PublicSettings = {has_token:true,base_url:'https://models.example.test/v1',model:'chat-model',connection_mode:'direct',proxy_url:'',reasoning_effort:'max'};
  try {
    const page = await browser.newPage();
    await page.route('**/projects',route=>route.fulfill({contentType:'application/json',body:'[]'}));
    await page.route('**/model-settings**',route=>{
      const request = route.request(), path = new URL(request.url()).pathname, body = request.postDataJSON() as SettingsPayload | null;
      calls.push({path,body});
      if (request.method() === 'PUT') {assert.ok(body);const {token,...saved}=body;state={...saved,has_token:true};}
      return route.fulfill({contentType:'application/json',body:JSON.stringify(path.endsWith('/test') ? {ok:true} : state)});
    });
    await page.goto(server.url+'/#llm',{waitUntil:'networkidle'});
    assert.equal(await page.locator('#llm-protocol').inputValue(),'anthropic');
    assert.deepEqual(await page.locator('#llm-protocol option').allTextContents(),['Anthropic Messages API','OpenAI Chat Completions']);
    await page.locator('#llm-protocol').selectOption('openai');
    assert.equal(await page.locator('#llm-url').inputValue(),state.base_url);
    assert.equal(await page.locator('#llm-model').inputValue(),state.model);
    assert.equal(await page.locator('#llm-effort').count(),0);
    assert.equal(await page.locator('.llm-summary-protocol').textContent(),'OpenAI');
    await page.locator('.llm-test').click();
    await page.waitForFunction(()=>document.querySelector<HTMLButtonElement>('.llm-save')?.disabled === false);
    assert.equal(lastSettingsBody(calls).protocol,'openai');
    await page.locator('#llm-protocol').selectOption('anthropic');
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    await page.locator('#llm-protocol').selectOption('openai');
    await page.locator('.llm-test').click();await page.locator('.llm-save').click();
    await page.waitForFunction(()=>document.querySelector('.llm-summary-status')?.textContent==='已保存');
    assert.equal(lastSettingsBody(calls).protocol,'openai');
    await page.reload({waitUntil:'networkidle'});
    assert.equal(await page.locator('#llm-protocol').inputValue(),'openai');
    assert.equal(await page.locator('#llm-url').inputValue(),state.base_url);
    assert.equal(await page.locator('#llm-key').inputValue(),'');
  } finally {await browser.close();await server.close();}
});

test('Kimi subscription sign-in saves only after a probe and GLM plan still requires its dedicated key', {
  timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE to check subscription authentication',
}, async () => {
  const server = await staticServer(), browser = await launch(), calls: SettingsCall[] = [];
  let state: PublicSettings = {has_token:false,base_url:'https://api.kimi.com/coding',model:'kimi-for-coding',connection_mode:'direct',proxy_url:'',reasoning_effort:'max'};
  try {
    const page = await browser.newPage();
    await page.route('**/projects',route=>route.fulfill({contentType:'application/json',body:'[]'}));
    await page.route('**/model-settings**',route=>{
      const request = route.request(), path = new URL(request.url()).pathname, body = request.postDataJSON() as SettingsPayload | null;
      calls.push({path,body});
      let result: unknown = state;
      if (path === '/model-settings/kimi/login') result = {login_id:'browser-login',user_code:'KIMI-1234',verification_uri:'https://www.kimi.com/code',expires_in:60,interval:1};
      else if (path === '/model-settings/kimi/login/browser-login') result = {status:'authorized'};
      else if (path.endsWith('/test')) result = {ok:true};
      else if (request.method() === 'PUT') {assert.ok(body);const {token,login_id,...saved}=body;state={...saved,has_token:true};result=state;}
      return route.fulfill({contentType:'application/json',body:JSON.stringify(result)});
    });
    await page.goto(server.url+'/#llm',{waitUntil:'networkidle'});
    await page.locator('#llm-auth-mode').selectOption('kimi_oauth');
    assert.equal(await page.locator('#llm-key').count(),0);
    assert.equal(await page.locator('#llm-protocol').isDisabled(),true);
    assert.equal(await page.locator('#llm-url').isDisabled(),true);
    await page.locator('.llm-login-start').click();
    await page.locator('.llm-login-code').waitFor();
    assert.equal(await page.locator('.llm-login-code').textContent(),'KIMI-1234');
    assert.equal(await page.locator('.llm-login-link').getAttribute('href'),'https://www.kimi.com/code');
    await page.waitForFunction(()=>document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    await page.locator('.llm-test').click();await page.locator('.llm-save').click();
    await page.waitForFunction(()=>document.querySelector('.llm-summary-status')?.textContent==='已保存');
    assert.equal(lastSettingsBody(calls).auth_mode,'kimi_oauth');
    assert.equal(lastSettingsBody(calls).login_id,'browser-login');
    assert.equal(lastSettingsBody(calls).token,'');
    await page.reload({waitUntil:'networkidle'});
    assert.equal(await page.locator('#llm-auth-mode').inputValue(),'kimi_oauth');
    await page.locator('.llm-test').click();
    await page.waitForFunction(()=>document.querySelector<HTMLButtonElement>('.llm-save')?.disabled === false);
    assert.equal(lastSettingsBody(calls).login_id,undefined);
    await page.locator('[data-llm-provider="glm"]').click();
    await page.locator('#llm-auth-mode').selectOption('glm_coding_plan');
    assert.equal(await page.locator('.llm-login-start').count(),0);
    assert.equal(await page.locator('#llm-key').isVisible(),true);
    assert.equal(await page.locator('#llm-effort').count(),0);
    assert.equal(await page.getByText('GLM Coding Plan 订阅与专用 API Key 说明').getAttribute('href'),'https://docs.bigmodel.cn/cn/coding-plan/extension/coding-tool-helper');
  } finally {await browser.close();await server.close();}
});

test('model browser blocks cross-origin saved credentials and clears provider drafts', {
  timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE to check model credential binding in a browser',
}, async () => {
  const server = await staticServer(), browser = await launch(), calls: SettingsCall[] = [];
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}});
    await page.route('**/projects',route=>route.fulfill({contentType:'application/json',body:'[]'}));
    await page.route('**/model-settings**',route=>{
      const path = new URL(route.request().url()).pathname, body = route.request().postDataJSON() as SettingsPayload | null;
      calls.push({path,body});
      return route.fulfill({contentType:'application/json',body:JSON.stringify(path.endsWith('/test') ? {ok:true,latency_ms:5,message:'fixture verified'} : {has_token:true,base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',connection_mode:'direct',proxy_url:'',reasoning_effort:'high'})});
    });
    await page.goto(server.url+'/#llm',{waitUntil:'networkidle'});
    await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
    await page.locator('#llm-url').fill('https://api.deepseek.com/another-path');
    await page.locator('#llm-model').fill('another-model'); await page.locator('.llm-test').click();
    await page.waitForFunction(()=>document.querySelector<HTMLButtonElement>('.llm-save')?.disabled === false);
    assert.equal(calls.filter(item=>item.path.endsWith('/test')).length,1);
    assert.equal(lastSettingsBody(calls).token,'');
    await page.locator('#llm-key').fill('old-provider-draft'); await page.locator('[data-llm-provider="kimi"]').click();
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.match(await page.locator('#llm-key').getAttribute('placeholder') ?? '',/新的 API Key/);
    await page.locator('.llm-test').click(); assert.match(await page.locator('.llm-status').textContent() ?? '',/新的 API Key/);
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    assert.equal(calls.length,2,'cross-origin empty-key actions must not send requests');
    await page.locator('#llm-key').fill('new-provider-draft'); await page.locator('.llm-test').click();
    await page.waitForFunction(()=>document.querySelector<HTMLButtonElement>('.llm-save')?.disabled === false);
    assert.equal(lastSettingsBody(calls).token,'new-provider-draft');
    assert.equal(lastSettingsBody(calls).base_url,'https://api.kimi.com/coding');
    await page.locator('#llm-url').fill('https://api.kimi.com/new-path'); assert.equal(await page.locator('#llm-key').inputValue(),'new-provider-draft');
    await page.locator('#llm-url').fill('https://new.example.test/anthropic'); assert.equal(await page.locator('#llm-key').inputValue(),'');
    await page.locator('.llm-test').click(); assert.equal(calls.length,3);
    await page.locator('#llm-url').fill('https://api.deepseek.com/anthropic');
    assert.match(await page.locator('#llm-key').getAttribute('placeholder') ?? '',/留空保留/);
  } finally { await browser.close(); await server.close(); }
});

test('model form only saves after a successful probe and discards unsaved drafts on return', {
  timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for model form integration',
}, async () => {
  const server = await staticServer(), browser = await launch(), calls: SettingsCall[] = [];
  let probeOK = false;
  let modelState: PublicSettings = {has_token:true,base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',connection_mode:'direct',proxy_url:'',reasoning_effort:'max'};
  try {
    const page = await browser.newPage(), errors: string[] = [];
    page.on('pageerror',error => errors.push(error.message));
    await page.route('**/projects',route=>route.fulfill({contentType:'application/json',body:'[]'}));
    await page.route('**/model-settings**',route=>{
      const request=route.request(),path=new URL(request.url()).pathname,body=request.postDataJSON() as SettingsPayload | null;calls.push({path,body});
      if(request.method()==='PUT'){assert.ok(body);const {token,...settings}=body;modelState={...settings,has_token:true};}
      return route.fulfill({contentType:'application/json',body:JSON.stringify(path.endsWith('/test')?{ok:probeOK,message:probeOK?'tools verified':'upstream 401'}:modelState)});
    });
    await page.goto(server.url+'/#llm',{waitUntil:'networkidle'});
    await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
    assert.equal(await page.locator('#llm-effort').inputValue(),'max');
    assert.equal(await page.locator('.llm-proxy-fields').isVisible(),false);
    assert.deepEqual(await page.locator('#llm-effort option').evaluateAll(options => options.map(option => (option as HTMLOptionElement).value)),['low','high','max']);
    await page.locator('#llm-model').fill('saved-model');await page.locator('.llm-test').click();
    await page.waitForFunction(()=>document.querySelector('.llm-status')?.textContent?.includes('401'));
    assert.match(await page.locator('.llm-status').textContent() ?? '',/失败.*401/);
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    probeOK = true; await page.locator('.llm-test').click();
    await page.waitForFunction(()=>document.querySelector<HTMLButtonElement>('.llm-save')?.disabled === false);
    await page.locator('#llm-key').fill('changed-after-verification');
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    const testAndSave = async () => {
      await page.locator('.llm-test').click();await page.locator('.llm-save').click();
      await page.waitForFunction(()=>document.querySelector('.llm-summary-status')?.textContent==='已保存');
    };
    await testAndSave();
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.equal(await page.locator('.llm-return').count(),0);
    assert.equal(await page.locator('.llm-save').isVisible(),true);
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    assert.equal(lastSettingsBody(calls).token,'changed-after-verification');
    assert.equal(lastSettingsBody(calls).protocol,'anthropic');
    assert.ok(calls.filter(call => call.path.endsWith('/test')).every(call => call.body?.protocol === 'anthropic'));
    await page.locator('#llm-connection-mode').selectOption('proxy');
    assert.equal(await page.locator('.llm-proxy-fields').isVisible(),true);
    await page.locator('#llm-effort').selectOption('high');
    for (const [protocol,label] of Object.entries(proxyProtocols)) {
      await page.locator('#llm-proxy-protocol').selectOption(protocol);
      await page.locator('#llm-proxy-host').fill('proxy.test');
      await page.locator('#llm-proxy-port').fill('8443');
      assert.equal(await page.locator('.llm-summary-connection').textContent(),label+' 代理');
      await testAndSave();
      assert.equal(await page.locator('#llm-proxy-protocol').inputValue(),protocol);
      assert.equal(lastSettingsBody(calls).proxy_url,protocol+'://proxy.test:8443');
    }
    await page.locator('#llm-proxy-host').fill('draft-proxy.test');await page.locator('#llm-proxy-port').fill('1081');
    await page.locator('#llm-url').fill('https://api.deepseek.com/draft');await page.locator('#llm-model').fill('draft-model');
    await page.locator('#llm-effort').selectOption('low');
    await page.locator('#llm-key').fill('leave-page-secret');
    const oldKey = await page.locator('#llm-key').elementHandle();
    assert.ok(oldKey);
    await page.locator('.llm-back').click();await page.locator('.llm-page').waitFor({state:'hidden'});
    assert.equal(await oldKey.evaluate(input=>(input as HTMLInputElement).value),'','return clears the key before removing its field');
    assert.equal(await page.locator('#model-summary').textContent(),'DeepSeek · saved-model');
    await page.locator('#model-entry').click();
    await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.equal(await page.locator('#llm-url').inputValue(),'https://api.deepseek.com/anthropic');
    assert.equal(await page.locator('#llm-model').inputValue(),'saved-model');
    assert.equal(await page.locator('#llm-connection-mode').inputValue(),'direct');
    assert.equal(await page.locator('#llm-effort').inputValue(),'max');
    assert.equal(await page.locator('.llm-proxy-fields').isVisible(),false);
    await page.locator('#llm-connection-mode').selectOption('proxy');
    assert.equal(await page.locator('#llm-proxy-protocol').inputValue(),'socks5h');
    assert.equal(await page.locator('#llm-proxy-host').inputValue(),'proxy.test');
    assert.equal(await page.locator('#llm-proxy-port').inputValue(),'8443');
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    await page.locator('.brand').click();await page.locator('.llm-page').waitFor({state:'hidden'});
    assert.deepEqual(errors,[],'leaving via navigation must release the detached form safely');
  } finally {await browser.close();await server.close();}
});
