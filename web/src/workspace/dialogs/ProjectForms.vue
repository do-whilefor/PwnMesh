<script setup lang="ts">
import {computed, nextTick, onBeforeUnmount, reactive, ref} from 'vue';
import Icon from '@/ui/Icon.vue';
import {useDialogMotion} from '@/ui/useDialogMotion';
import {PROJECT_TYPES, fileSize, isProjectClosed, validateFiles, validateHint, validateProject} from '../model/project-model';
import type {CreateDraft, HintDraft, ProjectView} from '../model/types';
import MaterialPicker from './MaterialPicker.vue';
import {errorFlag, errorMessage} from './types';

const props = defineProps<{
  getProject: (id: string) => ProjectView | undefined;
  create: (draft: CreateDraft) => Promise<unknown>;
  hint: (id: string, draft: HintDraft) => Promise<unknown>;
  isHintRetry?: (id: string, draft: HintDraft) => boolean;
  disabled?: boolean;
}>();
const emit = defineEmits<{toast: [message: string]}>();
const createDialog = ref<HTMLDialogElement>(), hintDialog = ref<HTMLDialogElement>();
const createMotion = useDialogMotion(createDialog), hintMotion = useDialogMotion(hintDialog);
const createForm = ref<HTMLFormElement>(), hintForm = ref<HTMLFormElement>();
const imported = ref<HTMLDetailsElement>();
const emptyCreate = (): CreateDraft => ({name: '', target: '', goal: '', type: 'pentest', files: []});
const createDraft = reactive<CreateDraft>(emptyCreate());
const hintDraft = reactive<HintDraft>({text: '', files: []});
const hintDrafts = new Map<string, HintDraft>();
const hintProjectId = ref<string | null>(null);
const createBusy = ref(false), hintBusy = ref(false), createLocked = ref(false), createUncertain = ref(false);
const createError = ref(''), hintError = ref(''), createInvalid = ref(''), hintInvalid = ref('');
const hintProject = computed(() => hintProjectId.value ? props.getProject(hintProjectId.value) : undefined);
const unavailable = (project?: ProjectView) => project?.readOnly ? '历史项目只读，请新建项目继续。' : isProjectClosed(project) ? '项目已结束，无法补充信息。' : '';
const hintStatus = computed(() => unavailable(hintProject.value) || (hintProject.value?.status === 'paused' ? '项目已暂停，补充内容将在继续后使用。' : ''));
const createDisabled = computed(() => createBusy.value || createLocked.value || props.disabled);
const scenarios: Record<string, {name: string; target: string; goal: string}> = {
  ctf: {name: '例如：Web 题目分析', target: '题目描述、题目地址或已知线索', goal: '例如：定位漏洞并获取 Flag'},
  pentest: {name: '例如：核心业务 API 渗透测试', target: '目标地址、测试范围或已知线索', goal: '需要验证的问题，以及判断完成的标准'},
  audit: {name: '例如：核心服务代码审计', target: '仓库或代码路径、分支与已知问题', goal: '需要审查的模块，以及判断完成的标准'},
};
const scenario = computed(() => scenarios[createDraft.type] || scenarios.pentest!);
let disposed = false;
function clearError(kind: 'create' | 'hint') {
  if (kind === 'create') {createError.value = ''; createInvalid.value = '';}
  else {hintError.value = ''; hintInvalid.value = '';}
}
function captureHint() {if (hintProjectId.value) hintDrafts.set(hintProjectId.value, {text: hintDraft.text, files: hintDraft.files.slice()});}
function closeCreate(immediate = false) {if (!createBusy.value) void createMotion.close(immediate);}
function closeHint(immediate = false) {if (!hintBusy.value) {captureHint(); void hintMotion.close(immediate);}}
async function openCreate() {
  if (createBusy.value || hintBusy.value || props.disabled) {emit('toast', '正在提交，请稍候。'); return false;}
  closeHint(true);
  createMotion.show();
  await nextTick(); createForm.value?.querySelector<HTMLInputElement>('#project-name')?.focus(); return true;
}
async function openHint(id: string) {
  if (createBusy.value || hintBusy.value || props.disabled) {emit('toast', '正在提交，请稍候。'); return false;}
  const project = props.getProject(id);
  if (!project || unavailable(project)) {emit('toast', project ? unavailable(project) : '项目不存在或已移除。'); return false;}
  closeCreate(true); captureHint(); hintProjectId.value = id;
  const draft = hintDrafts.get(id) || {text: '', files: []};
  Object.assign(hintDraft, {text: draft.text, files: draft.files.slice()}); clearError('hint');
  if (imported.value) imported.value.open = false;
  hintMotion.show();
  await nextTick(); hintForm.value?.querySelector<HTMLTextAreaElement>('#hint-text')?.focus(); return true;
}
function addFiles(kind: 'create' | 'hint', incoming: File[]) {
  if (kind === 'create' ? createDisabled.value : hintBusy.value || props.disabled) return;
  const draft = kind === 'create' ? createDraft : hintDraft;
  const next = [...draft.files, ...incoming], project = kind === 'hint' ? hintProject.value : undefined;
  const error = kind === 'hint' && (!project || unavailable(project)) ? unavailable(project) || '项目不存在或已移除。' : validateFiles(next, project?.files || []);
  clearError(kind);
  if (error) {if (kind === 'create') {createError.value = error; createInvalid.value = 'files';} else {hintError.value = error; hintInvalid.value = 'files';} return;}
  draft.files = next; if (kind === 'hint') captureHint();
}
function removeFile(kind: 'create' | 'hint', index: number) {
  if (kind === 'create' ? createDisabled.value || createUncertain.value : hintBusy.value || props.disabled) return;
  const draft = kind === 'create' ? createDraft : hintDraft;
  draft.files.splice(index, 1); clearError(kind); if (kind === 'hint') captureHint();
}
const settle = (started: number) => new Promise<void>(resolve => setTimeout(resolve, Math.max(0, 300 - (Date.now() - started))));
async function submitCreate() {
  if (createBusy.value || createUncertain.value || props.disabled) return;
  const values = {...createDraft, name: createDraft.name.trim(), target: createDraft.target.trim(), goal: createDraft.goal.trim(), files: createDraft.files.slice()};
  clearError('create'); createError.value = validateProject(values);
  if (createError.value) {
    createInvalid.value = !values.name || values.name.length > 200 ? 'name' : !values.target || values.target.length > 32768 ? 'target' : !values.goal || values.goal.length > 32768 ? 'goal' : 'files';
    await nextTick(); createForm.value?.querySelector<HTMLElement>(`#project-${createInvalid.value}`)?.focus(); return;
  }
  createBusy.value = true; const started = Date.now();
  try {await props.create(values); await settle(started); if (disposed) return; await createMotion.close(); if (!disposed) resetCreate();}
  catch (error) {await settle(started); if (disposed) return; createLocked.value = errorFlag(error, 'pendingCreate'); createUncertain.value = errorFlag(error, 'uncertainCreate'); createError.value = errorMessage(error, '创建失败，请重试。');}
  finally {if (!disposed) createBusy.value = false;}
}
async function submitHint() {
  if (hintBusy.value || !hintProjectId.value || props.disabled) return;
  const id = hintProjectId.value, values = {text: hintDraft.text.trim(), files: hintDraft.files.slice()}, project = props.getProject(id);
  const validationProject = project && props.isHintRetry?.(id, values) ? {...project, files: []} : project;
  clearError('hint'); hintError.value = unavailable(project) || validateHint(values, validationProject);
  if (hintError.value) {if (!values.text && !values.files.length) {hintInvalid.value = 'text'; await nextTick(); hintForm.value?.querySelector<HTMLElement>('#hint-text')?.focus();} return;}
  hintBusy.value = true; const started = Date.now();
  try {await props.hint(id, values); await settle(started); if (disposed) return; await hintMotion.close(); if (disposed) return; hintDrafts.delete(id); Object.assign(hintDraft, {text: '', files: []}); clearError('hint'); hintProjectId.value = null;}
  catch (error) {await settle(started); if (!disposed) hintError.value = errorMessage(error, '添加失败，请重试。');}
  finally {if (!disposed) hintBusy.value = false;}
}
function shortcut(event: KeyboardEvent, kind: 'create' | 'hint') {
  if (event.key !== 'Enter' || (!event.ctrlKey && !event.metaKey)) return;
  event.preventDefault(); event.stopPropagation(); if (!event.repeat) void (kind === 'create' ? submitCreate() : submitHint());
}
function resetCreate() {createLocked.value = false; createUncertain.value = false; Object.assign(createDraft, emptyCreate()); clearError('create');}
function close() {closeCreate(); closeHint();}
onBeforeUnmount(() => {disposed = true;});
defineExpose({openCreate, openHint, resetCreate, close});
</script>

<template>
  <dialog ref="createDialog" id="create-dialog" class="forms-dialog" aria-labelledby="create-title" @cancel.prevent="closeCreate()">
    <form ref="createForm" id="create-form" novalidate :aria-busy="createBusy" @submit.prevent="submitCreate" @input="clearError('create')" @keydown="shortcut($event, 'create')">
      <header class="dialog-header"><div class="forms-branded-heading"><span class="forms-brand"><img src="/static/brand.png" alt="PwnMesh"></span><h2 id="create-title">新建项目</h2></div><button type="button" class="icon-button" data-close="create-dialog" aria-label="关闭创建项目" :disabled="createBusy" @click="closeCreate()"><Icon name="x" /></button></header>
      <div class="type-choices forms-type-choices" role="radiogroup" aria-label="项目类型"><label v-for="type in PROJECT_TYPES" :key="type.value"><input v-model="createDraft.type" type="radio" name="type" :value="type.value" :disabled="createDisabled"><span><Icon :name="type.icon" />{{ type.label }}</span></label></div>
      <label class="field-label" for="project-name">项目名称</label><input id="project-name" v-model="createDraft.name" name="name" maxlength="200" required autocomplete="off" :placeholder="scenario.name" :disabled="createDisabled" :aria-invalid="createInvalid === 'name' || undefined">
      <label class="field-label" for="project-target">起点 / 已知信息</label><textarea id="project-target" v-model="createDraft.target" name="target" rows="2" maxlength="32768" required :placeholder="scenario.target" :disabled="createDisabled" :aria-invalid="createInvalid === 'target' || undefined"></textarea>
      <label class="field-label" for="project-goal">终点 / 项目目标</label><textarea id="project-goal" v-model="createDraft.goal" name="goal" rows="2" maxlength="32768" required :placeholder="scenario.goal" :disabled="createDisabled" :aria-invalid="createInvalid === 'goal' || undefined"></textarea>
      <MaterialPicker kind="create" :files="createDraft.files" :disabled="createDisabled" :remove-disabled="createUncertain" :invalid="createInvalid === 'files'" @add="addFiles('create', $event)" @remove="removeFile('create', $event)" />
      <p id="create-error" class="form-error" role="alert">{{ createError }}</p><footer class="dialog-footer"><button type="button" class="button secondary" data-close="create-dialog" :disabled="createBusy" @click="closeCreate()">取消</button><button type="submit" class="button primary" :disabled="createBusy || createUncertain || disabled">{{ createBusy ? '创建中…' : '创建并开始' }}</button></footer>
    </form>
  </dialog>
  <dialog ref="hintDialog" id="hint-dialog" class="forms-dialog" aria-labelledby="hint-title" @cancel.prevent="closeHint()" @close="captureHint">
    <form ref="hintForm" id="hint-form" novalidate :aria-busy="hintBusy" @submit.prevent="submitHint" @input="clearError('hint')" @keydown="shortcut($event, 'hint')">
      <header class="dialog-header"><div><h2 id="hint-title">补充信息</h2><p id="hint-project-name" class="forms-project-name">{{ hintProject?.name }}</p></div><button type="button" class="icon-button" data-close="hint-dialog" aria-label="关闭补充信息" :disabled="hintBusy" @click="closeHint()"><Icon name="x" /></button></header>
      <p id="hint-status" class="forms-status" role="status" :hidden="!hintStatus">{{ hintStatus }}</p><label class="field-label" for="hint-text">补充内容</label><textarea id="hint-text" v-model="hintDraft.text" name="text" rows="4" maxlength="32768" placeholder="补充线索、测试范围或需要关注的问题…" :disabled="hintBusy || disabled" :aria-invalid="hintInvalid === 'text' || undefined"></textarea>
      <details ref="imported" id="hint-imported" class="forms-imported" :hidden="!hintProject?.files.length"><summary><span>已导入材料</span><span id="hint-imported-count">{{ hintProject?.files.length || 0 }}</span><Icon name="chevron-down" /></summary><ul id="hint-imported-files" class="file-list forms-files"><li v-for="file in hintProject?.files || []" :key="file.id"><Icon name="file" /><span :title="file.name">{{ file.name }}</span><small>{{ fileSize(file.size) }}</small></li></ul></details>
      <MaterialPicker kind="hint" :files="hintDraft.files" :imported-count="hintProject?.files.length || 0" :disabled="hintBusy || disabled" :invalid="hintInvalid === 'files'" @add="addFiles('hint', $event)" @remove="removeFile('hint', $event)" />
      <p class="form-error" id="hint-error" role="alert">{{ hintError }}</p><footer class="dialog-footer"><span class="forms-shortcut">Ctrl / ⌘ + Enter</span><button type="button" class="button secondary" data-close="hint-dialog" :disabled="hintBusy" @click="closeHint()">取消</button><button type="submit" class="button primary" :disabled="hintBusy || disabled || !!unavailable(hintProject)">{{ hintBusy ? '添加中…' : '添加' }}</button></footer>
    </form>
  </dialog>
</template>
