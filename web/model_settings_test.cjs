'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {validate,parseProxy,providerFor,canReuseToken} = require('./static/model-settings.js');
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
