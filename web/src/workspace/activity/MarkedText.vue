<script setup lang="ts">
import { computed } from 'vue';
const props = defineProps<{ text: string; query: string }>();
const parts = computed(() => {
  const result: { value: string; hit: boolean }[] = [], needle = props.query.toLowerCase();
  if (!needle) return [{ value: props.text, hit: false }];
  let start = 0, at = props.text.toLowerCase().indexOf(needle);
  while (at !== -1) { result.push({ value: props.text.slice(start, at), hit: false }, { value: props.text.slice(at, at + needle.length), hit: true }); start = at + needle.length; at = props.text.toLowerCase().indexOf(needle, start); }
  result.push({ value: props.text.slice(start), hit: false }); return result;
});
</script>
<template><template v-for="(part, index) in parts" :key="index"><mark v-if="part.hit">{{ part.value }}</mark><template v-else>{{ part.value }}</template></template></template>
