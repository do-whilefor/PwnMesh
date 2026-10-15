<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';
import type { ProjectView } from './model/types';
import type { ProjectOperation } from './dialogs/types';
import { actionNames, actionIcons } from './dialogs/types';
import { actions } from './model/lifecycle';
import { PROJECT_TYPES } from './model/project-model';
import Icon from '@/ui/Icon.vue';
const props = defineProps<{ projects: readonly ProjectView[]; selected?: string; disabled: boolean }>();
const emit = defineEmits<{ create: []; action: [operation: ProjectOperation] }>();
const query = ref(''), input = ref<HTMLInputElement>(), menu = ref<HTMLElement>(), menuProject = ref<ProjectView>(), trigger = ref<HTMLElement>(), position = ref({ left: '0px', top: '0px' });
const types: Record<string, string> = Object.fromEntries([['', '通用'], ...PROJECT_TYPES.map(item => [item.value, item.label])]);
const labels: Record<string, string> = { running: '探索中', done: '已完成', paused: '已暂停', pending: '待开始', terminated: '已终止' };
const visible = computed(() => props.projects.filter(project => (project.name + ' ' + project.target + ' ' + types[project.type]).toLowerCase().includes(query.value.trim().toLowerCase())));
function close(restore = false) { menuProject.value = undefined; if (restore) trigger.value?.focus(); }
async function open(project: ProjectView, event: MouseEvent) {
  if (menuProject.value?.id === project.id) { close(); return; }
  menuProject.value = project; trigger.value = event.currentTarget as HTMLElement;
  await nextTick();
  const box = trigger.value.getBoundingClientRect(), width = menu.value?.offsetWidth || 0, height = menu.value?.offsetHeight || 0;
  position.value = { left: Math.max(8, Math.min(box.right - width, innerWidth - width - 8)) + 'px', top: Math.max(8, Math.min(box.bottom + 6, innerHeight - height - 8)) + 'px' };
  menu.value?.querySelector('button')?.focus();
}
function outside(event: PointerEvent) { if (!(event.target instanceof Element) || !event.target.closest('#project-menu,[data-project-menu]')) close(); }
function keyboard(event: KeyboardEvent) {
  if (!menuProject.value || !['ArrowDown', 'ArrowUp', 'Home', 'End', 'Escape', 'Tab'].includes(event.key)) return;
  if (event.key === 'Escape' || event.key === 'Tab') { close(event.key === 'Escape'); if (event.key === 'Escape') event.preventDefault(); return; }
  const buttons = [...menu.value?.querySelectorAll('button') || []], index = buttons.indexOf(document.activeElement as HTMLButtonElement);
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (index + (event.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length;
  event.preventDefault(); buttons[next]?.focus();
}
function resize() { close(); }
onMounted(() => { document.addEventListener('pointerdown', outside); document.addEventListener('keydown', keyboard); window.addEventListener('resize', resize); });
onBeforeUnmount(() => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', keyboard); window.removeEventListener('resize', resize); });
defineExpose({ focusSearch: () => input.value?.focus(), clear: () => { query.value = ''; close(); } });
</script>
<template>
  <aside class="sidebar" aria-label="工作空间导航"><a class="brand" :href="projects[0] ? '#/project/' + projects[0].id : '#/projects'" aria-label="PwnMesh 工作台"><span class="brand-art"><img src="/static/brand.png" alt="PwnMesh"></span></a><button class="button primary create-button" data-create @click="emit('create')"><Icon name="plus"/>新建项目<kbd>N</kbd></button><label class="project-search"><Icon name="search"/><input id="project-search" ref="input" v-model="query" type="search" aria-label="搜索项目" placeholder="搜索项目" @input="close()"><kbd>/</kbd></label><div class="nav-caption"><span>项目</span><span id="project-count">{{ query ? visible.length + '/' + projects.length : projects.length }}</span></div><nav id="project-list" aria-label="项目列表" @scroll="close()"><div v-for="project in visible" :key="project.id" class="project-row"><a class="project-item" :class="{ selected: selected === project.id }" :data-project-type="project.type" :data-project-status="project.status" :href="'#/project/' + project.id" :aria-current="selected === project.id ? 'page' : undefined"><span class="project-copy"><strong :title="project.name">{{ project.name }}</strong><small class="project-meta"><span class="project-type">{{ types[project.type] || '通用' }}</span><span class="project-state"><i class="status-dot" :class="project.status"/>{{ project.readOnly ? '历史只读' : labels[project.status] }}</span></small></span></a><button class="icon-button project-more" :data-project-menu="project.id" data-mutates :disabled="disabled || !actions(project).length" :aria-label="project.name + '的项目操作'" aria-haspopup="menu" :aria-expanded="menuProject?.id === project.id" title="项目操作" @click="open(project, $event)"><Icon name="ellipsis"/></button></div><div v-if="!visible.length" class="empty-state sidebar-empty">{{ projects.length ? '没有找到匹配的项目' : '暂无项目' }}<button v-if="projects.length" id="clear-search" class="text-button" @click="query = ''; input?.focus()">清除搜索</button></div></nav></aside>
  <Teleport to="body"><div id="project-menu" ref="menu" class="project-menu-popover" role="menu" aria-label="项目操作" :hidden="!menuProject" :style="position"><template v-if="menuProject"><button v-for="action in actions(menuProject).filter(value => ['restart', 'delete'].includes(value))" :key="action" role="menuitem" :data-action="action" :class="{ 'danger-item': action === 'delete' }" @click="emit('action', { id: menuProject.id, action, generation: menuProject.generation, status: menuProject.status }); close()"><Icon :name="actionIcons[action]"/>{{ actionNames[action] }}</button></template></div></Teleport>
</template>
