// @vitest-environment happy-dom
import {flushPromises, mount} from '@vue/test-utils';
import {afterEach, describe, expect, it, vi} from 'vitest';
import {Client} from '@/api/client';
import ModelSettingsPage from './ModelSettingsPage.vue';
import {kimiLogin} from './kimi-login';
import {canReuseToken, parseProxy, providers, validate} from './rules';
import type {PublicSettings, SettingsPayload} from './rules';

const settings = (): PublicSettings => ({base_url: 'https://api.deepseek.com/anthropic', model: 'deepseek-flash', has_token: true, connection_mode: 'direct', proxy_url: '', reasoning_effort: 'max'});
const response = (value: unknown) => new Response(JSON.stringify(value), {headers: {'Content-Type': 'application/json'}});
afterEach(() => vi.useRealTimers());
function fixture(initial = settings()) {
  const calls: {path: string; method: string; body?: Record<string, unknown>}[] = [];
  const client = new Client(async (input, options) => {
    const path = String(input), method = options?.method || 'GET';
    calls.push({path, method, body: typeof options?.body === 'string' ? JSON.parse(options.body) as Record<string, unknown> : undefined});
    if (method === 'PUT') {
      const {token: _token, ...saved} = JSON.parse(String(options?.body)) as SettingsPayload;
      initial = {...saved, has_token: true};
    }
    return response(path.endsWith('/test') ? {ok: true, latency_ms: 12, message: 'verified'} : initial);
  });
  return {client, calls};
}
describe('model settings page', () => {
  it.each([['kimi', 'kimi_oauth'], ['glm', 'glm_coding_plan']] as const)('reloads a saved %s subscription with a trailing slash into its provider controls', async (provider, authMode) => {
    const preset = providers[provider], {client, calls} = fixture({...settings(), protocol: 'anthropic', auth_mode: authMode, base_url: preset.base_url + '///', model: preset.model});
    const wrapper = mount(ModelSettingsPage, {props: {client}});
    try {
      await flushPromises();
      expect(wrapper.get('[data-llm-provider="' + provider + '"]').attributes('aria-pressed')).toBe('true');
      expect(wrapper.get<HTMLSelectElement>('#llm-auth-mode').element.value).toBe(authMode);
      await wrapper.get('.llm-test').trigger('click'); await flushPromises();
      expect(calls.at(-1)?.body).toMatchObject({auth_mode: authMode, base_url: preset.base_url, token: ''});
    } finally {wrapper.unmount();}
  });
  it('omits the regular key hint without leaving an empty row and retains plan-specific guidance', async () => {
    const {client} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}});
    await flushPromises();
    expect(wrapper.find('.llm-key-wrap + .llm-field-hint').exists()).toBe(false);
    await wrapper.get('[data-llm-provider="glm"]').trigger('click');
    expect(wrapper.find('.llm-key-wrap + .llm-field-hint').exists()).toBe(false);
    await wrapper.get('#llm-auth-mode').setValue('glm_coding_plan');
    expect(wrapper.get('.llm-key-wrap + .llm-field-hint').text()).toContain('Coding Plan');
    await wrapper.get('#llm-auth-mode').setValue('api_key');
    expect(wrapper.find('.llm-key-wrap + .llm-field-hint').exists()).toBe(false);
    wrapper.unmount();
  });
  it('requires a successful test for the exact draft and clears the key after save', async () => {
    const {client, calls} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}});
    await flushPromises();
    expect(wrapper.find('#llm-auth-mode').exists()).toBe(false);
    expect(wrapper.find('.llm-login').exists()).toBe(false);
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(true);
    await wrapper.get('.llm-form').trigger('submit');
    expect(calls).toHaveLength(1);
    expect(wrapper.get('.llm-status').text()).toContain('请先完成连接测试');
    await wrapper.get('#llm-key').setValue('temporary-secret');
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(false);
    await wrapper.get('#llm-model').setValue('edited-model');
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(true);
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    await wrapper.get('.llm-form').trigger('submit'); await flushPromises();
    expect(calls.filter(call => call.method === 'PUT')).toHaveLength(1);
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('');
    expect(wrapper.get('.llm-summary-status').text()).toBe('已保存');
    expect(wrapper.get('.llm-connection-dot').classes()).toContain('is-connected');
    expect(wrapper.emitted('saved')?.at(-1)).toEqual(['DeepSeek · edited-model']);
    wrapper.unmount();
  });
  it('tests and saves the selected API protocol without rewriting the endpoint and reloads it', async () => {
    const {client, calls} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}});
    await flushPromises();
    expect(wrapper.get<HTMLSelectElement>('#llm-protocol').element.value).toBe('anthropic');
    expect(wrapper.findAll('#llm-protocol option').map(option => option.text())).toEqual(['Anthropic Messages API', 'OpenAI Chat Completions']);
    await wrapper.get('#llm-url').setValue('https://api.deepseek.com/v1');
    await wrapper.get('#llm-model').setValue('custom-chat');
    await wrapper.get('#llm-key').setValue('saved-key');
    await wrapper.get('#llm-protocol').setValue('openai');
    expect(wrapper.get<HTMLInputElement>('#llm-url').element.value).toBe('https://api.deepseek.com/v1');
    expect(wrapper.get<HTMLInputElement>('#llm-model').element.value).toBe('custom-chat');
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('saved-key');
    expect(wrapper.find('#llm-effort').exists()).toBe(true);
    expect(wrapper.find('.llm-summary-effort').exists()).toBe(true);
    expect(wrapper.get('.llm-summary-protocol').text()).toBe('OpenAI');
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    expect(calls.at(-1)?.body?.protocol).toBe('openai');
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(false);
    await wrapper.get('#llm-protocol').setValue('anthropic');
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(true);
    expect(wrapper.find('#llm-effort').exists()).toBe(true);
    await wrapper.get('#llm-protocol').setValue('openai');
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    await wrapper.get('.llm-form').trigger('submit'); await flushPromises();
    expect(calls.at(-1)?.method).toBe('PUT');
    expect(calls.at(-1)?.body?.protocol).toBe('openai');
    wrapper.unmount();
    const reopened = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    expect(reopened.get<HTMLSelectElement>('#llm-protocol').element.value).toBe('openai');
    expect(reopened.get<HTMLInputElement>('#llm-url').element.value).toBe('https://api.deepseek.com/v1');
    await reopened.get('[data-llm-provider="glm"]').trigger('click');
    expect(reopened.get<HTMLSelectElement>('#llm-protocol').element.value).toBe('anthropic');
    await reopened.get('[data-llm-provider="custom"]').trigger('click');
    expect(reopened.get<HTMLSelectElement>('#llm-protocol').element.value).toBe('openai');
    expect(reopened.get<HTMLInputElement>('#llm-url').element.value).toBe('https://api.deepseek.com/v1');
    reopened.unmount();
  });
  it('binds secret drafts to their origin and preserves provider drafts without sharing keys', async () => {
    const {client} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    await wrapper.get('#llm-key').setValue('secret');
    await wrapper.get('#llm-url').setValue('https://api.deepseek.com/another-path');
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('secret');
    await wrapper.get('#llm-url').setValue('https://other.invalid/anthropic');
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('');
    await wrapper.get('.llm-test').trigger('click');
    expect(wrapper.get('.llm-status').text()).toContain('请输入该服务新的 API Key');
    await wrapper.get('#llm-key').setValue('other-secret');
    await wrapper.get('[data-llm-provider="glm"]').trigger('click');
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('');
    await wrapper.get('[data-llm-provider="deepseek"]').trigger('click');
    expect(wrapper.get<HTMLInputElement>('#llm-url').element.value).toBe('https://other.invalid/anthropic');
    wrapper.unmount();
  });
  it('keeps reasoning effort visible and selected across protocols, providers, and subscription modes', async () => {
    const {client} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    await wrapper.get('#llm-effort').setValue('high');
    const expectEffort = () => {
      expect(wrapper.find('#llm-effort').exists()).toBe(true);
      expect(wrapper.get<HTMLSelectElement>('#llm-effort').element.disabled).toBe(false);
      expect(wrapper.get<HTMLSelectElement>('#llm-effort').element.value).toBe('high');
      expect(wrapper.get('.llm-summary-effort').text()).toBe(wrapper.get('#llm-effort option[value="high"]').text());
    };
    await wrapper.get('#llm-protocol').setValue('openai'); expectEffort();
    expect(wrapper.findAll('#llm-effort option').map(option => option.text())).toEqual(['低 / 更快响应', '中 / 均衡推理', '高 / 深入推理']);
    expect(wrapper.get('.llm-summary-effort').text()).toBe('中 / 均衡推理');
    await wrapper.get('[data-llm-provider="kimi"]').trigger('click'); expectEffort();
    expect(wrapper.get('.llm-summary-effort').text()).toBe('高 / 深入推理');
    await wrapper.get('#llm-auth-mode').setValue('kimi_oauth'); expectEffort();
    await wrapper.get('#llm-auth-mode').setValue('api_key'); expectEffort();
    await wrapper.get('[data-llm-provider="glm"]').trigger('click'); expectEffort();
    await wrapper.get('#llm-auth-mode').setValue('glm_coding_plan'); expectEffort();
    await wrapper.get('[data-llm-provider="deepseek"]').trigger('click'); expectEffort();
    wrapper.unmount();
  });
  it('enters with direct/max while keeping the saved proxy draft and aborts late tests on return', async () => {
    const initial = {...settings(), connection_mode: 'proxy' as const, proxy_url: 'socks5h://host.docker.internal:7897', reasoning_effort: 'high' as const};
    let complete: ((result: Response) => void) | undefined, signal: AbortSignal | null | undefined;
    const client = new Client(async (input, options) => {
      if (!String(input).endsWith('/test')) return response(initial);
      signal = options?.signal;
      return new Promise<Response>(resolve => {complete = resolve;});
    });
    const wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    expect(wrapper.get<HTMLSelectElement>('#llm-connection-mode').element.value).toBe('direct');
    expect(wrapper.get<HTMLSelectElement>('#llm-effort').element.value).toBe('max');
    expect(wrapper.get<HTMLInputElement>('#llm-proxy-port').element.value).toBe('7897');
    await wrapper.get('#llm-key').setValue('dispose-secret');
    const key = wrapper.get<HTMLInputElement>('#llm-key').element;
    await wrapper.get('.llm-test').trigger('click');
    await wrapper.get('.llm-back').trigger('click');
    expect(signal?.aborted).toBe(true); expect(key.value).toBe(''); expect(wrapper.emitted('return')).toHaveLength(1);
    complete?.(response({ok: true})); await flushPromises();
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(true);
    wrapper.unmount();
  });
  it('never treats failed loads as editable configuration', async () => {
    const client = new Client(vi.fn(async () => new Response('{}', {status: 503})));
    const wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    expect(wrapper.get('.llm-status').text()).toContain('配置读取失败');
    expect(wrapper.get<HTMLButtonElement>('.llm-test').element.disabled).toBe(true);
    wrapper.unmount();
  });
  it.each(['load', 'test', 'save', 'logout'] as const)('ignores a late %s response after return without publishing new settings', async operation => {
    let finish: ((result: Response) => void) | undefined, signal: AbortSignal | null | undefined;
    const initial = operation === 'logout' ? {...settings(), auth_mode: 'kimi_oauth' as const, base_url: providers.kimi.base_url, model: providers.kimi.model} : settings();
    const client = new Client(async (input, options) => {
      const path = String(input), pending = operation === 'load' && options?.method === 'GET'
        || operation === 'test' && path.endsWith('/test')
        || operation === 'save' && options?.method === 'PUT'
        || operation === 'logout' && path.endsWith('/logout');
      if (pending) {signal = options?.signal; return new Promise<Response>(resolve => {finish = resolve;});}
      return response(path.endsWith('/test') ? {ok: true} : initial);
    });
    const wrapper = mount(ModelSettingsPage, {props: {client}});
    try {
      await flushPromises();
      if (operation === 'test' || operation === 'save') {await wrapper.get('.llm-test').trigger('click'); await flushPromises();}
      if (operation === 'save') await wrapper.get('.llm-form').trigger('submit');
      if (operation === 'logout') await wrapper.get('.llm-logout').trigger('click');
      await wrapper.get('.llm-back').trigger('click');
      expect(signal?.aborted).toBe(true); expect(wrapper.emitted('return')).toHaveLength(1);
      finish?.(response(operation === 'test' ? {ok: true} : {...initial, model: 'Stale result'})); await flushPromises();
      expect(wrapper.emitted('saved') || []).toHaveLength(operation === 'load' ? 0 : 1);
      expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(true);
    } finally {wrapper.unmount();}
  });
});
describe('subscription authentication', () => {
  const kimiSettings = (): PublicSettings => ({...settings(), base_url: providers.kimi.base_url, model: providers.kimi.model, has_token: false});
  function fixture(initial = kimiSettings(), polls = [{status: 'pending'}, {status: 'authorized'}]) {
    const calls: {path: string; method: string; body?: Record<string, unknown>}[] = [];
    const client = new Client(async (input, options) => {
      const path = String(input), method = options?.method || 'GET';
      const body = typeof options?.body === 'string' ? JSON.parse(options.body) as Record<string, unknown> : undefined;
      calls.push({path, method, body});
      if (method === 'DELETE') return new Response(null, {status: 204});
      if (path === '/model-settings/kimi/login') return response({login_id: 'opaque-login', user_code: 'CODE-1234', verification_uri: 'https://www.kimi.com/code/authorize', interval: 5, expires_in: 60});
      if (path === '/model-settings/kimi/login/opaque-login') return response(polls.shift() || {status: 'pending'});
      if (path === '/model-settings/kimi/logout') initial = {...initial, has_token: false};
      if (method === 'PUT') {
        const {token: _token, login_id: _id, ...saved} = body as unknown as SettingsPayload;
        initial = {...saved, has_token: true};
      }
      return response(path.endsWith('/test') ? {ok: true} : initial);
    });
    return {client, calls};
  }
  it('authorizes a Kimi device code before testing and saving, then reuses only the server credential', async () => {
    vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
    const {client, calls} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    expect(wrapper.get('label[for="llm-auth-mode"]').text()).toBe('接入方式');
    expect(wrapper.findAll('#llm-auth-mode option').map(option => option.text())).toEqual(['API Key', '订阅（Kimi Code）']);
    await wrapper.get('#llm-key').setValue('discard-this-key');
    await wrapper.get('#llm-protocol').setValue('openai');
    await wrapper.get('#llm-auth-mode').setValue('kimi_oauth');
    expect(wrapper.find('#llm-key').exists()).toBe(false);
    expect(wrapper.get<HTMLSelectElement>('#llm-protocol').element.value).toBe('anthropic');
    expect(wrapper.get<HTMLInputElement>('#llm-url').element.disabled).toBe(true);
    expect(wrapper.get<HTMLButtonElement>('.llm-test').element.disabled).toBe(true);
    await wrapper.get('.llm-login-start').trigger('click'); await flushPromises();
    expect(calls.at(-1)?.body).toEqual({connection_mode: 'direct', proxy_url: ''});
    expect(wrapper.get('.llm-login-code').text()).toBe('CODE-1234');
    expect(wrapper.get('.llm-login-link').attributes('href')).toBe('https://www.kimi.com/code/authorize');
    await vi.advanceTimersByTimeAsync(5000); await flushPromises();
    expect(wrapper.get<HTMLButtonElement>('.llm-test').element.disabled).toBe(true);
    await vi.advanceTimersByTimeAsync(5000); await flushPromises();
    expect(wrapper.get<HTMLButtonElement>('.llm-test').element.disabled).toBe(false);
    expect(wrapper.get<HTMLButtonElement>('.llm-save').element.disabled).toBe(true);
    expect(calls.some(call => call.method === 'PUT' || call.path.endsWith('/test'))).toBe(false);
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    expect(calls.at(-1)?.body).toMatchObject({auth_mode: 'kimi_oauth', login_id: 'opaque-login', token: ''});
    await wrapper.get('.llm-form').trigger('submit'); await flushPromises();
    expect(calls.at(-1)?.method).toBe('PUT');
    expect(calls.at(-1)?.body?.login_id).toBe('opaque-login');
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    expect(calls.at(-1)?.body?.login_id).toBeUndefined();
    wrapper.unmount();
    expect(calls.some(call => call.method === 'DELETE')).toBe(false);
    const reopened = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    expect(reopened.get<HTMLSelectElement>('#llm-auth-mode').element.value).toBe('kimi_oauth');
    expect(reopened.find('#llm-key').exists()).toBe(false);
    await reopened.get('.llm-logout').trigger('click'); await flushPromises();
    expect(calls.at(-1)?.path).toBe('/model-settings/kimi/logout');
    expect(reopened.get<HTMLButtonElement>('.llm-test').element.disabled).toBe(true);
    expect(reopened.find('.llm-logout').exists()).toBe(false);
    reopened.unmount();
  });
  it('requires a dedicated GLM plan key and never starts OAuth for GLM or DeepSeek', async () => {
    const {client, calls} = fixture({...settings(), base_url: providers.glm.base_url, model: providers.glm.model});
    const wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    expect(wrapper.get('label[for="llm-auth-mode"]').text()).toBe('接入方式');
    expect(wrapper.findAll('#llm-auth-mode option').map(option => option.text())).toEqual(['API Key', '订阅（GLM Coding Plan）']);
    await wrapper.get('#llm-key').setValue('api-key-draft');
    await wrapper.get('#llm-auth-mode').setValue('glm_coding_plan');
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('');
    expect(wrapper.get<HTMLSelectElement>('#llm-protocol').element.disabled).toBe(true);
    expect(wrapper.get<HTMLInputElement>('#llm-url').element.value).toBe(providers.glm.base_url);
    expect(wrapper.find('#llm-effort').exists()).toBe(true);
    expect(wrapper.find('.llm-login-start').exists()).toBe(false);
    expect(wrapper.find('a[href="https://docs.bigmodel.cn/cn/coding-plan/extension/coding-tool-helper"]').exists()).toBe(true);
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    expect(calls).toHaveLength(1);
    await wrapper.get('#llm-key').setValue('dedicated-plan-key');
    await wrapper.get('.llm-test').trigger('click'); await flushPromises();
    expect(calls.at(-1)?.body).toMatchObject({auth_mode: 'glm_coding_plan', token: 'dedicated-plan-key', protocol: 'anthropic'});
    expect(calls.at(-1)?.body?.login_id).toBeUndefined();
    await wrapper.get('#llm-auth-mode').setValue('api_key');
    expect(wrapper.get<HTMLInputElement>('#llm-key').element.value).toBe('');
    await wrapper.get('[data-llm-provider="deepseek"]').trigger('click');
    expect(wrapper.find('#llm-auth-mode').exists()).toBe(false);
    expect(wrapper.find('a[href*="coding"]').exists()).toBe(false);
    expect(wrapper.get<HTMLInputElement>('#llm-url').element.value).toBe(providers.deepseek.base_url);
    expect(wrapper.find('#llm-effort').exists()).toBe(true);
    expect(calls.some(call => call.path.includes('/kimi/'))).toBe(false);
    wrapper.unmount();
  });
  it('revokes an unsaved authorized login when returning without saving', async () => {
    vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
    const {client, calls} = fixture(kimiSettings(), [{status: 'authorized'}]), wrapper = mount(ModelSettingsPage, {props: {client}});
    await flushPromises(); await wrapper.get('#llm-auth-mode').setValue('kimi_oauth');
    await wrapper.get('.llm-login-start').trigger('click'); await flushPromises();
    await vi.advanceTimersByTimeAsync(5000); await flushPromises();
    await wrapper.get('.llm-back').trigger('click'); await flushPromises();
    expect(calls.at(-1)).toMatchObject({method: 'DELETE', path: '/model-settings/kimi/login/opaque-login'});
    wrapper.unmount();
    expect(calls.filter(call => call.method === 'DELETE')).toHaveLength(1);
  });
  it('ignores a late authorization response after a provider switch and aborts polling', async () => {
    vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
    let finish: ((value: Response) => void) | undefined, signal: AbortSignal | null | undefined;
    const calls: string[] = [];
    const client = new Client(async (input, options) => {
      const path = String(input); calls.push((options?.method || 'GET') + ' ' + path);
      if (options?.method === 'DELETE') return new Response(null, {status: 204});
      if (path.endsWith('/login')) return response({login_id: 'late-login', user_code: '1234', verification_uri: 'https://kimi.com/code', expires_in: 60, interval: 5});
      if (path.endsWith('/late-login')) {signal = options?.signal; return new Promise<Response>(resolve => {finish = resolve;});}
      return response(kimiSettings());
    });
    const wrapper = mount(ModelSettingsPage, {props: {client}}); await flushPromises();
    await wrapper.get('#llm-auth-mode').setValue('kimi_oauth');
    await wrapper.get('.llm-login-start').trigger('click'); await flushPromises();
    await vi.advanceTimersByTimeAsync(5000); await flushPromises();
    await wrapper.get('[data-llm-provider="deepseek"]').trigger('click'); await flushPromises();
    expect(signal?.aborted).toBe(true);
    expect(calls).toContain('DELETE /model-settings/kimi/login/late-login');
    finish?.(response({status: 'authorized'})); await flushPromises();
    await wrapper.get('[data-llm-provider="kimi"]').trigger('click');
    expect(wrapper.get<HTMLButtonElement>('.llm-test').element.disabled).toBe(true);
    expect(wrapper.find('.llm-login-code').exists()).toBe(false);
    await vi.advanceTimersByTimeAsync(15000);
    expect(calls.filter(call => call === 'POST /model-settings/kimi/login/late-login')).toHaveLength(1);
    wrapper.unmount();
  });
});
it('respects device poll backoff and expires a pending login without another upstream poll', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
  const calls: string[] = [];
  const client = new Client(async (input, options) => {
    const path = String(input); calls.push((options?.method || 'GET') + ' ' + path);
    if (options?.method === 'DELETE') return new Response(null, {status: 204});
    if (path.endsWith('/login')) return response({login_id: 'slow-login', user_code: '1234', verification_uri: 'https://kimi.com/code', expires_in: 12, interval: 2});
    return response({status: 'slow_down', interval: 8});
  });
  const login = kimiLogin(client, vi.fn());
  await login.start({connection_mode: 'direct', proxy_url: ''});
  await vi.advanceTimersByTimeAsync(2000);
  expect(calls.filter(call => call === 'POST /model-settings/kimi/login/slow-login')).toHaveLength(1);
  await vi.advanceTimersByTimeAsync(7999);
  expect(calls.filter(call => call === 'POST /model-settings/kimi/login/slow-login')).toHaveLength(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(calls.filter(call => call === 'POST /model-settings/kimi/login/slow-login')).toHaveLength(2);
  await vi.advanceTimersByTimeAsync(2000);
  expect(login.state.phase).toBe('error');
  expect(login.state.id).toBe('');
  expect(calls.at(-1)).toBe('DELETE /model-settings/kimi/login/slow-login');
  expect(calls.filter(call => call === 'POST /model-settings/kimi/login/slow-login')).toHaveLength(2);
});
it('revokes a late device grant after cancellation without displaying its code or scheduling polls', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
  let finish: ((value: Response) => void) | undefined, signal: AbortSignal | null | undefined;
  const calls: string[] = [];
  const client = new Client(async (input, options) => {
    calls.push((options?.method || 'GET') + ' ' + String(input));
    if (options?.method === 'DELETE') return new Response(null, {status: 204});
    signal = options?.signal;
    return new Promise<Response>(resolve => {finish = resolve;});
  });
  const login = kimiLogin(client, vi.fn());
  const start = login.start({connection_mode: 'direct', proxy_url: ''});
  login.cancel(); expect(signal?.aborted).toBe(true);
  finish?.(response({login_id: 'late-grant', user_code: '1234', verification_uri: 'https://kimi.com/code', expires_in: 60}));
  await start; await flushPromises(); await vi.advanceTimersByTimeAsync(10000);
  expect(login.state.phase).toBe('idle'); expect(login.state.code).toBe('');
  expect(calls).toEqual(['POST /model-settings/kimi/login', 'DELETE /model-settings/kimi/login/late-grant']);
});

it('allows login start and polls to complete within the server budget while cancellation stays immediate', async () => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
  const signals: AbortSignal[] = [];
  const client = new Client(async (input, options) => {
    if (options?.method === 'DELETE') return new Response(null, {status: 204});
    const signal = options?.signal as AbortSignal; signals.push(signal);
    return new Promise<Response>((resolve, reject) => {
      const timer = setTimeout(() => resolve(response(String(input).endsWith('/login')
        ? {login_id: 'slow-grant', user_code: '1234', verification_uri: 'https://kimi.com/code', expires_in: 60, interval: 1}
        : {status: 'authorized'})), 20000);
      signal.addEventListener('abort', () => {clearTimeout(timer); reject(signal.reason);}, {once: true});
    });
  });
  const login = kimiLogin(client, vi.fn());
  try {
    const start = login.start({connection_mode: 'direct', proxy_url: ''});
    await vi.advanceTimersByTimeAsync(20000); await start;
    expect(login.state.phase).toBe('pending'); expect(signals[0]?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(21000);
    expect(login.state.phase).toBe('authorized'); expect(signals[1]?.aborted).toBe(false);
    const restarted = login.start({connection_mode: 'direct', proxy_url: ''});
    login.cancel(); expect(signals.at(-1)?.aborted).toBe(true);
    await restarted; expect(login.state.phase).toBe('idle');
  } finally {login.cancel();}
});
it('validates proxy and credential reuse boundaries independently of rendering', () => {
  expect(canReuseToken(settings(), 'https://api.deepseek.com/new')).toBe(true);
  expect(canReuseToken(settings(), 'http://api.deepseek.com/new')).toBe(false);
  expect(canReuseToken({...settings(), auth_mode: 'kimi_oauth'}, settings().base_url)).toBe(false);
  expect(canReuseToken({...settings(), auth_mode: 'glm_coding_plan'}, settings().base_url, 'api_key')).toBe(false);
  expect(parseProxy('socks5h://localhost')).toEqual({protocol: 'socks5h', host: 'localhost', port: '1080'});
  const values = {base_url: settings().base_url, model: 'model', token: '', connection_mode: 'proxy', proxy_protocol: 'http', proxy_host: 'host.docker.internal', proxy_port: '7897', reasoning_effort: 'max'};
  expect(validate(values, true).proxy_url).toBe('http://host.docker.internal:7897');
  expect(validate(values, true).protocol).toBe('anthropic');
  expect(validate({...values, protocol: 'openai'}, true).protocol).toBe('openai');
  expect(() => validate({...values, protocol: 'unsupported'}, true)).toThrow('supported API protocol');
  expect(() => validate({...values, auth_mode: 'kimi_oauth'}, true)).toThrow('official service endpoint');
  expect(() => validate({...values, auth_mode: 'kimi_oauth', base_url: providers.kimi.base_url}, false)).toThrow('请先登录');
  expect(() => validate({...values, proxy_host: 'host:7897'}, true)).toThrow('端口单独填写');
  expect(() => validate({...values, token: 'key\nvalue'}, true)).toThrow('API Key 格式无效');
});
