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

<!-- Keep the existing global cascade order across pages and dialogs. -->
<style>
/* Connectors sit beneath opaque cards. */
.graph-edges{z-index:1}
.graph-nodes{position:absolute;inset:0;z-index:2;pointer-events:none}
.graph-node{pointer-events:auto;opacity:1}

.graph-node.dim{opacity:1;box-shadow:none}
.graph-node.dim .node-title,.graph-node.dim .node-kind,.graph-node.dim .node-status{color:#5c6f7c}
.graph-node.start.dim{border-color:#292a25}

.graph-edge{fill:none;stroke-linecap:round;stroke-linejoin:round;vector-effect:non-scaling-stroke;pointer-events:none}

.graph-edge.paused{stroke:#b38548}
.graph-edge.cancelled{stroke:#a39c91;stroke-dasharray:2 5}

.graph-edge-hit{fill:none;stroke:transparent;stroke-width:12;vector-effect:non-scaling-stroke;pointer-events:stroke;cursor:pointer;outline:none}
.graph-edge-hit:focus-visible{stroke:#bc60432b;stroke-width:12}
.graph-edge-label{visibility:hidden;opacity:0;pointer-events:none;font-family:var(--font);fill:#716e64;paint-order:stroke;stroke-linejoin:round;cursor:pointer;user-select:none}
.graph-edge-label.is-revealed,.graph-edge-label.selected{visibility:visible;opacity:1;pointer-events:all}
.graph-edge-label.running,.graph-edge-label.selected{fill:#b76145}
.graph-edge-label.paused{fill:#92743f}
.graph-edge-label:hover{fill:#a54f36}
.graph-source-port{pointer-events:none}

.graph-data-empty{position:absolute;inset:0;display:grid;place-items:center;color:#716e64;pointer-events:none;font-size:14px}
.graph-data-warning{position:absolute;left:50%;top:10px;transform:translateX(-50%);max-width:90%;padding:5px 9px;border:1px solid #e8dcc7;border-radius:5px;background:#faf4e8;color:#92743f;font-size:11px;pointer-events:none;z-index:3}
.graph-data-empty[hidden],.graph-data-warning[hidden]{display:none}
.graph-screen-reader{position:absolute;width:1px;height:1px;margin:-1px;overflow:hidden;clip-path:inset(50%);white-space:nowrap}
.graph-node.invalid{border-color:#b18748;background:#faf4e8}
.graph-node.invalid .node-status{color:#7d5b29}
.graph-node .node-status{white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.graph-node .node-status>span{overflow:hidden;text-overflow:ellipsis}
.graph-edge.invalid{stroke:#b18748}
.graph-edge-label.invalid{fill:#7d5b29}
.graph-edge.invalid.selected{stroke:#bc6043}
.graph-node.invalid.selected{border-color:#bc6043}
</style>

<style>
:root{--accent-hover:#df6947;--surface:#fff;--font:'Microsoft YaHei','PingFang SC','Noto Sans SC','SimHei',sans-serif;--sidebar:230px}
*{box-sizing:border-box}body{margin:0;min-width:1180px;color:var(--text);font-family:var(--font);font-size:13px;background:var(--bg);-webkit-font-smoothing:auto}button,input,textarea,select{font:inherit;color:inherit}button{cursor:pointer;border:0;background:none}button:disabled{opacity:.45;cursor:not-allowed}a{color:inherit;text-decoration:none}button,a,input,textarea,select,summary{outline-offset:4px}:focus-visible{outline:2px solid var(--accent)}h1,h2,h3,p{margin:0}button{transition:background .16s,border-color .16s,box-shadow .16s,color .16s}input,textarea,select{background:white;border:1px solid #dfe3df;border-radius:7px;padding:11px 12px;font-size:13px;min-width:0}input:focus,textarea:focus,select:focus{outline:0;border-color:#e8a38a;box-shadow:0 0 0 3px #f48b6611}input::placeholder,textarea::placeholder{color:#5c6f7c}textarea{resize:vertical;line-height:1.8}input[type=radio]{accent-color:var(--accent)}[hidden]{display:none!important}svg.lucide,.icon{width:17px;height:17px;stroke-width:1.65;flex:none}.icon{fill:none;stroke:currentColor;stroke-linecap:round;stroke-linejoin:round}.icon-library{position:absolute;width:0;height:0;overflow:hidden}.button{height:36px;border-radius:7px;padding:0 13px;display:inline-flex;align-items:center;justify-content:center;gap:8px;font-size:12px;white-space:nowrap}.primary{background:var(--accent);color:white;box-shadow:0 2px 3px #d5704515;border:1px solid var(--accent)}.primary:hover:not(:disabled){background:var(--accent-hover);border-color:var(--accent-hover)}.secondary{background:white;border:1px solid var(--line)}.icon-button{display:inline-flex;align-items:center;justify-content:center;width:30px;height:30px;border-radius:6px}kbd{font:10px var(--font);padding:2px 5px;border:1px solid #e7e9e5;border-radius:4px;color:#929a96}code{font-family:var(--font);font-variant-numeric:tabular-nums;font-size:12px}
.sidebar{position:fixed;inset:0 auto 0 0;width:var(--sidebar);display:flex;flex-direction:column;z-index:10}.brand{display:block}.brand-art{display:block;position:relative;overflow:hidden;isolation:isolate;mix-blend-mode:multiply}.brand-art img{position:absolute;width:286px;max-width:none;height:auto;left:-50px;top:-49px}.create-button{width:100%;justify-content:flex-start;padding:0 11px}.create-button kbd{margin-left:auto;border-color:#ffffff55;color:#fff}.project-search{display:flex;align-items:center}.project-search input{width:100%;min-width:0;border:0;padding:0;background:none;box-shadow:none}.project-search kbd{margin-left:auto}.nav-caption{display:flex;align-items:center;justify-content:space-between}#project-list{flex:1}.project-item{display:flex;border:1px solid transparent;position:relative}.project-copy strong{display:block}.project-copy small{align-items:center}.status-dot{display:inline-block;width:5px;height:5px;border-radius:50%;background:var(--accent);flex:none}.status-dot.done{background:var(--green)}.status-dot.pending,.status-dot.paused{background:#aeb6af}
.workbench{height:100dvh;display:flex;flex-direction:column}#main{flex:1;min-height:0;overflow:auto}.project-page{height:100%;display:flex;flex-direction:column;padding:27px 28px 15px}.project-heading{justify-content:space-between;margin-bottom:24px}.title-row{display:flex;align-items:center}.badge{display:inline-flex;align-items:center;gap:5px;line-height:1.2;background:#eff2ed;color:#7c8978;font-weight:400;white-space:nowrap}.project-description{display:flex;align-items:center}.meta-divider{display:inline-block;width:1px;height:10px;background:#dfe3da}.heading-actions{display:flex;align-items:center}.project-body{display:grid;grid-template-columns:minmax(0,1fr) var(--activity);gap:18px;flex:1;min-height:0}.canvas-panel{background:var(--surface);border:1px solid var(--line);display:flex;flex-direction:column;overflow:hidden;min-width:0}.canvas-header{display:flex;align-items:center;flex:none;border-bottom:1px solid var(--line)}.view-tabs{display:flex;align-items:center;height:100%}.view-tabs button{height:100%;display:flex;align-items:center;padding:0;position:relative}.view-tabs button.active:after{position:absolute;content:'';bottom:-1px;left:0;right:0}.view-tabs button small{padding:1px 5px;border-radius:4px}.graph-stage{position:relative;flex:1;min-height:380px;overflow:hidden}.graph-host{position:absolute;inset:0}.canvas-bottom{position:absolute;display:flex;align-items:center;justify-content:space-between;pointer-events:none;z-index:3}.zoom-controls{display:flex;align-items:center;gap:2px;pointer-events:auto}.zoom-controls>span{min-width:34px;text-align:center;font-variant-numeric:tabular-nums}.zoom-controls>i{height:13px;width:1px;margin:0 3px}.canvas-footer{flex:none;border-top:1px solid var(--line);display:flex;align-items:center;justify-content:space-between;background:#fff}.legend{display:flex}.legend button{display:flex;align-items:center;gap:5px;padding:4px 5px;border-radius:4px}.legend-dot{border-radius:2px}.legend-dot.pending{background:#fff;border:1px solid #b9c3ac}.task-progress{display:flex;align-items:center;gap:8px}.progress-track{border-radius:4px;overflow:hidden}.progress-track i{height:100%;display:block;border-radius:4px}
.graph-viewport{position:absolute;inset:0;overflow:hidden;touch-action:none;cursor:grab}.graph-viewport.is-panning{cursor:grabbing}.graph-world{position:absolute;transform-origin:0 0}.graph-edges{position:absolute;inset:0;overflow:visible;pointer-events:none}.graph-node{position:absolute;width:148px;height:78px;text-align:left;background:#fff;touch-action:none;user-select:none;cursor:grab;transition:border-color .16s,box-shadow .16s}.graph-node.pending{border-style:dashed;background:#fdfefb}.graph-node.start{color:white}.node-top{display:flex;justify-content:space-between;align-items:center}.node-kind{display:flex;align-items:center}.node-status{display:flex;align-items:center;gap:3px}.running .node-status{color:#d88d60}.node-title{text-overflow:ellipsis}.node-running-dot{border-radius:50%}.graph-edge.running{stroke:#deb497}.graph-edge.selected,.graph-edge.lineage{stroke:#d89b75}.graph-node.start.dim .node-title{color:#c0c8bb}
.activity-pane{border:1px solid var(--line);display:flex;flex-direction:column;min-height:0;overflow:hidden}.activity-heading{display:flex;align-items:center;justify-content:space-between;flex:none}.activity-heading h2{display:flex;align-items:center}.activity-heading>span{display:flex;align-items:center;gap:5px}.activity-content{flex:1}.timeline-entry{position:relative}.entry-meta{display:flex;justify-content:space-between;gap:5px;align-items:center}.selection-header{display:flex;justify-content:space-between;align-items:center}.inspector-title{overflow-wrap:anywhere}.inspector-meta{border-top:1px solid var(--line);display:grid}.inspector-meta>div{display:flex;justify-content:space-between;gap:12px}.tab-content{flex:1}.material-row{display:flex}.material-row strong{display:block}.material-row small{display:block}.result-card h3{font-weight:700}.empty-state{display:grid;place-items:center;padding:60px 20px;text-align:center;gap:13px}
dialog{border:1px solid #e7e9e1;width:550px;max-height:calc(100dvh - 50px);color:var(--text);background:#fff}.dialog-header{display:flex;justify-content:space-between}.dialog-header p{font-size:12px;color:var(--muted);margin-top:9px;line-height:1.8}.type-choices{display:grid;grid-template-columns:repeat(3,1fr)}.type-choices label{cursor:pointer}.type-choices input{position:absolute;opacity:0;width:0}.type-choices span{border:1px solid var(--line);display:flex;align-items:center;justify-content:center;gap:7px}.type-choices input:focus-visible+span{outline:2px solid var(--accent)}.field-label{display:flex;align-items:center;gap:8px;font-weight:700;margin:17px 0 8px}.field-label>span{font-size:10px;font-weight:400}dialog input:not([type=radio]):not([type=file]),dialog textarea{width:100%}.file-list{list-style:none;padding:0;margin:5px 0}.file-list li{display:flex;gap:8px;align-items:center;font-size:10px;margin-top:5px}.file-list li>span{flex:1;overflow-wrap:anywhere}.file-list .lucide{height:13px;width:13px}.form-error{color:#bd735e;font-size:11px;margin-top:12px;line-height:1.8}.form-error:empty{display:none}.dialog-footer{display:flex;align-items:center;gap:8px}.dialog-footer>span{margin-right:auto;font-size:10px}#toast{position:fixed;bottom:26px;left:50%;transform:translateX(-50%);border:1px solid #4d5945;padding:12px 20px;border-radius:9px;font-size:12px;z-index:100;box-shadow:0 5px 25px #222b1a16}
/* Desktop reading sizes: the canvas stays compact; navigation and logs remain legible. */
.project-copy small{gap:4px}.view-tabs button small{font-size:10px}.activity-heading>span{font-size:10px}
/* Compact console chrome reserves the viewport for the canvas and observation panel. */
.project-page{padding:12px 20px 8px}.project-heading{margin-bottom:12px}.title-row{gap:9px;margin:0 0 5px}.title-row h1{line-height:1.35}.project-description{gap:6px;line-height:17px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.meta-divider{margin:0 3px}.heading-actions{gap:7px;flex:none}.heading-actions .button{height:32px;font-size:11px;padding:0 10px}.heading-actions .button .lucide{width:14px;height:14px}.project-body{gap:14px}
/* Shared desktop finish: neutral surfaces, readable copy, quiet supporting chrome. */
:root{--green:#486b57}
.create-button{box-shadow:none}.project-search{padding:0 8px;border:1px solid #e8ebec}.project-search input{color:#3e5361}.project-search:focus-within{border-color:#e4b8a7;box-shadow:0 0 0 2px #f5e8e2}.project-search kbd{font-size:9px;border-color:#edf0f1;color:#a0a6ab;padding:1px 4px}.project-item:hover{background:#f0f3f4}.project-item.selected:before{content:'';position:absolute;left:-1px;top:17px;bottom:17px;width:2px;background:#ec8a67;border-radius:2px}.selected .project-copy strong{color:#9a4e32}
.project-page{padding-bottom:12px}.project-body{gap:12px}.canvas-panel,.activity-pane{border-color:#e2e7e9}.view-tabs button:hover{color:#56636d}.canvas-bottom{bottom:15px;left:15px;right:15px}.zoom-controls{background:#fff}.zoom-controls .icon-button{color:#647682}.canvas-footer{padding:0 15px}
.graph-node:hover{border-color:#b9ced7;box-shadow:0 3px 10px #3346500a}.graph-node.selected{border-color:#e49473;box-shadow:0 0 0 3px #e4947315}.graph-node.lineage{border-color:#b1c6ba}.graph-node.start{background:#2d3840;border-color:#2d3840}.graph-node.start .node-kind,.graph-node.start .node-status{color:#d5e0e6}.graph-node.start .node-title{color:#f6f9fb}.graph-node.goal{background:#f5f8f9;border-color:#ccdce3}.graph-node.finding{background:#fff8f2;border-color:#f1d7c4}.graph-node.running{border-color:#e6b79d;background:#fffdfb;box-shadow:0 0 0 2px #f3d2b913}.node-kind{color:#5c6f7c}.node-title{color:#273743;white-space:normal;display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:2;overflow:hidden;letter-spacing:0}.graph-node.dim .node-title{color:#5c6f7c}.graph-node.dim{border-color:#dde7ec}.graph-edge{stroke:#c1d1d7}.graph-edge.done{stroke:#a8bfb2}.graph-edge.pending{stroke:#c2d1d9}
.activity-heading>span{color:#8fa3ad}
dialog::backdrop{background:#233b4938;backdrop-filter:blur(2px)}.dialog-footer>span{color:#5c6f7c}.file-list li{color:#3e5361}
@media(prefers-reduced-motion:reduce){*,*::before,*::after{scroll-behavior:auto!important;transition:none!important;animation:none!important}}
</style>

<style>
.llm-page{display:flex;flex-direction:column;justify-content:center;min-height:100%;max-width:1100px;margin:0 auto;padding:24px 28px;color:var(--ink,#273743);scrollbar-width:none}
.llm-page *{scrollbar-width:none}
.llm-page::-webkit-scrollbar,.llm-page *::-webkit-scrollbar{display:none;width:0;height:0}
.llm-heading{display:flex;align-items:center;justify-content:space-between;gap:20px;margin-bottom:22px}
.llm-heading h1{font-size:22px;line-height:1.3;font-weight:700;letter-spacing:-.4px;margin:0;color:#273743}
.llm-back{display:inline-flex;align-items:center;gap:7px;padding:8px 0 8px 10px;border:0;background:transparent;color:#3e5361;font:inherit;font-size:12px;cursor:pointer}
.llm-back:hover{color:#273743}
.llm-layout{display:grid;grid-template-columns:minmax(0,1fr) 210px;gap:26px;align-items:start}
.llm-page .lucide{width:16px;height:16px;stroke-width:1.6;flex:none}
.llm-form{background:var(--surface,#fff);border:1px solid #d9e1e6;border-radius:14px;padding:22px 24px;min-width:0}
.llm-section-title{display:flex;align-items:center;margin-bottom:11px}
.llm-section-title h2{font-size:13px;font-weight:700;line-height:1.5;margin:0;color:#273743}
.llm-providers{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:4px;background:#f3f6f8;padding:4px;border-radius:9px}
.llm-provider{position:relative;display:flex;align-items:center;justify-content:center;min-width:0;height:40px;padding:0 20px 0 10px;background:transparent;border:0;border-radius:5px;color:#3e5361;cursor:pointer;font-family:inherit;transition:background .15s,color .15s}
.llm-provider:hover{background:#eaf0f2;color:#273743}
.llm-provider.is-selected{background:#fff;color:#9a4e32;box-shadow:0 1px 3px #243f4810}
.llm-provider strong{font-size:12px;font-weight:700;white-space:nowrap}
.llm-provider-logo{display:block;width:16px;height:16px;object-fit:contain;flex:none;margin-right:8px;opacity:1}

.llm-provider-check{position:absolute;right:5px;top:12px;display:none;color:#9a4e32}
.llm-provider.is-selected .llm-provider-check{display:flex}
.llm-connection{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.85fr);column-gap:14px;margin-top:22px}
.llm-connection>.llm-section-title,.llm-connection>.llm-field{grid-column:1 / -1}
.llm-connection>.llm-field:nth-child(2){grid-column:1}
.llm-connection>.llm-field:nth-child(3){grid-column:2}
.llm-field{margin-bottom:14px;min-width:0}
.llm-field>label{display:flex;align-items:center;justify-content:space-between;font-size:12px;font-weight:700;margin-bottom:7px;line-height:1.4;color:#3e5361}
.llm-field input,.llm-field select{display:block;width:100%;min-width:0;height:40px;box-sizing:border-box;border:1px solid #d9e1e6;border-radius:8px;padding:0 12px;background:#fbfcfd;color:#3e5361;font-family:inherit;font-size:13px;outline:none;transition:border-color .15s,box-shadow .15s,background .15s}
.llm-field input:focus,.llm-field select:focus{background:#fff;border-color:#b95535;box-shadow:0 0 0 3px #fbede780}
.llm-field input::placeholder{color:#5c6f7c;font-size:12px}
.llm-field input[readonly]{background:#f7f9fa;color:#3e5361;cursor:default}
.llm-field input[aria-invalid=true],.llm-field select[aria-invalid=true]{border-color:#b95535;background:#fffaf7}
.llm-field input[name=url],.llm-field input[name=model],.llm-field input[name=key]{font-family:var(--font);font-size:12px}
.llm-field-hint{font-size:11px;color:#5c6f7c;line-height:1.6;margin:6px 0 0}
.llm-select-wrap,.llm-key-wrap{position:relative}
.llm-select-wrap select{appearance:none;padding-right:37px}
.llm-select-wrap>.lucide{position:absolute;right:12px;top:12px;color:#5c6f7c;pointer-events:none}
@supports (appearance:base-select){
  .llm-select-wrap select,.llm-select-wrap select::picker(select){appearance:base-select}
  .llm-select-wrap select{display:flex;align-items:center;cursor:pointer}
  #llm-effort{anchor-name:--llm-effort}
  #llm-effort::picker(select){position-anchor:--llm-effort}
  #llm-connection-mode{anchor-name:--llm-connection-mode}
  #llm-connection-mode::picker(select){position-anchor:--llm-connection-mode}
  #llm-proxy-protocol{anchor-name:--llm-proxy-protocol}
  #llm-proxy-protocol::picker(select){position-anchor:--llm-proxy-protocol}
  .llm-select-wrap select::picker-icon{display:none}
  .llm-select-wrap select::picker(select){position:fixed;inset:auto;top:anchor(bottom);left:anchor(left);position-area:none;position-try-fallbacks:none;min-width:anchor-size(width);margin:6px 0 0;padding:5px;border:1px solid #e1e7eb;border-radius:10px;background:#fff;color:#3e5361;box-shadow:0 8px 30px #293c4517;font:inherit}
  .llm-select-wrap option{display:flex;align-items:center;gap:8px;min-height:35px;padding:8px 10px;border:0;border-radius:6px;background:transparent;cursor:pointer;font-size:13px}
  .llm-select-wrap option:hover,.llm-select-wrap option:focus{background:#f3f6f8;outline:none}
  .llm-select-wrap option:checked{background:#fff2eb;color:#9a4e32}
  .llm-select-wrap option::checkmark{order:1;margin-left:auto;color:#b95535;font-size:12px}
}
.llm-key-wrap input{padding-right:43px}
.llm-key-toggle{display:flex;align-items:center;justify-content:center;position:absolute;right:4px;top:4px;width:32px;height:32px;color:#5c6f7c;border:0;background:transparent;cursor:pointer;border-radius:4px}
.llm-key-toggle:hover{background:#f3f6f7;color:#3e5361}
.llm-effort{display:grid;grid-template-columns:minmax(0,1fr) 190px;align-items:center;gap:18px;margin:1px 0 0;padding:4px 0}
.llm-effort-description>label{display:flex;align-items:center;gap:8px;font-size:12px;font-weight:700;line-height:1.4;color:#3e5361}
.llm-effort-description>.llm-field-hint{margin-top:5px}
.llm-network{margin:12px 0 0}
.llm-network-row{display:grid;grid-template-columns:minmax(0,1fr) 190px;align-items:center;gap:18px}
.llm-network-row>label{font-size:12px;font-weight:700;line-height:1.4;color:#3e5361}
.llm-proxy-fields{display:grid;grid-template-columns:112px minmax(0,1fr) 86px;gap:0 12px;margin-top:17px}
.llm-proxy-fields[hidden],.llm-summary-proxy[hidden]{display:none}
.llm-proxy-fields>.llm-field{margin-bottom:0}
.llm-proxy-fields>.llm-field-hint{grid-column:1 / -1;margin-top:8px}
.llm-status{display:flex;align-items:flex-start;gap:8px;font-size:11px;line-height:1.6;color:#5c6f7c;margin-top:19px}
.llm-status:empty{display:none}
.llm-status-success{color:#37624b}.llm-status-error{color:#9a4e32}.llm-status-pending{color:#7b633f}
.llm-status-pending>.lucide{animation:llm-spin 1.5s linear infinite}
.llm-actions{display:flex;align-items:center;justify-content:flex-end;gap:12px;margin-top:20px;padding-top:18px;border-top:1px solid #d9e1e6}
.llm-actions>div{display:flex;align-items:center;gap:10px}
.llm-actions button{display:flex;align-items:center;justify-content:center;gap:7px;white-space:nowrap;height:37px;border-radius:8px;font-family:inherit;font-size:12px;cursor:pointer;padding:0 14px;transition:background .15s,border-color .15s}

.llm-test{background:#f4f6f7;border:1px solid transparent;color:#3e5361}
.llm-test:hover{background:#eaf0f3}
.llm-save{background:var(--accent,#ee805f);border:1px solid transparent;color:#fff}
.llm-save:hover:not(:disabled){background:var(--accent-hover,#df6947)}
.llm-actions [hidden]{display:none}
.llm-actions button:disabled{cursor:default;opacity:1;background:#edf1f4;border-color:#d9e1e6;color:#5c6f7c}
.llm-actions .llm-save:disabled{background:var(--accent,#ee805f);color:#fff;opacity:.45}
.llm-aside{padding-top:20px}
.llm-overview{padding:0}
.llm-diagram{padding:0 0 22px}
.llm-diagram-node{display:flex;align-items:center;gap:11px;min-width:0}
.llm-mini-brand{position:relative;display:block;overflow:hidden;flex-shrink:0;width:24px;height:24px;isolation:isolate;mix-blend-mode:multiply}
.llm-mini-brand>img{position:absolute;width:112px;max-width:none;left:-19.5px;top:-16.5px}
.llm-diagram-node>div{display:flex;flex-direction:column;gap:5px;min-width:0}
.llm-diagram-node strong{font-size:13px;font-weight:700;color:#273743}
.llm-diagram-node small{font-size:11px;color:#5c6f7c;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:176px}
.llm-model-node{margin-top:23px}
.llm-model-symbol{display:flex;align-items:center;justify-content:center;width:24px;flex:none}
.llm-model-symbol>img{display:block;width:16px;height:16px;object-fit:contain;opacity:1}
.llm-connection-dot{width:5px;height:5px;border-radius:50%;background:#899da9;margin-left:auto;flex:none}
.llm-connection-dot.is-connected{background:#b95535}
.llm-summary{margin:0;padding:0}
.llm-summary>div{display:flex;align-items:center;justify-content:space-between;gap:12px;margin:0 0 15px;font-size:12px}
.llm-summary dt{color:#5c6f7c}.llm-summary dd{margin:0;color:#3e5361;text-align:right}
.llm-summary-proxy-address{min-width:0;overflow-wrap:anywhere}
.llm-provider:focus-visible,.llm-actions button:focus-visible,.llm-key-toggle:focus-visible,.llm-back:focus-visible{outline:2px solid #9a4e32;outline-offset:3px}
@keyframes llm-spin{to{transform:rotate(360deg)}}
@media(max-width:1320px){.llm-page{padding:22px}.llm-layout{grid-template-columns:minmax(0,1fr) 184px;gap:22px}.llm-form{padding:21px}.llm-provider{padding-left:5px;padding-right:17px}.llm-provider-logo{margin-right:5px}.llm-effort,.llm-network-row{grid-template-columns:minmax(0,1fr) 175px;gap:14px}}
@media(max-width:1000px){.llm-layout{grid-template-columns:minmax(0,1fr)}.llm-aside{padding:0}.llm-overview{display:grid;grid-template-columns:190px minmax(0,1fr);gap:24px}.llm-summary{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));column-gap:24px}.llm-page{padding:20px}.llm-connection>.llm-field:nth-child(2),.llm-connection>.llm-field:nth-child(3){grid-column:1 / -1}}
@media(prefers-reduced-motion:reduce){.llm-page *{animation:none!important;transition:none!important}}
</style>

<style>
/* Inset desktop navigation, inspired by ARTEX's compact neutral shell. */
:root{--sidebar:246px}
.sidebar{inset:10px auto 10px 10px;width:226px;border:1px solid #e2e5e7;border-radius:14px;padding:20px 11px 10px;box-shadow:0 2px 4px #25333d03}
.brand{padding-left:5px;margin-bottom:22px}.brand-art{width:188px;height:48px}
.create-button{height:35px;border-radius:8px;font-size:12px;background:var(--accent);border-color:var(--accent)}
.project-search{margin:12px 0 22px;height:32px;border-radius:7px;gap:6px}
.project-search input{font-size:11px}.project-search .lucide{height:14px;width:14px}
.nav-caption{margin:0 9px 8px;letter-spacing:0}
#project-list{min-height:0;overflow-x:hidden;overflow-y:auto;scrollbar-color:auto;overscroll-behavior:contain}
#project-list::-webkit-scrollbar{width:5px}#project-list::-webkit-scrollbar-track{background:transparent}#project-list::-webkit-scrollbar-thumb{background:#ccd5db;border-radius:8px}#project-list::-webkit-scrollbar-thumb:hover{background:#aebfc9}
.sidebar>.brand,.sidebar>.create-button,.sidebar>.project-search,.sidebar>.nav-caption{flex:none}
.project-item{padding:10px 8px;gap:9px;margin:3px 0;border-radius:8px;align-items:flex-start}
.project-item.selected{background:#fff1eb;border-color:#f5e3d9}.project-item.selected:before{display:none}

.project-copy{flex:1;min-width:0;padding:0}.project-copy strong{font-size:12px;max-width:none;font-weight:400;line-height:1.8}
.project-copy small{display:block;margin-top:4px;white-space:nowrap;text-overflow:ellipsis;overflow:hidden}
.project-copy small span:last-child{color:#5c6f7c;margin:0}.project-copy .status-dot{margin-right:5px;width:4px;height:4px;vertical-align:2px}
.project-item.selected .project-copy strong{color:#9a4e32}.selected .project-copy small{color:#9a4e32}

.project-page{padding:12px 16px 12px 8px;min-height:600px}.project-heading{margin-bottom:12px}.title-row h1{font-size:20px}.project-body{gap:12px}.canvas-panel,.activity-pane{border-radius:12px}

@media(min-width:1700px){.sidebar{padding-top:22px}.brand{padding-left:11px}}

/* Shared visual language: one icon weight and quiet, consistent separators. */

svg.lucide,svg.icon{fill:none;stroke:currentColor;stroke-width:1.6;stroke-linecap:round;stroke-linejoin:round}
button svg.lucide,button svg.icon,a svg.lucide{pointer-events:none}
.button .lucide,.icon-button .lucide{width:16px;height:16px}
.button{font-weight:500}.secondary{border-color:#e2e7ea}.secondary:hover{background:#f7f9fa;border-color:#d3dce1}
.icon-button{color:#647682}.icon-button:hover{background:#f1f4f6;color:#485d6b}

.title-row h1{font-weight:700;color:#273743;letter-spacing:-.45px}.project-description{font-size:11px}.project-description .lucide{width:13px;height:13px}
.badge{border-radius:5px;padding:4px 7px;font-size:10px}.badge.running{background:#fff2e9;color:#9a4e32}.badge.done{background:#f0f5f1;color:#486b57}.badge.pending,.badge.paused{background:#f1f4f6;color:#5c6f7c}
.project-copy strong{color:#273743}.project-copy small{color:#5c6f7c;font-size:10px}

.canvas-panel,.activity-pane{box-shadow:none;border-color:#e2e7ea}.view-tabs button{gap:6px}.view-tabs button .lucide{width:14px;height:14px}.view-tabs button.active{font-weight:700}.view-tabs button.active:after{height:2px;border-radius:2px}.view-tabs button small{background:#f0f4f6;color:#5c6f7c;min-width:16px;text-align:center}
.canvas-footer{height:32px;border-color:#edf0f2}.legend{gap:10px}.legend button{font-size:10px;color:#5c6f7c}.legend button:hover,.legend button.active{background:#f1f5f6;color:#3e5361}.legend-dot{background:#9aafa7;width:4px;height:4px}.legend-dot.running{background:#df967c}.legend-dot.pending{border-color:#bdc9cf}.task-progress{font-size:10px;color:#5c6f7c}.progress-track{background:#edf1f3;width:38px;height:2px}.progress-track i{background:#dea389}
.graph-stage{background:#f2f6f8;background-image:radial-gradient(#dce5ea .5px,transparent .5px);background-size:22px 22px}
.canvas-empty-state{position:absolute;inset:0;display:grid;place-content:center;justify-items:center;gap:8px;padding:32px;text-align:center;color:var(--muted)}
.canvas-empty-state h2{margin:0;font-size:18px;line-height:26px;font-weight:700;color:var(--ink)}
.canvas-empty-state p{max-width:360px;margin:0;font-size:12px;line-height:20px}
/* Type colors identify nodes; state stays in labels, line styles and selection rings. */
.graph-node{border:1px solid #cdd9e1;border-radius:8px;padding:10px 11px;box-shadow:0 2px 5px #2e465008}
.graph-node.fact{--node-bg:#f3f8ff;--node-border:#bfd1ed;--node-accent:#466f9a;--node-title:#2b445e;--node-ring:#6f9dcc26}
.graph-node.task{--node-bg:#f7f4ff;--node-border:#cfbfee;--node-accent:#785e9a;--node-title:#473660;--node-ring:#927abf26}
.graph-node.goal{--node-bg:#edf8f4;--node-border:#a8d6c4;--node-accent:#327461;--node-title:#245546;--node-ring:#4b9c8026}
.graph-node.subgoal{--node-bg:#f2faf7;--node-border:#bddbca;--node-accent:#4b7f68;--node-title:#365e4c;--node-ring:#699e8226}
.graph-node.finding{--node-bg:#fff7e8;--node-border:#e6cca0;--node-accent:#906329;--node-title:#65491f;--node-ring:#c79b5126}
.graph-node.start{--node-bg:#344960;--node-border:#344960;--node-accent:#d5e0ef;--node-title:#f6f9fd;--node-ring:#6c8caf30}
.graph-node[data-view-type]{background:var(--node-bg,#fff);border-color:var(--node-border,#cdd9e1)}
.graph-node[data-view-type]:hover,.graph-node[data-view-type].lineage{border-color:var(--node-accent,#6b8092)}
.graph-node[data-view-type]:hover{box-shadow:0 3px 9px #344e5d10}
.graph-node[data-view-type].selected{border-color:var(--node-accent,#6b8092);box-shadow:0 0 0 3px var(--node-ring,#6b809226)}
.graph-node[data-view-type].dim{border-color:var(--node-border,#cdd9e1);box-shadow:none}
.node-top{margin-bottom:8px;gap:4px}.node-kind{font-size:10px;gap:4px;flex:none}.node-kind .icon{width:13px;height:13px}
.node-status{font-size:10px;color:#5c6f7c}.graph-node .node-status{min-width:0;max-width:72px}.node-status .icon{width:11px;height:11px;color:#486b57}
.node-title{font-size:13px;line-height:1.6;max-height:42px;font-weight:700}
.graph-node[data-view-type] .node-title,.graph-node[data-view-type].dim .node-title{color:var(--node-title,#273743)}
.graph-node[data-view-type] .node-kind,.graph-node[data-view-type].dim .node-kind{color:var(--node-accent,#5c6f7c)}
.graph-node.fact:not(.invalid) .node-status,.graph-node.start .node-status{display:none}.graph-node.task.done .node-status>span{display:none}
.graph-node[data-view-type].running .node-status{color:#9a4e32}.node-running-dot{background:#ef815f;width:4px;height:4px;box-shadow:none}
.graph-node[data-view-type].invalid{border-style:dashed}.graph-node[data-view-type].invalid .node-status{color:#a1545e}
.graph-edges .graph-edge[data-edge-kind]{stroke:var(--edge-color,#7b8b9e)}
.graph-edge{stroke-width:1.4}.graph-edge.done{stroke-dasharray:none}.graph-edge.pending{stroke-dasharray:4 5}
.graph-edge.running{stroke-width:1.6;stroke-dasharray:5 5;animation:none}.graph-edge.paused{stroke-dasharray:7 4 1 4}.graph-edge.invalid{stroke-dasharray:3 5}
.graph-edge.lineage{stroke-width:1.9}.graph-edge.selected,.graph-edge.invalid.selected{stroke-width:2.3}.graph-edge.dim{opacity:.28}
.graph-edge-hit:focus-visible{stroke:var(--edge-color,#7b8b9e);stroke-opacity:.16}
.graph-edge-label{font-size:12px;font-weight:500;stroke:#f2f6f8;stroke-width:5}
.graph-edges .graph-edge-label[data-edge-kind]{fill:var(--edge-color,#5c6f7c)}
.zoom-controls{box-shadow:0 2px 6px #2d485304;border:1px solid #e1e8ec;border-radius:8px;padding:3px}.zoom-controls .icon-button{width:28px;height:26px}.zoom-controls .lucide{width:14px;height:14px}.zoom-controls>i{background:#e9eef1}.zoom-controls>span{color:#5c6f7c;font-size:10px}

.entry-title{display:flex;align-items:flex-start}.timeline-entry h3{flex:1;min-width:0;margin:0}

.selection-header{padding:0 0 21px}.inspector-title{font-size:15px;font-weight:700;color:#506674;margin:10px 0 12px;line-height:1.8}.inspector-meta{border-color:#eaf0f3;margin-top:23px;padding-top:18px;color:#8ba0ae;gap:15px;font-size:11px}
.section-title{font-size:14px;font-weight:700;color:#273743;margin:0 0 18px}.material-row{margin:0;border:0;border-bottom:1px solid #edf1f4;border-radius:0}.material-row:last-child{border-bottom:0}.material-row>.lucide{width:17px;height:17px;color:#647682}.material-row>span:last-child{margin-left:0;color:inherit;font-size:inherit}.material-row small{font-size:10px}
.result-card{margin-top:12px;padding:18px;border:1px solid #e8edf0;border-radius:8px;background:#fff}.result-card h3{font-size:13px;color:#273743;margin:0 0 10px}.result-card p{font-size:12px;color:#3e5361;line-height:1.85}.result-scope{display:flex;align-items:center;gap:7px;margin-bottom:9px}.result-scope .lucide{width:13px;height:13px;color:#647682}.result-scope code{font-size:11px}.empty-state{color:#5c6f7c;font-size:12px}.empty-state .lucide{width:24px;height:24px;color:#647682}
dialog{padding:25px;border-color:#e2e9ed;border-radius:14px;box-shadow:0 20px 65px #2239471c}.dialog-header{margin-bottom:23px;align-items:center}.dialog-header h2{font-size:20px;font-weight:700;letter-spacing:-.4px;margin:0;color:#273743}.dialog-header>.icon-button{margin:0 -4px 0 0}.dialog-footer{justify-content:flex-end;border:0;padding-top:0;margin-top:24px}.field-label{color:#3e5361;font-size:12px;margin-top:18px}.field-label>span{color:#5c6f7c}.type-choices{gap:8px;margin-bottom:22px}.type-choices span{border-color:#e4ecef;border-radius:8px;color:#3e5361;height:40px;font-size:11px}.type-choices span .lucide{width:15px;height:15px}.type-choices input:checked+span{background:#fff6f0;border-color:#eccab9;color:#9a4e32}dialog input:not([type=radio]):not([type=file]),dialog textarea{border-color:#e2ebf0;background:#fff;border-radius:8px;font-size:12px;color:#5d7b8e}dialog input::placeholder,dialog textarea::placeholder{color:#a4bccb}

/* Project actions stay compact; details appear only when requested. */
.project-row{position:relative}.project-more{position:absolute}.project-more .lucide{width:15px;height:15px}.project-row .project-copy strong{max-width:none}
.project-heading-copy{flex:1}.target-summary{overflow:hidden;text-overflow:ellipsis}.project-description>span:first-of-type,.project-description>.meta-divider{flex:none}
.project-menu-popover{position:fixed;z-index:50;border:1px solid #e1e7eb;border-radius:10px;background:#fff}.project-menu-popover>button{display:flex;align-items:center;gap:10px;width:100%;text-align:left;border-radius:6px;padding:0 10px;color:#3e5361}.project-menu-popover>button:hover,.project-menu-popover>button:focus-visible{background:#f3f6f8}.project-menu-popover .lucide{width:15px;height:15px}.project-menu-popover>button.danger-item{color:#ba7d6d}.project-menu-popover>button.danger-item:hover{background:#fcf2ef}
.badge.terminated{background:#f8efec;color:#b58c7f}.status-dot.terminated{background:#bd9488}
.canvas-header{justify-content:space-between}.canvas-header .view-tabs{flex:none}#run-select{font-size:10px;padding:5px 24px 5px 9px;max-width:160px;height:29px;border-color:#e5ebee;color:#8b9ca7;background-color:#fafcfd}
.sidebar-empty{padding:22px 8px;line-height:1.8}.sidebar-empty .text-button{display:flex;margin:10px auto 0}
.text-button{display:inline-flex;align-items:center;gap:5px;padding:4px 0;color:#bd866e;font-size:11px;border-radius:4px}.text-button .lucide{width:13px;height:13px}.text-button:hover{color:#985d43}.section-heading{display:flex;justify-content:space-between;align-items:center;gap:12px}.section-heading .section-title{margin:0}.project-inputs{border:1px solid #e7edf0}.result-card .text-button{margin-top:10px}
.timeline-entry p{white-space:pre-wrap;overflow-wrap:anywhere}
#action-dialog{width:445px}#action-dialog .dialog-header{margin-bottom:18px}#action-title{font-size:19px}#action-project{font-size:13px;font-weight:700;color:#607b8c;overflow-wrap:anywhere;margin-bottom:11px}#action-description{font-size:12px;line-height:1.9;color:#8c9faa}.danger-button{background:#c77d69;border:1px solid #c77d69;color:white}.danger-button:hover{background:#b76d59;border-color:#b76d59}#action-dialog .form-error{color:#b97561}

/* Compact global model entry and project lifecycle timestamps. */
.visually-hidden{position:absolute!important;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip-path:inset(50%);white-space:nowrap;border:0}

.top-model-entry{display:inline-flex;align-items:center;flex:none}
.project-times{display:flex;align-items:center;flex-wrap:wrap;gap:6px 19px;font-size:10px;color:#a0adb7;line-height:16px;margin-top:6px;font-variant-numeric:tabular-nums}.project-times>span{display:inline-flex;gap:7px;white-space:nowrap}.project-times time{color:#8196a4}
.heading-actions{padding-top:3px}
.activity-pane,.activity-content{min-width:0}.activity-content{min-height:0;overflow-x:hidden;overflow-y:auto;overscroll-behavior:contain}.timeline-entry h3{overflow-wrap:anywhere}
.log-long-content{font-size:11px;min-width:0}.log-long-content summary{cursor:pointer;list-style:none}.log-long-content summary::-webkit-details-marker{display:none}.log-text-preview{display:-webkit-box;-webkit-box-orient:vertical;overflow:hidden;overflow-wrap:anywhere;white-space:pre-wrap}.log-long-content[open] .log-text-preview{display:none}.log-long-content:not([open]) .log-label-open,.log-long-content[open] .log-label-closed{display:none}

/* The project title is static; long names remain available through native hover text. */
.title-row h1{min-width:0;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;cursor:text}.title-row>.badge{flex:none}
.project-copy strong{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.project-row,.project-item{min-width:0}

/* Search stays visible while each log uses the observation panel's single scroll area. */
#activity-toolbar{flex:none}
.activity-search{display:flex;align-items:center;gap:7px;padding:0 8px;border:1px solid #e5ebee}
.activity-search:focus-within{border-color:#dfb7a5;background:#fff}.activity-search>.lucide{width:13px;height:13px}
.activity-search input{width:100%;min-width:0;border:0;background:transparent;padding:0;box-shadow:none}
.activity-search input:focus{box-shadow:none}.activity-search input::-webkit-search-cancel-button{display:none}
.activity-search .icon-button{width:20px;height:22px;flex:none;margin-right:-3px}.activity-search .icon-button .lucide{width:12px;height:12px}

#activity-collapse-all{gap:4px;white-space:nowrap;font-size:10px}
#activity-collapse-all:disabled{opacity:.4}
.activity-content{overflow-anchor:none}
.timeline-entry h3{-webkit-line-clamp:3;-webkit-box-orient:vertical}.entry-title{gap:4px}
.timeline-entry p,.log-text-preview{font-size:12px;color:#718793;line-height:1.9}
.log-expand{display:flex;align-items:center;gap:5px;min-height:28px}
.log-expand>.lucide{width:12px;height:12px}
.log-long-content[open]>summary{position:sticky;top:0;z-index:2;border-bottom:1px solid #edf1f3}
.log-long-content[open] .log-expand{margin:0 0 7px;padding:3px 0}.log-long-content[open] .log-expand>.lucide{transform:rotate(180deg)}
.log-long-content summary:focus-visible{outline-offset:1px}
.log-empty{padding:45px 0;line-height:1.8}
.activity-content mark{color:#996342;background:#fff0dc;border-radius:2px}
#activity-to-latest{position:absolute;bottom:12px;left:50%;transform:translateX(-50%);display:flex;align-items:center;gap:5px;padding:7px 11px;border-radius:7px;background:#fff;border:1px solid #dfe7eb;box-shadow:0 3px 13px #2e465013;color:#758d9d;font-size:10px;white-space:nowrap;z-index:4}
#activity-to-latest .lucide{width:12px;height:12px}#activity-to-latest:hover{border-color:#d4b49f;color:#a37557}

/* One overview surface groups identity, lifecycle dates and project actions. */
:root{--workspace-gap:12px;--shell-edge:12px;--nav-width:226px;--sidebar:calc(var(--shell-edge) + var(--nav-width) + var(--workspace-gap))}
.sidebar{inset:var(--shell-edge) auto var(--shell-edge) var(--shell-edge);width:var(--nav-width)}
.workbench{margin-left:var(--sidebar)}

.top-model-entry{height:34px;min-width:104px;justify-content:center;gap:8px;padding:0 12px;border:1px solid #dfe6ea;border-radius:8px;background:#fff;color:#3e5361;font-size:12px;line-height:18px}
.top-model-entry>.lucide{width:15px;height:15px;color:#647682}
.top-model-entry:hover,.top-model-entry.active{border-color:#e6c6b7;background:#fff5f0;color:#9a4e32}.top-model-entry:hover>.lucide,.top-model-entry.active>.lucide{color:inherit}
.project-page{padding:var(--shell-edge) var(--shell-edge) var(--shell-edge) 0}
.project-body{gap:var(--workspace-gap)}
.project-heading{display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:center;gap:22px;min-height:72px;flex:none;margin:0 0 var(--workspace-gap);padding:11px 16px;border:1px solid #e2e7ea;border-radius:12px;background:#fff}
.project-heading-copy{min-width:0;display:grid;gap:5px}
.project-heading-copy.empty-project-heading{visibility:hidden;min-height:49px}
.project-heading .title-row{margin:0;min-width:0;gap:10px}.project-heading .title-row h1{font-size:20px;line-height:26px}
.project-heading .project-description{min-width:0;margin:0;line-height:18px;font-size:11px;gap:18px;overflow:visible}
.project-identity{display:flex;align-items:center;gap:6px;min-width:0;max-width:42%;flex:0 1 auto}
.project-identity>.lucide,.project-identity>span:not(.target-summary),.project-identity>.meta-divider{flex:none}
.project-heading .target-summary{min-width:0;white-space:nowrap}
.project-heading .project-times{display:flex;flex:none;flex-wrap:nowrap;align-items:center;gap:8px;margin:0;padding:0 0 0 18px;min-height:18px;border-left:1px solid #e9edf0;font-size:12px;line-height:18px;color:#5c6f7c}
.project-heading .project-times>span{display:flex;align-items:center;gap:7px;white-space:nowrap}
.project-heading .project-times time{font-size:12px;font-variant-numeric:tabular-nums}
.project-heading .heading-actions{padding:0;border:0;justify-content:flex-end;align-self:stretch}
.project-heading .heading-actions .button{height:34px}
@media(min-width:1700px){:root{--nav-width:240px}}

/* Project navigation prioritizes identity, with an independent, reachable action target. */
.nav-caption{color:#586b8b;font-size:11px;margin-bottom:7px}
.nav-caption>span:last-child{color:#58739a;font-size:11px;font-variant-numeric:tabular-nums;background:#e8eef8;border-radius:5px;padding:1px 6px}
.project-row .project-item{min-height:62px;padding:10px 34px 10px 10px;margin:0 0 5px;gap:0;border-radius:9px}
.project-row .project-item:hover{background:#eef2f9;border-color:#dce5f0}

.project-row .project-copy strong{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:13px}
.project-row .project-item.selected .project-copy strong{color:#9a4e32}
.project-copy .project-meta{display:flex;align-items:center;margin-top:6px;line-height:15px;overflow:visible}

.project-meta .project-state{display:inline-flex;align-items:center;gap:5px}
.project-copy .project-state .status-dot{width:5px;height:5px;margin:0}
.selected .project-meta .project-state{color:#9a4e32}
.project-more{right:4px;top:7px;width:28px;height:28px;color:#647682;border-radius:6px}
.project-more:hover,.project-more:focus-visible,.project-more[aria-expanded=true]{background:#e7edef;color:#425969}
.project-row:has(.selected) .project-more:hover,.project-row:has(.selected) .project-more[aria-expanded=true]{background:#f6dfd2;color:#975b40}

/* Timeline entries share a quiet visual spine. */
.activity-heading{gap:8px}
.activity-heading h2{flex:none;gap:7px}

#activity-match-count{margin:0 0 0 auto;line-height:16px;border-radius:5px;white-space:nowrap}
#activity-collapse-all{height:28px;width:28px;flex:none;display:grid;place-items:center;padding:0;border-radius:5px;color:#71838e}
#activity-collapse-all .lucide{width:14px;height:14px}
#activity-collapse-all:hover{color:#9b654d;background:#fff3ed}

.activity-search{color:#8598a4;border-radius:7px}
.activity-search input{font-size:12px;color:#4f6574}
.activity-search input::placeholder{color:#8696a1}
.activity-content{scrollbar-color:#cdd7dd transparent}
.timeline-entry,.timeline-entry.running{border:0;margin:0;padding:0 0 22px 20px;border-radius:0;background:transparent;min-width:0}
.timeline-entry:last-child{border:0;padding-bottom:3px}
.log-stream>.timeline-entry:before{content:'';position:absolute;background:#fff;border:1px solid #b8c7cf;border-radius:50%;z-index:1}
.log-stream>.timeline-entry:after{content:'';position:absolute;width:1px}
.log-stream>.timeline-entry:last-child:after{display:none}
.log-stream>.is-finding:before{border-radius:2px;transform:rotate(45deg)}

.entry-headline{display:flex;align-items:flex-start;gap:8px;margin-bottom:3px}
.entry-title{min-width:0;flex:1;margin:0}
.entry-meta{flex:none;margin:0}
.entry-meta time{font-variant-numeric:tabular-nums;white-space:nowrap;cursor:default}
.timeline-entry h3{display:block;overflow:visible;font-size:13px;line-height:20px;font-weight:550;color:#405563}
.log-title{display:flex;align-items:center;gap:7px;width:calc(100% + 6px);margin-left:-6px;padding:3px 6px;text-align:left;font:inherit;color:inherit;border-radius:8px;overflow:hidden}
.log-title>span{flex:1;min-width:0;display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:2;overflow:hidden;overflow-wrap:anywhere}
.log-title>.lucide{flex:none;width:13px;height:13px;color:#99a7b1}
.log-stream .log-title>.lucide{display:none}

.log-title:hover,.log-title:focus-visible{color:#a06348}
.log-title:hover>.lucide,.log-title:focus-visible>.lucide{color:inherit}

.timeline-entry .log-text-preview{white-space:normal}
.log-long-content{color:#687d89;line-height:21px}

.log-expand{margin:3px 0 0;line-height:20px;background:transparent}
.log-long-content[open]>summary{background:#fff;box-shadow:0 -3px 0 #fff}
.log-long-content[open] .log-expand{background:transparent}

.log-read-sticky{display:none;margin-left:auto;align-items:center;gap:4px;min-height:28px;padding:3px 0;color:#5d7280;font-size:10px;border-radius:4px}
.log-read-sticky .lucide{width:12px;height:12px}
.log-read-sticky:hover{color:#a05f43}
.log-long-content[open] .log-read-sticky{display:inline-flex}

/* Feedback stays local to the chosen record; long reading never moves the canvas. */
.log-title.is-reading{background:#fff3eb;color:#a06348}

@media(prefers-reduced-motion:no-preference){
  .log-title{transition:background-color 140ms ease,color 140ms ease,transform 110ms ease}
  .log-title:active{transform:scale(.985)}
  .log-title>.lucide{transition:transform 160ms ease,color 140ms ease}
  .log-title:hover>.lucide{transform:translateX(2px)}
  .log-expand>.lucide{transition:transform 160ms ease}
  .log-long-content[open]>.log-body{animation:log-body-reveal 180ms ease-out both}
}
@keyframes log-body-reveal{from{opacity:0;transform:translateY(-3px)}to{opacity:1;transform:translateY(0)}}

/* Inputs and results share one reading area, with controls above the content. */
.tab-content{min-height:0;min-width:0;overflow-x:hidden;overflow-y:auto;overscroll-behavior:contain;scrollbar-color:#cdd7dd transparent;scroll-padding-top:16px}
.project-inputs{display:grid;margin:0 0 28px;padding:0;border:0}
.content-card{min-width:0;border:1px solid #e6ecef;border-radius:10px;padding:0 18px 18px;background:#fff}
.content-heading{position:sticky;top:0;z-index:2;display:flex;align-items:center;justify-content:space-between;background:#fff;border-radius:9px 9px 0 0}
.content-heading h3{margin:0;line-height:20px;flex:none}
.content-actions{display:flex;align-items:center;flex:none}
.content-actions .text-button{display:inline-flex;align-items:center;gap:5px;margin:0;white-space:nowrap}
.content-actions .text-button:hover{color:#a36d52}.content-actions .lucide{width:12px;height:12px}
.content-actions [aria-expanded=true]>.lucide{transform:rotate(180deg)}
.content-card .content-text{margin:0;font:13px/1.95 var(--font);white-space:pre-wrap;overflow-wrap:anywhere;word-break:normal;max-width:100%}
.content-text.is-collapsed{display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:5;overflow:hidden}
.material-row{min-width:0;align-items:flex-start}.material-row>.lucide{flex:none;margin-top:2px}.material-row>span{min-width:0;flex:1}.material-row strong{white-space:pre-wrap;overflow-wrap:anywhere}

.result-card .result-title{overflow-wrap:anywhere;display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden;margin:0 0 8px}
.result-card .result-scope{align-items:flex-start;margin:0 0 16px;min-width:0}
.result-scope>.lucide{flex:none;margin-top:3px}.result-scope code{font:11px/1.8 var(--font);overflow-wrap:anywhere;white-space:pre-wrap;min-width:0;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.result-card .result-locate{display:inline-flex;align-items:center;gap:6px}.result-locate>.lucide{width:13px;height:13px}

:root{--ink:#273743;--text:#3e5361;--muted:#5c6f7c;--line:#d9e1e6;--bg:#f2f5f7;--accent:#ee805f;--accent-soft:#fff3ed}
*{scrollbar-width:none}
*::-webkit-scrollbar{display:none;width:0;height:0}
#main,#project-list,.tab-content,.activity-content{scrollbar-width:none;scrollbar-gutter:auto}
#project-list{padding-right:0}
.sidebar{background:#f6f7fc;border-color:#dce2ee}
.project-search{background:#fff;border-color:#dbe2ee;color:#687d9a}
.project-search input::placeholder{color:#5c6f7c}
.project-copy .project-meta{font-size:11px;gap:9px;color:#5c6f7c}
.project-meta .project-type,.project-meta .project-state{color:#5c6f7c}
.project-row .project-copy strong{font-weight:400;line-height:20px;color:#273743}
.project-row .project-item.selected{background:#fff0e7;border-color:#f1d4c2}
.project-item[data-project-status="done"] .project-state{color:#39735f}
.project-item[data-project-status="done"] .status-dot{background:#559b7d}
.project-item[data-project-status="paused"] .status-dot{background:#9384ac}
.project-item[data-project-status="pending"] .status-dot{background:#8ca2b7}
.project-item[data-project-status="terminated"] .status-dot{background:#c17e87}

.project-heading,.canvas-panel,.activity-pane{border-color:var(--line)}
.project-heading .title-row h1{font-weight:700;letter-spacing:-.35px;color:#273743}
.project-heading .target-summary,.project-description{color:#5c6f7c}
.project-heading .project-times time{color:#5c6f7c}
.canvas-header{height:42px;gap:14px;padding:0 17px;border-color:#edf1f3}
.view-tabs{gap:22px}.view-tabs button{font-size:12px;color:#5c6f7c}
.view-tabs button.active{color:#273743}.view-tabs button.active:after{background:#ee9779}
.workspace-actions{display:flex;align-items:center;justify-content:flex-end;gap:8px;min-width:0}
.workspace-actions .button{height:30px;padding:0 10px;font-size:11px;gap:6px;color:#3e5361;border-color:#e5ebef;border-radius:7px}
.workspace-actions .button .lucide{width:13px;height:13px}
.workspace-actions #project-manage{background:transparent;border-color:transparent}
.workspace-actions #project-manage:hover{background:#f4f7f8;color:#435f70}
.workspace-actions #add-hint{background:#fff9f6;border-color:#f3e3da;color:#9a4e32}
.workspace-actions #add-hint:hover:not(:disabled){background:#fff1e9;border-color:#edc8b6;color:#9d583b}
.workspace-actions #run-select{max-width:125px;font-size:10px}
.tab-content{padding:18px 22px}
.section-heading{margin-bottom:12px}
.section-heading .section-title{font-size:13px;font-weight:700;color:#273743}
.project-inputs{gap:0;margin-bottom:22px;border:1px solid var(--line);border-radius:10px;overflow:clip;background:#fff}
.project-inputs .content-card{border:0;border-radius:0;padding:0 16px 13px;background:transparent}
.project-inputs .content-card+.content-card{border-top:1px solid #edf1f3}
.content-heading{padding:11px 0 8px;gap:10px}
.content-heading h3{font-size:12px;color:#3e5361;font-weight:700}
.content-actions{gap:12px}.content-actions .text-button{color:#5c6f7c;font-size:11px}
.content-card .content-text{font-size:13px;line-height:1.8;color:#3e5361}
.material-row{padding:11px 2px;gap:10px;border-color:#edf1f3}
.material-row strong{font-size:12px;font-weight:700;line-height:1.65;color:#273743}
.material-row small{color:#5c6f7c;margin-top:3px}
.tab-content>.empty-state{padding:36px 18px;gap:10px;font-size:12px;color:#5c6f7c}
.tab-content>.empty-state .lucide{width:24px;height:24px;color:#647682}
.result-card.content-card{padding:0 16px 14px;margin-top:10px;border-color:var(--line)}
.result-card .result-title{font-size:15px;line-height:1.55;font-weight:700;color:#273743;margin-bottom:8px}
.result-card .result-scope{padding:7px 10px;margin-bottom:11px;background:#f5f8f9;border-radius:6px}
.result-scope code{color:#3e5361}
.result-card .result-locate{margin-top:11px;font-size:11px;color:#9a4e32}

.project-menu-popover{width:142px;padding:5px;border-color:var(--line);box-shadow:0 8px 30px #293c4512}
.project-menu-popover>button{height:34px;font-size:12px}
.project-menu-popover>button:last-child{border-top:0;margin-top:2px;border-radius:6px}
#toast{background:#354952;border-color:#354952;color:#f5f9fa}

/* Project details and execution controls share one management surface. */
#project-dialog{width:680px;max-width:calc(100vw - 48px);height:min(650px,82dvh);max-height:calc(100dvh - 48px);padding:0;overflow:hidden;border:1px solid var(--line);border-radius:16px;box-shadow:0 22px 80px #253f4b20}
#project-dialog[open]{display:flex;flex-direction:column}
#project-dialog>.dialog-header{padding:23px 26px 18px;margin:0;flex:none;align-items:flex-start}
.dialog-eyebrow{font-size:11px;line-height:18px;color:#5c6f7c}
#project-dialog-title{font-size:21px;line-height:29px;font-weight:700;color:#273743;margin:4px 0 0}
.project-info-body{flex:1;min-height:0;overflow:auto;overscroll-behavior:contain;padding:0 26px 22px}
.project-info-body:focus-visible{outline-offset:-4px;outline-color:#f0c9b8}
.project-info-identity{display:flex;align-items:flex-start;gap:12px;padding:0 0 18px}
.project-info-identity h3{flex:1;min-width:0;white-space:pre-wrap;overflow-wrap:anywhere;font-size:15px;line-height:1.7;font-weight:700;color:#273743}
.project-info-identity .badge{margin-top:3px;flex:none}
.project-info-meta{display:grid;grid-template-columns:1fr 1fr;gap:14px 26px;margin:0 0 20px;padding:17px 18px;background:#f7f9fa;border-radius:9px}
.project-info-meta>div{min-width:0}.project-info-meta dt{font-size:11px;color:#5c6f7c;margin-bottom:5px}
.project-info-meta dd{font-size:12px;line-height:1.65;color:#3e5361;margin:0;overflow-wrap:anywhere;font-variant-numeric:tabular-nums}
.project-info-section+.project-info-section{border-top:1px solid #edf1f3;padding-top:18px;margin-top:20px}
.project-info-section h3{font-size:12px;font-weight:700;color:#3e5361;margin-bottom:8px}
.project-info-section p{font-size:13px;line-height:1.85;color:#3e5361;white-space:pre-wrap;overflow-wrap:anywhere}
.project-info-footer{display:flex;align-items:center;justify-content:space-between;gap:16px;padding:16px 26px;border-top:1px solid #edf1f3;flex:none;background:#fff}
.project-info-footer>span{font-size:11px;line-height:1.6;color:#5c6f7c}
.project-info-footer>div{display:flex;gap:8px}.project-info-footer .button{height:33px;font-size:12px}
.danger-outline{border:1px solid #efdcd7;color:#9a4e32;background:#fff}.danger-outline:hover{background:#fff5f1;border-color:#e8c4b8}

/* Blackboard overview: an activity, a timestamp and a short, readable summary. */
.activity-heading{height:48px;padding:0 16px}
.activity-heading h2{font-size:13px;font-weight:550;color:#405764}
.activity-heading h2 .lucide{color:#93a6b0;width:15px;height:15px}
#activity-match-count{padding:0;background:transparent;font-size:10px;color:#91a2ad}
#activity-toolbar{padding:0 15px 14px}
.activity-search{height:32px;background:#f6f8f9;border-color:#ebf0f2}
.activity-content{padding:0 15px 36px}

.timeline-entry p,.timeline-entry .log-text-preview{font-size:12px;line-height:20px;color:#637c8a}

.log-text-preview{-webkit-line-clamp:3}

.log-stream>.timeline-entry{padding-bottom:19px;padding-left:17px}
.log-stream .timeline-entry h3{font-size:13px;line-height:20px;font-weight:500;color:#4d6573}
.log-title{min-height:24px;padding-top:2px;padding-bottom:2px}
.entry-meta,.entry-meta time{font-size:10px;line-height:24px;color:#93a5af}
.log-stream>.timeline-entry:before{width:5px;height:5px;left:2px;top:10px;border-color:#c5d2d9}
.log-stream>.timeline-entry:after{left:4px;top:20px;bottom:-5px;background:#eaf0f3}
.log-stream>.is-finding:before{width:6px;height:6px;left:1.5px;top:9px;border:0;background:#dfaa90}
.log-stream .is-finding .log-title{color:#aa755d}
.log-expand{font-size:10px;color:#ac8974;margin-top:3px}
.log-long-content[open] .log-expand{color:#a37258}

.log-title:hover,.log-title:focus-visible{background:#f7f9fa}
</style>

<style>
:root{--activity:250px}
@media(min-width:1700px){:root{--activity:270px}}
.activity-pane{position:relative;min-width:0;background:#fff;border-color:#d7e3ee;border-radius:12px}
.activity-pane .activity-heading{height:56px;padding:0 18px;gap:10px;flex:none;border-bottom:0;background:#fff}
.activity-pane .activity-heading h2{display:flex;align-items:center;gap:8px;font-size:15px;font-weight:700;line-height:23px;letter-spacing:0;color:#273743}
.activity-pane .activity-heading h2 .lucide{width:17px;height:17px;color:#647682;stroke-width:1.6}
.activity-pane #activity-match-count{flex:none;margin-left:auto;padding:3px 7px;border-radius:6px;background:#f1f4f6;font-size:11px;font-weight:400;line-height:19px;color:#5c6f7c;white-space:nowrap;font-variant-numeric:tabular-nums}
.activity-pane #activity-toolbar{flex:none;padding:0 16px 16px;background:#fff}
.activity-pane .activity-search{height:35px;gap:8px;padding:0 10px;border:1px solid #d7e3ee;border-radius:8px;background:#fff;color:#5f7f9c}
.activity-pane .activity-search:focus-within{border-color:#9dbbda;background:#fff;box-shadow:0 0 0 2px #e6eff9}
.activity-pane .activity-search>.lucide{width:14px;height:14px}
.activity-pane .activity-search input{font-size:12px;line-height:20px;color:#3e5361}
.activity-pane .activity-search input::placeholder{color:#5c6f7c}
.activity-pane .activity-content{min-width:0;padding:0 16px 38px;overflow-x:hidden;overflow-y:auto;scrollbar-width:none;scrollbar-gutter:auto;scroll-padding-top:12px;outline-offset:-3px;background:#fff}
.activity-pane .activity-content::-webkit-scrollbar{display:none;width:0;height:0}
.activity-pane .activity-content:focus-visible{outline:1px solid #acc5dc;border-radius:8px}

.activity-pane .timeline-entry{position:relative;display:block;min-width:0;padding:0;margin:0;border:0;border-radius:0;background:transparent;box-shadow:none}
.activity-pane .entry-headline{display:flex;align-items:flex-start;gap:8px;margin:0 0 5px;min-width:0}
.activity-pane .entry-title{display:flex;align-items:flex-start;flex:1;min-width:0;margin:0;gap:0}
.activity-pane .timeline-entry h3{display:block;flex:1;min-width:0;margin:0;overflow:visible;font-size:14px;font-weight:700;line-height:22px;letter-spacing:0;color:#273743;overflow-wrap:anywhere}
.activity-pane .log-title{display:flex;align-items:center;gap:6px;width:calc(100% + 6px);max-width:none;min-height:26px;margin:0 0 0 -6px;padding:2px 6px;border:0;border-radius:6px;background:transparent;color:inherit;font:inherit;text-align:left;overflow:hidden;transform:none}
.activity-pane .log-title>.log-entry-title{display:-webkit-box;flex:1;min-width:0;-webkit-box-orient:vertical;-webkit-line-clamp:2;overflow:hidden;overflow-wrap:anywhere;white-space:normal}
.activity-pane .log-title>.lucide{display:block;flex:none;width:12px;height:12px;color:#5f7f9c}
.activity-pane .log-stream .log-title>.lucide{display:none}
.activity-pane .log-title:hover,.activity-pane .log-title:focus-visible,.activity-pane .log-title.is-reading{background:#f0f5fb;color:#273743}
.activity-pane .log-title:focus-visible{outline:2px solid #dfb69f;outline-offset:1px}
.activity-pane .entry-meta,.activity-pane .entry-meta time{flex:none;margin:0;font-size:11px;font-weight:400;line-height:26px;letter-spacing:0;color:#5c6f7c;white-space:nowrap;font-variant-numeric:tabular-nums}
.activity-pane .timeline-entry .log-body,.activity-pane .timeline-entry .log-body p,.activity-pane .timeline-entry .log-text-preview{font-size:13px;font-weight:400;line-height:23px;letter-spacing:0;color:#3e5361;overflow-wrap:anywhere}
.activity-pane .timeline-entry .log-body{max-width:100%}
.activity-pane .timeline-entry .log-body p{margin:0 0 10px;white-space:pre-wrap}
.activity-pane .timeline-entry .log-body p:last-child{margin-bottom:0}
.activity-pane .log-long-content{min-width:0;font-size:13px;line-height:23px;color:#3e5361}
.activity-pane .log-long-content>summary{display:block;list-style:none;cursor:pointer}
.activity-pane .log-long-content>summary::-webkit-details-marker{display:none}
.activity-pane .log-long-content>summary:focus-visible{outline:2px solid #dfb69f;outline-offset:2px;border-radius:4px}
.activity-pane .log-text-preview{display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:4;max-height:92px;margin:0;overflow:hidden;white-space:normal}
.activity-pane .log-expand{display:flex;align-items:center;gap:5px;min-height:29px;margin:5px 0 0;padding:0;background:transparent;font-size:11px;font-weight:400;line-height:21px;color:var(--accent)}
.activity-pane .log-expand>.lucide{width:12px;height:12px;flex:none}
.activity-pane .log-read-sticky{display:none;align-items:center;gap:5px;margin-left:auto;padding:3px 0;min-height:29px;border-radius:4px;background:transparent;font-size:11px;font-weight:400;line-height:21px;color:#5c6f7c;white-space:nowrap}
.activity-pane .log-read-sticky>.lucide{width:12px;height:12px}
.activity-pane .log-read-sticky:hover{color:var(--accent)}
.activity-pane .log-long-content[open]>summary{position:sticky;top:0;z-index:2;margin:0 0 10px;border-bottom:1px solid #d9e1e6;background:#fff;box-shadow:0 -3px 0 #fff}
.activity-pane .log-long-content[open] .log-text-preview{display:none}
.activity-pane .log-long-content[open] .log-expand{margin:0;padding:3px 0;color:var(--accent)}
.activity-pane .log-long-content[open] .log-expand>.lucide{transform:rotate(180deg)}
.activity-pane .log-long-content[open] .log-read-sticky{display:inline-flex}
.activity-pane .log-long-content:not([open]) .log-label-open,.activity-pane .log-long-content[open] .log-label-closed{display:none}

.activity-pane .timeline-entry[data-phase="WORKSPACE"]{--log-marker:#68978a;--log-marker-soft:#edf5f1}
.activity-pane .timeline-entry[data-phase="FINDING"]{--log-marker:var(--accent);--log-marker-soft:#fff1e8}
.activity-pane .log-stream>.timeline-entry{padding:0 0 23px 19px;margin:0;border:0;background:transparent}
.activity-pane .log-stream>.timeline-entry:last-child{padding-bottom:3px}
.activity-pane .log-stream>.timeline-entry:before{content:'';position:absolute;top:10px;left:1px;width:7px;height:7px;border:1px solid var(--log-marker,#8aa1b5);border-radius:50%;background:var(--log-marker-soft,#eef3f8);box-shadow:0 0 0 3px #fff;z-index:1}
.activity-pane .log-stream>.timeline-entry:after{content:'';display:block;position:absolute;left:4px;top:20px;bottom:-5px;width:1px;background:#d9e4ed}
.activity-pane .log-stream>.timeline-entry:last-child:after{display:none}
.activity-pane .log-stream>.is-finding:before{top:9px;left:1px;width:7px;height:7px;border:0;border-radius:1px;background:var(--accent);transform:rotate(45deg)}
.activity-pane .log-stream .is-finding .log-title{color:var(--accent)}
.activity-pane .log-stream .is-finding .log-title:hover,.activity-pane .log-stream .is-finding .log-title:focus-visible,.activity-pane .log-stream .is-finding .log-title.is-reading{background:#fff5ee;color:var(--accent)}

.activity-pane .log-empty{padding:44px 10px;font-size:12px;line-height:21px;color:#5c6f7c;gap:11px}
.activity-pane #activity-to-latest{bottom:15px;padding:7px 12px;border-color:#d7e3ee;background:#f7faff;color:#4f7192;box-shadow:0 4px 15px #344e5d0d;font-size:11px}
.activity-pane .activity-content mark{padding:0;border-radius:2px;background:#fff0d8;color:var(--accent)}
.activity-pane .node-inspector{padding:0 3px 14px}
@media(prefers-reduced-motion:reduce){.activity-pane .log-title,.activity-pane .log-expand>.lucide{transition:none}.activity-pane .log-long-content[open]>.log-body{animation:none}}
</style>

<style>
.forms-dialog{width:610px;max-width:calc(100vw - 40px);max-height:calc(100dvh - 40px);overflow:auto;overscroll-behavior:contain;scrollbar-width:none;scrollbar-gutter:auto;padding:24px 26px;border:1px solid #d9e1e6;border-radius:16px;color:#3e5361}
.forms-dialog::-webkit-scrollbar,.forms-dialog *::-webkit-scrollbar{display:none;width:0;height:0}
.forms-dialog textarea,.forms-dialog .forms-files{scrollbar-width:none}
.forms-dialog .lucide{width:16px;height:16px;stroke-width:1.6;flex:none}
.forms-dialog .dialog-header{align-items:center;margin:0 0 22px;gap:20px}
.forms-dialog .dialog-header h2{font-size:19px;font-weight:700;line-height:1.4;letter-spacing:-.3px;margin:0;color:#273743}
.forms-branded-heading{display:flex;align-items:center;gap:18px;min-width:0}
.forms-brand{position:relative;display:block;flex:none;width:130px;height:34px;overflow:hidden;isolation:isolate;mix-blend-mode:multiply}
.forms-brand img{position:absolute;width:200px;max-width:none;left:-35px;top:-34px}
.forms-branded-heading h2{padding-left:18px;border-left:1px solid #d9e1e6}
.forms-dialog .dialog-header .icon-button{margin:-2px -5px 0 0;color:#5c6f7c}
.forms-dialog .field-label{display:flex;align-items:center;gap:7px;font-size:12px;font-weight:700;color:#3e5361;margin:14px 0 7px}
.forms-dialog .field-label>span{font-size:11px;font-weight:400;color:#5c6f7c}
.forms-dialog input:not([type=radio]):not([type=file]),.forms-dialog textarea{display:block;width:100%;border:1px solid #d9e1e6;border-radius:9px;padding:9px 12px;font-size:13px;color:#3e5361;background:#fbfcfd;line-height:1.6;transition:border-color .15s,box-shadow .15s,background .15s}
.forms-dialog input:not([type=radio]):not([type=file]){height:39px}
.forms-dialog textarea{resize:vertical;min-height:64px;max-height:180px}
.forms-dialog input::placeholder,.forms-dialog textarea::placeholder{color:#5c6f7c}
.forms-dialog input[aria-invalid=true],.forms-dialog textarea[aria-invalid=true]{border-color:#b95535}
.forms-dialog input:focus,.forms-dialog textarea:focus{background:#fff;border-color:#b95535;box-shadow:0 0 0 3px #fbede780;outline:0}
.forms-type-choices{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:6px;padding:4px;background:#f3f5f6;border:0;border-radius:7px;margin:0 0 3px}
.forms-type-choices label{position:relative;min-width:0;cursor:pointer}
.forms-type-choices input{position:absolute;opacity:0;width:1px;height:1px}
.forms-type-choices span{display:flex;justify-content:center;align-items:center;gap:8px;height:37px;padding:0 10px;border:0;border-radius:5px;font-size:12px;color:#3e5361;background:transparent}
.forms-type-choices input:checked+span{background:#fff;box-shadow:0 1px 3px #23394510;color:#9a4e32;border-color:transparent}
.forms-type-choices input:focus-visible+span{outline:2px solid #9a4e32;outline-offset:2px}
.forms-dialog .forms-material-label{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-top:14px}
.forms-material-label .field-label{margin:0 0 7px}
.forms-file-count{font-size:10px;font-variant-numeric:tabular-nums;color:#5c6f7c;margin-bottom:7px}
.forms-upload{position:relative;display:flex;align-items:center;gap:8px;padding:11px 12px;border:1px dashed #d9e1e6;border-radius:9px;background:#fbfcfd;color:#3e5361;font-size:12px;cursor:pointer;overflow:hidden}
.forms-upload:hover{border-color:#b7cbd6;background:#f5f9fb}
.forms-upload:focus-within{outline:2px solid #9a4e32;outline-offset:2px}
.forms-upload input[type=file]{position:absolute;inset:0;width:100%;height:100%;opacity:0;cursor:pointer;z-index:1}
.forms-upload>small{margin-left:auto;font-size:10px;color:#5c6f7c}
.forms-file-limits{font-size:10px;line-height:1.7;color:#5c6f7c;margin:6px 0 0}
.forms-dialog .forms-files{padding:0;margin:9px 0 0;max-height:135px;overflow:auto;list-style:none;scrollbar-width:none}
.forms-files:empty{display:none}
.forms-dialog .forms-files li{display:flex;align-items:center;gap:8px;min-width:0;padding:6px 0;border:0;font-size:11px;line-height:1.5;color:#3e5361;background:transparent}
.forms-files li>.lucide{color:#5c6f7c}
.forms-files li>span{flex:1;min-width:0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.forms-files li>small{font-size:10px;color:#5c6f7c;font-variant-numeric:tabular-nums;white-space:nowrap}
.forms-files .icon-button{width:24px;height:24px;color:#5c6f7c;flex:none}
.forms-files .icon-button:hover{color:#9a4e32;background:#fbf2ed}
.forms-dialog .form-error{font-size:11px;color:#9a4e32;line-height:1.6;margin:12px 0 0;min-height:0}
.forms-dialog .form-error:empty{display:none}
.forms-dialog .dialog-footer{display:flex;align-items:center;gap:9px;padding:18px 0 0;margin-top:18px;border-top:1px solid #d9e1e6;background:none}
.forms-dialog .dialog-footer .button{height:37px;font-size:12px;border-radius:8px}
.forms-dialog .forms-shortcut{margin-right:auto;font-size:10px;letter-spacing:.1px;color:#5c6f7c}
.forms-project-name{font-size:11px;color:#5c6f7c;line-height:1.6;margin:6px 0 0;max-width:460px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.forms-status{padding:10px 12px;font-size:11px;line-height:1.7;color:#7b633f;background:#fbf7ef;border-radius:5px;margin:-3px 0 17px}
.forms-imported{margin-top:17px;background:#f7f9fa;padding:11px 12px;border-radius:6px}
.forms-imported summary{display:flex;align-items:center;gap:7px;list-style:none;color:#3e5361;font-size:11px;cursor:pointer}
.forms-imported summary::-webkit-details-marker{display:none}
.forms-imported summary>span:nth-child(2){font-size:10px;color:#5c6f7c}
.forms-imported summary>.lucide{margin-left:auto;color:#5c6f7c}
.forms-imported[open] summary>.lucide{transform:rotate(180deg)}
.forms-imported .forms-files{max-height:120px;margin-top:8px}
.forms-dialog form[aria-busy=true] .forms-upload{pointer-events:none;opacity:.6}
.forms-dialog form[aria-busy=true] .forms-type-choices{opacity:.6}
@media(max-width:520px){.forms-dialog{padding:20px}.forms-branded-heading{gap:12px}.forms-branded-heading h2{padding-left:12px;font-size:17px}.forms-brand{width:110px;height:29px}.forms-brand img{width:170px;left:-30px;top:-29px}.forms-dialog .forms-shortcut{display:none}}
</style>

<style>
.activity-content:has(>.node-inspector){overflow-x:hidden;min-width:0;overscroll-behavior:contain}
.node-inspector{min-width:0;padding-bottom:14px}
.node-inspector .selection-header{position:sticky;top:0;z-index:4;height:38px;padding:0 0 10px;gap:10px;align-items:center;background:#fff}
.inspector-icon{width:14px;height:14px;fill:none;stroke:currentColor;stroke-width:1.6;stroke-linecap:round;stroke-linejoin:round;flex:none}
.inspector-back{padding:3px 0;display:inline-flex;align-items:center;gap:5px;color:#5c6f7c;font-size:11px}
.inspector-back:hover{color:var(--accent)}
.inspector-heading{min-width:0}
.node-inspector .inspector-title{margin:4px 0 10px;color:#273743;line-height:1.6;font-size:15px;font-weight:700;white-space:pre-wrap;overflow-wrap:anywhere;display:-webkit-box;-webkit-line-clamp:4;-webkit-box-orient:vertical;overflow:hidden}
.inspector-summary{display:flex;align-items:baseline;gap:6px 10px;flex-wrap:wrap;margin-bottom:10px}
.inspector-summary>.badge{padding:0;background:none;border:0;border-radius:0;font-size:11px;line-height:1.7;color:#5c6f7c;flex:none}
.node-inspector .inspector-meta{display:flex;flex-wrap:wrap;flex:1;gap:5px 12px;min-width:0;margin:0;padding:0;border:0;font-size:11px}
.node-inspector .inspector-meta>div{display:flex;gap:6px;min-width:0;line-height:1.7}
.node-inspector .inspector-meta dt{color:#5c6f7c;flex:none}
.node-inspector .inspector-meta dd{margin:0;color:#3e5361;min-width:0;overflow-wrap:anywhere;white-space:pre-wrap}
.node-inspector .inspector-status-row dt{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap}
.node-inspector .inspector-status-row dd:before{content:'';display:inline-block;width:4px;height:4px;margin:0 6px 2px 0;border-radius:50%;background:#647682}
.node-inspector .inspector-scope-row{flex-basis:100%}
.node-inspector .inspector-scope-row dd{display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.inspector-tools{display:flex;align-items:center;justify-content:flex-end;gap:14px;margin:0 0 12px;padding:2px 0 10px;border-bottom:1px solid #d9e1e6}
.inspector-tools>button{display:inline-flex;align-items:center;justify-content:center;gap:5px;font-size:11px;color:#5c6f7c;border:0;border-radius:4px;padding:3px 0;margin:0;background:none;white-space:nowrap}
.inspector-tools>button:hover{color:var(--accent)}
.inspector-tools>.inspector-expand-text{margin-right:auto;color:var(--accent)}
.inspector-tools .inspector-icon{width:12px;height:12px}
.inspector-body-section{margin-bottom:16px;min-width:0}
.inspector-text{margin:0;font:12px/1.85 var(--font);color:#3e5361;white-space:pre-wrap;overflow-wrap:anywhere;word-break:normal;max-width:100%;min-width:0;tab-size:2}
.inspector-body-text.is-collapsed{max-height:164px;overflow:hidden;display:-webkit-box;-webkit-line-clamp:7;-webkit-box-orient:vertical}
.inspector-expand-text{display:flex;align-items:center;gap:5px;margin:8px 0 0;padding:0;font-size:10px;color:var(--accent)}
.inspector-expand-text:hover{color:var(--accent)}
.inspector-expand-text .inspector-icon{width:11px;height:11px;transform:rotate(90deg)}
.inspector-expand-text[aria-expanded=true] .inspector-icon{transform:rotate(-90deg)}
.inspector-body-collapse .inspector-icon{transform:rotate(-90deg)}
.inspector-section-collapse{display:flex;align-items:center;gap:5px;padding:0;margin:11px 0 14px;color:#5c6f7c;font-size:10px}
.inspector-section-collapse:hover{color:var(--accent)}
.inspector-section-collapse .inspector-icon{width:11px;height:11px;transform:rotate(-90deg)}
.inspector-fold{border-top:1px solid #d9e1e6;min-width:0}
.inspector-fold>summary{list-style:none;display:flex;align-items:center;gap:7px;cursor:pointer;padding:13px 0;font-size:11px;color:#3e5361;user-select:none}
.inspector-fold>summary::-webkit-details-marker,.inspector-evidence-item>summary::-webkit-details-marker{display:none}
.inspector-fold>summary>span{flex:1}
.inspector-fold>summary>small{font-size:9px;color:#5c6f7c;font-variant-numeric:tabular-nums}
.inspector-fold>summary>.inspector-icon:last-child{width:11px;height:11px;color:#647682}
.inspector-fold[open]>summary>.inspector-icon:last-child{transform:rotate(90deg)}
.inspector-fold[open]>summary{position:sticky;top:38px;z-index:2;background:#fff}
.inspector-fold>.inspector-text{margin-bottom:14px}
.inspector-code{font-family:var(--font);font-size:10px;line-height:1.8;padding:11px 12px;border:1px solid #d9e1e6;border-radius:7px;background:#f7f9fa;color:#3e5361}
.inspector-evidence-item{border:1px solid #d9e1e6;border-radius:7px;margin:0 0 8px;min-width:0;background:#fcfdfd}
.inspector-evidence-item>summary{display:flex;align-items:center;gap:7px;padding:10px;list-style:none;cursor:pointer;font-size:10px;line-height:1.7;color:#3e5361}
.inspector-evidence-item>summary>span{font-size:9px;color:#5c6f7c;font-variant-numeric:tabular-nums}
.inspector-evidence-item>summary>strong{font-weight:400;flex:1;min-width:0;overflow-wrap:anywhere}
.inspector-evidence-item>summary>.inspector-icon{width:10px;height:10px;color:#647682}
.inspector-evidence-item[open]>summary>.inspector-icon{transform:rotate(90deg)}
.inspector-evidence-item[open]>summary{position:sticky;top:80px;z-index:1;background:#fcfdfd}
.inspector-evidence-item>div{padding:0 11px 12px}
.inspector-evidence-item .inspector-text{font-size:11px;line-height:1.85}
.inspector-evidence-path{display:block;font:10px/1.8 var(--font);color:#5c6f7c;white-space:pre-wrap;overflow-wrap:anywhere;margin-bottom:7px;max-width:100%}
.inspector-evidence-reference{color:#5c6f7c;font-size:9px;line-height:1.7;margin:0 0 8px;overflow-wrap:anywhere}
dialog.inspector-reader{width:min(760px,calc(100vw - 48px));max-width:none;height:min(780px,78dvh);block-size:min(780px,78dvh);min-height:0;max-height:78dvh;max-block-size:78dvh;padding:0;border:1px solid #d9e1e6;border-radius:16px;overflow:hidden;box-shadow:0 20px 64px #243a4b24;background:#fff}
dialog.inspector-reader[open]{display:flex;flex-direction:column}
dialog.inspector-reader::backdrop{background:#26374330;backdrop-filter:blur(2px)}
dialog.inspector-reader[open]::backdrop{animation:inspector-backdrop-in 180ms ease-out both}
dialog.inspector-reader.is-closing::backdrop{animation:inspector-backdrop-out 140ms ease-in both}
@keyframes inspector-backdrop-in{from{opacity:0}to{opacity:1}}
@keyframes inspector-backdrop-out{from{opacity:1}to{opacity:0}}
.inspector-reader-header{display:flex;align-items:flex-start;justify-content:space-between;gap:24px;padding:25px 28px 20px;background:#fff;flex:none;min-width:0}
.inspector-reader-header>div{min-width:0}
.inspector-reader-header span{font-size:13px;line-height:1.5;letter-spacing:0;color:#5c6f7c}
.inspector-reader-header h2{font-size:22px;font-weight:700;line-height:1.45;color:#273743;margin:8px 0 0;white-space:pre-wrap;overflow-wrap:anywhere;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.inspector-reader-header .icon-button{flex:none;width:30px;height:30px;border-radius:8px;color:#647682}
.inspector-reader-header .icon-button:hover{background:#f3f5f6;color:#273743}
.inspector-reader-content{overflow:auto;overscroll-behavior:contain;min-height:0;min-width:0;flex:1 1 auto;padding:0 28px 25px;scrollbar-width:none;-ms-overflow-style:none}
.inspector-reader-content::-webkit-scrollbar{display:none;width:0;height:0}
.inspector-reader-content:focus-visible{outline:2px solid #f3b9a5;outline-offset:-4px;border-radius:8px}
.inspector-reader-meta{display:flex;flex-wrap:wrap;gap:7px 22px;padding:0 0 18px;margin:0 0 20px;border-bottom:1px solid #d9e1e6}
.inspector-reader-meta p{margin:0;font-size:12px;line-height:1.75;color:#3e5361;overflow-wrap:anywhere;white-space:pre-wrap;min-width:0;max-width:100%}
.inspector-reader-meta p>span{color:#5c6f7c}
.inspector-reader-content>section{margin-bottom:22px;min-width:0}
.inspector-reader-content>section:last-child{margin-bottom:0}
.inspector-reader-content h3{font-size:12px;font-weight:700;color:#273743;margin:0 0 10px}
.inspector-reader-full-title>p{font-size:15px;font-weight:700;line-height:1.85;color:#273743;white-space:pre-wrap;overflow-wrap:anywhere;margin:0}
.inspector-reader-content .inspector-text{font-size:14px;line-height:1.9;color:#3e5361}
.inspector-reader-content .inspector-code{font-size:12px;line-height:1.85;padding:16px 18px;background:#f7f9fa;border:0;border-radius:8px}
.inspector-reader-evidence{padding:14px 16px;border:1px solid #d9e1e6;border-radius:8px;margin-top:10px;background:#fafcfc;min-width:0}
.inspector-reader-evidence h4{font-size:12px;font-weight:700;color:#273743;margin:0 0 10px;overflow-wrap:anywhere}
.inspector-reader-evidence .inspector-text{font-size:13px}
.inspector-reader-evidence .inspector-evidence-path{font-size:11px}
.inspector-reader-footer{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:15px 28px;border-top:1px solid #d9e1e6;flex:none;background:#fff}
.inspector-reader-top{display:flex;align-items:center;gap:6px;padding:5px 0;font-size:12px;color:#5c6f7c;white-space:nowrap}
.inspector-reader-top:hover{color:var(--accent)}.inspector-reader-top .inspector-icon{transform:rotate(90deg);width:12px;height:12px}
.inspector-reader-locate{display:inline-flex;align-items:center;justify-content:center;gap:6px;height:34px;margin-left:auto;padding:0 13px;font-size:12px;border:1px solid #f3d9cc;background:#fff2eb;color:var(--accent);border-radius:8px;white-space:nowrap}
.inspector-reader-locate:hover:not(:disabled){background:#fce7dc;border-color:#efc8b8;color:var(--accent)}
.inspector-reader-locate:disabled{color:#a6b2ba;background:#f7f8f9;border-color:#ebeff1;cursor:default}
.inspector-reader-locate .inspector-icon{width:13px;height:13px}
.inspector-reader-footer .button{height:34px;padding:0 13px;font-size:12px;background:#fff;border:1px solid #d9e1e6;border-radius:8px;color:#3e5361;white-space:nowrap}
.inspector-reader-footer .button:hover{background:#f6f8f9;border-color:#d9e1e6;color:#273743}
.inspector-reader button:focus-visible{outline:2px solid #f0af97;outline-offset:3px}
@media(max-width:540px){dialog.inspector-reader{width:calc(100vw - 24px);height:calc(100dvh - 32px);block-size:calc(100dvh - 32px);max-height:calc(100dvh - 32px);max-block-size:calc(100dvh - 32px)}.inspector-reader-header{padding:20px 20px 18px;gap:16px}.inspector-reader-header h2{font-size:20px}.inspector-reader-content{padding:0 20px 20px}.inspector-reader-footer{padding:13px 20px;gap:8px}.inspector-reader-footer .button,.inspector-reader-locate{padding:0 9px;font-size:11px}.inspector-reader-top{font-size:11px}}
@media(prefers-reduced-motion:reduce){dialog.inspector-reader[open]::backdrop,dialog.inspector-reader.is-closing::backdrop{animation:none}}
.inspector-record-mount{position:fixed;inset:0 auto auto 0;width:0;height:0;z-index:0}
.inspector-copy-buffer{position:fixed;left:-9999px;top:0;width:1px;height:1px;opacity:0}
</style>
