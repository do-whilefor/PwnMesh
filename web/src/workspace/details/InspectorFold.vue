<script setup lang="ts">
import {ref, watch} from 'vue';
import {useDisclosureMotion} from '@/ui/useDisclosureMotion';

const props = defineProps<{resetKey?: unknown}>();
const emit = defineEmits<{collapse: [element: HTMLElement]}>();
const element = ref<HTMLDetailsElement>(),expanded = ref(false);
const motion = useDisclosureMotion(expanded,element),displayed = motion.displayed;
watch(() => props.resetKey,() => {expanded.value = false;motion.reset();});
function toggle(event: MouseEvent) {
  const target = event.target as HTMLElement,details = element.value;
  if (!details) return;
  // Nested content may change while this container is resizing; release its height first.
  if (target.closest('details') !== details) {motion.reset();return;}
  const summary = target.closest('summary'),collapse = target.closest('[data-inspector-action="collapse-section"]');
  if (!summary && !collapse) return;
  event.preventDefault();
  expanded.value = collapse ? false : !expanded.value;
  if (!expanded.value) emit('collapse',details);
}
</script>
<template><details ref="element" :open="displayed" @click="toggle"><summary :aria-expanded="expanded"><slot name="summary"/></summary><slot/></details></template>
