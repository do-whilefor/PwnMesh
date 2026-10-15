<script setup lang="ts">
import { onBeforeUnmount, watch } from 'vue';
import type { ProjectView } from '../model/types';
import { canHint } from '../model/lifecycle';
import { fileSize } from '../model/project-model';
import { textFile, read, downloadURL } from '../model/input-preview';
import { dateTime, errorMessage } from '@/ui/format';
import type { DetailRecord } from '../details/detail';
import Icon from '@/ui/Icon.vue';
import ContentCard from '../ContentCard.vue';
const props = defineProps<{ project: ProjectView; active?: boolean }>();
const emit = defineEmits<{ hint: []; read: [record: DetailRecord] }>();
let controller: AbortController | undefined, version = 0;
function cancel() { version++; controller?.abort(); controller = undefined; }
watch([() => props.project.id, () => props.project.generation, () => props.active], cancel); onBeforeUnmount(cancel);
async function material(index: number) {
  cancel(); const ticket = version, file = props.project.files[index]; if (!file) return;
  let body = '二进制文件或超过 256 KiB 的材料，请下载原始文件查看。';
  if (textFile(file)) {
    const request = new AbortController(); controller = request;
    const timer = setTimeout(() => request.abort(), 15000);
    try { body = await read(downloadURL(props.project.id, file.id), { signal: request.signal }); }
    catch (error) { body = error instanceof Error && error.name === 'AbortError' ? '读取超时，请下载原始文件查看。' : errorMessage(error); }
    finally { clearTimeout(timer); if (controller === request) controller = undefined; }
  }
  if (ticket !== version) return;
  emit('read', { kind: '项目材料', title: file.name, body, meta: [{ label: '大小', value: fileSize(file.size) }, { label: '导入时间', value: dateTime(file.created_at) }, { label: '路径', value: file.path || '' }, { label: 'SHA-256', value: file.sha256 || '' }], download: { url: downloadURL(props.project.id, file.id), name: file.name } });
}
function input(key: 'target' | 'goal') { emit('read', { kind: '项目输入', title: key === 'target' ? '起点 / 已知信息' : '终点 / 项目目标', body: props.project[key], meta: [{ label: '项目', value: props.project.name }] }); }
defineExpose({ cancel });
</script>
<template><div class="section-heading"><h2 class="section-title">项目输入</h2></div><div class="project-inputs"><ContentCard label="起点 / 已知信息" content-key="target" read-key="input:target" :body="project.target" @read="input('target')"/><ContentCard label="终点 / 项目目标" content-key="goal" read-key="input:goal" :body="project.goal" @read="input('goal')"/></div><div class="section-heading"><h2 class="section-title">材料</h2><button v-if="canHint(project)" id="add-materials" class="text-button" @click="emit('hint')"><Icon name="plus"/>添加材料</button></div><div v-for="(file, index) in project.files" :key="file.id" class="material-row" role="button" tabindex="0" :data-material="index" :aria-label="'查看材料：' + file.name" @click="material(index)" @keydown.enter.prevent="material(index)" @keydown.space.prevent="material(index)"><Icon name="file-text"/><span><strong :title="file.name">{{ file.name }}</strong><small>{{ fileSize(file.size) }}</small></span></div><div v-if="!project.files.length" class="empty-state"><Icon name="folder-open"/>{{ project.filesKnown === false ? '此历史轮次未保存材料清单。可在当前轮次查看项目保留的材料。' : '暂无材料' }}</div></template>
