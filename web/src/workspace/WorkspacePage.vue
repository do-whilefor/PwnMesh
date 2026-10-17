<script setup lang="ts">
import { computed, nextTick, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, shallowRef, watch } from 'vue';
import type { ProjectView } from './model/types';
import type { GraphEdgeDetails, GraphLayout, GraphNode } from './graph/types';
import { nodePresentation } from './graph/graph-view';
import { selectionDetail } from './details/detail';
import type { DetailRecord } from './details/detail';
import { PROJECT_TYPES } from './model/project-model';
import { canHint } from './model/lifecycle';
import { dateTime, errorMessage } from '@/ui/format';
import Icon from '@/ui/Icon.vue';
import {useViewMotion} from '@/ui/useViewMotion';
import GraphCanvas from './graph/GraphCanvas.vue';
import ActivityPanel from './activity/ActivityPanel.vue';
import MaterialsView from './materials/MaterialsView.vue';
import ResultsView from './results/ResultsView.vue';
import RecordReader from './details/RecordReader.vue';
defineOptions({ name: 'WorkspacePage' });
const props = defineProps<{ project?: ProjectView; projectIds?: readonly string[]; disabled: boolean; modelSummary: string; loadRound: (project: ProjectView, generation: number) => Promise<unknown> }>();
const emit = defineEmits<{ create: []; hint: [id: string]; manage: [project: ProjectView]; toast: [message: string] }>();
type Tab = 'graph' | 'materials' | 'result';
const tab = ref<Tab>('graph'), run = ref('current'), graph = ref<InstanceType<typeof GraphCanvas>>(), reader = ref<InstanceType<typeof RecordReader>>(), filter = ref('all'), zoom = ref(1), active = ref(true);
const view = ref<HTMLElement>(), page = ref<HTMLElement>();
const {displayed: visibleTab, ready: viewReady, reset: resetTabMotion} = useViewMotion(tab, view);
const materials = ref<InstanceType<typeof MaterialsView>>();
const activity = ref<InstanceType<typeof ActivityPanel>>();
const selectedNode = ref<GraphNode | null>(null), selectedEdge = ref<GraphEdgeDetails | null>(null);
const layouts = new Map<string, GraphLayout>();
watch(() => props.projectIds, ids => { if (ids) for (const [key, layout] of layouts) if (!ids.includes(layout.projectId)) layouts.delete(key); });
let roundVersion = 0, focusVersion = 0;
const visibleProject = shallowRef(props.project), visibleRun = ref(run.value);
const projectSwitching = computed(() => visibleProject.value?.id !== props.project?.id || visibleProject.value?.generation !== props.project?.generation);
watch([() => props.project?.id, () => props.project?.generation], () => { roundVersion++; focusVersion++; run.value = 'current'; materials.value?.cancel(); reader.value?.close(); }, {flush: 'sync'});
const requestedIdentity = computed(() => JSON.stringify([props.project?.id, props.project?.generation, run.value]));
const {displayed: visibleIdentity} = useViewMotion(requestedIdentity, page, {inert: false});
const switching = computed(() => visibleIdentity.value !== requestedIdentity.value);
watch([visibleIdentity, () => props.project, run], () => {
  if (switching.value) return;
  const projectChanged = projectSwitching.value;
  const changed = projectChanged || visibleRun.value !== run.value;
  visibleProject.value = props.project; visibleRun.value = run.value;
  if (!changed) return;
  filter.value = 'all'; graph.value?.setStatusFilter('all'); clearSelection();
  if (projectChanged) tab.value = 'graph';
  resetTabMotion();
  // The enclosing fade already covers these changes; settle nested panels before entry.
  void nextTick(() => {if (!switching.value) activity.value?.reset?.();});
}, {flush: 'sync'});
const viewed = computed(() => visibleRun.value === 'current' ? visibleProject.value : visibleProject.value?.history.find(project => String(project.generation) === visibleRun.value) || visibleProject.value);
const state = computed(() => viewed.value?.state || null);
const summaryOnly = computed(() => viewed.value?.readOnly && state.value === null);
const types: Record<string, string> = Object.fromEntries([['', '通用'], ...PROJECT_TYPES.map(value => [value.value, value.label])]);
const statusLabels: Record<string, string> = { running: '探索中', done: '已完成', paused: '已暂停', pending: '待开始', terminated: '已终止' };
const selection = computed(() => selectedNode.value ? selectionDetail(selectedNode.value, 'node', nodePresentation(selectedNode.value, 'goal:goal'), state.value) : selectedEdge.value ? selectionDetail(selectedEdge.value, 'edge') : null);
const completed = computed(() => state.value?.steps.filter(step => step.status === 'completed').length || 0);
const tabs: { id: Tab; icon: string; label: string }[] = [{ id: 'graph', icon: 'workflow', label: '画布' }, { id: 'materials', icon: 'paperclip', label: '材料' }, { id: 'result', icon: 'file-check-2', label: '结果' }];
function clearSelection() { selectedNode.value = null; selectedEdge.value = null; graph.value?.clearSelection(); }
function node(value: GraphNode | null) { if (switching.value) return; selectedNode.value = value; selectedEdge.value = null; }
function edge(value: GraphEdgeDetails | null) { if (switching.value) return; selectedEdge.value = value; selectedNode.value = null; }
async function focus(key: string) {
  if (switching.value) return;
  const ticket = ++focusVersion; tab.value = 'graph'; materials.value?.cancel();
  await viewReady(); await nextTick();
  if (ticket !== focusVersion || tab.value !== 'graph' || !active.value) return;
  filter.value = 'all'; graph.value?.setStatusFilter('all'); graph.value?.selectNode(key); graph.value?.focusNode(key);
  await nextTick();
  await activity.value?.ready();
  if (ticket === focusVersion && active.value) document.getElementById('clear-selection')?.focus({preventScroll: true});
}
async function selectTab(value: Tab) {
  if (switching.value) return;
  focusVersion++; tab.value = value;
  if (value !== 'materials') materials.value?.cancel();
  clearSelection(); filter.value = 'all'; await nextTick();
  if (tab.value === value && active.value) document.getElementById('view-tab-' + value)?.focus();
}
function keyboard(event: KeyboardEvent) {
  if (!active.value || switching.value) return;
  const target = event.target instanceof Element ? event.target : null;
  if (target?.closest('[role=tab]') && ['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) { event.preventDefault(); const index = tabs.findIndex(value => value.id === tab.value); const next = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length; void selectTab(tabs[next]!.id); }
  if (event.key === 'Escape' && !target?.closest('input,textarea,select,dialog') && !document.querySelector('dialog[open]')) clearSelection();
}
async function selectRound(value: string) {
  const project = props.project, ticket = ++roundVersion; if (!project || project.id !== visibleProject.value?.id || project.generation !== visibleProject.value?.generation) return;
  if (value === 'current') { showRound(value); return; }
  try { await props.loadRound(project, Number(value)); if (ticket !== roundVersion || props.project?.id !== project.id || !active.value) return; showRound(value); }
  catch (error) { if (ticket === roundVersion) emit('toast', errorMessage(error)); }
}
function showRound(value: string) { focusVersion++; run.value = value; materials.value?.cancel(); }
function changeRound(event: Event) {
  const select = event.target as HTMLSelectElement, requested = select.value;
  select.value = run.value;
  void selectRound(requested);
}
function setFilter(value: string) { if (switching.value) return; filter.value = value === filter.value ? 'all' : value; graph.value?.setStatusFilter(filter.value); }
function read(record: DetailRecord) { if (switching.value) return; materials.value?.cancel(); void reader.value?.open(record); }
function hint() {if (!switching.value && visibleProject.value) emit('hint', visibleProject.value.id);}
function manage() {if (!switching.value && viewed.value) emit('manage', viewed.value);}
watch(summaryOnly, value => { if (value) { roundVersion++; showRound('current'); reader.value?.close(); } });
onMounted(() => document.addEventListener('keydown', keyboard)); onBeforeUnmount(() => document.removeEventListener('keydown', keyboard));
onActivated(() => active.value = true); onDeactivated(() => { active.value = false; roundVersion++; focusVersion++; reader.value?.close(); });
defineExpose({ reset: () => { focusVersion++; run.value = 'current'; tab.value = 'graph'; clearSelection(); } });
</script>
<template>
  <section class="project-page" :class="{ 'empty-project-page': !viewed }">
    <div ref="page" class="project-content" :aria-busy="switching">
    <header class="project-heading"><div v-if="viewed" class="project-heading-copy"><div class="title-row"><h1 :title="viewed.name">{{ viewed.name }}</h1><span class="badge" :class="viewed.archived || viewed.readOnly ? 'paused' : viewed.status"><i class="status-dot" :class="viewed.status"/>{{ viewed.archived ? '已归档' : viewed.readOnly ? '历史项目 · 只读' : statusLabels[viewed.status] }}</span></div><div class="project-description"><div class="project-identity"><Icon :name="viewed.type === 'audit' ? 'code-2' : viewed.type === 'ctf' ? 'flag' : 'globe'"/><span>{{ types[viewed.type] || '通用' }}</span><i class="meta-divider"/><span class="target-summary" :title="viewed.target">{{ viewed.target }}</span></div><div class="project-times" aria-label="项目时间范围"><span><time id="project-created-at" title="创建时间" :datetime="viewed.createdAt || ''">{{ dateTime(viewed.createdAt) }}</time></span><span class="project-time-separator" aria-hidden="true">～</span><span><time id="project-ended-at" :title="viewed.endedAt ? '结束时间' : '尚未结束'" :datetime="viewed.endedAt || undefined">{{ dateTime(viewed.endedAt) }}</time></span></div></div></div><div v-else class="project-heading-copy empty-project-heading" aria-hidden="true"><div class="title-row"><h1>工作台</h1></div><div class="project-description"><span>尚未创建项目</span></div></div><div class="heading-actions"><a id="model-entry" class="top-model-entry" href="#/llm" data-nav="llm" :title="'模型接入 · ' + modelSummary"><Icon name="plug"/><span>模型接入</span><small id="model-summary" class="visually-hidden">{{ modelSummary }}</small></a></div></header>
    <div class="project-body"><section class="canvas-panel"><header class="canvas-header"><div class="view-tabs" role="tablist" aria-label="项目视图"><button v-for="item in tabs" :id="'view-tab-' + item.id" :key="item.id" :data-view="item.id" role="tab" :aria-selected="tab === item.id" aria-controls="project-view" :tabindex="tab === item.id ? 0 : -1" :class="{ active: tab === item.id }" :disabled="!viewed || switching" @click="selectTab(item.id)"><Icon :name="item.icon"/>{{ item.label }}<small v-if="item.id === 'materials'">{{ viewed?.filesKnown === false ? '?' : viewed?.files.length || 0 }}</small></button></div><div class="workspace-actions"><select v-if="visibleProject?.history.length" id="run-select" :value="run" :disabled="projectSwitching" aria-label="查看项目轮次" @change="changeRound"><option value="current">当前轮 · {{ visibleProject.generation + 1 }}</option><option v-for="round in [...visibleProject.history].reverse()" :key="round.generation" :value="String(round.generation)">第 {{ round.generation + 1 }} 轮 · 已归档</option></select><button :id="visibleProject ? 'project-manage' : undefined" class="button secondary" aria-haspopup="dialog" :disabled="!viewed || switching" @click="manage"><Icon name="sliders-horizontal"/>项目管理</button><button v-if="visibleRun !== 'current'" id="return-current" class="button secondary" @click="selectRound('current')"><Icon name="arrow-left"/>返回当前轮</button><button v-else :id="visibleProject ? 'add-hint' : undefined" class="button secondary" data-mutates :disabled="!visibleProject || disabled || switching || !canHint(visibleProject)" @click="hint"><Icon name="message-square-plus"/>补充信息</button></div></header>
      <div id="project-view" ref="view" :class="visibleTab === 'graph' && !summaryOnly ? 'graph-stage' : 'tab-content'" role="tabpanel" :aria-labelledby="'view-tab-' + visibleTab">
        <div v-if="summaryOnly" class="empty-state"><Icon name="info"/>此历史项目仅提供基本信息，详情暂不可查看。请新建项目继续。</div>
        <template v-else-if="visibleTab === 'graph'"><GraphCanvas v-if="viewed" ref="graph" :state="state" :project-layouts="layouts" @select-node="node" @select-edge="edge" @zoom="zoom = $event"/><template v-else><div id="graph-host" class="graph-host"/><div class="canvas-empty-state"><h2>每一次探索，从一个目标开始。</h2><p>创建一个项目，把复杂的问题一步步展开。</p><button class="button primary" data-create @click="emit('create')"><Icon name="plus"/>新建项目</button></div></template><div class="canvas-bottom"><div class="zoom-controls"><button id="zoom-out" class="icon-button" aria-label="缩小画布" :disabled="!viewed" @click="graph?.zoomBy(1 / 1.18)"><Icon name="minus"/></button><span id="zoom-label">{{ Math.round(zoom * 100) }}%</span><button id="zoom-in" class="icon-button" aria-label="放大画布" :disabled="!viewed" @click="graph?.zoomBy(1.18)"><Icon name="plus"/></button><i/><button id="fit-graph" class="icon-button" aria-label="适应画布" title="适应画布 · 0" :disabled="!viewed" @click="graph?.fit()"><Icon name="maximize"/></button></div></div></template>
        <MaterialsView v-else-if="viewed && visibleTab === 'materials'" ref="materials" :key="viewed.id + ':' + viewed.generation" :project="viewed" :active="active && !switching && tab === 'materials'" @hint="hint" @read="read"/>
        <ResultsView v-else-if="viewed" :key="viewed.id + ':' + viewed.generation" :project="viewed" :disabled="switching" @read="read" @locate="focus" @toast="emit('toast', $event)"/>
      </div><footer v-if="!summaryOnly" class="canvas-footer"><div class="legend" role="group" aria-label="按节点状态筛选"><template v-if="tab === 'graph'"><button v-for="(label, value) in { done: '已完成', running: '运行中', pending: '待执行' }" :key="value" :data-filter="value" :disabled="!viewed || visibleTab !== 'graph'" :class="{ active: filter === value }" :aria-pressed="filter === value" @click="setFilter(value)"><i class="legend-dot" :class="value"/>{{ label }}</button></template></div><div class="task-progress"><span>{{ completed }} / {{ state?.steps.length || 0 }} 个任务</span><span class="progress-track"><i :style="{ width: (state?.steps.length ? completed / state.steps.length * 100 : 0) + '%' }"/></span></div></footer></section>
      <ActivityPanel ref="activity" :project="viewed" :project-ids="projectIds" :selection="selection" :selection-key="selectedNode?.key || selectedEdge?.id" @back="clearSelection" @read="read"/>
    </div>
    </div>
    <Teleport to="body"><div class="inspector-record-mount"><section class="node-inspector"><RecordReader ref="reader" @locate="focus" @toast="emit('toast', $event)"/></section></div></Teleport>
  </section>
</template>
<style scoped>
.project-content{display:flex;flex-direction:column;flex:1;min-height:0}
</style>
