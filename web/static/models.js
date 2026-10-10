(function (root, factory) {
  'use strict';
  const library = factory();
  if (typeof module === 'object' && module.exports) module.exports = library;
  else { root.PwnMeshModelSettings = library; if (root.document) library.mount(root); }
}(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  const providers = {
    deepseek: {name:'DeepSeek', base_url:'https://api.deepseek.com/anthropic', model:'deepseek-flash', mark:'D'},
    glm: {name:'GLM', base_url:'https://open.bigmodel.cn/api/anthropic', model:'glm-5.3', mark:'G'},
    kimi: {name:'Kimi', base_url:'https://api.kimi.com/coding', model:'kimi-for-coding', mark:'K'},
    custom: {name:'自定义', base_url:'', model:'', mark:'+'}
  };
  function validate(values, hasToken) {
    const base = String(values.base_url || '').trim(), model = String(values.model || '').trim();
    let url; try { url = new URL(base); } catch { throw new Error('请输入完整的 Base URL。'); }
    if (!['http:','https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error('Base URL 需为 HTTP(S) 地址，不能包含密码、查询或片段。');
    if (!model) throw new Error('请输入模型名称。');
    const token = String(values.token || '').trim();
    if (!token && !hasToken) throw new Error('请输入 API Key。');
    const mode = values.connection_mode;
    if (!['direct','proxy'].includes(mode)) throw new Error('请选择连接方式。');
    let proxy = '';
    if (mode === 'proxy') {
      const host = String(values.proxy_host || '').trim(), port = String(values.proxy_port || '').trim();
      if (!host || /[\s\\/?#@]/.test(host) || (host.includes(':') && !/^\[[a-f\d:]+\]$/i.test(host))) throw new Error('请填写有效的代理主机，端口单独填写。');
      if (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) throw new Error('代理端口需为 1–65535 的整数。');
      if (!['http','https','socks5'].includes(values.proxy_protocol)) throw new Error('请选择有效的代理协议。');
      proxy = values.proxy_protocol + '://' + host + ':' + Number(port);
      try { new URL(proxy); } catch { throw new Error('代理地址无效。'); }
    }
    if (!['low','high','max'].includes(values.reasoning_effort || 'high')) throw new Error('思考强度无效。');
    return {protocol:'anthropic',base_url:base.replace(/\/+$/,''),model,token,connection_mode:mode,proxy_url:proxy,reasoning_effort:values.reasoning_effort || 'high'};
  }
  function parseProxy(value) {
    try { const url = new URL(value); return {protocol:url.protocol.replace(':',''),host:url.hostname,port:url.port || (url.protocol === 'https:' ? '443' : '80')}; }
    catch { return {protocol:'http',host:'host.docker.internal',port:'7897'}; }
  }
  function originOf(value) { try { const url = new URL(value); return ['http:','https:'].includes(url.protocol) ? url.origin : ''; } catch { return ''; } }
  function canReuseToken(settings, base) { const origin = originOf(base); return !!(settings?.has_token && origin && origin === originOf(settings.base_url)); }
  function providerFor(value) { return Object.keys(providers).find(key => providers[key].base_url === value) || 'custom'; }
  function mount(root) {
    const document = root.document, $ = id => document.getElementById(id), host = $('models-page');
    if (!host) return;
    const api = new root.PwnMeshAPI.Client(); let saved = null, loaded = false, busy = false, dirty = false, operation = 0, keyOrigin = '';
    host.innerHTML = `<header class="models-heading"><div><h1>模型接入</h1><p>配置驱动项目探索的模型与连接方式。</p></div><button class="button secondary" id="llm-return" type="button">← 返回项目</button></header>
      <div class="models-layout"><form id="llm-form" novalidate autocomplete="off">
        <section class="models-section"><h2>提供商</h2><div class="model-providers">${Object.entries(providers).map(([id,item]) => `<button type="button" class="model-provider" data-provider="${id}" aria-pressed="false"><span class="provider-symbol provider-${id}">${id === 'custom' ? item.mark : `<img src="/static/provider-${id}.svg" alt="">`}</span><strong>${item.name}</strong><span class="provider-check">✓</span></button>`).join('')}</div></section>
        <section class="models-section"><h2>连接配置</h2><div class="model-field"><label for="llm-protocol">API 协议</label><input id="llm-protocol" value="Anthropic Messages" readonly></div>
          <div class="model-field"><label for="llm-url">Base URL</label><input id="llm-url" type="url" placeholder="https://api.example.com/anthropic" spellcheck="false" required></div>
          <div class="model-field"><label for="llm-model">模型名称</label><input id="llm-model" placeholder="输入模型 ID" spellcheck="false" required></div>
          <div class="model-field"><label for="llm-key">API Key</label><input id="llm-key" type="password" placeholder="输入 API Key" autocomplete="new-password" spellcheck="false"><p class="model-hint" id="llm-key-hint">密钥保存在服务端，页面不会回显。</p></div>
          <div class="model-field model-inline"><label for="llm-effort">思考强度</label><select id="llm-effort"><option value="low">低 / 更快响应</option><option value="high">高 / 深入推理</option><option value="max">最高 / 充分推理</option></select></div>
          <p class="model-hint effort-hint">可用强度取决于模型与服务商。</p>
          <div class="model-field model-inline model-network"><label for="llm-connection-mode">连接方式</label><select id="llm-connection-mode"><option value="direct">直连</option><option value="proxy">使用代理</option></select></div>
          <div id="llm-proxy-fields" class="model-proxy-fields" hidden><div class="model-field"><label for="llm-proxy-protocol">代理协议</label><select id="llm-proxy-protocol"><option value="http">HTTP</option><option value="https">HTTPS</option><option value="socks5">SOCKS5</option></select></div><div class="model-field"><label for="llm-proxy-host">主机</label><input id="llm-proxy-host" value="host.docker.internal" spellcheck="false"></div><div class="model-field"><label for="llm-proxy-port">端口</label><input id="llm-proxy-port" value="7897" inputmode="numeric"></div></div>
          <p class="model-hint network-hint" id="llm-network-hint">连接配置在新运行中生效。</p>
        </section>
        <p id="llm-status" class="model-status" role="status" aria-live="polite"></p>
        <footer class="model-actions"><button type="button" class="button secondary" id="llm-test"><svg class="icon"><use href="#i-plug"/></svg>模拟测试</button><button type="submit" class="button primary" id="llm-save">保存配置</button></footer>
      </form>
      <aside class="models-overview"><div class="model-diagram"><span class="model-mini-brand"><img src="/static/brand.png" alt="PwnMesh"></span><span class="model-diagram-link"></span><div class="model-endpoint"><span class="provider-symbol" id="llm-provider-symbol">D</span><div><strong id="llm-summary-provider">DeepSeek</strong><small id="llm-summary-model">待配置</small></div><i></i></div></div><dl class="model-summary"><div><dt>状态</dt><dd id="llm-summary-status">读取配置</dd></div><div><dt>协议</dt><dd>Anthropic Messages</dd></div><div><dt>连接方式</dt><dd id="llm-summary-connection">直连</dd></div><div id="llm-summary-proxy-row" hidden><dt>代理地址</dt><dd id="llm-summary-proxy"></dd></div><div><dt>生效范围</dt><dd>新调度 / 新 Worker</dd></div></dl><p class="model-overview-note">模拟测试会向当前模型发起真实工具调用往返，验证协议、密钥与连接是否正常。</p><div id="llm-test-result" class="model-test-result" hidden></div></aside></div>`;
    const fields = ['llm-url','llm-model','llm-key','llm-effort','llm-connection-mode','llm-proxy-protocol','llm-proxy-host','llm-proxy-port'];
    function status(message, kind = '') { $('llm-status').textContent = message; $('llm-status').dataset.kind = kind; }
    function sync() {
      const proxy = $('llm-connection-mode').value === 'proxy', provider = providers[providerFor($('llm-url').value)];
      const retainedToken = canReuseToken(saved, $('llm-url').value);
      $('llm-key').placeholder = retainedToken ? '已保存，留空保留现有密钥' : saved?.has_token ? '服务地址已变更，请输入新的 API Key' : '输入 API Key';
      $('llm-key-hint').textContent = retainedToken ? '已有密钥保存在服务端。留空即可继续使用；输入新值后保存会替换。' : saved?.has_token ? 'Base URL 已切换到其他服务，请输入该服务的新密钥。原有密钥不会发送到新地址。' : '密钥保存在服务端，页面不会回显。';
      $('llm-proxy-fields').hidden = !proxy; $('llm-summary-proxy-row').hidden = !proxy;
      $('llm-network-hint').textContent = proxy ? '模型和 Worker 的 HTTP(S) 请求使用代理；新运行生效。Docker 访问本机代理请使用 host.docker.internal:7897，并允许局域网连接。' : '模型与 Worker 直接连接网络；新运行生效。';
      $('llm-summary-provider').textContent = provider.name; $('llm-provider-symbol').replaceChildren();
      const providerId = providerFor($('llm-url').value);
      if (providerId === 'custom') $('llm-provider-symbol').textContent = provider.mark; else { const logo = document.createElement('img'); logo.src = '/static/provider-' + providerId + '.svg'; logo.alt = ''; $('llm-provider-symbol').append(logo); }
      $('llm-summary-model').textContent = $('llm-model').value || '待配置';
      $('llm-summary-connection').textContent = proxy ? '使用代理' : '直连';
      $('llm-summary-proxy').textContent = $('llm-proxy-host').value + ':' + $('llm-proxy-port').value;
      $('llm-summary-status').textContent = !loaded ? '配置未加载' : dirty ? '待保存' : saved?.has_token ? '已保存' : '待配置';
      host.querySelectorAll('[data-provider]').forEach(button => button.setAttribute('aria-pressed', String(button.dataset.provider === providerFor($('llm-url').value))));
      for (const id of fields) $(id).disabled = busy || !loaded || (!proxy && id.startsWith('llm-proxy-'));
      host.querySelectorAll('[data-provider]').forEach(button => { button.disabled = busy || !loaded; });
      $('llm-save').disabled = busy || !loaded; $('llm-test').disabled = busy || !loaded; $('llm-return').disabled = busy;
    }
    function apply(settings) {
      saved = settings; loaded = true; dirty = false;
      $('llm-url').value = settings.base_url || providers.deepseek.base_url; $('llm-model').value = settings.model || providers.deepseek.model;
      $('llm-key').value = ''; keyOrigin = '';
      $('llm-effort').value = settings.reasoning_effort || 'high'; $('llm-connection-mode').value = settings.connection_mode || 'direct';
      const proxy = parseProxy(settings.proxy_url); $('llm-proxy-protocol').value = proxy.protocol; $('llm-proxy-host').value = proxy.host; $('llm-proxy-port').value = proxy.port; sync();
    }
    function values() {
      const retainedToken = canReuseToken(saved, $('llm-url').value);
      if (!$('llm-key').value.trim() && saved?.has_token && !retainedToken) throw new Error('服务地址已变更，请输入该服务新的 API Key。');
      return validate({base_url:$('llm-url').value,model:$('llm-model').value,token:$('llm-key').value,reasoning_effort:$('llm-effort').value,connection_mode:$('llm-connection-mode').value,proxy_protocol:$('llm-proxy-protocol').value,proxy_host:$('llm-proxy-host').value,proxy_port:$('llm-proxy-port').value}, retainedToken); }
    async function open() {
      host.hidden = false; document.body.classList.add('models-open');
      if (loaded) { sync(); return; }
      const version = ++operation; busy = true; status('正在读取服务端配置…'); sync();
      try { const settings = await api.request('/model-settings'); if (version !== operation) return; apply(settings); status(''); }
      catch (error) { if (version === operation) status('配置读取失败：' + error.message + '。返回项目后可重新打开重试。', 'error'); }
      finally { if (version === operation) { busy = false; sync(); } }
    }
    function close() { if (busy) return; host.hidden = true; document.body.classList.remove('models-open'); $('llm-key').value = ''; keyOrigin = ''; }
    root.PwnMeshModels = {open,close};
    $('open-models').addEventListener('click', open); $('llm-return').addEventListener('click', close);
    for (const id of fields) $(id).addEventListener('input', () => {
      if (id === 'llm-key') keyOrigin = originOf($('llm-url').value);
      if (id === 'llm-url' && $('llm-key').value && keyOrigin !== originOf($('llm-url').value)) { $('llm-key').value = ''; keyOrigin = ''; }
      dirty = true; status(''); $('llm-test-result').hidden = true; sync();
    });
    $('llm-connection-mode').addEventListener('change', () => { dirty = true; sync(); });
    host.querySelectorAll('[data-provider]').forEach(button => button.addEventListener('click', () => {
      const provider = providers[button.dataset.provider]; $('llm-key').value = ''; keyOrigin = ''; $('llm-url').value = provider.base_url; $('llm-model').value = provider.model; dirty = true; status(''); $('llm-test-result').hidden = true; sync(); $('llm-url').focus();
    }));
    $('llm-form').addEventListener('submit', async event => {
      event.preventDefault(); if (busy || !loaded) return;
      let payload; try { payload = values(); } catch (error) { status(error.message, 'error'); return; }
      busy = true; status('正在保存配置…'); sync();
      try { const settings = await api.request('/model-settings', {method:'PUT',body:payload}); apply(settings); status('配置已保存，新的模型请求与 Worker 将使用此配置。', 'success'); }
      catch (error) { status(error.message, 'error'); }
      finally { busy = false; sync(); }
    });
    $('llm-test').addEventListener('click', async () => {
      if (busy || !loaded) return;
      let payload; try { payload = values(); } catch (error) { status(error.message, 'error'); return; }
      busy = true; status('正在发送 Anthropic Messages 测试请求…'); $('llm-test-result').hidden = true; sync();
      try {
        const result = await api.request('/model-settings/test', {method:'POST',body:payload,timeout:120000});
        const text = result.ok ? '连接成功 · ' + result.latency_ms + ' ms' : '连接失败';
        status(text + (result.message ? ' · ' + result.message : ''), result.ok ? 'success' : 'error');
        const panel = $('llm-test-result'); panel.hidden = false; panel.dataset.kind = result.ok ? 'success' : 'error'; panel.replaceChildren();
        const title = document.createElement('strong'), detail = document.createElement('p'); title.textContent = text; detail.textContent = result.message || '模型已返回有效响应。'; panel.append(title,detail);
      } catch (error) { status('测试失败：' + error.message, 'error'); }
      finally { busy = false; sync(); }
    });
  }
  return {validate,parseProxy,providerFor,providers,canReuseToken,mount};
}));
