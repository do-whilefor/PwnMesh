<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, useId } from 'vue';
import type { DetailRecord } from './detail';
import InspectorIcon from './InspectorIcon.vue';
import Icon from '@/ui/Icon.vue';
const emit = defineEmits<{ locate: [key: string]; toast: [message: string]; closed: [] }>();
const dialog = ref<HTMLDialogElement>(), body = ref<HTMLElement>(), record = ref<DetailRecord>();
const id = useId();
const longTitle = computed(() => !!record.value && (record.value.title.length > 64 || /[\r\n]/.test(record.value.title)));
let origin: HTMLElement | null = null, motion: Animation | undefined, navigating = false;
const copying = ref(false);
function animate(closing: boolean, finish?: () => void) {
  motion?.cancel(); motion = undefined;
  const element = dialog.value;
  if (!element || matchMedia('(prefers-reduced-motion: reduce)').matches || !element.animate) { finish?.(); return; }
  const visible = { opacity: 1, transform: 'translateY(0) scale(1)' };
  const hidden = { opacity: 0, transform: `translateY(${closing ? 8 : 12}px) scale(.985)` };
  motion = element.animate(closing ? [visible, hidden] : [hidden, visible], { duration: closing ? 140 : 180, easing: closing ? 'cubic-bezier(.4, 0, 1, 1)' : 'cubic-bezier(.16, 1, .3, 1)', fill: 'both' });
  motion.onfinish = () => { motion?.cancel(); motion = undefined; finish?.(); };
}
async function open(value: DetailRecord) {
  origin?.classList.remove('is-reading');
  origin = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  origin?.classList.add('is-reading'); origin?.setAttribute('aria-expanded', 'true');
  record.value = value; navigating = false; await nextTick();
  dialog.value?.showModal(); if (body.value) body.value.scrollTop = 0; animate(false);
}
function close() { if (dialog.value?.open) animate(true, () => dialog.value?.close()); }
function closed() {
  motion?.cancel(); motion = undefined;
  origin?.classList.remove('is-reading'); origin?.setAttribute('aria-expanded', 'false');
  if (!navigating && origin?.isConnected) origin.focus({ preventScroll: true });
  emit('closed');
}
function locate() { const key = record.value?.nodeKey; if (!key) return; navigating = true; motion?.cancel(); dialog.value?.close(); emit('locate', key); }
function backdrop(event: MouseEvent) { if (event.target !== dialog.value) return; const box = dialog.value.getBoundingClientRect(); if (event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom) close(); }
async function copy() {
  if (copying.value || !dialog.value) return;
  copying.value = true;
  const text = [...dialog.value.querySelectorAll('[data-inspector-copy-part]')].map(part => part.textContent).join('\n\n');
  let copied = false;
  try { await navigator.clipboard.writeText(text); copied = true; } catch { /* Preserve local copy when clipboard permission is unavailable. */ }
  if (!copied) { const input = document.createElement('textarea'); input.value = text; input.className = 'inspector-copy-buffer'; dialog.value.append(input); try { input.select(); copied = document.execCommand('copy'); } finally { input.remove(); } }
  copying.value = false; emit('toast', copied ? '完整内容已复制' : '复制未完成，请在放大阅读中选择文本。');
}
onBeforeUnmount(() => { motion?.cancel(); if (dialog.value?.open) dialog.value.close(); origin?.classList.remove('is-reading'); });
defineExpose({ open, close });
</script>
<template>
  <dialog ref="dialog" class="inspector-reader" :class="{ 'log-record-reader': record?.kind === '黑板日志' }" :aria-labelledby="id + '-reader-title'" @cancel.prevent="close" @close="closed" @click="backdrop">
    <template v-if="record">
      <header class="inspector-reader-header"><div><span>{{ record.kind }}</span><h2 :id="id + '-reader-title'" :title="record.title" :data-inspector-copy-part="longTitle ? undefined : ''">{{ record.title }}</h2></div><button type="button" class="icon-button" data-inspector-action="close" aria-label="关闭放大阅读" @click="close"><InspectorIcon name="close"/></button></header>
      <div ref="body" class="inspector-reader-content" tabindex="0" role="region" aria-label="完整内容">
        <div v-if="record.meta.length" class="inspector-reader-meta"><p v-for="item in record.meta" :key="item.label" data-inspector-copy-part><span>{{ item.label }}：</span>{{ item.value }}</p></div>
        <section v-if="longTitle" class="inspector-reader-full-title"><p data-inspector-copy-part>{{ record.title }}</p></section>
        <section v-if="record.body !== record.title" class="inspector-reader-body"><pre class="inspector-text" data-inspector-copy-part>{{ record.body }}</pre></section>
        <section v-if="record.output"><h3 data-inspector-copy-part>执行输出</h3><pre class="inspector-text inspector-code" data-inspector-copy-part>{{ record.output }}</pre></section>
        <section v-if="record.evidence?.length"><h3 data-inspector-copy-part>关联证据 · {{ record.evidence.length }}</h3><section v-for="(item, index) in record.evidence" :key="index" class="inspector-reader-evidence"><h4 data-inspector-copy-part>{{ String(index + 1).padStart(2, '0') }} · {{ item.title }}</h4><code v-if="item.path" class="inspector-evidence-path" data-inspector-copy-part>{{ item.path }}</code><p v-if="item.reference" class="inspector-evidence-reference" data-inspector-copy-part>{{ item.reference }}</p><pre class="inspector-text" data-inspector-copy-part>{{ item.body }}</pre></section></section>
      </div>
      <footer class="inspector-reader-footer"><button type="button" class="inspector-reader-top" data-inspector-action="top" @click="body?.scrollTo(0, 0)"><InspectorIcon name="back"/>回到顶部</button><button v-if="record.nodeKey || record.kind === '黑板日志'" type="button" class="inspector-reader-locate" data-inspector-action="locate" :disabled="!record.nodeKey" :title="record.nodeKey ? undefined : '此日志没有对应节点'" @click="locate"><InspectorIcon name="locate"/>定位节点</button><button type="button" class="button secondary" data-inspector-action="copy" :disabled="copying" @click="copy"><InspectorIcon name="copy"/>复制完整内容</button><a v-if="record.download" class="button secondary" :href="record.download.url" :download="record.download.name"><Icon name="download"/>下载原始文件</a></footer>
    </template>
  </dialog>
</template>
