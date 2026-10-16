// @vitest-environment happy-dom
import { mount } from '@vue/test-utils';
import { expect, test } from 'vitest';
import type { WorkspaceSnapshot } from '@/api/types';
import { blackboardView } from '../model/blackboard';
import { selectionDetail } from '../details/detail';
import BlackboardView from './BlackboardView.vue';

function fixture(): WorkspaceSnapshot {
  return {
    graph: {project: {id: 'project', title: 'Evidence', status: 'active', generation: 2}}, revision: 4,
    goals: [], steps: [{id: 'work', description: 'Inspect endpoint'}],
    fact_records: [{id: 'raw', description: 'Raw observation', status: 'refuted'}, {id: 'derived', description: 'Computed observation', derived_from: ['raw'], support_invalid: true}],
    findings: [{id: 'finding', claim: 'Curated claim', support_valid: false}],
    candidates: [{id: 'note', claim: 'Producer claim', status: 'verified', reason: 'Saved bytes', sources: ['derived'], supersedes: 'earlier'}],
    disputes: [{id: 'dispute', finding_id: 'finding', candidate_ids: ['note'], status: 'open', question: 'Independent check required', reason: 'Conflicting observations', review_step_ids: ['review'], review_fact_ids: ['raw']}],
    assets: [{id: 'a', kind: 'endpoint', value: 'https://example.test/a', method: 'GET'}, {id: 'b', kind: 'endpoint', value: 'https://example.test/b', method: 'GET'}],
    asset_anchors: [{asset_id: 'a', node_kind: 'step', node_id: 'work'}, {asset_id: 'a', node_kind: 'fact', node_id: 'derived', discovery_step_id: 'work'}, {asset_id: 'b', node_kind: 'candidate', node_id: 'note'}, {asset_id: 'b', node_kind: 'finding', node_id: 'finding'}],
  };
}

test('asset filtering preserves discovery provenance and never manufactures evidence or sibling membership', () => {
  const state = fixture(), before = JSON.stringify(state), view = blackboardView(state, 'a');
  expect(view.records.map(item => item.kind + ':' + item.record.id)).toEqual(['step:work', 'fact:derived']);
  expect(view.records[1]?.associations[0]?.provenance).toBe('Discovery via Step work');
  expect(view.records[1]?.record.support_invalid).toBe(true);
  expect(view.candidates).toEqual([]); expect(view.disputes).toEqual([]);
  expect(blackboardView(state, 'b').disputes.map(item => item.id)).toEqual(['dispute']);
  expect(JSON.stringify(state)).toBe(before);
  expect(blackboardView({...state, assets: undefined, asset_anchors: undefined, candidates: undefined, disputes: undefined})).toEqual({records: [], candidates: [], disputes: []});
});

test('Fact details keep derived sources separate from discovery associations', () => {
  const state = fixture();
  const detail = selectionDetail({id: 'derived', type: 'fact', label: 'Computed', raw: state.fact_records[1]}, 'node', undefined, state);
  expect(detail.meta).toContainEqual({label: 'Derived from', value: 'raw'});
  expect(detail.meta.find(item => item.label === 'Asset associations')?.value).toContain('Discovery via Step work');
  expect(detail.evidence).toEqual([]);
});

test('workspace presents judgments and reviews, preserves filters on refresh, and clears them on round change', async () => {
  const state = fixture(), wrapper = mount(BlackboardView, {props: {state}});
  try {
    expect(wrapper.get('[data-candidate="note"]').text()).toContain('Supersedes earlier');
    expect(wrapper.get('[data-dispute="dispute"]').text()).toContain('Review Facts: raw');
    await wrapper.get('[data-candidate="note"] .record-title').trigger('click');
    expect(wrapper.emitted('read')?.[0]?.[0]).toMatchObject({nodeKey: undefined, body: expect.stringContaining('Saved bytes')});
    await wrapper.get('select').setValue('a');
    expect(wrapper.find('[data-candidate="note"]').exists()).toBe(false);
    await wrapper.setProps({state: {...state, revision: 5}});
    expect((wrapper.get('select').element as HTMLSelectElement).value).toBe('a');
    await wrapper.get('[data-record="fact:derived"] .text-button').trigger('click');
    expect(wrapper.emitted('locate')?.[0]).toEqual(['fact:derived']);
    await wrapper.setProps({state: {...state, graph: {project: {...state.graph.project, generation: 3}}}});
    expect((wrapper.get('select').element as HTMLSelectElement).value).toBe('');
    expect(wrapper.find('[data-candidate="note"]').exists()).toBe(true);
  } finally { wrapper.unmount(); }
});
