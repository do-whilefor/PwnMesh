<script setup lang="ts">
import {computed, nextTick, ref} from 'vue';
import Icon from '@/ui/Icon.vue';
import {useDialogMotion} from '@/ui/useDialogMotion';
import {dateTime} from '@/ui/format';
import {actions} from '../model/lifecycle';
import {PROJECT_TYPES} from '../model/project-model';
import type {ProjectAction, ProjectView} from '../model/types';
import {actionIcons, actionNames} from './types';
import type {ProjectOperation} from './types';
const props = defineProps<{project?: ProjectView; disabled?: boolean}>();
const emit = defineEmits<{action: [operation: ProjectOperation]; closed: []}>();
const dialog = ref<HTMLDialogElement>();
const motion = useDialogMotion(dialog);
const labels = {running: '探索中', done: '已完成', paused: '已暂停', pending: '待开始', terminated: '已终止'};
const controls = computed(() => props.project ? actions(props.project).filter(action => ['pause', 'resume', 'terminate'].includes(action)) : []);
const projectType = computed(() => PROJECT_TYPES.find(type => type.value === props.project?.type)?.label || '通用');
const footer = computed(() => props.project?.archived ? '历史记录 · 仅供查看' : props.project?.readOnly ? '历史项目只读，请新建项目继续' : props.project?.status === 'paused' ? '已暂停，现有记录已保留' : ['terminated', 'done'].includes(props.project?.status || '') ? '项目已结束，记录已保留' : '暂停可继续，终止保留记录');
async function open() {await nextTick(); if (props.project) motion.show();}
function close(immediate = false) {return motion.close(immediate);}
function action(value: ProjectAction) {
  const project = props.project;
  if (!project || props.disabled) return;
  if (value === 'terminate') void close(true);
  emit('action', {id: project.id, action: value, generation: project.generation, status: project.status});
}
function focusAction() {dialog.value?.querySelector<HTMLButtonElement>('[data-action]')?.focus({preventScroll: true});}
defineExpose({open, close, focusAction});
</script>
<template>
  <dialog ref="dialog" id="project-dialog" aria-labelledby="project-dialog-title" @cancel.prevent="close()" @close="emit('closed')">
    <template v-if="project">
      <header class="dialog-header"><div><span class="dialog-eyebrow">项目资料与运行状态</span><h2 id="project-dialog-title">项目管理</h2></div><button class="icon-button" data-close="project-dialog" aria-label="关闭项目管理" @click="close()"><Icon name="x" /></button></header>
      <div class="project-info-body" tabindex="0" role="region" aria-label="项目资料"><div class="project-info-identity"><h3>{{ project.name }}</h3><span class="badge" :class="project.archived || project.readOnly ? 'paused' : project.status"><i class="status-dot" :class="project.status"></i>{{ project.archived ? '已归档' : project.readOnly ? '历史项目 · 只读' : labels[project.status] }}</span></div>
        <dl class="project-info-meta"><div><dt>项目类型</dt><dd>{{ projectType }}</dd></div><div><dt>材料</dt><dd>{{ project.filesKnown === false ? '未知' : `${project.files.length} 份` }}</dd></div><div><dt>创建时间</dt><dd>{{ dateTime(project.createdAt) }}</dd></div><div><dt>结束时间</dt><dd>{{ dateTime(project.endedAt) }}</dd></div></dl>
        <section class="project-info-section"><h3>起点 / 已知信息</h3><p>{{ project.target }}</p></section><section class="project-info-section"><h3>终点 / 项目目标</h3><p>{{ project.goal }}</p></section>
      </div>
      <footer class="project-info-footer"><span>{{ footer }}</span><div><button v-for="value in controls" :key="value" class="button" :class="value === 'terminate' ? 'danger-outline' : 'secondary'" :data-action="value" :data-action-project="project.id" data-mutates :disabled="disabled" @click="action(value)"><Icon :name="actionIcons[value]" />{{ actionNames[value] }}</button></div></footer>
    </template>
  </dialog>
</template>
