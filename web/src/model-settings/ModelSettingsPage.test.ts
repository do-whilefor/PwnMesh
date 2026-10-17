// @vitest-environment happy-dom
import {flushPromises, mount} from '@vue/test-utils';
import {describe, expect, it, vi} from 'vitest';
import {Client} from '@/api/client';
import ModelSettingsPage from './ModelSettingsPage.vue';
import {canReuseToken, parseProxy, validate} from './rules';
import type {PublicSettings, SettingsPayload} from './rules';

const settings = (): PublicSettings => ({base_url: 'https://api.deepseek.com/anthropic', model: 'deepseek-flash', has_token: true, connection_mode: 'direct', proxy_url: '', reasoning_effort: 'max'});
const response = (value: unknown) => new Response(JSON.stringify(value), {headers: {'Content-Type': 'application/json'}});
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
  it('requires a successful test for the exact draft and clears the key after save', async () => {
    const {client, calls} = fixture(), wrapper = mount(ModelSettingsPage, {props: {client}});
    await flushPromises();
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
    expect(wrapper.find('#llm-effort').exists()).toBe(false);
    expect(wrapper.find('.llm-summary-effort').exists()).toBe(false);
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
});
it('validates proxy and credential reuse boundaries independently of rendering', () => {
  expect(canReuseToken(settings(), 'https://api.deepseek.com/new')).toBe(true);
  expect(canReuseToken(settings(), 'http://api.deepseek.com/new')).toBe(false);
  expect(parseProxy('socks5h://localhost')).toEqual({protocol: 'socks5h', host: 'localhost', port: '1080'});
  const values = {base_url: settings().base_url, model: 'model', token: '', connection_mode: 'proxy', proxy_protocol: 'http', proxy_host: 'host.docker.internal', proxy_port: '7897', reasoning_effort: 'max'};
  expect(validate(values, true).proxy_url).toBe('http://host.docker.internal:7897');
  expect(validate(values, true).protocol).toBe('anthropic');
  expect(validate({...values, protocol: 'openai'}, true).protocol).toBe('openai');
  expect(() => validate({...values, protocol: 'unsupported'}, true)).toThrow('supported API protocol');
  expect(() => validate({...values, proxy_host: 'host:7897'}, true)).toThrow('端口单独填写');
  expect(() => validate({...values, token: 'key\nvalue'}, true)).toThrow('API Key 格式无效');
});
