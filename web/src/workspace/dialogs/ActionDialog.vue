<script setup lang="ts">
import {computed, onBeforeUnmount, ref, shallowRef} from 'vue';
import Icon from '@/ui/Icon.vue';
import {actions} from '../model/lifecycle';
import type {ProjectAction, ProjectView} from '../model/types';
import {actionNames, errorMessage} from './types';
import type {ProjectOperation} from './types';
const props = defineProps<{perform: (operation: ProjectOperation) => Promise<unknown>; disabled?: boolean}>();
const emit = defineEmits<{closed: []}>();
const dialog = ref<HTMLDialogElement>(), operation = shallowRef<ProjectOperation | null>(null);
const name = ref(''), error = ref(''), busy = ref(false), invalid = ref(false);
let disposed = false;
const label = computed(() => operation.value ? actionNames[operation.value.action] + '项目' : '');
const descriptions: Partial<Record<ProjectAction, string>> = {
  restart: '归档当前轮，清空当前轮任务、执行记录与结论后重新开始。项目输入、材料、补充信息与历史轮次会保留。',
  terminate: '停止所有未完成任务并保留现有记录。目标不会标为完成，之后可重启。',
  delete: '删除这个项目及其全部历史记录，同时停止未完成任务。此操作无法撤销。',
};
function open(project: ProjectView, action: ProjectAction) {
  if (busy.value || props.disabled || !['restart', 'terminate', 'delete'].includes(action) || !actions(project).includes(action)) return false;
  operation.value = {id: project.id, action, generation: project.generation, status: project.status};
  name.value = project.name; error.value = ''; invalid.value = false;
  if (!dialog.value?.open) dialog.value?.showModal(); return true;
}
function close() {if (!busy.value) dialog.value?.close();}
function onClose() {operation.value = null; emit('closed');}
async function submit() {
  if (!operation.value || busy.value || props.disabled || invalid.value) return;
  const captured = {...operation.value}; busy.value = true; error.value = '';
  try {await props.perform(captured); if (!disposed) dialog.value?.close();}
  catch (failure) {
    if (disposed) return;
    const status = typeof failure === 'object' && failure !== null && 'status' in failure ? failure.status : undefined;
    invalid.value = status === 0 || status === 409;
    error.value = errorMessage(failure, '操作失败，请重试。') + (invalid.value ? '；请关闭窗口，刷新后重新确认。' : '');
  } finally {if (!disposed) busy.value = false;}
}
onBeforeUnmount(() => {disposed = true;});
defineExpose({open, close});
</script>
<template>
  <dialog ref="dialog" id="action-dialog" aria-labelledby="action-title" aria-describedby="action-description" @cancel="busy && $event.preventDefault()" @close="onClose">
    <form id="action-form" @submit.prevent="submit">
      <header class="dialog-header"><h2 id="action-title">{{ label }}</h2><button type="button" class="icon-button" data-close="action-dialog" aria-label="关闭项目操作" :disabled="busy" @click="close"><Icon name="x" /></button></header>
      <p id="action-project">{{ name }}</p><p id="action-description">{{ operation ? descriptions[operation.action] : '' }}</p><p id="action-error" class="form-error" role="alert">{{ error }}</p>
      <footer class="dialog-footer"><button type="button" class="button secondary" data-close="action-dialog" :disabled="busy" @click="close">取消</button><button type="submit" class="button" :class="operation?.action === 'restart' ? 'primary' : 'danger-button'" id="confirm-action" :disabled="busy || disabled || invalid">{{ busy ? '处理中…' : label }}</button></footer>
    </form>
  </dialog>
</template>
