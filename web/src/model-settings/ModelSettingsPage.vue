<script setup lang="ts">
import {computed, onBeforeUnmount, onMounted, reactive, ref} from 'vue';
import {Client} from '@/api/client';
import Icon from '@/ui/Icon.vue';
import {apiProtocols, canReuseToken, connectionSettings, defaultReasoningEffort, efforts, originOf, parseProxy, providerFor, providers, proxyProtocols, validate} from './rules';
import type {APIProtocol, AuthMode, Provider, PublicSettings, ReasoningEffort} from './rules';
import {kimiLogin} from './kimi-login';

const props = withDefaults(defineProps<{returnLabel?: string; projectName?: string; client?: Pick<Client, 'request'>}>(), {returnLabel: '返回项目', projectName: ''});
const emit = defineEmits<{return: []; saved: [summary: string]}>();
const client = props.client || new Client();
const provider = ref<Provider>('deepseek');
const values = reactive({apiProtocol: 'anthropic' as APIProtocol, authMode: 'api_key' as AuthMode, url: providers.deepseek.base_url as string, model: providers.deepseek.model as string, reasoningEffort: defaultReasoningEffort as ReasoningEffort, connectionMode: 'direct' as 'direct' | 'proxy', ...parseProxy()});
const token = ref(''), revealKey = ref(false), verified = ref(false), busy = ref(true), loaded = ref(false);
const connected = ref(false);
const keyInput = ref<HTMLInputElement>();
const serverSettings = ref<PublicSettings | null>(null);
const summaryStatus = ref('待配置'), statusKind = ref(''), statusMessage = ref('');
type ProviderDraft = {apiProtocol: APIProtocol; authMode: AuthMode; url: string; model: string};
const providerDrafts = Object.fromEntries(Object.entries(providers).map(([id, preset]) => [id, {apiProtocol: 'anthropic', authMode: 'api_key', url: preset.base_url, model: preset.model}])) as Record<Provider, ProviderDraft>;
let version = 0, disposed = false, request: AbortController | null = null, keyOrigin = '';
const disabled = computed(() => busy.value || !loaded.value);
const retained = computed(() => canReuseToken(serverSettings.value, values.url.trim(), values.authMode));
const subscription = computed(() => values.authMode !== 'api_key');
const oauth = computed(() => values.authMode === 'kimi_oauth');
const showEffort = computed(() => values.apiProtocol === 'anthropic' && values.authMode !== 'glm_coding_plan');
const login = kimiLogin(client, invalidate);
const oauthReady = computed(() => login.state.phase === 'authorized' || (retained.value && ['idle', 'error'].includes(login.state.phase)));
const keyPlaceholder = computed(() => retained.value ? '已保存，留空保留现有密钥' : serverSettings.value?.has_token ? '服务地址已变更，请输入新的 API Key' : '输入 API Key');
const statusIcon = computed(() => ({success: 'circle-check', error: 'circle-alert', pending: 'loader-circle'}[statusKind.value] || 'info'));
const providerIDs = Object.keys(providers) as Provider[];
const apiProtocolIDs = Object.keys(apiProtocols) as APIProtocol[];
const effortIDs = Object.keys(efforts) as ReasoningEffort[];
const proxyIDs = Object.keys(proxyProtocols) as (keyof typeof proxyProtocols)[];
const message = (error: unknown) => error instanceof Error ? error.message : '请求失败';
function status(kind: string, text: string) {statusKind.value = kind; statusMessage.value = text;}
function resetKey() {token.value = ''; if (keyInput.value) keyInput.value.value = ''; revealKey.value = false; keyOrigin = '';}
function invalidate() {version++; request?.abort(); request = null; verified.value = false; connected.value = false; summaryStatus.value = '未保存'; status('', '');}
function rememberDraft() {providerDrafts[provider.value] = {apiProtocol: values.apiProtocol, authMode: values.authMode, url: values.url, model: values.model};}
function edit(event: Event) {
  const field = event.target;
  if (field instanceof HTMLSelectElement && field.name === 'authMode') return;
  if (field instanceof HTMLInputElement && field.name === 'key') keyOrigin = originOf(values.url);
  if (field instanceof HTMLInputElement && field.name === 'url' && token.value && originOf(values.url) !== keyOrigin) resetKey();
  if ((field instanceof HTMLInputElement || field instanceof HTMLSelectElement) && ['connectionMode', 'proxyProtocol', 'proxyHost', 'proxyPort'].includes(field.name)) login.cancel();
  rememberDraft();
  invalidate();
}
function selectProvider(selected: Provider) {
  if (disabled.value || selected === provider.value) return;
  rememberDraft(); login.cancel();
  provider.value = selected;
  Object.assign(values, providerDrafts[selected]);
  resetKey(); invalidate();
}
function selectAuth() {
  login.cancel(); resetKey();
  if (subscription.value) {values.apiProtocol = 'anthropic'; values.url = providers[values.authMode === 'kimi_oauth' ? 'kimi' : 'glm'].base_url;}
  rememberDraft(); invalidate();
}
function network() {return connectionSettings({connection_mode: values.connectionMode, proxy_protocol: values.protocol, proxy_host: values.host, proxy_port: values.port});}
async function startLogin() {
  if (disabled.value || !oauth.value) return;
  try {await login.start(network());} catch (error) {status('error', message(error));}
}
function cancelLogin() {login.cancel(); invalidate();}
async function logout() {
  if (disabled.value) return;
  cancelLogin(); const currentVersion = version; request = new AbortController(); busy.value = true;
  try {
    const result = await client.request<PublicSettings>('/model-settings/kimi/logout', {method: 'POST', signal: request.signal});
    if (disposed || currentVersion !== version) return;
    fill(result); summaryStatus.value = '待配置'; status('success', 'Kimi 订阅已断开，可重新登录。');
  } catch (error) {if (!disposed && currentVersion === version) status('error', message(error));}
  finally {if (!disposed && currentVersion === version) {busy.value = false; request = null;}}
}
function payload() {
  if (!oauth.value && !token.value.trim() && serverSettings.value?.has_token && !retained.value) throw new Error('服务地址或接入方式已变更，请输入该服务新的 API Key。');
  return validate({protocol: values.apiProtocol, auth_mode: values.authMode, login_id: login.state.phase === 'authorized' ? login.state.id : undefined, base_url: values.url, model: values.model, token: token.value, reasoning_effort: values.reasoningEffort, connection_mode: values.connectionMode, proxy_protocol: values.protocol, proxy_host: values.host, proxy_port: values.port}, retained.value);
}
function fill(settings: PublicSettings, entering = false) {
  serverSettings.value = settings; provider.value = providerFor(settings.base_url);
  Object.assign(values, {apiProtocol: settings.protocol || 'anthropic', authMode: settings.auth_mode || 'api_key', url: settings.base_url, model: settings.model, reasoningEffort: entering ? defaultReasoningEffort : settings.reasoning_effort || defaultReasoningEffort, connectionMode: entering ? 'direct' : settings.connection_mode}, parseProxy(settings.proxy_url));
  rememberDraft();
  resetKey();
  emit('saved', settings.has_token ? `${providers[provider.value].name} · ${settings.model}` : '尚未配置模型');
}
async function testConnection() {
  if (disabled.value) return;
  invalidate();
  let body;
  try {body = payload();} catch (error) {status('error', message(error)); return;}
  const currentVersion = version; request = new AbortController(); busy.value = true;
  summaryStatus.value = '连接测试中'; status('pending', `正在验证 ${apiProtocols[values.apiProtocol]} 与工具调用…`);
  try {
    const result = await client.request<{ok: boolean; latency_ms?: number; message?: string}>('/model-settings/test', {method: 'POST', body, signal: request.signal, timeout: 60000});
    if (disposed || currentVersion !== version) return;
    verified.value = result.ok === true; connected.value = verified.value; summaryStatus.value = verified.value ? '连接测试通过' : '连接测试失败';
    status(verified.value ? 'success' : 'error', `${summaryStatus.value}${result.latency_ms != null ? ' · ' + result.latency_ms + ' ms' : ''}${result.message ? ' · ' + result.message : ''}`);
  } catch (error) {if (!disposed && currentVersion === version) {summaryStatus.value = '连接测试失败'; status('error', '测试失败：' + message(error));}}
  finally {if (!disposed && currentVersion === version) {busy.value = false; request = null;}}
}
async function save() {
  if (disabled.value) return;
  if (!verified.value) {status('error', '请先完成连接测试，再保存配置。'); return;}
  let body;
  try {body = payload();} catch (error) {status('error', message(error)); return;}
  const currentVersion = version; request = new AbortController(); busy.value = true; status('pending', '正在保存配置…');
  try {
    const result = await client.request<PublicSettings>('/model-settings', {method: 'PUT', body, signal: request.signal});
    if (disposed || currentVersion !== version) return;
    login.cancel(false); fill(result); verified.value = false; summaryStatus.value = '已保存'; status('success', '配置已保存，新启动的任务将使用此配置；运行中的任务保持原配置。');
  } catch (error) {if (!disposed && currentVersion === version) status('error', '保存失败：' + message(error));}
  finally {if (!disposed && currentVersion === version) {busy.value = false; request = null;}}
}
function dispose() {if (disposed) return; disposed = true; version++; request?.abort(); request = null; login.cancel(); resetKey();}
function returnToProject() {dispose(); emit('return');}
onMounted(async () => {
  request = new AbortController();
  try {
    const settings = await client.request<PublicSettings>('/model-settings', {signal: request.signal});
    if (disposed) return;
    fill(settings, true); loaded.value = true; busy.value = false;
    const dirty = settings.has_token && ((settings.reasoning_effort || defaultReasoningEffort) !== defaultReasoningEffort || settings.connection_mode !== 'direct');
    summaryStatus.value = dirty ? '未保存' : settings.has_token ? '已保存' : '待配置';
  } catch (error) {if (!disposed) {busy.value = false; status('error', '配置读取失败：' + message(error) + '。返回项目后可重新打开重试。');}}
});
onBeforeUnmount(dispose);
</script>

<template>
  <section class="llm-page" aria-labelledby="llm-title">
    <header class="llm-heading"><h1 id="llm-title">模型接入</h1><button type="button" class="llm-back" data-llm-return :title="projectName ? `${returnLabel} · ${projectName}` : returnLabel" @click="returnToProject"><Icon name="arrow-left" />{{ returnLabel }}</button></header>
    <div class="llm-layout"><form class="llm-form" novalidate autocomplete="off" @submit.prevent="save" @input="edit" @change="edit">
      <section class="llm-section"><div class="llm-section-title"><h2>提供商</h2></div><div class="llm-providers">
        <button v-for="id in providerIDs" :key="id" class="llm-provider" :class="{'is-selected': provider === id}" type="button" :data-llm-provider="id" :aria-pressed="provider === id" :disabled="disabled" @click="selectProvider(id)">
          <img v-if="providers[id].icon" class="llm-provider-logo" :src="providers[id].icon" alt="" aria-hidden="true"><Icon v-else class="llm-provider-logo" name="settings-2" /><strong>{{ providers[id].name }}</strong><span class="llm-provider-check" aria-hidden="true"><Icon name="check" /></span>
        </button>
      </div></section>
      <section class="llm-section llm-connection"><div class="llm-section-title"><h2>连接配置</h2></div>
        <div v-if="provider === 'kimi' || provider === 'glm'" class="llm-field"><label for="llm-auth-mode">接入方式</label><div class="llm-select-wrap"><select id="llm-auth-mode" v-model="values.authMode" name="authMode" :disabled="disabled" @change="selectAuth"><option value="api_key">API Key</option><option v-if="provider === 'kimi'" value="kimi_oauth">订阅（Kimi Code）</option><option v-else value="glm_coding_plan">订阅（GLM Coding Plan）</option></select><Icon name="chevron-down" /></div><p class="llm-field-hint"><a v-if="provider === 'kimi'" href="https://www.kimi.com/code" target="_blank" rel="noopener noreferrer">Kimi Code 订阅说明</a><a v-else href="https://docs.bigmodel.cn/cn/coding-plan/extension/coding-tool-helper" target="_blank" rel="noopener noreferrer">GLM Coding Plan 订阅与专用 API Key 说明</a></p></div>
        <div class="llm-field llm-protocol"><label for="llm-protocol">API 协议</label><div class="llm-select-wrap"><select id="llm-protocol" v-model="values.apiProtocol" name="protocol" :disabled="disabled || subscription"><option v-for="protocol in apiProtocolIDs" :key="protocol" :value="protocol">{{ apiProtocols[protocol] }}</option></select><Icon name="chevron-down" /></div></div>
        <div class="llm-field llm-endpoint"><label for="llm-url">Base URL</label><input id="llm-url" v-model="values.url" name="url" type="url" placeholder="https://api.example.com/v1" spellcheck="false" :disabled="disabled || subscription"></div>
        <div class="llm-field"><label for="llm-model">模型名称</label><input id="llm-model" v-model="values.model" name="model" type="text" placeholder="输入模型 ID" spellcheck="false" :disabled="disabled"></div>
        <div v-if="!oauth" class="llm-field"><label for="llm-key">{{ values.authMode === 'glm_coding_plan' ? 'Coding Plan API Key' : 'API Key' }}</label><div class="llm-key-wrap"><input ref="keyInput" id="llm-key" v-model="token" name="key" :type="revealKey ? 'text' : 'password'" :placeholder="keyPlaceholder" autocomplete="off" spellcheck="false" :disabled="disabled"><button type="button" class="llm-key-toggle" :aria-label="revealKey ? '隐藏 API Key' : '显示 API Key'" :aria-pressed="revealKey" :disabled="disabled" @click="revealKey = !revealKey"><Icon :name="revealKey ? 'eye-off' : 'eye'" /></button></div><p class="llm-field-hint">{{ values.authMode === 'glm_coding_plan' ? '使用 GLM Coding Plan 套餐专用 API Key，不使用网页登录。' : '密钥保存在服务端，页面不会回显；留空保留同一服务的已存密钥。' }}</p></div>
        <div v-else class="llm-field llm-login"><div class="llm-login-message" role="status" aria-live="polite">{{ login.state.message || (retained ? 'Kimi Code 订阅已连接。' : '登录 Kimi Code 订阅后，完成连接测试并保存。') }}</div><p v-if="login.state.phase === 'pending'" class="llm-field-hint"><a class="llm-login-link" :href="login.state.url" target="_blank" rel="noopener noreferrer">打开 Kimi 官方登录页面</a> · 验证码：<code class="llm-login-code">{{ login.state.code }}</code></p><div class="llm-auth-actions"><button v-if="!['starting', 'pending'].includes(login.state.phase)" class="llm-auth-button llm-login-start" type="button" :disabled="disabled" @click="startLogin">{{ retained || login.state.phase === 'authorized' ? '重新登录' : '登录 Kimi Code' }}</button><button v-if="['starting', 'pending', 'authorized'].includes(login.state.phase)" class="llm-auth-button llm-login-cancel" type="button" :disabled="disabled" @click="cancelLogin">取消登录</button><button v-if="retained" class="llm-auth-button llm-logout" type="button" :disabled="disabled" @click="logout">断开订阅</button></div></div>
        <div v-if="showEffort" class="llm-field llm-effort"><div class="llm-effort-description"><label for="llm-effort">思考强度</label><p class="llm-field-hint" id="llm-effort-hint">可用强度取决于模型与服务商。</p></div><div class="llm-select-wrap"><select id="llm-effort" v-model="values.reasoningEffort" name="reasoningEffort" aria-describedby="llm-effort-hint" :disabled="disabled"><option v-for="effort in effortIDs" :key="effort" :value="effort">{{ efforts[effort] }}</option></select><Icon name="chevron-down" /></div></div>
        <div class="llm-field llm-network"><div class="llm-network-row"><label for="llm-connection-mode">连接方式</label><div class="llm-select-wrap"><select id="llm-connection-mode" v-model="values.connectionMode" name="connectionMode" :disabled="disabled"><option value="direct">直连</option><option value="proxy">使用代理</option></select><Icon name="chevron-down" /></div></div>
          <div class="llm-proxy-fields" :hidden="values.connectionMode === 'direct'">
            <div class="llm-field"><label for="llm-proxy-protocol">代理协议</label><div class="llm-select-wrap"><select id="llm-proxy-protocol" v-model="values.protocol" name="proxyProtocol" :disabled="disabled || values.connectionMode === 'direct'" :required="values.connectionMode === 'proxy'"><option v-for="protocol in proxyIDs" :key="protocol" :value="protocol">{{ proxyProtocols[protocol] }}</option></select><Icon name="chevron-down" /></div></div>
            <div class="llm-field"><label for="llm-proxy-host">主机</label><input id="llm-proxy-host" v-model="values.host" name="proxyHost" placeholder="host.docker.internal" spellcheck="false" :disabled="disabled || values.connectionMode === 'direct'" :required="values.connectionMode === 'proxy'"></div>
            <div class="llm-field"><label for="llm-proxy-port">端口</label><input id="llm-proxy-port" v-model="values.port" name="proxyPort" inputmode="numeric" placeholder="7897" :disabled="disabled || values.connectionMode === 'direct'" :required="values.connectionMode === 'proxy'"></div>
            <p class="llm-field-hint">代理地址需能从服务端访问。Docker 访问宿主机代理可用 host.docker.internal。</p>
          </div>
        </div>
      </section>
      <div class="llm-status" :class="statusKind ? `llm-status-${statusKind}` : ''" role="status" aria-live="polite"><template v-if="statusMessage"><Icon :name="statusIcon" /><span>{{ statusMessage }}</span></template></div>
      <footer class="llm-actions"><div><button class="llm-test" type="button" :disabled="disabled || (oauth && !oauthReady)" @click="testConnection"><Icon name="plug" />连接测试</button><button class="llm-save" type="submit" :disabled="disabled || !verified">保存配置</button></div></footer>
    </form>
    <aside class="llm-aside"><div class="llm-overview"><div class="llm-diagram"><div class="llm-diagram-node"><span class="llm-mini-brand" aria-hidden="true"><img src="/static/brand.png" alt=""></span><strong>PwnMesh</strong></div><div class="llm-diagram-node llm-model-node"><span class="llm-model-symbol" aria-hidden="true"><img v-if="providers[provider].icon" :src="providers[provider].icon" alt="" aria-hidden="true"><Icon v-else name="settings-2" /></span><div><strong class="llm-summary-provider">{{ providers[provider].name }}</strong><small class="llm-summary-model">{{ values.model.trim() || '待选择模型' }}</small></div><span class="llm-connection-dot" :class="{'is-connected': connected}"></span></div></div>
      <dl class="llm-summary"><div><dt>状态</dt><dd class="llm-summary-status">{{ summaryStatus }}</dd></div><div><dt>协议</dt><dd class="llm-summary-protocol">{{ values.apiProtocol === 'openai' ? 'OpenAI' : 'Anthropic' }}</dd></div><div v-if="showEffort"><dt>思考强度</dt><dd class="llm-summary-effort">{{ efforts[values.reasoningEffort] }}</dd></div><div><dt>连接方式</dt><dd class="llm-summary-connection">{{ values.connectionMode === 'proxy' ? `${proxyProtocols[values.protocol]} 代理` : '直连' }}</dd></div><div class="llm-summary-proxy" :hidden="values.connectionMode === 'direct'"><dt>代理地址</dt><dd class="llm-summary-proxy-address">{{ values.host }}:{{ values.port }}</dd></div></dl>
    </div></aside></div>
  </section>
</template>
