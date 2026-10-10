'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {validate,parseProxy,providerFor,canReuseToken,proxyProtocols} = require('./static/models.js');
const defaults = {base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',token:'test-only-key',connection_mode:'direct',reasoning_effort:''};
test('model setup uses Anthropic and retains a server-side token only when one exists', () => {
  const result = validate(defaults,false); assert.equal(result.protocol,'anthropic'); assert.equal(result.proxy_url,'');
  assert.equal(validate({...defaults,token:''},true).token,'');
  assert.throws(() => validate({...defaults,token:''},false), /API Key/);
  for (const base_url of ['javascript:alert(1)','https://user:secret@example.com','https://example.com/?key=secret','https://example.com/?','https://example.com/#','https://example.com:0','https://example.com:65536']) assert.throws(() => validate({...defaults,base_url},true), /Base URL/);
  assert.throws(() => validate({...defaults,model:'x'.repeat(201)},true),/模型名称/);
  for (const token of ['key\nvalue','key\rvalue','key\0value','x'.repeat(8193)]) assert.throws(() => validate({...defaults,token},true),/API Key/);
});
test('proxy setup targets the Docker host on port 7897 and rejects ambiguous host input', () => {
  assert.deepEqual(parseProxy(''),{protocol:'http',host:'host.docker.internal',port:'7897'});
  const proxy = {...defaults,connection_mode:'proxy',proxy_protocol:'http',proxy_host:'host.docker.internal',proxy_port:'7897'};
  assert.equal(validate(proxy,false).proxy_url,'http://host.docker.internal:7897');
  assert.equal(validate({...proxy,proxy_protocol:'socks5',proxy_host:'[::1]'},false).proxy_url,'socks5://[::1]:7897');
  for (const [protocol,port] of [['http','80'],['https','443'],['socks5','1080'],['socks5h','1080']]) {
    assert.deepEqual(parseProxy(protocol+'://proxy.test'),{protocol,host:'proxy.test',port});
    assert.equal(validate({...proxy,proxy_protocol:protocol},false).proxy_url,protocol+'://host.docker.internal:7897');
  }
  for (const proxy_host of ['localhost:7897','http://host','user:password@host','host/path','host name']) assert.throws(() => validate({...proxy,proxy_host},false), /代理主机/);
  for (const proxy_port of ['0','65536','7.5','7897/path']) assert.throws(() => validate({...proxy,proxy_port},false), /代理端口/);
});
test('model reasoning choices match the Messages provider contract', () => {
  for (const reasoning_effort of ['low','high','max']) assert.equal(validate({...defaults,reasoning_effort},false).reasoning_effort,reasoning_effort);
  assert.equal(validate({...defaults,reasoning_effort:''},false).reasoning_effort,'max');
  assert.throws(() => validate({...defaults,reasoning_effort:'medium'},false), /思考强度/);
  assert.equal(providerFor('https://api.deepseek.com/anthropic'),'deepseek'); assert.equal(providerFor('https://private.test'),'custom');
});

test('saved credentials can only be reused for the same HTTP origin', () => {
  const saved = {has_token:true,base_url:'https://api.deepseek.com/anthropic'};
  for (const base of ['https://api.deepseek.com/v2/messages','https://API.DEEPSEEK.COM:443/another-path']) {
    assert.equal(canReuseToken(saved,base),true);
    assert.equal(validate({...defaults,base_url:base,token:''},canReuseToken(saved,base)).token,'');
  }
  for (const base of ['https://api.kimi.com/coding','http://api.deepseek.com/anthropic','https://api.deepseek.com:8443/anthropic','https://api.deepseek.com.evil.test','invalid','']) {
    assert.equal(canReuseToken(saved,base),false);
    assert.throws(() => validate({...defaults,base_url:base,token:''},canReuseToken(saved,base)),/API Key|Base URL/);
  }
  assert.equal(canReuseToken({...saved,has_token:false},saved.base_url),false);
  assert.equal(canReuseToken(null,saved.base_url),false);
});

test('model browser blocks cross-origin saved credentials and clears provider drafts', {
  timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE to check model credential binding in a browser',
}, async () => {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
  const browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}});
    await page.setContent('<main id="main"></main>');
    await page.evaluate(() => {
      window.modelCalls = [];
      window.PwnMeshAPI = {Client:class {async request(path,options={}) {
        window.modelCalls.push({path,options});
        if (path.endsWith('/test')) return {ok:true,latency_ms:5,message:'fixture verified'};
        return {has_token:true,base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',connection_mode:'direct',reasoning_effort:'high'};
      }}};
    });
    await page.addScriptTag({path:require('node:path').join(__dirname,'static/models.js')});
    await page.addScriptTag({path:require('node:path').join(__dirname,'static/llm.js')});
    await page.evaluate(() => {const host = document.getElementById('main');host.innerHTML = PwnLLMDemo.render();PwnLLMDemo.bind(host);});
    await page.waitForFunction(() => !document.querySelector('.llm-test').disabled);
    await page.locator('#llm-url').fill('https://api.deepseek.com/another-path');
    await page.locator('#llm-model').fill('another-model'); await page.locator('.llm-test').click();
    assert.equal(await page.evaluate(() => modelCalls.filter(item => item.path.endsWith('/test')).length),1);
    assert.equal(await page.evaluate(() => modelCalls.at(-1).options.body.token),'');
    await page.locator('#llm-key').fill('old-provider-draft'); await page.locator('[data-llm-provider="kimi"]').click();
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.match(await page.locator('#llm-key').getAttribute('placeholder'),/新的 API Key/);
    await page.locator('.llm-test').click(); assert.match(await page.locator('.llm-status').textContent(),/新的 API Key/);
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    assert.equal(await page.evaluate(() => modelCalls.length),2,'cross-origin empty-key actions must not send requests');
    await page.locator('#llm-key').fill('new-provider-draft'); await page.locator('.llm-test').click();
    assert.equal(await page.evaluate(() => modelCalls.at(-1).options.body.token),'new-provider-draft');
    assert.equal(await page.evaluate(() => modelCalls.at(-1).options.body.base_url),'https://api.kimi.com/coding');
    await page.locator('#llm-url').fill('https://api.kimi.com/new-path'); assert.equal(await page.locator('#llm-key').inputValue(),'new-provider-draft');
    await page.locator('#llm-url').fill('https://new.example.test/anthropic'); assert.equal(await page.locator('#llm-key').inputValue(),'');
    await page.locator('.llm-test').click(); assert.equal(await page.evaluate(() => modelCalls.length),3);
    await page.locator('#llm-url').fill('https://api.deepseek.com/anthropic');
    assert.match(await page.locator('#llm-key').getAttribute('placeholder'),/留空保留/);
  } finally { await browser.close(); }
});

test('the Demo model form only saves a successful real probe and cancels stale work on return', {
  timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for model form integration',
}, async () => {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE);
  const browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
  try {
    const page = await browser.newPage();await page.setContent('<main id="main"></main>');
    const errors = [];page.on('pageerror',error => errors.push(error.message));
    await page.evaluate(() => {
      window.calls = [];window.probeOK = false;
      window.modelState = {has_token:true,base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',connection_mode:'direct',reasoning_effort:'max'};
      window.PwnMeshAPI = {Client:class {async request(path,options={}) {
        window.calls.push({path,body:options.body});
        if (path.endsWith('/test')) return {ok:window.probeOK,message:window.probeOK ? 'tools verified' : 'upstream 401'};
        if (options.method === 'PUT') {const {token,...settings} = options.body;window.modelState = {...settings,has_token:true};}
        return window.modelState;
      }}};
    });
    for (const name of ['models.js','llm.js']) await page.addScriptTag({path:require('node:path').join(__dirname,'static',name)});
    await page.evaluate(() => {const host = document.getElementById('main');host.innerHTML = PwnLLMDemo.render();PwnLLMDemo.bind(host,{onReturn:() => host.replaceChildren()});});
    await page.waitForFunction(() => !document.querySelector('.llm-test').disabled);
    assert.equal(await page.locator('#llm-effort').inputValue(),'max');
    assert.deepEqual(await page.locator('#llm-effort option').evaluateAll(options => options.map(option => option.value)),['low','high','max']);
    await page.locator('#llm-model').fill('saved-model');await page.locator('.llm-test').click();
    assert.match(await page.locator('.llm-status').textContent(),/失败.*401/);
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    await page.evaluate(() => {window.probeOK = true;});await page.locator('.llm-test').click();
    assert.equal(await page.locator('.llm-save').isEnabled(),true);
    await page.locator('#llm-key').fill('changed-after-verification');
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    await page.locator('.llm-test').click();await page.locator('.llm-save').click();
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.equal(await page.locator('.llm-return').isVisible(),true);
    assert.equal(await page.evaluate(() => calls.at(-1).body.token),'changed-after-verification');
    await page.locator('#llm-connection-mode').selectOption('proxy');
    for (const [protocol,label] of Object.entries(proxyProtocols)) {
      await page.locator('#llm-proxy-protocol').selectOption(protocol);
      await page.locator('#llm-proxy-host').fill('proxy.test');
      await page.locator('#llm-proxy-port').fill('8443');
      assert.equal(await page.locator('.llm-summary-connection').textContent(),label+' 代理');
      await page.locator('.llm-test').click();await page.locator('.llm-save').click();
      assert.equal(await page.locator('#llm-proxy-protocol').inputValue(),protocol);
      assert.equal(await page.evaluate(() => calls.at(-1).body.proxy_url),protocol+'://proxy.test:8443');
    }
    await page.locator('#llm-proxy-host').fill('draft-proxy.test');await page.locator('#llm-proxy-port').fill('1081');
    await page.locator('#llm-connection-mode').selectOption('direct');
    await page.locator('#llm-key').fill('leave-page-secret');await page.locator('.llm-back').click();
    await page.evaluate(() => {const host = document.getElementById('main');host.innerHTML = PwnLLMDemo.render();PwnLLMDemo.bind(host);});
    await page.waitForFunction(() => !document.querySelector('.llm-test').disabled);
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.equal(await page.locator('#llm-model').inputValue(),'saved-model');
    assert.equal(await page.locator('#llm-connection-mode').inputValue(),'direct');
    await page.locator('#llm-connection-mode').selectOption('proxy');
    assert.equal(await page.locator('#llm-proxy-protocol').inputValue(),'socks5h');
    assert.equal(await page.locator('#llm-proxy-host').inputValue(),'draft-proxy.test');
    assert.equal(await page.locator('#llm-proxy-port').inputValue(),'1081');
    assert.equal(await page.locator('.llm-save').isDisabled(),true);
    await page.evaluate(() => {document.getElementById('main').innerHTML = '<section>project</section>';});
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
    assert.deepEqual(errors,[],'leaving via project navigation must release the detached form safely');
  } finally {await browser.close();}
});
