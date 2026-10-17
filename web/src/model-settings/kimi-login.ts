import {reactive} from 'vue';
import type {Client} from '@/api/client';
import type {SettingsPayload} from './rules';

interface DeviceLogin {login_id: string; user_code: string; verification_uri: string; expires_in: number; interval?: number}
interface LoginStatus {status: 'pending' | 'slow_down' | 'authorized' | 'expired' | 'denied'; interval?: number}

export function kimiLogin(client: Pick<Client, 'request'>, changed: () => void) {
  const state = reactive({id: '', code: '', url: '', phase: 'idle', message: ''});
  let generation = 0, request: AbortController | null = null, timer: ReturnType<typeof setTimeout> | undefined;
  const remove = (id: string) => {void client.request('/model-settings/kimi/login/' + encodeURIComponent(id), {method: 'DELETE'}).catch(() => {});};
  function cancel(revoke = true) {
    generation++; request?.abort(); request = null; clearTimeout(timer); timer = undefined;
    if (revoke && state.id) remove(state.id);
    Object.assign(state, {id: '', code: '', url: '', phase: 'idle', message: ''});
  }
  function fail(text: string) {cancel(); state.phase = 'error'; state.message = text; changed();}
  async function start(connection: Pick<SettingsPayload, 'connection_mode' | 'proxy_url'>) {
    cancel(); changed(); state.phase = 'starting'; state.message = '正在获取 Kimi 登录验证码…';
    const current = generation; request = new AbortController();
    try {
      const login = await client.request<DeviceLogin>('/model-settings/kimi/login', {method: 'POST', body: connection, signal: request.signal});
      if (current !== generation) {if (login.login_id) remove(login.login_id); return;}
      state.id = login.login_id;
      const url = new URL(login.verification_uri);
      if (!login.login_id || url.protocol !== 'https:' || !['kimi.com', 'www.kimi.com', 'auth.kimi.com'].includes(url.hostname) || url.username || url.password) throw new Error('Invalid Kimi verification link.');
      Object.assign(state, {code: login.user_code, url: url.href, phase: 'pending', message: '请打开 Kimi 官方页面并输入验证码。'});
      let interval = Math.max(1, login.interval || 5);
      const expires = Date.now() + Math.max(0, login.expires_in) * 1000;
      const schedule = () => {timer = setTimeout(() => void poll(), Math.max(0, Math.min(interval * 1000, expires - Date.now())));};
      async function poll() {
        if (current !== generation) return;
        if (Date.now() >= expires) {fail('登录已过期，请重新登录。'); return;}
        request = new AbortController();
        try {
          const result = await client.request<LoginStatus>('/model-settings/kimi/login/' + encodeURIComponent(login.login_id), {method: 'POST', signal: request.signal});
          if (current !== generation) return;
          if (result.status === 'authorized') {state.phase = 'authorized'; state.code = ''; state.url = ''; state.message = '登录成功，请完成连接测试并保存配置。'; changed(); return;}
          if (result.status === 'expired' || result.status === 'denied') {fail(result.status === 'expired' ? '登录已过期，请重新登录。' : '登录授权被拒绝，请重试。'); return;}
          if (result.status !== 'pending' && result.status !== 'slow_down') throw new Error('Unexpected Kimi sign-in status.');
          interval = result.interval ? Math.max(interval, result.interval) : result.status === 'slow_down' ? interval + 5 : interval;
          schedule();
        } catch (error) {if (current === generation) fail(error instanceof Error ? error.message : '登录失败，请重试。');}
      }
      schedule();
    } catch (error) {if (current === generation) fail(error instanceof Error ? error.message : '登录失败，请重试。');}
  }
  return {state, start, cancel};
}
