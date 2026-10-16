<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import type { CandidateDTO, EntityDTO, WorkspaceSnapshot } from '@/api/types';
import { blackboardView } from '../model/blackboard';
import type { DetailRecord } from '../details/detail';
import { selectionDetail } from '../details/detail';

const props = defineProps<{ state: WorkspaceSnapshot }>();
const emit = defineEmits<{ read: [record: DetailRecord]; locate: [key: string] }>();
const assetID = ref('');
const view = computed(() => blackboardView(props.state, assetID.value));
const assets = computed(() => props.state.assets || []);
watch([() => props.state.graph.project.id, () => props.state.graph.project.generation], () => assetID.value = '');
watch(assets, value => { if (!value.some(asset => asset.id === assetID.value)) assetID.value = ''; });
const title = (record: EntityDTO) => record.claim || record.description || record.id;
function read(kind: string, record: EntityDTO) {
  const detail = selectionDetail({id: record.id, type: kind, key: kind + ':' + record.id, label: title(record), raw: record}, 'node', {label: kind, statusLabel: record.status || 'Recorded'}, props.state);
  if (kind === 'candidate') {
    detail.nodeKey = undefined;
    const candidate = record as CandidateDTO;
    detail.meta.push({label: 'Producer judgment', value: candidate.status || 'candidate'});
    if (candidate.supersedes) detail.meta.push({label: 'Supersedes', value: candidate.supersedes});
    detail.body = [candidate.claim, candidate.reason, candidate.sources?.length ? 'Source Facts: ' + candidate.sources.join(', ') : ''].filter(Boolean).join('\n\n');
  }
  emit('read', detail);
}
</script>

<template>
  <section class="blackboard-view">
    <header class="section-heading"><h2 class="section-title">Blackboard</h2><label class="asset-filter">Asset<select v-model="assetID" aria-label="Filter by asset"><option value="">All assets</option><option v-for="asset in assets" :key="asset.id" :value="asset.id">{{ asset.method ? asset.method + ' ' : '' }}{{ asset.value }}</option></select></label></header>
    <p class="board-note">Asset associations help find records. Evidence and its current support determine conclusions.</p>
    <section aria-label="Asset associations"><h3>Asset associations <small>{{ view.records.length }}</small></h3>
      <p v-if="!view.records.length" class="board-note">No associated records in this view.</p>
      <article v-for="item in view.records" :key="item.kind + ':' + item.record.id" class="board-record" :data-record="item.kind + ':' + item.record.id">
        <button class="record-title" @click="read(item.kind, item.record)">{{ title(item.record) }}</button><span class="record-kind">{{ item.kind }} · {{ item.record.id }}</span>
        <ul><li v-for="association in item.associations" :key="association.asset.id + ':' + association.provenance"><code>{{ association.asset.method }} {{ association.asset.value }}</code><span>{{ association.provenance }}</span></li></ul>
        <button v-if="item.kind !== 'candidate'" class="text-button" @click="emit('locate', item.kind + ':' + item.record.id)">Locate on canvas</button>
      </article>
    </section>
    <section aria-label="Producer judgments"><h3>Producer judgments <small>{{ view.candidates.length }}</small></h3><p class="board-note">Retained Candidate judgments are separate from curated Findings. Earlier judgments remain available.</p>
      <p v-if="!view.candidates.length" class="board-note">No producer judgments in this view.</p>
      <article v-for="candidate in view.candidates" :key="candidate.id" class="board-record" :data-candidate="candidate.id"><button class="record-title" @click="read('candidate', candidate)">{{ candidate.claim }}</button><span class="record-kind">{{ candidate.id }} · {{ candidate.status || 'candidate' }}</span><p v-if="candidate.scope">{{ candidate.scope }}</p><p>{{ candidate.reason }}</p><p v-if="candidate.supersedes" class="board-note">Supersedes {{ candidate.supersedes }}</p></article>
    </section>
    <section aria-label="Disputes"><h3>Disputes <small>{{ view.disputes.length }}</small></h3><p v-if="!view.disputes.length" class="board-note">No disputes in this view.</p>
      <article v-for="dispute in view.disputes" :key="dispute.id" class="board-record" :data-dispute="dispute.id"><h4>{{ dispute.question || dispute.id }}</h4><span class="record-kind">{{ dispute.id }} · {{ dispute.status }}</span><p>{{ dispute.reason }}</p><p class="board-note">Finding: {{ dispute.finding_id }} · Candidates: {{ dispute.candidate_ids.join(', ') || 'None' }}</p><p v-if="dispute.review_fact_ids.length">Review Facts: {{ dispute.review_fact_ids.join(', ') }}</p><p v-if="dispute.review_step_ids.length">Review Steps: {{ dispute.review_step_ids.join(', ') }}</p><button v-if="state.findings.some(finding => finding.id === dispute.finding_id)" class="text-button" @click="emit('locate', 'finding:' + dispute.finding_id)">Locate Finding</button></article>
    </section>
    <section v-if="state.curation?.request" aria-label="Curation request" class="board-record"><h3>Curation request</h3><p>{{ state.curation.request.reason }}</p><p>Source Facts: {{ state.curation.request.sources.join(', ') }}</p><p v-if="state.curation.request_review">{{ state.curation.request_review.disposition }} · {{ state.curation.request_review.reason }}</p></section>
  </section>
</template>

<style scoped>
.blackboard-view { display: grid; gap: 24px; }
.blackboard-view h3 { margin: 0 0 12px; font-size: 14px; }
.blackboard-view h3 small { margin-left: 8px; font-weight: 400; }
.asset-filter { display: flex; gap: 8px; align-items: center; min-width: 0; font-size: 12px; }
.asset-filter select { max-width: min(320px, 40vw); padding: 6px 8px; border: 1px solid var(--line); border-radius: 6px; background: var(--surface); color: inherit; }
.board-note, .record-kind { color: var(--muted); font-size: 12px; line-height: 1.6; }
.board-record { border-top: 1px solid var(--line); padding: 14px 0; overflow-wrap: anywhere; }
.board-record p { margin: 8px 0; white-space: pre-wrap; font-size: 13px; }
.board-record h4 { margin: 0; font-size: 14px; }
.record-title { display: block; border: 0; padding: 0; background: none; color: inherit; font: inherit; font-size: 14px; text-align: left; cursor: pointer; }
.record-title:hover { text-decoration: underline; }
.record-kind { display: block; margin-top: 4px; }
.board-record ul { padding-left: 18px; margin: 10px 0; font-size: 12px; }
.board-record li span { display: block; margin-top: 4px; color: var(--muted); }
</style>
