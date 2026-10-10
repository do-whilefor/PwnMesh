'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {validate,parseProxy,providerFor,canReuseToken,mount} = require('./static/models.js');
const defaults = {base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',token:'test-only-key',connection_mode:'direct',reasoning_effort:''};
test('model setup uses Anthropic and retains a server-side token only when one exists', () => {
  const result = validate(defaults,false); assert.equal(result.protocol,'anthropic'); assert.equal(result.proxy_url,'');
  assert.equal(validate({...defaults,token:''},true).token,'');
  assert.throws(() => validate({...defaults,token:''},false), /API Key/);
  for (const base_url of ['javascript:alert(1)','https://user:secret@example.com','https://example.com/?key=secret']) assert.throws(() => validate({...defaults,base_url},true), /Base URL/);
});
test('proxy setup targets the Docker host on port 7897 and rejects ambiguous host input', () => {
  assert.deepEqual(parseProxy(''),{protocol:'http',host:'host.docker.internal',port:'7897'});
  const proxy = {...defaults,connection_mode:'proxy',proxy_protocol:'http',proxy_host:'host.docker.internal',proxy_port:'7897'};
  assert.equal(validate(proxy,false).proxy_url,'http://host.docker.internal:7897');
  assert.equal(validate({...proxy,proxy_protocol:'socks5',proxy_host:'[::1]'},false).proxy_url,'socks5://[::1]:7897');
  for (const proxy_host of ['localhost:7897','http://host','user:password@host','host/path','host name']) assert.throws(() => validate({...proxy,proxy_host},false), /代理主机/);
  for (const proxy_port of ['0','65536','7.5','7897/path']) assert.throws(() => validate({...proxy,proxy_port},false), /代理端口/);
});
test('model reasoning choices match the Messages provider contract', () => {
  for (const reasoning_effort of ['low','high','max']) assert.equal(validate({...defaults,reasoning_effort},false).reasoning_effort,reasoning_effort);
  assert.equal(validate({...defaults,reasoning_effort:''},false).reasoning_effort,'high');
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
    await page.setContent('<button id="open-models">模型接入</button><section id="models-page" hidden></section>');
    await page.evaluate(() => {
      window.modelCalls = [];
      window.PwnMeshAPI = {Client:class {async request(path,options={}) {
        window.modelCalls.push({path,options});
        if (path.endsWith('/test')) return {ok:true,latency_ms:5,message:'fixture verified'};
        return {has_token:true,base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',connection_mode:'direct',reasoning_effort:'high'};
      }}};
    });
    await page.addScriptTag({path:require('node:path').join(__dirname,'static/models.js')});
    await page.locator('#open-models').click(); await page.waitForFunction(() => !document.getElementById('llm-test').disabled);
    await page.locator('#llm-url').fill('https://api.deepseek.com/another-path');
    await page.locator('#llm-model').fill('another-model'); await page.locator('#llm-test').click();
    assert.equal(await page.evaluate(() => modelCalls.filter(item => item.path.endsWith('/test')).length),1);
    assert.equal(await page.evaluate(() => modelCalls.at(-1).options.body.token),'');
    await page.locator('#llm-key').fill('old-provider-draft'); await page.locator('[data-provider="kimi"]').click();
    assert.equal(await page.locator('#llm-key').inputValue(),'');
    assert.match(await page.locator('#llm-key').getAttribute('placeholder'),/新的 API Key/);
    await page.locator('#llm-test').click(); assert.match(await page.locator('#llm-status').textContent(),/新的 API Key/);
    await page.locator('#llm-save').click();
    assert.equal(await page.evaluate(() => modelCalls.length),2,'cross-origin empty-key actions must not send requests');
    await page.locator('#llm-key').fill('new-provider-draft'); await page.locator('#llm-test').click();
    assert.equal(await page.evaluate(() => modelCalls.at(-1).options.body.token),'new-provider-draft');
    assert.equal(await page.evaluate(() => modelCalls.at(-1).options.body.base_url),'https://api.kimi.com/coding');
    await page.locator('#llm-url').fill('https://api.kimi.com/new-path'); assert.equal(await page.locator('#llm-key').inputValue(),'new-provider-draft');
    await page.locator('#llm-url').fill('https://new.example.test/anthropic'); assert.equal(await page.locator('#llm-key').inputValue(),'');
    await page.locator('#llm-test').click(); assert.equal(await page.evaluate(() => modelCalls.length),3);
    await page.locator('#llm-url').fill('https://api.deepseek.com/anthropic');
    assert.match(await page.locator('#llm-key').getAttribute('placeholder'),/留空保留/);
  } finally { await browser.close(); }
});
