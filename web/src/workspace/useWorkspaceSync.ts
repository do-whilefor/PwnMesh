import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue';
import type { Ref } from 'vue';
import { ProjectResource } from './project-resource';
import { ProjectCommands } from './project-commands';
import { errorMessage } from '@/ui/format';

/** One application-scoped scheduler publishes only accepted resource reads. */
export function useWorkspaceSync(projectId: Ref<string>, toast: (message: string) => void, resource = new ProjectResource()) {
  const commands = new ProjectCommands(resource);
  const projects = shallowRef(resource.projects), connected = ref(false), mutating = ref(false);
  const current = computed(() => projects.value.find(project => project.id === projectId.value) || projects.value[0]);
  let timer: ReturnType<typeof setTimeout> | undefined, stopped = false, refreshVersion = 0;
  let active: { preferred: string; promise: Promise<void> } | undefined;
  function publish() { projects.value = resource.projects; }
  function cancelRefresh() { refreshVersion++; active = undefined; clearTimeout(timer); resource.cancel(); }
  function refresh(preferred = projectId.value): Promise<void> {
    clearTimeout(timer); if (stopped || mutating.value) return Promise.resolve();
    if (active?.preferred === preferred) return active.promise;
    const version = ++refreshVersion;
    // A changed route starts immediately. Obsolete reads may ignore abort, but
    // neither their completion nor their finally block owns this schedule.
    const promise = (async () => {
      try { const result = await resource.refresh(preferred); if (result && version === refreshVersion && !stopped && !mutating.value) { publish(); connected.value = true; } }
      catch (error) { if (version === refreshVersion && !(error instanceof Error && error.name === 'AbortError') && !resource.scope.controller?.signal.aborted && !stopped) { connected.value = false; toast(errorMessage(error)); } }
      finally { if (version === refreshVersion) { active = undefined; if (!stopped && !mutating.value) timer = setTimeout(() => void refresh(), document.hidden ? 10000 : 2500); } }
    })();
    active = {preferred, promise};
    return promise;
  }
  async function mutate<T>(work: () => Promise<T>) {
    if (mutating.value) throw new Error('正在处理上一项操作，请稍后重试。');
    mutating.value = true; cancelRefresh();
    try { return await work(); } finally {
      mutating.value = false; await refresh();
      // Navigation can supersede the reconciliation read while it is pending.
      // Resolve the command after the latest scheduled read has settled too.
      while (active && !stopped) await active.promise;
    }
  }
  const visible = () => { if (!document.hidden) void refresh(); };
  const hide = () => { stopped = true; cancelRefresh(); };
  const show = (event: PageTransitionEvent) => { if (event.persisted) { stopped = false; void refresh(); } };
  onMounted(() => { void refresh(); document.addEventListener('visibilitychange', visible); window.addEventListener('pagehide', hide); window.addEventListener('pageshow', show); });
  onBeforeUnmount(() => { hide(); document.removeEventListener('visibilitychange', visible); window.removeEventListener('pagehide', hide); window.removeEventListener('pageshow', show); });
  watch(projectId, () => void refresh());
  return { resource, commands, projects, current, connected, mutating, refresh, mutate, publish };
}
