<script setup lang="ts">
import { glyphs } from './icons';
defineProps<{ name: string }>();
</script>

<template>
  <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" :class="['lucide', 'lucide-' + name]" :data-lucide="name" aria-hidden="true">
    <component :is="tag" v-for="([tag, attrs], index) in glyphs[name]" :key="index" v-bind="attrs" />
  </svg>
</template>
