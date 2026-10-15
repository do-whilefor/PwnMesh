<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import Icon from '@/ui/Icon.vue';
import { dateTime } from '@/ui/format';
import MarkedText from './MarkedText.vue';
export interface Entry { id: string; phase: string; time: string; timestamp?: string; title: string; body: string; evidence: string; key: string | null }
const props = defineProps<{ entry: Entry; query: string; expanded: boolean }>();
const emit = defineEmits<{ expand: [id: string, value: boolean]; read: [entry: Entry] }>();
const root = ref<HTMLElement>(), body = ref<HTMLElement>(), detail = ref<HTMLDetailsElement>();
const fold = ref(true);
let observer: ResizeObserver | undefined;
const content = computed(() => [props.entry.body, props.entry.evidence].filter(Boolean).filter((value, index, all) => all.indexOf(value) === index).join('\n\n'));
const paragraphs = computed(() => content.value.split(/\r?\n\s*\r?\n/));
const preview = computed(() => { const hit = props.query ? content.value.toLowerCase().indexOf(props.query.toLowerCase()) : -1; const start = Math.max(0, hit - 25); return (start ? '…' : '') + content.value.slice(start, start + 260) + (start + 260 < content.value.length ? '…' : ''); });
async function measure() {
  await nextTick(); if (!body.value || !root.value) return;
  const probe = body.value.cloneNode(true) as HTMLElement;
  Object.assign(probe.style, { position: 'absolute', visibility: 'hidden', pointerEvents: 'none', width: (detail.value || root.value).getBoundingClientRect().width + 'px', margin: '0' });
  root.value.append(probe); const tall = probe.getBoundingClientRect().height > parseFloat(getComputedStyle(probe).lineHeight) * 4 + 1; probe.remove();
  fold.value = tall;
}
function toggle(event: MouseEvent) {
  event.preventDefault(); const open = !props.expanded; emit('expand', props.entry.id, open);
  if (!open) nextTick(() => { const host = root.value?.closest('#activity-content'); if (host && root.value) host.scrollTop += root.value.getBoundingClientRect().top - host.getBoundingClientRect().top - 6; detail.value?.querySelector('summary')?.focus({ preventScroll: true }); });
}
onMounted(() => { void measure(); if (root.value) { observer = new ResizeObserver(() => void measure()); observer.observe(root.value); } });
onBeforeUnmount(() => observer?.disconnect());
watch(content, measure);
</script>
<template>
  <article ref="root" class="timeline-entry" :class="{ 'is-finding': entry.phase === 'FINDING' }" :data-record="entry.id" :data-phase="entry.phase">
    <div class="entry-headline"><div class="entry-title"><h3><button class="log-title" :data-log-read="entry.id" :aria-label="'阅读全文：' + entry.title" aria-haspopup="dialog" title="查看完整日志" @click="emit('read', entry)"><span class="log-entry-title"><MarkedText :text="entry.title" :query="query"/></span><Icon name="chevron-right"/></button></h3></div><div class="entry-meta"><time :datetime="entry.timestamp || ''" :title="entry.timestamp ? dateTime(entry.timestamp) : entry.time">{{ entry.time }}</time></div></div>
    <template v-if="content"><details v-if="fold" ref="detail" class="log-long-content" :data-log-id="entry.id" :open="expanded"><summary @click="toggle"><span class="log-text-preview"><MarkedText :text="preview" :query="query"/></span><span class="log-expand"><span class="log-label-closed">展开全文</span><span class="log-label-open">收起全文</span><Icon name="chevron-down"/><button class="log-read-sticky" :data-log-read="entry.id" title="在独立窗口中阅读日志全文" @click.stop.prevent="emit('read', entry)"><Icon name="maximize-2"/>放大阅读</button></span></summary><div ref="body" class="log-body"><p v-for="(text, index) in paragraphs" :key="index"><MarkedText :text="text" :query="query"/></p></div></details><div v-else ref="body" class="log-body"><p v-for="(text, index) in paragraphs" :key="index"><MarkedText :text="text" :query="query"/></p></div></template>
  </article>
</template>
