<script setup lang="ts">
import { computed, ref, useId, watch } from 'vue';
import type { DetailRecord } from './detail';
import InspectorIcon from './InspectorIcon.vue';
import MotionDetails from '@/ui/MotionDetails.vue';
import { useDisclosureMotion } from '@/ui/useDisclosureMotion';
const props = defineProps<{ record: DetailRecord; recordKey?: string | null }>();
const emit = defineEmits<{ back: []; read: [record: DetailRecord] }>();
const expanded = ref(false), panel = ref<HTMLElement>(), body = ref<HTMLElement>();
const motion = useDisclosureMotion(expanded, body), displayed = motion.displayed;
const identity = computed(() => props.recordKey ?? props.record);
const id = useId();
const long = computed(() => props.record.body.length > 280 || props.record.body.split('\n').length > 6);
watch(identity, () => { expanded.value = false; motion.reset(); });
function align(section?: HTMLElement | null) {
  const host = panel.value?.parentElement; if (!host || !section) return;
  const header = panel.value?.querySelector('.selection-header')?.getBoundingClientRect().height || 0;
  const parent = section.parentElement?.closest('.inspector-fold[open]')?.querySelector('summary')?.getBoundingClientRect().height || 0;
  host.scrollTop = Math.max(0, host.scrollTop + section.getBoundingClientRect().top - host.getBoundingClientRect().top - header - parent);
  section.querySelector<HTMLElement>('summary,button')?.focus({ preventScroll: true });
}
function toggle(value: boolean) { expanded.value = value; if (!value) align(panel.value?.querySelector('.inspector-tools')); }
</script>
<template>
  <section ref="panel" class="node-inspector">
    <div class="selection-header"><button id="clear-selection" type="button" class="inspector-back" @click="emit('back')"><InspectorIcon name="back"/>黑板日志</button></div>
    <div class="inspector-heading"><h3 class="inspector-title" :title="record.title">{{ record.title }}</h3><div class="inspector-summary"><span class="badge">{{ record.kind }}</span><dl class="inspector-meta"><div v-for="item in record.meta" :key="item.label" :class="{ 'inspector-scope-row': item.label === '范围', 'inspector-status-row': item.label === '状态' }"><dt>{{ item.label }}</dt><dd :title="item.value">{{ item.value }}</dd></div></dl></div></div>
    <div class="inspector-tools"><button v-if="long" type="button" class="inspector-expand-text" data-inspector-action="toggle" :aria-expanded="expanded" :aria-controls="id + '-body'" @click="toggle(!expanded)">{{ expanded ? '收起全文' : '展开全文' }}<InspectorIcon name="chevron"/></button><button type="button" data-inspector-action="read" aria-haspopup="dialog" @click="emit('read', record)"><InspectorIcon name="expand"/>放大阅读</button></div>
    <section v-if="record.body !== record.title || long" ref="body" class="inspector-body-section"><pre :id="id + '-body'" class="inspector-text inspector-body-text" :class="{ 'is-collapsed': long && !displayed }">{{ record.body }}</pre><button v-if="long" type="button" class="inspector-expand-text inspector-body-collapse" data-inspector-action="collapse-body" :hidden="!displayed" @click="toggle(false)">收起全文<InspectorIcon name="chevron"/></button></section>
    <MotionDetails v-if="record.output" :reset-key="identity" class="inspector-fold" @collapse="align"><template #summary><InspectorIcon name="output"/><span>执行输出</span><InspectorIcon name="chevron"/></template><pre class="inspector-text inspector-code">{{ record.output }}</pre><button type="button" class="inspector-section-collapse" data-inspector-action="collapse-section">收起执行输出<InspectorIcon name="chevron"/></button></MotionDetails>
    <MotionDetails v-if="record.evidence?.length" :reset-key="identity" class="inspector-fold inspector-evidence" @collapse="align"><template #summary><InspectorIcon name="evidence"/><span>关联证据</span><small>{{ String(record.evidence.length).padStart(2, '0') }}</small><InspectorIcon name="chevron"/></template><div><MotionDetails v-for="(item, index) in record.evidence" :key="index" :reset-key="identity" class="inspector-evidence-item" @collapse="align"><template #summary><span>{{ String(index + 1).padStart(2, '0') }}</span><strong>{{ item.title }}</strong><InspectorIcon name="chevron"/></template><div><code v-if="item.path" class="inspector-evidence-path">{{ item.path }}</code><p v-if="item.reference" class="inspector-evidence-reference">{{ item.reference }}</p><pre class="inspector-text">{{ item.body }}</pre><button type="button" class="inspector-section-collapse" data-inspector-action="collapse-section">收起此证据<InspectorIcon name="chevron"/></button></div></MotionDetails></div><button type="button" class="inspector-section-collapse" data-inspector-action="collapse-section">收起全部证据<InspectorIcon name="chevron"/></button></MotionDetails>
  </section>
</template>
