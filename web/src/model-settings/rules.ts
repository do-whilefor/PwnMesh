export const providers = {
  deepseek: {name: 'DeepSeek', base_url: 'https://api.deepseek.com/anthropic', model: 'deepseek-flash', icon: '/static/assets/provider-deepseek.svg'},
  glm: {name: 'GLM', base_url: 'https://open.bigmodel.cn/api/anthropic', model: 'glm-5.3', icon: '/static/assets/provider-glm.svg'},
  kimi: {name: 'Kimi', base_url: 'https://api.kimi.com/coding', model: 'kimi-for-coding', icon: '/static/assets/provider-kimi.svg'},
  custom: {name: '自定义', base_url: '', model: '', icon: ''},
} as const;
export type Provider = keyof typeof providers;
export const apiProtocols = {anthropic: 'Anthropic Messages API', openai: 'OpenAI Chat Completions'} as const;
export type APIProtocol = keyof typeof apiProtocols;
export type AuthMode = 'api_key' | 'kimi_oauth' | 'glm_coding_plan';
export const proxyProtocols = {http: 'HTTP', https: 'HTTPS', socks5: 'SOCKS5', socks5h: 'SOCKS5h'} as const;
export type ProxyProtocol = keyof typeof proxyProtocols;
export const efforts = {low: '低 / 更快响应', high: '高 / 深入推理', max: '最高 / 充分推理'} as const;
export type ReasoningEffort = keyof typeof efforts;
export const defaultReasoningEffort: ReasoningEffort = 'max';
export interface PublicSettings {
  protocol?: APIProtocol;
  auth_mode?: AuthMode | '';
  base_url: string;
  model: string;
  has_token: boolean;
  connection_mode: 'direct' | 'proxy';
  proxy_url: string;
  reasoning_effort?: ReasoningEffort;
}
export interface SettingsValues {
  protocol?: string;
  auth_mode?: string;
  login_id?: string;
  base_url: string;
  model: string;
  token: string;
  connection_mode: string;
  proxy_protocol: string;
  proxy_host: string;
  proxy_port: string;
  reasoning_effort: string;
}
export interface SettingsPayload {
  protocol: APIProtocol;
  auth_mode: AuthMode;
  login_id?: string;
  base_url: string;
  model: string;
  token: string;
  connection_mode: 'direct' | 'proxy';
  proxy_url: string;
  reasoning_effort: ReasoningEffort;
}
export interface ProxyDraft {protocol: ProxyProtocol; host: string; port: string}
export function originOf(value: string): string {
  try {const url = new URL(value); return ['http:', 'https:'].includes(url.protocol) ? url.origin : '';}
  catch {return '';}
}
export function canReuseToken(settings: PublicSettings | null, base: string, authMode: AuthMode = 'api_key'): boolean {
  const origin = originOf(base);
  return Boolean(settings?.has_token && (settings.auth_mode || 'api_key') === authMode && origin && origin === originOf(settings.base_url));
}
export function providerFor(value: string): Provider {
  const endpoint = normalizeEndpoint(value);
  return (Object.keys(providers) as Provider[]).find(key => providers[key].base_url === endpoint) || 'custom';
}
function normalizeEndpoint(value: string): string {return value.trim().replace(/\/+$/, '');}
export function parseProxy(value?: string): ProxyDraft {
  try {
    const url = new URL(value || ''), protocol = url.protocol.replace(':', '');
    if (!Object.hasOwn(proxyProtocols, protocol)) throw new Error('Unknown proxy protocol');
    return {protocol: protocol as ProxyProtocol, host: url.hostname, port: url.port || (protocol.startsWith('socks5') ? '1080' : protocol === 'https' ? '443' : '80')};
  } catch {return {protocol: 'http', host: 'host.docker.internal', port: '7897'};}
}
export function connectionSettings(values: Pick<SettingsValues, 'connection_mode' | 'proxy_protocol' | 'proxy_host' | 'proxy_port'>): Pick<SettingsPayload, 'connection_mode' | 'proxy_url'> {
  const mode = values.connection_mode;
  if (mode !== 'direct' && mode !== 'proxy') throw new Error('请选择连接方式。');
  let proxy = '';
  if (mode === 'proxy') {
    const host = values.proxy_host.trim(), port = values.proxy_port.trim();
    if (!host || /[\s\\/?#@]/.test(host) || (host.includes(':') && !/^\[[a-f\d:]+\]$/i.test(host))) throw new Error('请填写有效的代理主机，端口单独填写。');
    if (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) throw new Error('代理端口需为 1–65535 的整数。');
    if (!Object.hasOwn(proxyProtocols, values.proxy_protocol)) throw new Error('请选择有效的代理协议。');
    proxy = values.proxy_protocol + '://' + host + ':' + Number(port);
    try {new URL(proxy);} catch {throw new Error('代理地址无效。');}
  }
  return {connection_mode: mode, proxy_url: proxy};
}
export function validate(values: SettingsValues, hasToken: boolean): SettingsPayload {
  const protocol = values.protocol || 'anthropic', authMode = values.auth_mode || 'api_key';
  if (!Object.hasOwn(apiProtocols, protocol)) throw new Error('Please select a supported API protocol.');
  if (!['api_key', 'kimi_oauth', 'glm_coding_plan'].includes(authMode)) throw new Error('Please select a supported authentication method.');
  const base = normalizeEndpoint(values.base_url), model = values.model.trim();
  let url: URL;
  try {url = new URL(base);} catch {throw new Error('请输入完整的 Base URL。');}
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || /[?#]/.test(base) || url.port === '0') throw new Error('Base URL 需为 HTTP(S) 地址和有效端口，不能包含密码、查询或片段。');
  if (authMode !== 'api_key' && (protocol !== 'anthropic' || base !== providers[authMode === 'kimi_oauth' ? 'kimi' : 'glm'].base_url)) throw new Error('Subscription authentication requires the official service endpoint.');
  if (!model || model.length > 200) throw new Error('请输入不超过 200 字符的模型名称。');
  const token = values.token.trim();
  if (authMode === 'kimi_oauth') {
    if (token) throw new Error('Kimi subscription sign-in does not use an API Key.');
    if (!values.login_id && !hasToken) throw new Error('请先登录 Kimi Code 订阅。');
  } else {
    if (!token && !hasToken) throw new Error('请输入 API Key。');
    if (/[\r\n\x00]/.test(token) || token.length > 8192) throw new Error('API Key 格式无效。');
  }
  const effort = values.reasoning_effort || defaultReasoningEffort;
  if (!Object.hasOwn(efforts, effort)) throw new Error('思考强度无效。');
  return {protocol: protocol as APIProtocol, auth_mode: authMode as AuthMode, ...(authMode === 'kimi_oauth' && values.login_id ? {login_id: values.login_id} : {}), base_url: base, model, token, ...connectionSettings(values), reasoning_effort: effort as ReasoningEffort};
}
