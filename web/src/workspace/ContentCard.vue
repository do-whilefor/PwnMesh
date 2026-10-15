<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import Icon from '@/ui/Icon.vue';
const props = defineProps<{ label: string; contentKey: string; body: string; readKey: string }>();
const emit = defineEmits<{ read: [] }>();
const expanded = ref(false), overflow = ref(false), bodyElement = ref<HTMLElement>(), card = ref<HTMLElement>();
let observer: ResizeObserver | undefined;
async function measure() { await nextTick(); const el = bodyElement.value; if (el) overflow.value = el.scrollHeight > parseFloat(getComputedStyle(el).lineHeight) * 5 + 2; }
function toggle() { expanded.value = !expanded.value; if (!expanded.value) nextTick(() => { const host = card.value?.closest('#project-view'); if (host && card.value) host.scrollTop += card.value.getBoundingClientRect().top - host.getBoundingClientRect().top - 16; }); }
onMounted(() => { void measure(); if (bodyElement.value) { observer = new ResizeObserver(() => void measure()); observer.observe(bodyElement.value); } });
watch(() => props.body, measure); onBeforeUnmount(() => observer?.disconnect());
</script>
<template><section ref="card" class="content-card" :data-content-key="contentKey"><header class="content-heading"><h3>{{ label }}</h3><div class="content-actions"><button class="text-button" data-content-toggle :hidden="!expanded && !overflow" :aria-expanded="expanded" :aria-controls="'content-' + readKey.split(':').pop()" @click="toggle">{{ expanded ? '收起全文' : '展开全文' }}<Icon name="chevron-down"/></button><button class="text-button" :data-content-read="readKey" aria-haspopup="dialog" @click="emit('read')"><Icon name="maximize-2"/>放大阅读</button></div></header><slot/><p :id="'content-' + readKey.split(':').pop()" ref="bodyElement" class="content-text" :class="{ 'is-collapsed': !expanded }">{{ body }}</p><slot name="footer"/></section></template>
