<script setup lang="ts">
import { computed } from 'vue';
import type { ProjectView } from '../model/types';
import type { FindingDTO } from '@/api/types';
import { buildResult } from '../model/data';
import { object, plain } from '@/ui/format';
import type { DetailRecord } from '../details/detail';
import ContentCard from '../ContentCard.vue';
import Icon from '@/ui/Icon.vue';
const props = defineProps<{ project: ProjectView }>();
const emit = defineEmits<{ read: [record: DetailRecord]; locate: [key: string]; toast: [message: string] }>();
const state = computed(() => props.project.state || { graph: { project: props.project.raw }, revision: 0, goals: [], steps: [], fact_records: [], findings: [], fact_relations: [] });
const result = computed(() => buildResult(state.value, props.project.logs));
function findingBody(finding: FindingDTO) {
  const fact = state.value?.fact_records.find(record => record.id === finding.sources?.[0]);
  const step = state.value?.steps.find(record => record.id === fact?.source_step_id);
  return [finding.support_valid === false ? '支持证据已失效，请核对来源。' : '', plain(object(object(step).detail).body) || fact?.description || finding.reason || '', ...(finding.evidence || []).map(item => [item.path, item.excerpt].filter(Boolean).join('\n'))].filter(Boolean).join('\n\n');
}
function read(finding?: FindingDTO) { emit('read', { kind: '探索结论', title: finding?.claim || '探索结论', body: finding ? findingBody(finding) : result.value.summary || result.value.notice, meta: [{ label: '项目', value: props.project.name }, ...(finding ? [{ label: '范围', value: finding.scope || '' }] : [])], nodeKey: finding ? 'finding:' + finding.id : undefined }); }
function download() {
  const body = '# ' + props.project.name + '\n\nPwnMesh 项目记录 · 第 ' + (props.project.generation + 1) + ' 轮\n\n' + result.value.summary + '\n\n' + (state.value?.findings || []).map(item => '## ' + item.claim + '\n\n范围：' + item.scope + '\n\n' + findingBody(item)).join('\n\n');
  const url = URL.createObjectURL(new Blob([body], { type: 'text/markdown;charset=utf-8' })); const link = document.createElement('a');
  link.href = url; link.download = props.project.name.replace(/[<>:"/\\|?*\x00-\x1f]/g, '_').slice(0, 100) + '-第' + (props.project.generation + 1) + '轮.md'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); emit('toast', '记录已导出');
}
</script>
<template><div class="section-heading"><h2 class="section-title">{{ project.status === 'done' ? '探索结论' : '阶段性发现' }}</h2><button v-if="state?.findings.length || result.status === 'completed'" id="export-results" class="text-button" @click="download"><Icon name="download"/>导出记录</button></div><ContentCard v-if="result.status === 'completed'" class="result-card" label="探索结论" content-key="completion" read-key="completion:goal" :body="result.summary" @read="read()"/><article v-else-if="result.status === 'unverified'" class="result-card"><p class="content-text">{{ result.notice }}</p></article><ContentCard v-for="finding in state?.findings || []" :key="finding.id" class="result-card" label="验证记录" :content-key="finding.id" :read-key="'result:' + finding.id" :body="findingBody(finding)" @read="read(finding)"><h3 class="result-title" :title="finding.claim">{{ finding.claim }}</h3><p class="result-scope"><Icon name="code-2"/><code>{{ finding.scope }}</code></p><template #footer><button class="text-button result-locate" :data-focus="'finding:' + finding.id" @click="emit('locate', 'finding:' + finding.id)"><Icon name="locate-fixed"/>画布定位</button></template></ContentCard><div v-if="!state?.findings.length && !['completed', 'unverified'].includes(result.status)" class="empty-state"><Icon name="file-search"/>暂无结果</div></template>
