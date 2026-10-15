<script setup lang="ts">
defineProps<{ name: string }>();
const paths: Record<string, string> = { close: 'm6 6 12 12M6 18 18 6', back: 'M19 12H5m6-6-6 6 6 6', expand: 'M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5', chevron: 'm9 5 7 7-7 7', output: 'm4 6 5 5-5 5m9 2h7', evidence: 'M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9Zm0 0v6h6M8 13h8M8 17h5' };
</script>
<template><svg class="inspector-icon" viewBox="0 0 24 24" aria-hidden="true"><template v-if="name === 'copy'"><rect x="8" y="8" width="12" height="13" rx="2" /><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3" /></template><template v-else-if="name === 'locate'"><circle cx="12" cy="12" r="6"/><circle cx="12" cy="12" r="2"/><path d="M12 2v4m0 12v4M2 12h4m12 0h4"/></template><path v-else :d="paths[name] || paths.evidence"/></svg></template>
