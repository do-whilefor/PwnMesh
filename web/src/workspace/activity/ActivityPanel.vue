<script setup lang="ts">
import { computed, nextTick, ref, shallowRef, watch } from 'vue';
import type { ProjectView } from '../model/types';
import { buildEntries } from '../model/activity-data';
import type { DetailRecord } from '../details/detail';
import SelectionInspector from '../details/SelectionInspector.vue';
import LogEntry from './LogEntry.vue';
import type { Entry } from './LogEntry.vue';
import Icon from '@/ui/Icon.vue';
import { dateTime } from '@/ui/format';
import { useViewMotion } from '@/ui/useViewMotion';
const props = defineProps<{ project?: ProjectView; projectIds?: readonly string[]; selection?: DetailRecord | null; selectionKey?: string }>();
const emit = defineEmits<{ back: []; read: [record: DetailRecord] }>();
const pane = ref<HTMLElement>(), host = ref<HTMLElement>(), search = ref<HTMLInputElement>();
const selectionId = computed(() => props.selection ? props.selectionKey || props.selection.nodeKey || `${props.selection.kind}:${props.selection.title}` : '');
const {displayed, ready, reset} = useViewMotion(selectionId, pane);
const visibleSelection = shallowRef(props.selection);
// Keep the outgoing record through the fade, but refresh the same record in place.
watch([displayed, () => props.selection], ([shown, record]) => {if (shown === selectionId.value) visibleSelection.value = record;}, {flush: 'sync'});
const query = ref(''), expanded = ref(new Set<string>()), scrollTop = ref(0);
const cache = new Map<string, { query: string; expanded: Set<string>; scrollTop: number; revision: string }>();
const key = computed(() => props.project ? props.project.id + ':' + props.project.generation : '');
const entries = computed(() => props.project ? buildEntries(props.project) : []);
const filtered = computed(() => entries.value.filter(entry => !query.value.trim() || [entry.title, entry.body, entry.evidence].join('\n').toLowerCase().includes(query.value.trim().toLowerCase())));
const revision = computed(() => JSON.stringify(entries.value.map(({ id, timestamp, title, body, evidence }) => [id, timestamp, title, body, evidence])));
watch(() => props.projectIds, ids => { if (ids) for (const stored of cache.keys()) if (!ids.some(id => stored.startsWith(id + ':'))) cache.delete(stored); });
watch([key, revision], ([value, content], [old, previousContent]) => {
  if (value !== old) {
    if (old && (!props.projectIds || props.projectIds.some(id => old.startsWith(id + ':')))) cache.set(old, { query: query.value, expanded: expanded.value, scrollTop: scrollTop.value, revision: previousContent });
    const saved = cache.get(value); query.value = saved?.query || ''; expanded.value = saved?.expanded || new Set();
    scrollTop.value = saved?.revision === content ? saved.scrollTop : 0;
  } else if (content !== previousContent) scrollTop.value = 0;
  nextTick(() => { if (host.value && !visibleSelection.value) host.value.scrollTop = scrollTop.value; });
});
watch(displayed, value => nextTick(() => { if (host.value && value === displayed.value) host.value.scrollTop = visibleSelection.value ? 0 : scrollTop.value; }));
function scroll() { if (!visibleSelection.value) scrollTop.value = host.value?.scrollTop || 0; }
function filter(value: string) { query.value = value; scrollTop.value = 0; host.value?.scrollTo(0, 0); }
function input(event: Event) { if (!(event as InputEvent).isComposing) filter((event.target as HTMLInputElement).value); }
function expand(id: string, value: boolean) { const next = new Set(expanded.value); if (value) next.add(id); else next.delete(id); expanded.value = next; }
function read(entry: Entry) { emit('read', { title: entry.title, kind: '黑板日志', body: [entry.body, entry.evidence].filter(Boolean).join('\n\n'), meta: [{ label: '项目', value: props.project?.name || '' }, { label: '时间', value: entry.timestamp ? dateTime(entry.timestamp) : entry.time }], nodeKey: entry.key || undefined }); }
function clear() { filter(''); search.value?.focus(); }
defineExpose({ready, reset});
</script>
<template>
  <aside ref="pane" class="activity-pane" aria-labelledby="activity-title">
    <header class="activity-heading"><h2><Icon name="notebook-text"/><span id="activity-title">{{ visibleSelection ? visibleSelection.kind === '关系' ? '关系详情' : '节点详情' : '黑板日志' }}</span></h2><span id="activity-match-count" role="status" aria-live="polite" :hidden="!!visibleSelection">{{ filtered.length }} 条{{ query.trim() ? '匹配' : '记录' }}</span><button id="activity-collapse-all" class="icon-button" title="收起全部展开的日志" aria-label="收起全部展开的日志" :hidden="!!visibleSelection || !expanded.size" @click="expanded = new Set(); host?.scrollTo(0, 0); search?.focus()"><Icon name="list-collapse"/></button></header>
    <div id="activity-toolbar" :hidden="!!visibleSelection"><div class="activity-search"><label for="activity-search" class="visually-hidden">搜索黑板日志全文</label><Icon name="search"/><input id="activity-search" ref="search" type="search" placeholder="搜索日志全文" :value="query" :disabled="!project" autocomplete="off" @input="input" @compositionend="filter(($event.target as HTMLInputElement).value)" @keydown.esc.prevent="clear"><button id="activity-clear-search" class="icon-button" aria-label="清除日志搜索" :hidden="!query" @click="clear"><Icon name="x"/></button></div></div>
    <div id="activity-content" ref="host" class="activity-content" role="region" aria-labelledby="activity-title" tabindex="0" @scroll.passive="scroll"><SelectionInspector v-if="visibleSelection" :record="visibleSelection" :record-key="displayed" @back="emit('back')" @read="emit('read', $event)"/><div v-else-if="filtered.length" class="log-stream"><LogEntry v-for="entry in filtered" :key="entry.id" :entry="entry" :query="query.trim()" :expanded="expanded.has(entry.id)" @expand="expand" @read="read"/></div><div v-else-if="project" class="empty-state log-empty"><Icon name="search"/>没有匹配的记录<button id="activity-reset" class="text-button" @click="clear">清除筛选</button></div></div>
    <button id="activity-to-latest" title="返回最新记录" :hidden="!!visibleSelection || scrollTop < 100" @click="host?.scrollTo(0, 0)"><Icon name="arrow-up"/>最新记录</button>
  </aside>
</template>
