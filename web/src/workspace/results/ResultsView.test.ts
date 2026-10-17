// @vitest-environment happy-dom
import { expect, test, vi } from 'vitest';
import { mount } from '@vue/test-utils';
import ResultsView from './ResultsView.vue';
import { snapshotView } from '../project-resource';

test('finding cards, reader and export retain status and evidence validity', async () => {
  const project = snapshotView({
    graph: {project: {id: 'project', title: 'Project', status: 'active', orchestration_version: 1}}, revision: 1,
    goals: [], steps: [], fact_records: [{id: 'proof', status: 'valid', description: 'Observation'}, {id: 'stale', status: 'valid', support_invalid: true}],
    findings: [
      {id: 'candidate', claim: 'Candidate', status: 'candidate', sources: [], support_valid: false},
      {id: 'refuted', claim: 'Refuted', status: 'refuted', sources: ['proof'], support_valid: true},
      {id: 'withdrawn', claim: 'Withdrawn', status: 'withdrawn', sources: ['proof'], support_valid: true},
      {id: 'unsupported', claim: 'Unsupported', status: 'verified', sources: ['stale']},
    ],
  });
  const wrapper = mount(ResultsView, {props: {project}});
  const labels = {candidate: '待验证', refuted: '已反驳', withdrawn: '已撤回', unsupported: '已验证 · 证据失效'};
  for (const [id, label] of Object.entries(labels)) {
    expect(wrapper.get('[data-content-key="' + id + '"] .content-heading').text()).toContain(label);
    await wrapper.get('[data-content-read="result:' + id + '"]').trigger('click');
    expect(wrapper.emitted('read')?.at(-1)?.[0]).toMatchObject({meta: expect.arrayContaining([{label: '状态', value: label}])});
  }
  expect(wrapper.get('[data-content-key="candidate"] .content-text').text()).not.toContain('支持证据已失效');
  expect(wrapper.get('[data-content-key="unsupported"] .content-text').text()).toContain('支持证据已失效');
  const createURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:results');
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
  await wrapper.get('#export-results').trigger('click');
  const output = await (createURL.mock.calls[0]![0] as Blob).text();
  for (const label of Object.values(labels)) expect(output).toContain('状态：' + label);
  expect(output).not.toContain('undefined');
  await wrapper.setProps({disabled: true});
  await wrapper.get('#export-results').trigger('click');
  expect(createURL).toHaveBeenCalledTimes(1);
  wrapper.unmount();
});
