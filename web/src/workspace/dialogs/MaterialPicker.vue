<script setup lang="ts">
import Icon from '@/ui/Icon.vue';
import {fileSize} from '../model/project-model';
defineProps<{kind: 'create' | 'hint'; files: File[]; importedCount?: number; disabled?: boolean; removeDisabled?: boolean; invalid?: boolean}>();
const emit = defineEmits<{add: [files: File[]]; remove: [index: number]}>();
function add(event: Event) {
  const input = event.target;
  if (!(input instanceof HTMLInputElement)) return;
  const incoming = Array.from(input.files || []); input.value = '';
  if (incoming.length) emit('add', incoming);
}
</script>
<template>
  <div class="forms-material-label"><label class="field-label" :for="`${kind === 'create' ? 'project' : 'hint'}-files`">材料 <span>可选</span></label><span :id="`${kind}-file-count`" class="forms-file-count">{{ files.length + (importedCount || 0) }} / 32{{ kind === 'hint' ? '（含已导入）' : '' }}</span></div>
  <label class="forms-upload" :for="`${kind === 'create' ? 'project' : 'hint'}-files`"><Icon name="paperclip" /><span>选择文件</span><small>上传原始文件</small><input :id="`${kind === 'create' ? 'project' : 'hint'}-files`" type="file" multiple :aria-describedby="`${kind === 'create' ? 'project' : 'hint'}-file-limits`" :aria-invalid="invalid || undefined" :disabled="disabled" @change="add"></label>
  <p class="forms-file-limits" :id="`${kind === 'create' ? 'project' : 'hint'}-file-limits`">最多 32 个 · 单个 ≤ 256 MiB · 总计 ≤ 512 MiB</p>
  <ul :id="kind === 'create' ? 'selected-files' : 'hint-selected-files'" class="file-list forms-files"><li v-for="(file, index) in files" :key="index"><Icon name="file" /><span :title="file.name">{{ file.name }}</span><small>{{ fileSize(file.size) }}</small><button type="button" class="icon-button" :data-remove-file="kind === 'create' ? index : undefined" :data-remove-hint-file="kind === 'hint' ? index : undefined" :aria-label="`移除 ${file.name}`" :disabled="disabled || removeDisabled" @click="emit('remove', index)"><Icon name="x" /></button></li></ul>
</template>
