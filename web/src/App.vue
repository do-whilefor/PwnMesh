<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { useWorkspaceSync } from './workspace/useWorkspaceSync';
import { CommandError } from './workspace/project-commands';
import { actions, canHint } from './workspace/model/lifecycle';
import type { CreateProjectValues, HintValues, ProjectView } from './workspace/model/types';
import type { ProjectOperation } from './workspace/dialogs/types';
import { actionNames } from './workspace/dialogs/types';
import { errorMessage } from './ui/format';
import ProjectSidebar from './workspace/ProjectSidebar.vue';
import ProjectForms from './workspace/dialogs/ProjectForms.vue';
import ProjectManagement from './workspace/dialogs/ProjectManagement.vue';
import ActionDialog from './workspace/dialogs/ActionDialog.vue';
const route = useRoute(), router = useRouter(), returnId = ref(''), toastText = ref(''), modelSummary = ref('尚未配置模型');
let toastTimer: ReturnType<typeof setTimeout> | undefined;
function toast(message: string) { toastText.value = message; clearTimeout(toastTimer); toastTimer = setTimeout(() => toastText.value = '', 3200); }
const projectId = computed(() => typeof route.params.id === 'string' ? route.params.id : returnId.value);
const { resource, commands, projects, current, connected, mutating, mutate, publish } = useWorkspaceSync(projectId, toast);
const disabled = computed(() => mutating.value || !connected.value);
const sidebar = ref<InstanceType<typeof ProjectSidebar>>(), forms = ref<InstanceType<typeof ProjectForms>>(), management = ref<InstanceType<typeof ProjectManagement>>(), confirmation = ref<InstanceType<typeof ActionDialog>>();
const managementProject = ref<ProjectView>();
const latestManagement = computed(() => managementProject.value?.archived ? managementProject.value : projects.value.find(project => project.id === managementProject.value?.id));
async function loadRound(project: ProjectView, generation: number) { const value = await resource.archive(project, generation); publish(); return value; }
const pageProps = computed(() => route.name === 'models' ? { returnLabel: current.value ? '返回项目' : '返回工作台', projectName: current.value?.name || '' } : { project: current.value, projectIds: projects.value.map(project => project.id), disabled: disabled.value, modelSummary: modelSummary.value, loadRound });
watch(() => route.params.id, value => { if (typeof value === 'string') { returnId.value = value; try { localStorage.setItem('pwnmesh.selected-project', value); } catch { /* Storage can be disabled. */ } } }, { immediate: true });
// Resolve missing routes only from accepted reads. A navigation can name a newly
// created project before that project has appeared in the published list.
watch(projects, () => { if (route.name === 'models' || !connected.value || mutating.value) return; if (current.value && (route.name === 'projects' || current.value.id !== projectId.value)) void router.replace('/project/' + current.value.id); });
async function create(values: CreateProjectValues) { await mutate(async () => {
  try { const id = await commands.create(values); sidebar.value?.clear(); await router.push('/project/' + id); toast('项目已创建并开始'); }
  catch (error) { if (error instanceof CommandError && !error.pendingCreate && !error.uncertainCreate) forms.value?.resetCreate(); throw error; }
}); }
async function hint(id: string, values: HintValues) { await mutate(async () => { const project = projects.value.find(value => value.id === id); if (!project || !canHint(project)) throw new Error(project?.readOnly ? '历史项目只读，请新建项目继续。' : '项目已结束，重启后可补充信息。'); await commands.hint(id, values); toast(project.status === 'paused' ? '补充已保存，继续后读取' : '补充已添加'); }); }
async function perform(operation: ProjectOperation) {
  await mutate(async () => {
    const project = projects.value.find(value => value.id === operation.id);
    if (!project) throw new Error('项目已不存在，请关闭窗口并重新选择。');
    if (project.generation !== operation.generation || project.status !== operation.status) throw new Error('项目状态已变化，请关闭窗口后重新确认。');
    if (!actions(project).includes(operation.action)) throw new Error('当前状态不支持此操作。');
    const pending = commands.pendingCreate?.created.project.id === project.id;
    await commands.change(project.id, operation.action, project.generation);
    if (pending && !commands.pendingCreate) forms.value?.resetCreate();
    if (operation.action === 'delete') { if (project.id === current.value?.id) { returnId.value = ''; await router.push('/projects'); } }
    toast('项目已' + actionNames[operation.action]);
  });
}
async function action(operation: ProjectOperation) {
  if (operation.action === 'pause' || operation.action === 'resume') { try { await perform(operation); management.value?.focusAction(); } catch (error) { toast(errorMessage(error)); } }
  else { management.value?.close(); const project = projects.value.find(value => value.id === operation.id); if (project) confirmation.value?.open(project, operation.action); }
}
function manage(project: ProjectView) { managementProject.value = project; management.value?.open(); }
function keyboard(event: KeyboardEvent) {
  const target = event.target instanceof Element ? event.target : null;
  if (target?.closest('input,textarea,select,[contenteditable]') || document.querySelector('dialog[open]') || event.ctrlKey || event.metaKey || event.altKey) return;
  if (event.key.toLowerCase() === 'n') { event.preventDefault(); forms.value?.openCreate(); }
  if (event.key === '/') { event.preventDefault(); sidebar.value?.focusSearch(); }
}
onMounted(() => { document.addEventListener('keydown', keyboard); });
onBeforeUnmount(() => { clearTimeout(toastTimer); document.removeEventListener('keydown', keyboard); });
</script>
<template>
  <svg class="icon-library" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><defs><symbol id="i-flag" viewBox="0 0 24 24"><path d="M4 22V3m0 0c4-3 8 3 12 0v10c-4 3-8-3-12 0"/></symbol><symbol id="i-node" viewBox="0 0 24 24"><rect x="3" y="3" width="6" height="6" rx="1.5"/><rect x="15" y="15" width="6" height="6" rx="1.5"/><path d="M6 9v6a3 3 0 0 0 3 3h6"/></symbol><symbol id="i-file" viewBox="0 0 24 24"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8ZM14 2v6h6M8 13h8M8 17h5"/></symbol><symbol id="i-check" viewBox="0 0 24 24"><path d="m5 12 4 4L19 6"/></symbol><symbol id="i-target" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="5"/><circle cx="12" cy="12" r="1"/></symbol><symbol id="i-shield" viewBox="0 0 24 24"><path d="M12 3 4 6v6c0 5 8 9 8 9s8-4 8-9V6ZM12 8v5m0 3h.01"/></symbol></defs></svg>
  <ProjectSidebar ref="sidebar" :projects="projects" :selected="route.name === 'project' ? current?.id : undefined" :disabled="disabled" @create="forms?.openCreate()" @action="action"/>
  <div class="workbench"><main id="main"><RouterView v-slot="{ Component }"><KeepAlive include="WorkspacePage"><component :is="Component" v-bind="pageProps" @create="forms?.openCreate()" @hint="forms?.openHint($event)" @manage="manage" @toast="toast" @return="router.push(current ? '/project/' + current.id : '/projects')" @saved="modelSummary = $event"/></KeepAlive></RouterView></main></div>
  <ProjectForms ref="forms" :get-project="id => projects.find(project => project.id === id)" :create="create" :hint="hint" :is-hint-retry="(id, values) => commands.isHintRetry(id, values)" :disabled="disabled" @toast="toast"/>
  <ProjectManagement ref="management" :project="latestManagement" :disabled="disabled" @action="action"/>
  <ActionDialog ref="confirmation" :perform="perform" :disabled="disabled"/>
  <div id="toast" role="status" aria-live="polite" :hidden="!toastText">{{ toastText }}</div>
</template>
