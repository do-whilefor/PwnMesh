(function (root, factory) {
  'use strict';
  const library = factory();
  if (typeof module === 'object' && module.exports) module.exports = library;
  else root.PwnMeshModelSettings = library;
}(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  const providers = {
    deepseek: {name:'DeepSeek', base_url:'https://api.deepseek.com/anthropic', model:'deepseek-flash', icon:'/static/assets/provider-deepseek.svg'},
    glm: {name:'GLM', base_url:'https://open.bigmodel.cn/api/anthropic', model:'glm-5.3', icon:'/static/assets/provider-glm.svg'},
    kimi: {name:'Kimi', base_url:'https://api.kimi.com/coding', model:'kimi-for-coding', icon:'/static/assets/provider-kimi.svg'},
    custom: {name:'自定义', base_url:'', model:''}
  };
  const proxyProtocols = {http:'HTTP',https:'HTTPS',socks5:'SOCKS5',socks5h:'SOCKS5h'};
  const defaultReasoningEffort = 'max';
  function validate(values, hasToken) {
    const base = String(values.base_url || '').trim(), model = String(values.model || '').trim();
    let url; try { url = new URL(base); } catch { throw new Error('请输入完整的 Base URL。'); }
    if (!['http:','https:'].includes(url.protocol) || url.username || url.password || /[?#]/.test(base) || url.port === '0') throw new Error('Base URL 需为 HTTP(S) 地址和有效端口，不能包含密码、查询或片段。');
    if (!model || model.length > 200) throw new Error('请输入不超过 200 字符的模型名称。');
    const token = String(values.token || '').trim();
    if (!token && !hasToken) throw new Error('请输入 API Key。');
    if (/[\r\n\x00]/.test(token) || token.length > 8192) throw new Error('API Key 格式无效。');
    const mode = values.connection_mode;
    if (!['direct','proxy'].includes(mode)) throw new Error('请选择连接方式。');
    let proxy = '';
    if (mode === 'proxy') {
      const host = String(values.proxy_host || '').trim(), port = String(values.proxy_port || '').trim();
      if (!host || /[\s\\/?#@]/.test(host) || (host.includes(':') && !/^\[[a-f\d:]+\]$/i.test(host))) throw new Error('请填写有效的代理主机，端口单独填写。');
      if (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) throw new Error('代理端口需为 1–65535 的整数。');
      if (!Object.hasOwn(proxyProtocols,values.proxy_protocol)) throw new Error('请选择有效的代理协议。');
      proxy = values.proxy_protocol + '://' + host + ':' + Number(port);
      try { new URL(proxy); } catch { throw new Error('代理地址无效。'); }
    }
    if (!['low','high','max'].includes(values.reasoning_effort || defaultReasoningEffort)) throw new Error('思考强度无效。');
    return {protocol:'anthropic',base_url:base.replace(/\/+$/,''),model,token,connection_mode:mode,proxy_url:proxy,reasoning_effort:values.reasoning_effort || defaultReasoningEffort};
  }
  function parseProxy(value) {
    try { const url = new URL(value); return {protocol:url.protocol.replace(':',''),host:url.hostname,port:url.port || (url.protocol.startsWith('socks5') ? '1080' : url.protocol === 'https:' ? '443' : '80')}; }
    catch { return {protocol:'http',host:'host.docker.internal',port:'7897'}; }
  }
  function originOf(value) { try { const url = new URL(value); return ['http:','https:'].includes(url.protocol) ? url.origin : ''; } catch { return ''; } }
  function canReuseToken(settings, base) { const origin = originOf(base); return !!(settings?.has_token && origin && origin === originOf(settings.base_url)); }
  function providerFor(value) { return Object.keys(providers).find(key => providers[key].base_url === value) || 'custom'; }
  return {validate,parseProxy,providerFor,providers,canReuseToken,proxyProtocols,defaultReasoningEffort};
}));
