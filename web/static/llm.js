(function () {
  'use strict';

  const helpers = window.PwnMeshModelSettings;
  const providers = Object.fromEntries(Object.entries(helpers.providers).map(([id,preset]) => [id,{...preset,url:preset.base_url}]));
  const bindings = new WeakMap();
  const efforts = { low: '低 / 更快响应', high: '高 / 深入推理', max: '最高 / 充分推理' };
  let saved = null;
  let draft = null;
  let dirty = false;
  const providerDefaults = () => Object.fromEntries(Object.entries(providers).map(([id, preset]) => [id, { url: preset.url, model: preset.model }]));
  let providerDrafts = providerDefaults();
  let proxyDraft = { protocol: 'http', host: 'host.docker.internal', port: '7897' };
  const defaults = () => ({ provider: 'deepseek', protocol: 'anthropic', url: providers.deepseek.url, model: providers.deepseek.model, reasoningEffort: helpers.defaultReasoningEffort, connection: { mode: 'direct' } });
  const esc = (value) => String(value).replace(/[&<>"']/g, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[character]));
  const providerIcon = (id, className = '') => providers[id].icon ? `<img class="${className}" src="${providers[id].icon}" alt="" aria-hidden="true">` : `<i class="${className}" data-lucide="settings-2" aria-hidden="true"></i>`;
  const connectionLabel = (connection) => connection.mode === 'proxy' ? `${helpers.proxyProtocols[connection.protocol]} 代理` : '直连';

  function render({ returnLabel = '返回项目', projectName = '' } = {}) {
    const state = draft || saved || defaults();
    const showReturn = Boolean(saved && !dirty);
    const proxy = state.connection.mode === 'proxy' ? state.connection : proxyDraft;
    return `<section class="llm-page" aria-labelledby="llm-title">
      <header class="llm-heading"><h1 id="llm-title">模型接入</h1><button type="button" class="llm-back" data-llm-return title="${esc(projectName ? `${returnLabel} · ${projectName}` : returnLabel)}"><i data-lucide="arrow-left"></i>${esc(returnLabel)}</button></header>
      <div class="llm-layout"><form class="llm-form" novalidate autocomplete="off">
        <section class="llm-section"><div class="llm-section-title"><h2>提供商</h2></div><div class="llm-providers">${Object.entries(providers).map(([id, item]) => `<button class="llm-provider${state.provider === id ? ' is-selected' : ''}" type="button" data-llm-provider="${id}" aria-pressed="${state.provider === id}">${providerIcon(id, 'llm-provider-logo')}<strong>${item.name}</strong><span class="llm-provider-check" aria-hidden="true"><i data-lucide="check"></i></span></button>`).join('')}</div></section>
        <section class="llm-section llm-connection"><div class="llm-section-title"><h2>连接配置</h2></div>
          <div class="llm-field"><label for="llm-protocol">API 协议</label><input id="llm-protocol" name="protocol" type="text" value="Anthropic Messages" readonly aria-readonly="true"></div>
          <div class="llm-field"><label for="llm-url">Base URL</label><input id="llm-url" name="url" type="url" value="${esc(state.url)}" placeholder="https://api.example.com/v1" spellcheck="false"></div>
          <div class="llm-field"><label for="llm-model">模型名称</label><input id="llm-model" name="model" type="text" value="${esc(state.model)}" placeholder="输入模型 ID" spellcheck="false"></div>
          <div class="llm-field"><label for="llm-key">API Key</label><div class="llm-key-wrap"><input id="llm-key" name="key" type="password" value="" placeholder="输入 API Key" autocomplete="off" spellcheck="false"><button type="button" class="llm-key-toggle" aria-label="显示 API Key" aria-pressed="false"><i data-lucide="eye"></i></button></div><p class="llm-field-hint">密钥保存在服务端，页面不会回显；留空保留同一服务的已存密钥。</p></div>
          <div class="llm-field llm-effort"><div class="llm-effort-description"><label for="llm-effort">思考强度</label><p class="llm-field-hint" id="llm-effort-hint">可用强度取决于模型与服务商。</p></div><div class="llm-select-wrap"><select id="llm-effort" name="reasoningEffort" aria-describedby="llm-effort-hint">${Object.entries(efforts).map(([value, label]) => `<option value="${value}"${state.reasoningEffort === value ? ' selected' : ''}>${label}</option>`).join('')}</select><i data-lucide="chevron-down"></i></div></div>
          <div class="llm-field llm-network"><div class="llm-network-row"><label for="llm-connection-mode">连接方式</label><div class="llm-select-wrap"><select id="llm-connection-mode" name="connectionMode"><option value="direct"${state.connection.mode === 'direct' ? ' selected' : ''}>直连</option><option value="proxy"${state.connection.mode === 'proxy' ? ' selected' : ''}>使用代理</option></select><i data-lucide="chevron-down"></i></div></div>
            <div class="llm-proxy-fields"${state.connection.mode === 'direct' ? ' hidden' : ''}>
              <div class="llm-field"><label for="llm-proxy-protocol">代理协议</label><div class="llm-select-wrap"><select id="llm-proxy-protocol" name="proxyProtocol"${state.connection.mode === 'direct' ? ' disabled' : ''}>${Object.entries(helpers.proxyProtocols).map(([value,label]) => `<option value="${value}"${proxy.protocol === value ? ' selected' : ''}>${label}</option>`).join('')}</select><i data-lucide="chevron-down"></i></div></div>
              <div class="llm-field"><label for="llm-proxy-host">主机</label><input id="llm-proxy-host" name="proxyHost" value="${esc(proxy.host)}" placeholder="host.docker.internal" spellcheck="false"${state.connection.mode === 'direct' ? ' disabled' : ''}></div>
              <div class="llm-field"><label for="llm-proxy-port">端口</label><input id="llm-proxy-port" name="proxyPort" value="${esc(proxy.port)}" inputmode="numeric" placeholder="7897"${state.connection.mode === 'direct' ? ' disabled' : ''}></div>
              <p class="llm-field-hint">代理地址需能从服务端访问。Docker 访问宿主机代理可用 host.docker.internal。</p>
            </div>
          </div>
        </section>
        <div class="llm-status" role="status" aria-live="polite"></div>
        <footer class="llm-actions"><div><button class="llm-test" type="button"><i data-lucide="plug"></i>连接测试</button><button class="llm-save" type="submit" disabled${showReturn ? ' hidden' : ''}>保存配置</button><button class="llm-return" type="button" data-llm-return${showReturn ? '' : ' hidden'}>完成并返回<i data-lucide="arrow-right"></i></button></div></footer>
      </form>
      <aside class="llm-aside"><div class="llm-overview"><div class="llm-diagram"><div class="llm-diagram-node"><span class="llm-mini-brand" aria-hidden="true"><img src="/static/brand.png" alt=""></span><strong>PwnMesh</strong></div><div class="llm-diagram-node llm-model-node"><span class="llm-model-symbol" aria-hidden="true">${providerIcon(state.provider)}</span><div><strong class="llm-summary-provider">${providers[state.provider].name}</strong><small class="llm-summary-model">${esc(state.model || '待选择模型')}</small></div><span class="llm-connection-dot"></span></div></div><dl class="llm-summary"><div><dt>状态</dt><dd class="llm-summary-status">${saved ? '已保存' : '待配置'}</dd></div><div><dt>协议</dt><dd class="llm-summary-protocol">Anthropic</dd></div><div><dt>思考强度</dt><dd class="llm-summary-effort">${efforts[state.reasoningEffort]}</dd></div><div><dt>连接方式</dt><dd class="llm-summary-connection">${connectionLabel(state.connection)}</dd></div><div class="llm-summary-proxy"${state.connection.mode === 'direct' ? ' hidden' : ''}><dt>代理地址</dt><dd class="llm-summary-proxy-address">${esc(proxy.host)}:${esc(proxy.port)}</dd></div></dl></div></aside></div>
    </section>`;
  }

  let serverSettings = null;
  const api = new window.PwnMeshAPI.Client();
  const origin = (value) => { try { return new URL(value).origin; } catch { return ''; } };
  function fromServer(settings) {
    const proxy = helpers.parseProxy(settings.proxy_url);
    return {provider: helpers.providerFor(settings.base_url), protocol:'anthropic', url:settings.base_url,
      model:settings.model, reasoningEffort:settings.reasoning_effort || helpers.defaultReasoningEffort,
      connection:settings.connection_mode === 'proxy' ? {mode:'proxy',...proxy} : {mode:'direct'}};
  }
  function bind(host, {onReturn} = {}) {
    bindings.get(host)?.();
    const form = host.querySelector('.llm-form');
    if (!form) return;
    let provider = (draft || saved || defaults()).provider, verified = false, busy = false, loaded = false;
    let generation = 0, disposed = false, request = null, keyOrigin = '';
    const controller = new AbortController(), options = {signal:controller.signal};
    const field = name => form.elements.namedItem(name), query = selector => host.querySelector(selector);
    const icons = () => window.lucide?.createIcons({attrs:{'stroke-width':1.6}});
    const current = () => ({provider,protocol:'anthropic',url:field('url').value.trim(),model:field('model').value.trim(),
      reasoningEffort:field('reasoningEffort').value, connection:field('connectionMode').value === 'proxy'
        ? {mode:'proxy',protocol:field('proxyProtocol').value,host:field('proxyHost').value.trim(),port:field('proxyPort').value.trim()}
        : {mode:'direct'}});
    const status = (kind,message) => {
      query('.llm-status').className = `llm-status${kind ? ' llm-status-'+kind : ''}`;
      query('.llm-status').innerHTML = message ? `<i data-lucide="${kind === 'success' ? 'circle-check' : kind === 'error' ? 'circle-alert' : kind === 'pending' ? 'loader-circle' : 'info'}"></i><span>${esc(message)}</span>` : '';
      icons();
    };
    const remember = (changed = true) => {
      draft = current();
      providerDrafts[provider] = {url:field('url').value,model:field('model').value};
      proxyDraft = {protocol:field('proxyProtocol').value,host:field('proxyHost').value,port:field('proxyPort').value};
      if (changed) dirty = true;
    };
    const sync = () => {
      const state = current(), proxy = state.connection.mode === 'proxy';
      query('.llm-proxy-fields').hidden = !proxy;
      for (const input of form.querySelectorAll('input,select,[data-llm-provider]')) input.disabled = busy || !loaded;
      for (const name of ['proxyProtocol','proxyHost','proxyPort']) {field(name).disabled = busy || !loaded || !proxy;field(name).required = proxy;}
      query('.llm-test').disabled = busy || !loaded;
      query('.llm-save').disabled = busy || !loaded || !verified;
      query('.llm-save').hidden = Boolean(saved && !dirty);
      query('.llm-return').hidden = !(saved && !dirty);
      query('.llm-key-toggle').disabled = busy || !loaded;
      form.querySelectorAll('[data-llm-provider]').forEach(button => {
        const selected = button.dataset.llmProvider === provider;
        button.classList.toggle('is-selected',selected);button.setAttribute('aria-pressed',String(selected));
      });
      const retained = helpers.canReuseToken(serverSettings,state.url);
      field('key').placeholder = retained ? '已保存，留空保留现有密钥' : serverSettings?.has_token ? '服务地址已变更，请输入新的 API Key' : '输入 API Key';
      query('.llm-summary-provider').textContent = providers[provider].name;
      query('.llm-model-symbol').innerHTML = providerIcon(provider);
      query('.llm-summary-model').textContent = state.model || '待选择模型';
      query('.llm-summary-effort').textContent = efforts[state.reasoningEffort];
      query('.llm-summary-connection').textContent = connectionLabel(state.connection);
      query('.llm-summary-proxy').hidden = !proxy;
      query('.llm-summary-proxy-address').textContent = `${field('proxyHost').value}:${field('proxyPort').value}`;
      icons();
    };
    const resetKey = () => {
      field('key').value = '';field('key').type = 'password';keyOrigin = '';
      query('.llm-key-toggle').setAttribute('aria-label','显示 API Key');
      query('.llm-key-toggle').setAttribute('aria-pressed','false');
      query('.llm-key-toggle').innerHTML = '<i data-lucide="eye"></i>';
    };
    const fill = state => {
      provider = state.provider;
      for (const name of ['url','model','reasoningEffort']) field(name).value = state[name];
      field('connectionMode').value = state.connection.mode;
      const proxy = state.connection.mode === 'proxy' ? state.connection : proxyDraft;
      for (const [name,key] of [['proxyProtocol','protocol'],['proxyHost','host'],['proxyPort','port']]) field(name).value = proxy[key];
      resetKey();sync();
    };
    const invalidate = () => {
      generation++;request?.abort();request = null;verified = false;
      query('.llm-connection-dot').classList.remove('is-connected');
      query('.llm-summary-status').textContent = '未保存';status('','');
    };
    const payload = () => {
      const state = current(), retained = helpers.canReuseToken(serverSettings,state.url);
      if (!field('key').value.trim() && serverSettings?.has_token && !retained) throw new Error('服务地址已变更，请输入该服务新的 API Key。');
      return helpers.validate({base_url:state.url,model:state.model,token:field('key').value,reasoning_effort:state.reasoningEffort,
        connection_mode:state.connection.mode,proxy_protocol:field('proxyProtocol').value,proxy_host:field('proxyHost').value,proxy_port:field('proxyPort').value},retained);
    };
    const edit = event => {
      if (event.target === field('key')) keyOrigin = origin(field('url').value);
      if (event.target === field('url') && field('key').value && origin(field('url').value) !== keyOrigin) resetKey();
      remember();invalidate();sync();
    };
    form.addEventListener('input',edit,options);form.addEventListener('change',edit,options);
    form.addEventListener('click',async event => {
      const choice = event.target.closest('[data-llm-provider]');
      if (choice && !busy && loaded) {
        const selected = choice.dataset.llmProvider;if (selected === provider) return;
        remember(false);fill({...current(),provider:selected,...providerDrafts[selected]});remember();invalidate();sync();return;
      }
      if (event.target.closest('.llm-key-toggle')) {
        const reveal = field('key').type === 'password';field('key').type = reveal ? 'text' : 'password';
        query('.llm-key-toggle').setAttribute('aria-label',reveal ? '隐藏 API Key' : '显示 API Key');
        query('.llm-key-toggle').setAttribute('aria-pressed',String(reveal));
        query('.llm-key-toggle').innerHTML = `<i data-lucide="${reveal ? 'eye-off' : 'eye'}"></i>`;icons();return;
      }
      if (!event.target.closest('.llm-test') || busy || !loaded) return;
      invalidate();let body;try {body = payload();} catch(error) {status('error',error.message);sync();return;}
      const version = generation;busy = true;request = new AbortController();sync();
      query('.llm-summary-status').textContent = '连接测试中';status('pending','正在验证 Anthropic Messages 与工具调用…');
      try {
        const result = await api.request('/model-settings/test',{method:'POST',body,signal:request.signal,timeout:60000});
        if (disposed || version !== generation) return;
        verified = result.ok === true;
        query('.llm-summary-status').textContent = verified ? '连接测试通过' : '连接测试失败';
        query('.llm-connection-dot').classList.toggle('is-connected',verified);
        status(verified ? 'success' : 'error',`${verified ? '连接测试通过' : '连接测试失败'}${result.latency_ms != null ? ' · '+result.latency_ms+' ms' : ''}${result.message ? ' · '+result.message : ''}`);
      } catch(error) {if (!disposed && version === generation) {query('.llm-summary-status').textContent = '连接测试失败';status('error','测试失败：'+error.message);}}
      finally {if (!disposed && version === generation) {busy = false;request = null;sync();}}
    },options);
    form.addEventListener('submit',async event => {
      event.preventDefault();if (busy || !loaded) return;
      if (!verified) {status('error','请先完成连接测试，再保存配置。');return;}
      let body;try {body = payload();} catch(error) {status('error',error.message);return;}
      busy = true;request = new AbortController();const version = generation;sync();status('pending','正在保存配置…');
      try {
        const result = await api.request('/model-settings',{method:'PUT',body,signal:request.signal});
        if (disposed || version !== generation) return;
        serverSettings = result;proxyDraft = helpers.parseProxy(result.proxy_url);saved = fromServer(result);draft = saved;dirty = false;verified = false;fill(saved);
        query('.llm-summary-status').textContent = '已保存';status('success','配置已保存，新启动的任务将使用此配置；运行中的任务保持原配置。');
        window.dispatchEvent(new CustomEvent('llm-updated',{detail:{provider:providers[saved.provider].name,model:saved.model,connection:{...saved.connection}}}));
      } catch(error) {if (!disposed && version === generation) status('error','保存失败：'+error.message);}
      finally {if (!disposed && version === generation) {busy = false;request = null;sync();}}
    },options);
    const cleanup = () => {
      if (disposed) return;disposed = true;if (loaded) remember(false);
      generation++;request?.abort();controller.abort();field('key').value = '';keyOrigin = '';observer.disconnect();
      if (bindings.get(host) === cleanup) bindings.delete(host);
    };
    const observer = new MutationObserver(() => {if (!form.isConnected) cleanup();});
    host.addEventListener('click',event => {
      if (!event.target.closest('[data-llm-return]')) return;
      const detail = {saved:Boolean(saved && !dirty),draftRetained:dirty};cleanup();
      if (typeof onReturn === 'function') onReturn(detail);else window.dispatchEvent(new CustomEvent('llm-return',{detail}));
    },options);
    observer.observe(host,{childList:true,subtree:true});bindings.set(host,cleanup);
    busy = true;sync();status('pending','正在读取服务端配置…');
    request = new AbortController();
    api.request('/model-settings',{signal:request.signal}).then(settings => {
      if (disposed) return;
      if (!dirty) proxyDraft = helpers.parseProxy(settings.proxy_url);
      serverSettings = settings;saved = settings.has_token ? fromServer(settings) : null;loaded = true;busy = false;
      fill(dirty && draft ? draft : fromServer(settings));
      query('.llm-summary-status').textContent = dirty ? '未保存' : saved ? '已保存' : '待配置';
    }).catch(error => {if (!disposed) {busy = false;status('error','配置读取失败：'+error.message+'。返回项目后可重新打开重试。');sync();}});
    return cleanup;
  }
  window.PwnLLMDemo = {render,bind,summary:() => saved ? `${providers[saved.provider].name} · ${saved.model}` : '尚未配置模型'};
}());
